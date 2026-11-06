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
//   - MCP_PIDLOG: appends "pid=<pid>" at startup and, in HTTP mode, the
//     bound "http=<addr>" line (single-server-process proof).
//   - MCP_CALLLOG: appends "call=<name> msg=<message>" per tools/call that
//     actually reached the fixture (denied/unknown calls never appear).
//   - MCP_REPLY_MODE=garbage_once: writes one garbage frame ahead of the
//     first real reply; framed-robust clients skip it.
//   - MCP_PROTOCOL=2026-07-28: speak the stateless core of the 2026-07-28
//     revision. server/discover is served (per-request _meta protocol
//     version is strict: missing/unsupported answers -32022 per request),
//     every result carries resultType ("complete"; "input_required" for the
//     mrtr tool), the initialize handshake is no longer answered, and each
//     successful result stamps _meta serverInfo. Default ("") keeps the
//     legacy 2025-03-26 dialect byte-identical.
//   - MCP_TTL_MS: stamp ttlMs on tools/list and server/discover results
//     (modern mode only).
//   - MCP_METHODLOG: appends "method=<method>" per request in any dialect
//     (legacy handshakes and probes included).
//   - MCP_MRTR=1: adds the "mrtr" tool (modern mode only) answering a
//     SEP-2322 input_required result with one sampling/createMessage
//     input request.
//   - MCP_HTTP_LISTEN=<host:port>: serve the same JSON-RPC core over the
//     Streamable HTTP transport instead of the stdio streams (POST-only
//     endpoint, standard Mcp-Method/Mcp-Name headers required per message,
//     discovery probe + stateless core, subscriptions/listen accepted).
//     The pidlog line "http=<addr>" reports the bound address (use port 0
//     for an ephemeral port). The stdio streams are not served in this mode.
//   - MCP_HTTP_SSE=1: answer every request with text/event-stream (one
//     progress notification event for tools/call, then the reply envelope).
//     MCP_HTTP_SSE=break_once streams the first tools/call progress event
//     and drops the connection before the reply (client-side no-resumability
//     semantics: the in-flight request is lost without tearing the session).
//   - MCP_ID_STYLE=number: post-handshake reply envelopes carry a JSON
//     number id (the request id re-encoded as a number) so clients prove
//     numeric-id normalization. String ids are echoed verbatim otherwise.
//   - MCP_REPLY_MODE=nullid_once: the first post-handshake tools/call is
//     answered with the spec's global-failure shape — a -32600 error with a
//     literal null id — to exercise client id attribution (a null-id reply
//     binds to the sole outstanding request, which must then stay
//     addressable). MCP_REPLY_MODE=batch_once: the first post-handshake
//     tools/call is answered with a JSON-RPC batch array — a client receive
//     violation the bridge must close the session on.
var (
	ndjsonMode  = strings.EqualFold(os.Getenv("MCP_FRAMING"), "ndjson")
	faultCode   = parseFaultCode(os.Getenv("MCP_FAIL_CODE"))
	callLogFile = os.Getenv("MCP_CALLLOG")
	pidLogFile  = os.Getenv("MCP_PIDLOG")
	garbageOnce = strings.EqualFold(os.Getenv("MCP_REPLY_MODE"), "garbage_once")
	modernMode  = os.Getenv("MCP_PROTOCOL") == protocolRevision20260728
	ttlMs       = parseTTLms(os.Getenv("MCP_TTL_MS"))
	methodLog   = os.Getenv("MCP_METHODLOG")
	mrtrTool    = modernMode && strings.EqualFold(os.Getenv("MCP_MRTR"), "1")
	httpListen  = os.Getenv("MCP_HTTP_LISTEN")
	httpSSE     = os.Getenv("MCP_HTTP_SSE")
	idNumbered  = strings.EqualFold(os.Getenv("MCP_ID_STYLE"), "number")

	// mode flags derived from MCP_REPLY_MODE ("" leaves replies untouched).
	replyBatchOnce  = strings.EqualFold(os.Getenv("MCP_REPLY_MODE"), "batch_once")
	replyNullidOnce = strings.EqualFold(os.Getenv("MCP_REPLY_MODE"), "nullid_once")

	// handshakeServed flips once the session's discovery/initialize
	// handshake has been answered; the reply-shaping knobs bend the first
	// post-handshake reply so probes never receive a shaped envelope.
	handshakeServed atomic.Bool
	shapedOnce      atomic.Bool
)

// protocolRevision20260728 is the stateless-core revision the fixture can
// be told to speak.
const protocolRevision20260728 = "2026-07-28"

