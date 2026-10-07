package mcpclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFrameRoundTrip(t *testing.T) {
	payload := []byte(`{"jsonrpc":"2.0","method":"ping"}`)
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer pw.Close()
		if err := writeFrame(pw, payload); err != nil {
			t.Errorf("writeFrame: %v", err)
		}
	}()

	got, err := readFrame(bufio.NewReader(pr))
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch: got %q want %q", string(got), string(payload))
	}
	<-done
}

func TestClientMethodDispatch(t *testing.T) {
	serverRead, clientWrite := io.Pipe()
	clientRead, serverWrite := io.Pipe()
	defer clientWrite.Close()
	defer clientRead.Close()

	var mu sync.Mutex
	notifications := make([]Notification, 0, 1)
	client := New(clientRead, clientWrite, func(n Notification) {
		mu.Lock()
		notifications = append(notifications, n)
		mu.Unlock()
	})
	defer client.Close()

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		reader := bufio.NewReader(serverRead)
		for i := 0; i < 3; i++ {
			payload, err := readFrame(reader)
			if err != nil {
				return
			}
			var req map[string]any
			if err := json.Unmarshal(payload, &req); err != nil {
				return
			}
			method, _ := req["method"].(string)
			id, _ := req["id"].(string)
			switch method {
			case "initialize":
				_ = writeJSON(serverWrite, map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"protocolVersion": "2025-03-26"}})
			case "tools/list":
				_ = writeJSON(serverWrite, map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"tools": []map[string]any{{"name": "echo"}}}})
			case "tools/call":
				_ = writeJSON(serverWrite, map[string]any{"jsonrpc": "2.0", "method": "notifications/progress", "params": map[string]any{"progress": 1, "total": 1}})
				_ = writeJSON(serverWrite, map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"content": []map[string]any{{"type": "text", "text": "ok"}}, "isError": false}})
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := client.Initialize(ctx, "test", "0.0.1"); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	tools, err := client.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("unexpected tools result: %+v", tools)
	}
	result, err := client.CallTool(ctx, "echo", map[string]any{"message": "hi"})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if result.IsError {
		t.Fatal("expected non-error call result")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(notifications) == 0 {
		t.Fatal("expected progress notification callback")
	}
	if notifications[0].Method != "notifications/progress" {
		t.Fatalf("unexpected notification method: %s", notifications[0].Method)
	}

	_ = serverWrite.Close()
	_ = serverRead.Close()
	<-serverDone
}

