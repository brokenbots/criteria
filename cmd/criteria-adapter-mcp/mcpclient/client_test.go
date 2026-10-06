package mcpclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
