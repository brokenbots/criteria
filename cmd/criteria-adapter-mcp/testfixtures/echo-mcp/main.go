package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type request struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      any            `json:"id,omitempty"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params,omitempty"`
}

type response struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id,omitempty"`
	Result  any    `json:"result,omitempty"`
	Error   any    `json:"error,omitempty"`
}

// writeMu serializes stdout frames: tools/call requests are answered from
// concurrent goroutines (KB-155), so header+payload pairs must not
// interleave on the shared frame stream.
var writeMu sync.Mutex

// inFlight counts concurrent tools/call requests being served and maxInFlight
// records the observed high-water mark. The progress notification carries both
// so test callers can deterministically prove concurrent fan-out (overlap > 1)
// versus serialized execution (overlap == 1).
var inFlight, maxInFlight atomic.Int64

func main() {
	reader := bufio.NewReader(os.Stdin)
	for {
		payload, err := readFrame(reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			return
		}
		var req request
		if err := json.Unmarshal(payload, &req); err != nil {
			continue
		}

		switch req.Method {
		case "initialize":
			_ = writeResponse(response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
				"protocolVersion": "2025-03-26",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "echo-mcp", "version": "0.2.0"},
			}})
		case "tools/list":
			_ = writeResponse(response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
				"tools": []map[string]any{{
					"name":        "echo",
					"description": "Echoes the argument map as text",
					"inputSchema": map[string]any{"type": "object"},
				}, {
					"name":        "structured",
					"description": "Returns text content plus a structuredContent payload",
					"inputSchema": map[string]any{"type": "object"},
				}},
			}})
		case "tools/call":
			// KB-155: serve each call from its own goroutine so two
			// concurrent calls genuinely overlap (the overlap marker in the
			// progress notification proves it). Requests and responses stay
			// correlated by JSON-RPC id; the bridge multiplexes them per
			// Execute stream.
			inFlight.Add(1)
			n := inFlight.Load()
			for {
				hw := maxInFlight.Load()
				if n <= hw || maxInFlight.CompareAndSwap(hw, n) {
					break
				}
			}
			go func(req request) {
				defer inFlight.Add(-1)
				_ = writeProgress(req)
				_ = writeResponse(handleToolCall(req))
			}(req)
		case "notifications/cancelled":
			// Best-effort notification from the bridge; per-request work is
			// not preempted, the call settles and its late response is
			// dropped by the bridge-side pending map.
		default:
			_ = writeResponse(response{JSONRPC: "2.0", ID: req.ID, Error: map[string]any{"code": -32601, "message": "method not found"}})
		}
	}
}

// writeProgress emits the tools/call progress notification, echoing the
// caller's `_meta.progressToken` when the request carried one (KB-155) and
// stamping the concurrency markers.
func writeProgress(req request) {
	meta, _ := req.Params["_meta"].(map[string]any)
	params := map[string]any{"progress": 1, "total": 1}
	if token, _ := meta["progressToken"].(string); token != "" {
		params["progressToken"] = token
	}
	params["criteria_inflight"] = inFlight.Load()
	params["criteria_overlap"] = maxInFlight.Load()
	_ = writeNotification("notifications/progress", params)
}

// handleToolCall answers a tools/call request. echo returns the argument map
// JSON-encoded as text (plus a resource content block); structured returns
// text plus structuredContent; reserved adapter routing keys must not leak
// into MCP arguments. echo honors a sleep_ms argument (numeric or numeric
// string) to hold the call in flight for overlap tests.
func handleToolCall(req request) response {
	reply := func(result map[string]any) response {
		return response{JSONRPC: "2.0", ID: req.ID, Result: result}
	}
	name, _ := req.Params["name"].(string)
	args, _ := req.Params["arguments"].(map[string]any)
	switch name {
	case "structured":
		return reply(map[string]any{
			"isError":           false,
			"content":           []map[string]any{{"type": "text", "text": "structured payload"}},
			"structuredContent": map[string]any{"count": 2, "items": []string{"a", "b"}},
		})
	case "echo":
		for _, reserved := range []string{"tool", "success_outcome"} {
			if _, leaked := args[reserved]; leaked {
				return reply(map[string]any{
					"isError": true,
					"content": []map[string]any{{"type": "text", "text": "reserved key leaked: " + reserved}},
				})
			}
		}
		if ms := sleepMillis(args["sleep_ms"]); ms > 0 {
			time.Sleep(time.Duration(ms) * time.Millisecond)
		}
		return reply(map[string]any{
			"isError": false,
			"content": []map[string]any{
				{"type": "text", "text": encodeArgs(args)},
				{"type": "resource", "uri": "memory://echo"},
			},
		})
	default:
		return reply(map[string]any{
			"isError": true,
			"content": []map[string]any{{"type": "text", "text": "unknown tool"}},
		})
	}
}

// sleepMillis reads a sleep_ms argument as JSON number or numeric string
// (the bridge passes adapter inputs of type string through verbatim).
func sleepMillis(raw any) int64 {
	switch v := raw.(type) {
	case float64:
		return int64(v)
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

func encodeArgs(args map[string]any) string {
	if len(args) == 0 {
		return "{}"
	}
	b, err := json.Marshal(args)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func writeNotification(method string, params map[string]any) error {
	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return err
	}
	return writeFrame(os.Stdout, payload)
}

func writeResponse(resp response) error {
	payload, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	return writeFrame(os.Stdout, payload)
}

func writeFrame(w io.Writer, payload []byte) error {
	writeMu.Lock()
	defer writeMu.Unlock()
	if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(payload)); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func readFrame(r *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) && line == "" {
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
		return nil, fmt.Errorf("missing content-length")
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	return payload, nil
}
