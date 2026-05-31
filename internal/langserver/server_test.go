package langserver

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

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