// metaServerInfoKey is the reserved _meta key under which each result
// carries server identity (stateless core: per result, not per session).
const metaServerInfoKey = "io.modelcontextprotocol/serverInfo"

// metaProtocolVersionKey is the reserved _meta key clients must send on
// every request in the stateless core.
const metaProtocolVersionKey = "io.modelcontextprotocol/protocolVersion"

func parseFaultCode(raw string) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return -32020
	}
	return n
}

// parseTTLms reads MCP_TTL_MS; unset, unparsable, or negative values mean
// "do not stamp ttlMs".
func parseTTLms(raw string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || n <= 0 {
		return 0
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
	if httpListen != "" {
		serveHTTPMode()
		return
	}
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
	req, reply, kind := routeEnvelope(payload)
	switch kind {
	case envKindNotification:
		return
	case envKindToolCall:
		dispatchToolCall(req)
	case envKindBatch:
		// A batch array is one frame like any other payload; the receiving
		// client (never this fixture) treats it as framed receive traffic.
		payload, err := json.Marshal([]map[string]any{reply})
		if err == nil {
			_ = writeFixtureFrame(payload)
		}
	default:
		_ = writeRawResponse(reply)
	}
}

// Envelope kinds routeEnvelope reports for one parsed inbound message.
const (
	envKindReply        = iota // reply carries the answer envelope
	envKindToolCall            // the request is a tools/call for per-transport serving
	envKindNotification        // a request notification: answered by no envelope
	envKindBatch               // the reply is a batch array (reply-mode knob)
)

// routeEnvelope is the JSON-RPC 2.0 routing core shared by both transports:
// it parses one inbound message, applies the revision rules (strict _meta
// protocol version in the stateless core, removed initialize handshake) and
// the reply-shaping knobs, and classifies the message. Transports own reply
// serving (stdio writes frames, optionally from its own goroutine for
// tools/call; http answers on the request goroutine and streams SSE).
func routeEnvelope(payload []byte) (req request, reply map[string]any, kind int) {
	if err := json.Unmarshal(payload, &req); err != nil {
		// Spec-conformant parse handling: an unparseable payload gets a
		// -32700 error response with a null id instead of silence, so a
		// framing-mismatched peer fails deterministically.
		return req, specErrorResponse(nil, -32700, "parse error"), envKindReply
	}
	if req.Method == "" {
		// Valid JSON that is not a request: -32600, id preserved when
		// present, null otherwise.
		return req, specErrorResponse(req.ID, -32600, "invalid request"), envKindReply
	}
	if strings.HasPrefix(req.Method, "notifications/") {
		// Notifications are best-effort and never answered: per-request
		// work is not preempted, the call settles and its late response is
		// dropped by the bridge-side pending map. A -32601 reply with a
		// null id could collide with an in-flight correlated request.
		return req, nil, envKindNotification
	}
	if methodLog != "" {
		// Every request logs, both dialects: assertions must see initialize
		// probes and legacy handshakes too, not just modern traffic.
		appendLog(methodLog, "method="+req.Method)
	}
	if methodLog != "" {
		// Every request logs, both dialects: assertions must see initialize
		// probes and legacy handshakes too, not just modern traffic.
		appendLog(methodLog, "method="+req.Method)
	}
	if shaped := shapeFirstReply(req); shaped != nil {
		kind := envKindReply
		if replyBatchOnce {
			kind = envKindBatch
		}
		return req, styledID(shaped), kind
	}
	if modernMode {
		if !metaVersionOK(req) {
			return req, unsupportedVersionError(req.ID), envKindReply
		}
		if req.Method == "server/discover" {
			reply = envelope(req.ID, "result", discoverResult())
			handshakeServed.Store(true)
			return req, styledID(reply), envKindReply
		}
		if req.Method == "initialize" {
			// The initialize handshake was removed by the stateless core.
			handshakeServed.Store(true)
			return req, specErrorResponse(req.ID, -32601, "method not found: initialize was removed in "+protocolRevision20260728), envKindReply
		}
	}
	switch req.Method {
	case "initialize":
		handshakeServed.Store(true)
		return req, envelope(req.ID, "result", map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "echo-mcp", "version": "0.2.0"},
		}), envKindReply
	case "tools/list":
		list := map[string]any{
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
		}
		if mrtrTool {
			tools := list["tools"].([]map[string]any)
			list["tools"] = append(tools, map[string]any{
				"name":        "mrtr",
				"description": "Answers an interim input_required result (SEP-2322)",
				"inputSchema": map[string]any{"type": "object"},
			})
		}
		if modernMode {
			stampModern(list)
			if ttlMs > 0 {
				list["ttlMs"] = ttlMs
				list["cacheScope"] = "session"
			}
		}
		return req, styledID(envelope(req.ID, "result", list)), envKindReply
	case "tools/call":
		return req, nil, envKindToolCall
	default:
		return req, styledID(envelope(req.ID, "error", map[string]any{"code": -32601, "message": "method not found"})), envKindReply
	}
}

