package mcpclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
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

func writeJSON(w io.Writer, payload map[string]any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return writeFrame(w, data)
}
