// fake-copilot is a deterministic stub that speaks the Copilot SDK's
// JSON-RPC 2.0 stdio protocol (Content-Length framing) without requiring the
// real copilot CLI or network access. It is used by the copilot adapter
// conformance test so those tests run in the default (no env-var) lane.
//
// Supported methods:
//   - ping                                             → {protocolVersion:3}
//   - status.get                                      → {version, protocolVersion}
//   - session.create                                  → {sessionId}
//   - session.send                                    → {messageId}; async events follow
//   - session.destroy                                 → {}
//   - session.permissions.handlePendingPermissionRequest → {}
//   - (all other methods)                             → {} (empty success)
//
// session.send behaviour:
//   - Normal prompt (no "fetch"): emits AssistantMessageDelta + AssistantMessage("RESULT: success") + SessionIdle
//   - Permission prompt (contains "fetch"): emits PermissionRequested, waits for
//     handlePendingPermissionRequest, then emits AssistantMessage + SessionIdle.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// rpcMsg is the combined wire shape for both incoming requests and outgoing
// responses / notifications.
type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // absent on notifications
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

var (
	wrMu    sync.Mutex // serialises all writes to os.Stdout
	permsMu sync.Mutex // protects pendingPerms
	// pendingPerms maps a permRequestID to a channel that is closed
	// when session.permissions.handlePendingPermissionRequest arrives.
	pendingPerms = map[string]chan struct{}{}
	evtSeq       int64  // monotonic counter for synthetic event IDs
	permSeq      int64  // monotonic counter for permission request IDs
)

func main() {
	r := bufio.NewReader(os.Stdin)
	for {
		data, err := readFrame(r)
		if err != nil {
			return
		}
		var msg rpcMsg
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		if msg.Method == "" {
			continue // ignore responses
		}
		handleRequest(msg)
	}
}

func handleRequest(msg rpcMsg) {
	switch msg.Method {
	case "ping":
		ts := time.Now().UnixMilli()
		v := 3
		respond(msg.ID, map[string]any{
			"message":         "",
			"timestamp":       ts,
			"protocolVersion": v,
		})

	case "status.get":
		respond(msg.ID, map[string]any{
			"version":         "fake-copilot-0.0.1",
			"protocolVersion": 3,
		})

	case "session.create":
		var p struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		respond(msg.ID, map[string]any{"sessionId": p.SessionID})

	case "session.send":
		handleSessionSend(msg)

	case "session.destroy":
		respond(msg.ID, map[string]any{})

	case "session.permissions.handlePendingPermissionRequest":
		var p struct {
			RequestID string `json:"requestId"`
		}
		_ = json.Unmarshal(msg.Params, &p)

		permsMu.Lock()
		ch := pendingPerms[p.RequestID]
		delete(pendingPerms, p.RequestID)
		permsMu.Unlock()

		if ch != nil {
			close(ch)
		}
		respond(msg.ID, map[string]any{"success": true})

	default:
		// Forward-compatible: unknown calls return an empty success.
		if len(msg.ID) > 0 {
			respond(msg.ID, map[string]any{})
		}
	}
}

// handleSessionSend responds immediately with a messageId and then sends
// async session events in a goroutine.
func handleSessionSend(msg rpcMsg) {
	var p struct {
		SessionID string `json:"sessionId"`
		Prompt    string `json:"prompt"`
	}
	_ = json.Unmarshal(msg.Params, &p)

	const msgID = "fake-msg-1"
	respond(msg.ID, map[string]any{"messageId": msgID})

	if isPermissionPrompt(p.Prompt) {
		// Permission scenario: emit a permission request, wait for the SDK to
		// call handlePendingPermissionRequest, then complete the turn.
		permReqID := newPermID()
		ch := make(chan struct{})
		permsMu.Lock()
		pendingPerms[permReqID] = ch
		permsMu.Unlock()

		go func() {
			sendEvent(p.SessionID, "permission.requested", map[string]any{
				"requestId": permReqID,
				"permissionRequest": map[string]any{
					"kind": "web",
				},
			})
			<-ch // blocks until handlePendingPermissionRequest arrives
			sendEvent(p.SessionID, "assistant.message", map[string]any{
				"messageId":    msgID,
				"content":      "RESULT: success",
				"toolRequests": []any{},
			})
			sendEvent(p.SessionID, "session.idle", map[string]any{})
		}()
	} else {
		// Normal scenario: stream a delta then the final message.
		go func() {
			sendEvent(p.SessionID, "assistant.message_delta", map[string]any{
				"messageId":    msgID,
				"deltaContent": "RESULT: success",
			})
			sendEvent(p.SessionID, "assistant.message", map[string]any{
				"messageId":    msgID,
				"content":      "RESULT: success",
				"toolRequests": []any{},
			})
			sendEvent(p.SessionID, "session.idle", map[string]any{})
		}()
	}
}

// isPermissionPrompt reports whether the prompt should trigger the permission
// request flow rather than the normal streaming response.
func isPermissionPrompt(prompt string) bool {
	return strings.Contains(prompt, "fetch")
}

// newPermID returns a unique permission request ID.
func newPermID() string {
	return fmt.Sprintf("fake-perm-%d", atomic.AddInt64(&permSeq, 1))
}

// sendEvent sends a session.event notification to the SDK.
func sendEvent(sessionID, eventType string, data any) {
	seq := atomic.AddInt64(&evtSeq, 1)
	notify("session.event", map[string]any{
		"sessionId": sessionID,
		"event": map[string]any{
			"id":        fmt.Sprintf("evt-%d", seq),
			"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
			"type":      eventType,
			"data":      data,
		},
	})
}

// notify sends a JSON-RPC 2.0 notification (no id field).
func notify(method string, params any) {
	writeJSON(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
}

// respond sends a JSON-RPC 2.0 response.
func respond(id json.RawMessage, result any) {
	writeJSON(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
}

// writeJSON serialises v and writes it as a Content-Length-framed message.
func writeJSON(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	wrMu.Lock()
	defer wrMu.Unlock()
	_ = writeFrame(os.Stdout, data)
}

// writeFrame writes a Content-Length-framed payload.
func writeFrame(w io.Writer, payload []byte) error {
	if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(payload)); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// readFrame reads a Content-Length-framed payload from r.
func readFrame(r *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if err == io.EOF && line == "" {
				return nil, io.EOF
			}
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(parts[0]), "Content-Length") {
			n, err := strconv.Atoi(strings.TrimSpace(parts[1]))
			if err != nil {
				return nil, err
			}
			length = n
		}
	}
	if length < 0 {
		return nil, fmt.Errorf("missing content-length header")
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