// envelope builds a reply with the id always present (JSON-RPC responses to
// requests carry the id; a nil id marshals as the spec's literal null).
func envelope(id any, key string, val any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, key: val}
}

// styledID re-encodes reply ids under MCP_ID_STYLE=number: a string id that
// parses as an integer is re-encoded as a JSON number, exercising client
// numeric-id normalization. Request ids arrive either way; string ids are
// echoed verbatim otherwise (id preservation).
func styledID(reply map[string]any) map[string]any {
	if idNumbered {
		if s, ok := reply["id"].(string); ok {
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				reply["id"] = n
			}
		}
	}
	return reply
}

// shapeFirstReply bends the first post-handshake tools/call under the
// reply-mode knobs (both transports): batch_once answers it with a batch
// array (a client receive violation), nullid_once with a null-id -32600
// global error (client id attribution). nil means the reply is untouched.
func shapeFirstReply(req request) map[string]any {
	if !handshakeServed.Load() {
		return nil
	}
	if req.Method != "tools/call" || replyBatchOnce == replyNullidOnce {
		return nil
	}
	if !shapedOnce.CompareAndSwap(false, true) {
		return nil
	}
	if replyBatchOnce {
		return envelope(req.ID, "result", map[string]any{})
	}
	return specErrorResponse(nil, -32600, "null-id reply mode")
}

// metaVersionOK enforces the stateless-core rule that every request carries
// its protocol version in _meta.
func metaVersionOK(req request) bool {
	meta, _ := req.Params["_meta"].(map[string]any)
	v, _ := meta[metaProtocolVersionKey].(string)
	return v == protocolRevision20260728
}

// unsupportedVersionError builds the spec-reserved -32022 reply: data names
// the versions the server supports and the version requested.
func unsupportedVersionError(id any) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]any{
			"code":    -32022,
			"message": "unsupported protocol version",
			"data": map[string]any{
				"requested": "",
				"supported": []string{protocolRevision20260728},
			},
		},
	}
}

// discoverResult is the spec-shaped server/discover result: identity and
// capabilities are stateless too — they travel in each result instead of a
// session handshake.
func discoverResult() map[string]any {
	res := map[string]any{
		"resultType":        "complete",
		"supportedVersions": []string{protocolRevision20260728, "2025-03-26"},
		"capabilities":      map[string]any{"tools": map[string]any{}},
		"instructions":      "echo fixture",
		"cacheScope":        "session",
		"_meta": map[string]any{
			metaServerInfoKey: map[string]any{"name": "echo-mcp", "version": "0.3.0"},
		},
	}
	if ttlMs > 0 {
		res["ttlMs"] = ttlMs
	}
	return res
}

// stampModern adds the stateless-core fields to a successful result:
// required resultType plus per-result server identity.
func stampModern(result map[string]any) {
	if result == nil {
		return
	}
	if _, present := result["resultType"]; !present {
		result["resultType"] = "complete"
	}
	result["_meta"] = map[string]any{
		metaServerInfoKey: map[string]any{"name": "echo-mcp", "version": "0.3.0"},
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
		resp := handleToolCall(req)
		if modernMode {
			if m, ok := resp.Result.(map[string]any); ok {
				stampModern(m)
			}
		}
		_ = writeResponse(resp)
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
	_ = writeNotification("notifications/progress", progressParams(req))
}

// progressParams builds the notifications/progress payload for one
// tools/call: the caller's token when present plus the stdio concurrency
// markers. The Streamable HTTP transport reuses it for its SSE progress
// events.
func progressParams(req request) map[string]any {
	meta, _ := req.Params["_meta"].(map[string]any)
	params := map[string]any{"progress": 1, "total": 1}
	if token, _ := meta["progressToken"].(string); token != "" {
		params["progressToken"] = token
	}
	params["criteria_inflight"] = inFlight.Load()
	params["criteria_overlap"] = maxInFlight.Load()
	return params
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
	case "mrtr":
		// SEP-2322 interim result: the server needs more input before it can
		// finish; a full client would retry with inputResponses. The bridge's
		// first-pass posture maps this to a typed call_error.
		return reply(map[string]any{
			"resultType": "input_required",
			"inputRequests": map[string]any{
				"req-1": map[string]any{
					"method": "sampling/createMessage",
				},
			},
			"requestState": "state-req-1",
			"content":      []map[string]any{{"type": "text", "text": "input required before completion"}},
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
