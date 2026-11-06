package main

// http_serve.go — the Streamable HTTP face of the fixture (MCP_HTTP_LISTEN).
// The same JSON-RPC routing core the stdio loop drives is exposed over
// POST-only HTTP: the standard Mcp-Method / Mcp-Name headers are required on
// every message, the discovery probe runs without a session handshake, and
// replies are application/json by default or text/event-stream under
// MCP_HTTP_SSE (one progress event for tools/call ahead of the reply
// envelope; break_once drops the connection before the first tools/call
// reply to exercise client-side no-resumability semantics).

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
)

// MCP_HTTP_SSE modes.
const (
	sseOff       = ""
	sseOn        = "1"
	sseBreakOnce = "break_once"
)

// breakOnceServed tracks whether the broken-stream drop already happened.
var breakOnceServed atomic.Bool

// serveMu serializes reply writes per connection-independent response
// writers; routeEnvelope's reply shapes are computed per POST so only the
// pidlog appends (already locked) are shared state.
var serveMu sync.Mutex

// serveHTTPMode binds the configured address, reports it on the pidlog, and
// serves until the listener fails (the fixture process stays alive).
func serveHTTPMode() {
	ln, err := net.Listen("tcp", httpListen)
	if err != nil {
		panic(fmt.Sprintf("echo-mcp http listen %s: %v", httpListen, err))
	}
	appendLog(pidLogFile, "http="+ln.Addr().String())
	server := &http.Server{Handler: http.HandlerFunc(handleHTTPRequest)}
	if err := server.Serve(ln); err != nil {
		panic(fmt.Sprintf("echo-mcp http serve: %v", err))
	}
}

// handleHTTPRequest serves one Streamable HTTP message: header conformance
// (Mcp-Method must match the body's method, Mcp-Name must be present),
// the shared routing core, and per-mode reply writing.
func handleHTTPRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "MCP endpoint speaks POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	req, reply, kind := routeEnvelope(body)
	if err := enforceMessageHeaders(r, req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// The fixture has no push streams: a subscribe is accepted and the
	// correlation settles normally (the routing default would reject the
	// method, which the client tolerates but the pidlog must not see).
	if req.Method == "subscriptions/listen" {
		kind = envKindReply
		reply = styledID(envelope(req.ID, "result", map[string]any{}))
	}
	serveEnvelope(w, req, reply, kind)
}

// enforceMessageHeaders checks per-message header conformance: the method
// header must equal the body's method (header/body desync is a 400), and
// the name header must be present (session identity, not authentication).
// Unparseable bodies keep the spec's -32700 path.
func enforceMessageHeaders(r *http.Request, req request) error {
	if req.Method != "" && r.Header.Get("Mcp-Method") != req.Method {
		return errors.New("Mcp-Method header must match the body method")
	}
	if r.Header.Get("Mcp-Name") == "" {
		return errors.New("Mcp-Name header required")
	}
	return nil
}

// serveEnvelope serves one routed message per kind: notifications get 202,
// the batch knob wraps the reply in a one-element array, tools/call serves
// per-call (break-once fault, progress-ahead SSE), everything else is a
// plain reply.
func serveEnvelope(w http.ResponseWriter, req request, reply map[string]any, kind int) {
	switch kind {
	case envKindNotification:
		w.WriteHeader(http.StatusAccepted)
	case envKindBatch:
		serveBatch(w, styledID(reply))
	case envKindToolCall:
		serveToolCall(w, req)
	default:
		writeReplyHTTP(w, req, styledID(reply))
	}
}

// serveBatch writes the batch_once reply: a one-element array as JSON, or
// as an SSE event under the SSE knob.
func serveBatch(w http.ResponseWriter, reply map[string]any) {
	serveMu.Lock()
	defer serveMu.Unlock()
	payload, marshalErr := json.Marshal([]map[string]any{reply})
	if marshalErr != nil {
		http.Error(w, "unencodable reply", http.StatusInternalServerError)
		return
	}
	if httpSSE == sseOn {
		writeSSEFrames(w, [][]byte{payload})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(payload)
}

// serveToolCall serves one tools/call: the break-once fault drops the
// stream mid-transfer on the first call, later calls answer normally.
func serveToolCall(w http.ResponseWriter, req request) {
	logToolCall(req)
	if httpSSE == sseBreakOnce && breakOnceServed.CompareAndSwap(false, true) {
		serveMu.Lock()
		defer serveMu.Unlock()
		writeSSEBreak(w, req)
		return
	}
	resp := handleToolCall(req)
	if modernMode {
		if m, ok := resp.Result.(map[string]any); ok {
			stampModern(m)
		}
	}
	writeReplyHTTP(w, req, styledID(envelopeFromCall(req, resp)))
}

// writeReplyHTTP writes one reply envelope as application/json, or as an
// SSE event stream with the tools/call progress event ahead of the reply.
func writeReplyHTTP(w http.ResponseWriter, req request, env map[string]any) {
	serveMu.Lock()
	defer serveMu.Unlock()
	payload, marshalErr := json.Marshal(env)
	if marshalErr != nil {
		http.Error(w, "unencodable reply", http.StatusInternalServerError)
		return
	}
	if httpSSE == sseOn {
		frames := [][]byte{payload}
		if req.Method == "tools/call" {
			progress, _ := json.Marshal(httpProgressEnvelope(req))
			frames = [][]byte{progress, payload}
		}
		writeSSEFrames(w, frames)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(payload)
}

// envelopeFromCall builds the reply envelope for a served tools/call: the
// error wins when the response carries one (the fault tool), the result
// otherwise.
func envelopeFromCall(req request, resp response) map[string]any {
	if resp.Error != nil {
		return map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": resp.Error}
	}
	return map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": resp.Result}
}

// writeSSEFrames writes pre-encoded payloads as text/event-stream data
// events, flushing between frames.
func writeSSEFrames(w http.ResponseWriter, frames [][]byte) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	for _, frame := range frames {
		fmt.Fprintf(w, "data: %s\n\n", frame)
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// httpProgress is the SSE progress event for a served tools/call. The stdio
// concurrency markers only make sense on the framed stream, so the HTTP
// event carries the plain progress shape (token when the request carried
// one).
func httpProgressEnvelope(req request) map[string]any {
	meta, _ := req.Params["_meta"].(map[string]any)
	params := map[string]any{"progress": 1, "total": 1}
	if token, _ := meta["progressToken"].(string); token != "" {
		params["progressToken"] = token
	}
	return map[string]any{"jsonrpc": "2.0", "method": "notifications/progress", "params": params}
}

// writeSSEBreak streams the call's progress event and then drops the
// connection with the request still outstanding: a conformant client loses
// the in-flight request (no client-side resumability) without losing the
// session.
func writeSSEBreak(w http.ResponseWriter, req request) {
	progress, _ := json.Marshal(httpProgressEnvelope(req))
	fmt.Fprintf(w, "data: %s\n\n", progress)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return
	}
	if conn, _, err := hijacker.Hijack(); err == nil {
		_ = conn.Close()
	}
}