// TestClientCancelSendsCancelledNotification pins the KB-155 cancellation
// contract: when a caller gives up on an in-flight request, the client
// deletes the pending entry and best-effort notifies the server with
// notifications/cancelled carrying the client-internal JSON-RPC id — so a
// multiplexed server learns the call was abandoned instead of waiting for
// the session to end.
func TestClientCancelSendsCancelledNotification(t *testing.T) {
	serverRead, clientWrite := io.Pipe()
	clientRead, serverWrite := io.Pipe()
	defer clientWrite.Close()
	defer clientRead.Close()

	client := New(clientRead, clientWrite, func(n Notification) {})
	defer client.Close()

	gotCall := make(chan struct{})
	canceled := make(chan map[string]any, 1)
	callID := make(chan string, 1)
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		defer serverWrite.Close()
		defer serverRead.Close()
		reader := bufio.NewReader(serverRead)
		for {
			payload, err := readFrame(reader)
			if err != nil {
				return
			}
			var req map[string]any
			if err := json.Unmarshal(payload, &req); err != nil {
				return
			}
			method, _ := req["method"].(string)
			switch method {
			case "initialize":
				_ = writeJSON(serverWrite, map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": map[string]any{"protocolVersion": "2025-03-26"}})
			case "tools/call":
				id, _ := req["id"].(string)
				callID <- id
				close(gotCall)
			case "notifications/cancelled":
				params, _ := req["params"].(map[string]any)
				canceled <- params
				return
			}
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := client.Initialize(ctx, "test", "0.0.1"); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		_, err := client.CallToolTracked(ctx, "echo", map[string]any{"message": "abandoned"}, "criteria-1")
		errCh <- err
	}()

	select {
	case <-gotCall:
	case <-time.After(2 * time.Second):
		t.Fatal("server never received the tools/call")
	}
	// The call's client-internal JSON-RPC id, before any cancellation.
	wantID := <-callID
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("CallToolTracked error = %v want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CallToolTracked did not settle after cancel")
	}

	select {
	case params := <-canceled:
		if got, _ := params["requestId"].(string); got != wantID {
			t.Fatalf("cancelled requestId = %q want %q", got, wantID)
		}
		if got, _ := params["reason"].(string); !strings.Contains(got, "context canceled") {
			t.Fatalf("cancelled notification reason = %q want context-canceled reason", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server never received notifications/cancelled")
	}
	<-serverDone
}

func writeJSON(w io.Writer, payload map[string]any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return writeFrame(w, data)
}

// writeNDJSON marshals one payload and writes it as a newline-delimited JSON
// frame (the MCP stdio wire shape).
func writeNDJSON(w io.Writer, payload map[string]any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return writeNDJSONTestFrame(w, data)
}

// writeNDJSONTestFrame writes an already-encoded payload as one NDJSON line.
func writeNDJSONTestFrame(w io.Writer, data []byte) error {
	_, err := w.Write(append(append([]byte{}, data...), '\n'))
	return err
}

func TestParseFraming(t *testing.T) {
	cases := []struct {
		in   string
		want Framing
		ok   bool
	}{
		{in: "", want: FramingLSP, ok: true},
		{in: "lsp", want: FramingLSP, ok: true},
		{in: "LSP", want: FramingLSP, ok: true},
		{in: " ndjson ", want: FramingNDJSON, ok: true},
		{in: "NDJSON", want: FramingNDJSON, ok: true},
		{in: "websocket", want: "", ok: false},
		{in: "ndjsonx", want: "", ok: false},
	}
	for _, tc := range cases {
		got, err := ParseFraming(tc.in)
		if tc.ok {
			if err != nil {
				t.Errorf("ParseFraming(%q) unexpected error: %v", tc.in, err)
				continue
			}
			if got != tc.want {
				t.Errorf("ParseFraming(%q) = %q want %q", tc.in, got, tc.want)
			}
			continue
		}
		if err == nil {
			t.Errorf("ParseFraming(%q) = %q want error", tc.in, got)
		}
	}
}

// TestReadFrameAutoDetect pins the read-side framing posture: each frame is
// detected independently, so one stream can mix Content-Length header
// frames, NDJSON lines, and stray garbage without desynchronizing. A
// truncated final payload surfaces as ErrUnexpectedEOF.
func TestReadFrameAutoDetect(t *testing.T) {
	ndjsonPayload := `{"jsonrpc":"2.0","method":"notifications/progress","params":{"progress":1}}`
	lspPayload := `{"jsonrpc":"2.0","method":"ping"}`
	stream := strings.Join([]string{
		"{" + strings.Repeat(`"k":"x",`, 5) + `"end":true}`, // NDJSON line
		"not json at all", // garbage: skipped
		fmt.Sprintf("Content-Length: %d\r\n", len(lspPayload)) + "\r\n" + lspPayload,
		ndjsonPayload,
	}, "\n") + "\n"

	r := bufio.NewReader(strings.NewReader(stream))

	got, err := readFrame(r)
	if err != nil {
		t.Fatalf("frame 1 (ndjson): %v", err)
	}
	if !bytes.Equal(got, []byte("{"+strings.Repeat(`"k":"x",`, 5)+`"end":true}`)) {
		t.Fatalf("frame 1 payload = %q", got)
	}

	got, err = readFrame(r)
	if err != nil {
		t.Fatalf("frame 2 (lsp after garbage): %v", err)
	}
	if !bytes.Equal(got, []byte(lspPayload)) {
		t.Fatalf("frame 2 payload = %q", got)
	}

	got, err = readFrame(r)
	if err != nil {
		t.Fatalf("frame 3 (ndjson after lsp): %v", err)
	}
	if !bytes.Equal(got, []byte(ndjsonPayload)) {
		t.Fatalf("frame 3 payload = %q", got)
	}

	if _, err := readFrame(r); !errors.Is(err, io.EOF) {
		t.Fatalf("final read = %v want io.EOF", err)
	}

	// A header block left truncated (no payload bytes) fails deterministically.
	truncated := "Content-Length: 42\r\n\r\nshort"
	if _, err := readFrame(bufio.NewReader(strings.NewReader(truncated))); err == nil {
		t.Fatal("truncated header-framed body read succeeded, want error")
	}
}

// TestClientNDJSONSession pins the newline-delimited transport end to end:
// with FramingNDJSON writes every request leaves the client as one JSON
// line, the NDJSON-only server runs the full initialize/list/call lane, and
// progress notifications still flow.
func TestClientNDJSONSession(t *testing.T) {
	serverRead, clientWrite := io.Pipe()
	clientRead, serverWrite := io.Pipe()
	defer clientWrite.Close()
	defer clientRead.Close()

	var mu sync.Mutex
	notifications := 0
	client := NewWithFraming(clientRead, clientWrite, FramingNDJSON, func(n Notification) {
		mu.Lock()
		notifications++
		mu.Unlock()
	})
	defer client.Close()

	// NDJSON-only server: reads lines and writes lines, no headers anywhere.
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		defer serverWrite.Close()
		defer serverRead.Close()
		reader := bufio.NewReader(serverRead)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if strings.Contains(line, "Content-Length") {
				t.Errorf("server received header-framed bytes in NDJSON mode: %q", line)
				return
			}
			var req map[string]any
			if err := json.Unmarshal([]byte(line), &req); err != nil {
				t.Errorf("server received unparseable line: %q", line)
				return
			}
			method, _ := req["method"].(string)
			id, _ := req["id"].(string)
			switch method {
			case "initialize":
				_ = writeNDJSON(serverWrite, map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"protocolVersion": "2025-03-26"}})
			case "tools/list":
				_ = writeNDJSON(serverWrite, map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"tools": []map[string]any{{"name": "echo"}}}})
			case "tools/call":
				_ = writeNDJSON(serverWrite, map[string]any{"jsonrpc": "2.0", "method": "notifications/progress", "params": map[string]any{"progress": 1, "total": 1}})
				_ = writeNDJSON(serverWrite, map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"content": []map[string]any{{"type": "text", "text": "ok"}}, "isError": false}})
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Initialize(ctx, "test", "0.0.1"); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	tools, err := client.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("unexpected tools result: %+v", tools)
	}
	result, err := client.CallTool(ctx, "echo", map[string]any{"message": "hi"})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if result.IsError {
		t.Fatal("expected non-error call result")
	}

	mu.Lock()
	defer mu.Unlock()
	if notifications == 0 {
		t.Fatal("expected progress notification callback")
	}

	_ = serverWrite.Close()
	_ = serverRead.Close()
	<-serverDone
}

