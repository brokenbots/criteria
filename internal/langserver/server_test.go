package langserver

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"
)

func TestInitializeReturnsCapabilities(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	s := newServer()
	s.conn = &stdioConn{
		stdin:  bufio.NewReader(stdinR),
		stdout: stdoutW,
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		msg, err := s.conn.readMessage()
		require.NoError(t, err)
		require.Equal(t, "initialize", msg.Method)

		var params protocol.InitializeParams
		err = json.Unmarshal(msg.Params, &params)
		require.NoError(t, err)

		result := s.handleInitialize(&params)
		require.NotNil(t, result.Capabilities.TextDocumentSync)
		opts, ok := result.Capabilities.TextDocumentSync.(*protocol.TextDocumentSyncOptions)
		require.True(t, ok)
		require.True(t, opts.OpenClose)
		require.NotNil(t, result.Capabilities.DocumentSymbolProvider)
		require.NotNil(t, result.Capabilities.DefinitionProvider)

		err = s.conn.reply(msg.ID, result, nil)
		require.NoError(t, err)
	}()

	// Write initialize request.
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{},
	}
	writeJSON(t, stdinW, req)

	// Read response.
	resp := readResponse(t, stdoutR)
	require.Equal(t, float64(1), resp["id"])
	require.Nil(t, resp["error"])
	result, ok := resp["result"].(map[string]any)
	require.True(t, ok)
	caps, ok := result["capabilities"].(map[string]any)
	require.True(t, ok)
	require.NotNil(t, caps["textDocumentSync"])
	require.NotNil(t, caps["documentSymbolProvider"])
	require.NotNil(t, caps["definitionProvider"])

	_ = stdinW.Close()
	_ = stdoutW.Close()
	<-done
}

func TestShutdownThenExit(t *testing.T) {
	var out strings.Builder
	s := newServer()
	s.conn = &stdioConn{
		stdin:  bufio.NewReader(strings.NewReader("")),
		stdout: &out,
	}

	// shutdown request
	shutdownMsg := &jsonRPCMessage{
		JSONRPC: "2.0",
		ID:      json.RawMessage("2"),
		Method:  "shutdown",
	}
	s.handleMessage(shutdownMsg)

	// exit notification
	exitMsg := &jsonRPCMessage{
		JSONRPC: "2.0",
		Method:  "exit",
	}
	s.handleMessage(exitMsg)

	require.True(t, s.shutdown)

	// Verify the shutdown reply was written.
	output := out.String()
	require.Contains(t, output, "Content-Length:")
	require.Contains(t, output, `"id":2`)
}

// TestEndToEndDidOpenPublishDiagnostics sends initialize → initialized →
// textDocument/didOpen through the JSON-RPC wire and verifies that
// textDocument/publishDiagnostics notifications are produced.
func TestEndToEndDidOpenPublishDiagnostics(t *testing.T) {
	dir := t.TempDir()
	wfPath := filepath.Join(dir, "workflow.hcl")
	src := `workflow {
  name = "test"
  version = "1.0"
  initial_state = "hello"
  target_state = "hello"
}

adapter "noop" "default" {
  config {}
}

step "hello" {
  target = adapter.noop.default
  input { command = "echo hi" }
  outcome "success" { next = state.hello }
}
`
	err := os.WriteFile(wfPath, []byte(src), 0o644)
	require.NoError(t, err)

	stdinR, stdinW := io.Pipe()
	var outBuf bytes.Buffer
	stdoutW := &safeWriter{w: &outBuf}

	s := newServer()
	s.conn = &stdioConn{
		stdin:  bufio.NewReader(stdinR),
		stdout: stdoutW,
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.run()
	}()

	// 1. initialize
	initReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{},
	}
	writeJSON(t, stdinW, initReq)
	// Give the server time to process and respond.
	time.Sleep(100 * time.Millisecond)

	// 2. initialized notification
	writeJSON(t, stdinW, map[string]any{
		"jsonrpc": "2.0",
		"method":  "initialized",
		"params":  map[string]any{},
	})

	// 3. textDocument/didOpen
	writeJSON(t, stdinW, map[string]any{
		"jsonrpc": "2.0",
		"method":  "textDocument/didOpen",
		"params": map[string]any{
			"textDocument": map[string]any{
				"uri":        string(pathToURI(wfPath)),
				"languageId": "hcl",
				"version":    1,
				"text":       src,
			},
		},
	})

	// Wait for diagnostics to be published.
	time.Sleep(200 * time.Millisecond)

	// 4. Signal exit.
	writeJSON(t, stdinW, map[string]any{
		"jsonrpc": "2.0",
		"method":  "exit",
	})
	_ = stdinW.Close()
	<-done

	// Parse all messages from the output buffer.
	notes := readAllNotifications(t, &outBuf)
	require.NotEmpty(t, notes, "expected at least one message from the server")

	// Find the initialize response.
	var initResp map[string]any
	for _, n := range notes {
		if n["id"] != nil && n["method"] == nil {
			initResp = n
			break
		}
	}
	require.NotNil(t, initResp, "expected initialize response")

	// Find publishDiagnostics notification.
	var found bool
	for _, n := range notes {
		m, ok := n["method"].(string)
		if !ok || m != "textDocument/publishDiagnostics" {
			continue
		}
		found = true
		params, ok := n["params"].(map[string]any)
		require.True(t, ok, "publishDiagnostics params should be an object")
		uri, ok := params["uri"].(string)
		require.True(t, ok, "publishDiagnostics should contain uri")
		require.NotEmpty(t, uri)
		break
	}
	require.True(t, found, "expected textDocument/publishDiagnostics notification")
}

// safeWriter wraps an io.Writer with a mutex so concurrent writes from
// diagnostics goroutines are safe.
type safeWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (sw *safeWriter) Write(p []byte) (int, error) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.w.Write(p)
}

func readAllNotifications(t *testing.T, r io.Reader) []map[string]any {
	t.Helper()
	var notes []map[string]any
	reader := bufio.NewReader(r)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "Content-Length: ") {
			continue
		}
		v, err := parseInt(strings.TrimPrefix(line, "Content-Length: "))
		if err != nil {
			continue
		}
		// Skip the empty line after the header.
		_, err = reader.ReadString('\n')
		if err != nil {
			break
		}
		body := make([]byte, v)
		_, err = io.ReadFull(reader, body)
		if err != nil {
			break
		}
		var msg map[string]any
		if err := json.Unmarshal(body, &msg); err != nil {
			continue
		}
		notes = append(notes, msg)
	}
	return notes
}

func writeJSON(t *testing.T, w io.Writer, v map[string]any) {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	_, err = fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(b), b)
	require.NoError(t, err)
}

func readResponse(t *testing.T, r io.Reader) map[string]any {
	t.Helper()
	reader := bufio.NewReader(r)
	var contentLength int
	for {
		line, err := reader.ReadString('\n')
		require.NoError(t, err)
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "Content-Length: ") {
			v, err := parseInt(strings.TrimPrefix(line, "Content-Length: "))
			require.NoError(t, err)
			contentLength = v
		}
	}
	body := make([]byte, contentLength)
	_, err := io.ReadFull(reader, body)
	require.NoError(t, err)

	var resp map[string]any
	err = json.Unmarshal(body, &resp)
	require.NoError(t, err)
	return resp
}

func parseInt(s string) (int, error) {
	var v int
	_, err := fmt.Sscanf(s, "%d", &v)
	return v, err
}
