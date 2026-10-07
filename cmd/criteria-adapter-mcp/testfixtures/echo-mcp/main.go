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

// logMu serializes the optional MCP_PIDLOG/MCP_CALLLOG appends.
var logMu sync.Mutex

// inFlight counts concurrent tools/call requests being served and maxInFlight
// records the observed high-water mark. The progress notification carries both
// so test callers can deterministically prove concurrent fan-out (overlap > 1)
// versus serialized execution (overlap == 1).
var inFlight, maxInFlight atomic.Int64

// Fixture-mode knobs, read once at startup:
//   - MCP_FRAMING=ndjson: speak newline-delimited JSON (one JSON object per
//     line) on both directions, strictly. Default ("" or "lsp") speaks the
//     Content-Length header dialect. A framed client writing the wrong shape
//     into either server has its frames rejected with a -32700 parse error.
//   - MCP_FAIL_CODE: fault tool's JSON-RPC error code (default -32020). The
//     fault tool answers tools/call with a JSON-RPC error response — not an
//     isError result — so clients can assert exact error-code boundaries.
//   - MCP_PIDLOG: appends "pid=<pid>" at startup (single-server-process proof).
//   - MCP_CALLLOG: appends "call=<name> msg=<message>" per tools/call that
//     actually reached the fixture (denied/unknown calls never appear).
//   - MCP_REPLY_MODE=garbage_once: writes one garbage frame ahead of the
//     first real reply; framed-robust clients skip it.
var (
	ndjsonMode  = strings.EqualFold(os.Getenv("MCP_FRAMING"), "ndjson")
	faultCode   = parseFaultCode(os.Getenv("MCP_FAIL_CODE"))
	callLogFile = os.Getenv("MCP_CALLLOG")
	pidLogFile  = os.Getenv("MCP_PIDLOG")
	garbageOnce = strings.EqualFold(os.Getenv("MCP_REPLY_MODE"), "garbage_once")
)

func parseFaultCode(raw string) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return -32020
	}
	return n
}

// appendLog appends one line to path; unset paths are a no-op. Logging must
// never fail the fixture (no stderr noise: stderr is the panic channel).
func appendLog(path, line string) {
	if path == "" {
		return
	}
	logMu.Lock()
	defer logMu.Unlock()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	fmt.Fprintf(f, "%s\n", line)
}

func main() {
	appendLog(pidLogFile, fmt.Sprintf("pid=%d", os.Getpid()))
	if garbageOnce {
		// One garbage frame ahead of the first real reply: ndjson mode emits
		// a raw non-JSON line; header mode emits a well-formed frame whose
		// payload is not JSON. Frame-robust clients skip it and keep talking.
		_ = writeFixtureFrame([]byte("not json at all"))
	}
	serve(os.Stdin)
}

// serve reads frames until the peer's stream ends or a framing failure
// surfaces; either ends the session silently (stderr is the panic channel).
func serve(r io.Reader) {
	reader := bufio.NewReader(r)
	for {
		payload, err := readFixtureFrame(reader)
		if err != nil {
			return
		}
		dispatchFrame(payload)
	}
}

func dispatchFrame(payload []byte) {
	var req request
	if err := json.Unmarshal(payload, &req); err != nil {
		// Spec-conformant parse handling: an unparseable payload gets a
		// -32700 error response with a null id instead of silence, so a
		// framing-mismatched peer fails deterministically.
		_ = writeRawResponse(specErrorResponse(nil, -32700, "parse error"))
		return
	}
	if req.Method == "" {
		// Valid JSON that is not a request: -32600, id preserved when
		// present, null otherwise.
		_ = writeRawResponse(specErrorResponse(req.ID, -32600, "invalid request"))
		return
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
			}, {
				"name":        "fault",
				"description": "Answers with a JSON-RPC error response (MCP_FAIL_CODE, default -32020)",
				"inputSchema": map[string]any{"type": "object"},
			}},
		}})
	case "tools/call":
		dispatchToolCall(req)
	case "notifications/cancelled":
		// Best-effort notification from the bridge; per-request work is
		// not preempted, the call settles and its late response is
		// dropped by the bridge-side pending map.
	default:
		_ = writeResponse(response{JSONRPC: "2.0", ID: req.ID, Error: map[string]any{"code": -32601, "message": "method not found"}})
	}
}

// specErrorResponse builds a hand-marshalable error envelope; the nil-typed
// id marshals as the spec-mandated literal null (`id: null` stays present;
// response's omitempty would drop it).
func specErrorResponse(id any, code int, message string) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	}
}

// dispatchToolCall serves tools/call from its own goroutine so two concurrent
// calls genuinely overlap (the overlap marker in the progress notification
// proves it). Requests and responses stay correlated by JSON-RPC id; the
// bridge multiplexes them per Execute stream.
func dispatchToolCall(req request) {
	logToolCall(req)
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
		writeProgress(req)
		_ = writeResponse(handleToolCall(req))
	}(req)
}

// logToolCall appends one call-log line with the serving pid so a caller can
// verify every call in a run was served by ONE server process (KB-161).
func logToolCall(req request) {
	name, _ := req.Params["name"].(string)
	if name == "" {
		return
	}
	args, _ := req.Params["arguments"].(map[string]any)
	line := fmt.Sprintf("pid=%d call=%s", os.Getpid(), name)
	if msg, ok := args["message"].(string); ok {
		line += " msg=" + msg
	}
	appendLog(callLogFile, line)
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
	case "fault":
		// KB-161: JSON-RPC error RESPONSE (not an isError result) so clients
		// can assert exact spec-band error-code boundaries end to end.
		return response{JSONRPC: "2.0", ID: req.ID, Error: map[string]any{
			"code":    faultCode,
			"message": fmt.Sprintf("spec band error: %d", faultCode),
		}}
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
	return writeFixtureFrame(payload)
}

func writeResponse(resp response) error {
	payload, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	return writeFixtureFrame(payload)
}

// writeRawResponse marshals and writes a hand-built response envelope. The
// parse (-32700) and invalid-request (-32600) replies go through here so an
// unknown id marshals as the spec-mandated null (`id: null` stays present;
// response's omitempty would drop it).
func writeRawResponse(env map[string]any) error {
	payload, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return writeFixtureFrame(payload)
}

// writeFixtureFrame writes one already-encoded payload in the fixture's
// selected framing under the stdout lock.
func writeFixtureFrame(payload []byte) error {
	writeMu.Lock()
	defer writeMu.Unlock()
	if ndjsonMode {
		line := make([]byte, 0, len(payload)+1)
		line = append(line, payload...)
		line = append(line, '\n')
		_, err := os.Stdout.Write(line)
		return err
	}
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(payload))
	if _, err := os.Stdout.WriteString(header); err != nil {
		return err
	}
	_, err := os.Stdout.Write(payload)
	return err
}

// readFixtureFrame reads one frame in the fixture's selected framing. In
// ndjson mode every non-blank line is a frame (bad lines are answered with
// -32700 by the main loop); in header mode any framing mismatch is
// surfaced as an error and ends the session.
func readFixtureFrame(r *bufio.Reader) ([]byte, error) {
	if ndjsonMode {
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				if errors.Is(err, io.EOF) {
					t := strings.TrimRight(line, "\r\n")
					if strings.TrimSpace(t) == "" {
						return nil, io.EOF
					}
					return []byte(t), nil
				}
				return nil, err
			}
			t := strings.TrimRight(line, "\r\n")
			if strings.TrimSpace(t) == "" {
				continue
			}
			return []byte(t), nil
		}
	}
	return readFrame(r)
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
