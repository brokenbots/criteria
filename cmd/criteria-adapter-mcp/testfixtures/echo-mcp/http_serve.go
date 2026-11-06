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
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/brokenbots/criteria/cmd/criteria-adapter-mcp/mcpclient"
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
// per the revision's Server Validation (Mcp-Method against the body method,
// Mcp-Name on name-bearing methods only, Mcp-Param-* mirrors for the
// annotated tool), the shared routing core, and per-mode reply writing. A
// conformance failure is answered with a JSON-RPC error envelope, not a
// plain-text body, so the response stays on the dialect a JSON-RPC client
// parses.
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
	if failure := validateMessageHeaders(r, req); failure != "" {
		writeHeaderViolation(w, req, failure)
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

// headerViolationCode is the JSON-RPC error code a header-conformance
// failure answers with: the revision's Server Validation header-mismatch
// band.
const headerViolationCode = -32020

// writeHeaderViolation answers one header-conformance failure with a 400
// and a JSON-RPC error envelope carrying the mismatch code.
func writeHeaderViolation(w http.ResponseWriter, req request, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	envelope := map[string]any{
		"jsonrpc": "2.0",
		"id":      req.ID,
		"error":   map[string]any{"code": headerViolationCode, "message": message},
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		payload = []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32020,"message":"header conformance"}}`)
	}
	_, _ = w.Write(payload)
}

// nameBearingMethods reports whether the method's params carry a name the
// Mcp-Name header must mirror: params.name for tools/call and prompts/get,
// params.uri for resources/read. Every other method must carry no name.
func nameBearingMethods(method string) bool {
	return method == "tools/call" || method == "prompts/get" || method == "resources/read"
}

// requestName returns the request name in the body's params for this
// method ("" when the method is name-bearing but malformed).
func requestName(req request) string {
	if name, _ := req.Params["name"].(string); name != "" {
		return name
	}
	if uri, _ := req.Params["uri"].(string); uri != "" {
		return uri
	}
	return ""
}

// validateMessageHeaders checks per-message header conformance against the
// body: the method header must equal the body's method; name-bearing
// methods carry exactly one Mcp-Name equaling params.name/params.uri
// (decoded from the Base64 sentinel when present), every other method
// carries none; and an annotated tool's required Mcp-Param-* mirrors must
// match the argument values. Unparseable bodies keep the spec's -32700 path.
func validateMessageHeaders(r *http.Request, req request) string {
	if req.Method != "" && r.Header.Get("Mcp-Method") != req.Method {
		return "Mcp-Method header must match the body method"
	}
	names := r.Header.Values("Mcp-Name")
	requested := ""
	if nameBearingMethods(req.Method) {
		requested = requestName(req)
		if len(names) != 1 {
			return "Mcp-Name header required on " + req.Method
		}
		if got := mcpclient.DecodeHeaderValue(names[0]); got != requested {
			return fmt.Sprintf("Mcp-Name header %q must equal the request name %q", got, requested)
		}
	} else if len(names) != 0 {
		return "Mcp-Name must be absent on " + req.Method
	}
	if req.Method == "tools/call" {
		return validateParamHeaders(r, req)
	}
	return ""
}

// validateParamHeaders checks the x-mcp-header mirror set for one
// tools/call against the argument values at the annotated path: every
// required Mcp-Param-* must be present, decoded, and matching; a mirror
// outside the declared name set is rejected (the annotation's name table
// is closed by additionalProperties: false). Without the annotation knob
// the declared set is empty, so any Mcp-Param-* mirror is rejected.
func validateParamHeaders(r *http.Request, req request) string {
	declared := toolParamNames()
	args, _ := req.Params["arguments"].(map[string]any)
	seen := map[string]string{}
	prefix := http.CanonicalHeaderKey(mcpclient.ParamHeaderPrefix)
	for canonical := range r.Header {
		if !strings.HasPrefix(canonical, prefix) {
			continue
		}
		name := strings.TrimPrefix(canonical, prefix)
		if _, ok := declared[name]; !ok {
			return "Mcp-Param-* header outside the tool's declared mirror set: " + name
		}
		if _, dup := seen[name]; dup {
			return "duplicate Mcp-Param-" + name + " header"
		}
		seen[name] = canonical
	}
	if !headerSchemaMode {
		return ""
	}
	for name := range declared {
		canonical, ok := seen[name]
		if !ok {
			return "missing required Mcp-Param-" + name + " header"
		}
		values := r.Header.Values(canonical)
		if len(values) != 1 {
			return "duplicate Mcp-Param-" + name + " header"
		}
		want, failure := toolParamValue(name, args)
		if failure != "" {
			return failure
		}
		got := mcpclient.DecodeHeaderValue(values[0])
		if got != want {
			return fmt.Sprintf("Mcp-Param-%s %q must equal the argument value %q", name, got, want)
		}
	}
	return ""
}

// toolParamNames is the fixture's annotated-tool mirror declaration when
// MCP_HDR_SCHEMA is set: the trace argument property is annotated, whose
// required X-Trace member (a string) mirrors to Mcp-Param-X-Trace.
func toolParamNames() map[string]string {
	if !headerSchemaMode {
		return nil
	}
	return map[string]string{"X-Trace": "string"}
}

// toolParamValue renders the argument value at the annotated path the way
// the mirrored header encodes it (bools as true/false, JSON numbers as
// shortest round-trip floats, strings verbatim): the server compares the
// decoded header against this form.
func toolParamValue(name string, args map[string]any) (headerValue, note string) {
	trace, _ := args["trace"].(map[string]any)
	if trace == nil {
		return "", "argument carries no trace object to mirror"
	}
	value, ok := trace[name]
	if !ok || value == nil {
		return "", "mirror header " + name + " has no argument value"
	}
	switch v := value.(type) {
	case string:
		return v, ""
	case bool:
		return fmt.Sprintf("%v", v), ""
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64), ""
	case json.Number:
		return v.String(), ""
	default:
		return "", "mirror header " + name + " argument is not a primitive"
	}
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