// TestRPCErrorTyped pins the typed error-code contract: a server error
// response surfaces as *RPCError with the exact code and message bytes
// preserved, so conformance callers can assert spec-band boundaries with
// errors.As instead of string matching.
func TestRPCErrorTyped(t *testing.T) {
	serverRead, clientWrite := io.Pipe()
	clientRead, serverWrite := io.Pipe()
	defer clientWrite.Close()
	defer clientRead.Close()

	client := New(clientRead, clientWrite, nil)
	defer client.Close()

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		defer serverWrite.Close()
		defer serverRead.Close()
		reader := bufio.NewReader(serverRead)
		for {
			payload, err := readFrame(reader)
			if err != nil {
				return
			}
			var req map[string]any
			if err := json.Unmarshal(payload, &req); err != nil {
				return
			}
			if method, _ := req["method"].(string); method == "tools/call" {
				_ = writeJSON(serverWrite, map[string]any{
					"jsonrpc": "2.0",
					"id":      req["id"],
					"error":   map[string]any{"code": -32020, "message": "spec band error: -32020", "data": "detail"},
				})
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := client.CallTool(ctx, "fault", nil)
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("CallTool error = %v (%T) want *RPCError", err, err)
	}
	if rpcErr.Code != -32020 {
		t.Errorf("RPCError.Code = %d want -32020", rpcErr.Code)
	}
	if rpcErr.Message != "spec band error: -32020" {
		t.Errorf("RPCError.Message = %q want exact server message", rpcErr.Message)
	}
	if string(rpcErr.Data) != `"detail"` {
		t.Errorf("RPCError.Data = %s want \"detail\"", rpcErr.Data)
	}

	_ = serverWrite.Close()
	_ = serverRead.Close()
	<-serverDone
}

// TestRPCErrorSessionClose pins the transport-synthesized error: when the
// server half-closes mid-call, pending requests settle with a *RPCError
// carrying the client's code -32000 and the transport failure message.
func TestRPCErrorSessionClose(t *testing.T) {
	serverRead, clientWrite := io.Pipe()
	clientRead, serverWrite := io.Pipe()
	defer clientWrite.Close()
	defer clientRead.Close()

	client := New(clientRead, clientWrite, nil)
	defer client.Close()

	errCh := make(chan error, 1)
	go func() {
		_, err := client.CallTool(context.Background(), "echo", nil)
		errCh <- err
	}()
	// Let the request reach the server, then half-close the stream.
	reader := bufio.NewReader(serverRead)
	if _, err := readFrame(reader); err != nil {
		t.Fatalf("server read: %v", err)
	}
	serverWrite.Close()

	select {
	case err := <-errCh:
		var rpcErr *RPCError
		if !errors.As(err, &rpcErr) {
			t.Fatalf("pending error = %v (%T) want *RPCError", err, err)
		}
		if rpcErr.Code != -32000 {
			t.Errorf("RPCError.Code = %d want -32000", rpcErr.Code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pending call did not settle on session close")
	}
	_ = serverRead.Close()
}
