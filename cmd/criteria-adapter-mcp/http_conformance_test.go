package main

// http_conformance_test.go — KB-159: Streamable HTTP bridge conformance end
// to end. OpenSession with transport=http speaks the 2026-07-28 stateless
// core over an httptest MCP fake: the POST contract (Mcp-Method / Mcp-Name /
// Mcp-Protocol-Version plus config custom headers), tools/list discovery,
// the per-request x-mcp-header seam (delivered on the wire, stripped from
// tool arguments, per-call entries winning on collision), SSE response
// streams with progress routing, the no-resumability broken-stream
// semantics (the in-flight call fails, the session survives), non-2xx
// per-call failures, and the batch posture (a receive-side batch is a
// protocol violation that closes the session). The config matrix pins
// parseServerConfig's transport knob validation.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/cmd/criteria-adapter-mcp/mcpclient"
)

// mcpHTTPFake is one fake MCP server speaking the stateless core over
// Streamable HTTP. Every POST records its Mcp-Method header, a header
// snapshot, and the raw body; routing follows the recorded method. The
// tools/call behavior is switched per test through callMode.
type mcpHTTPFake struct {
	mu            sync.Mutex
	seenMcp       map[string][]string              // method -> Mcp-* header snapshots
	seenHdrs      map[string][]map[string][]string // method -> per-call header snapshots
	seenBodies    map[string][]string              // method -> raw bodies
	callMode      int                              // tools/call behavior (toolsCallNormal default)
	callCount     int
	faultCode     int    // 0=off; tools/call answered with this error code (normal + SSE modes)
	idStyle       string // "number": integer reply ids re-encoded as JSON numbers
	nullIDReply   bool   // one-shot: the first tools/call is answered -32600 with a literal null id
	nullIDServed  bool   // nullIDReply consumed
	toolsListJSON string // tools/list answer override (the default serves the flat echo surface)
}

const (
	toolsCallNormal = iota
	toolsCallSSE
	toolsCallBreakStream
	toolsCallStatus500
	toolsCallBatch
)

const fakeHTTPDiscoverResult = `{"resultType":"complete","supportedVersions":["2026-07-28"],"capabilities":{"tools":{}},"ttlMs":30000,"cacheScope":"session"}`

// fakeHTTPToolsListResult is the fake's default tool surface: a flat echo
// tool with the schemaless input schema.
const fakeHTTPToolsListResult = `{"resultType":"complete","ttlMs":30000,"tools":[{"name":"echo","description":"echo input","inputSchema":{"type":"object"}}]}`

func (f *mcpHTTPFake) record(method string, r *http.Request, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.seenMcp == nil {
		f.seenMcp = map[string][]string{}
		f.seenHdrs = map[string][]map[string][]string{}
		f.seenBodies = map[string][]string{}
	}
	sub := map[string][]string{}
	for name, vals := range r.Header {
		key := strings.ToLower(name)
		switch {
		case key == "mcp-method", key == "mcp-name", key == "mcp-protocol-version",
			key == "content-type", key == "accept", key == "x-tenant", key == "x-trace":
			sub[key] = vals
		case strings.HasPrefix(key, "mcp-param-"):
			sub[key] = vals
		}
	}
	f.seenMcp[method] = append(f.seenMcp[method], firstOf(sub, "mcp-method"), firstOf(sub, "mcp-name"), firstOf(sub, "mcp-protocol-version"))
	f.seenHdrs[method] = append(f.seenHdrs[method], sub)
	f.seenBodies[method] = append(f.seenBodies[method], body)
}

// firstOf returns the first value of an optional recorded header ("" when
// absent) — non-name-bearing POSTs carry no Mcp-Name.
func firstOf(sub map[string][]string, key string) string {
	if len(sub[key]) == 0 {
		return ""
	}
	return sub[key][0]
}

func (f *mcpHTTPFake) headers(t *testing.T, method string) []map[string][]string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seenHdrs[method]
}

func (f *mcpHTTPFake) bodies(t *testing.T, method string) []string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seenBodies[method]
}

func (f *mcpHTTPFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "MCP endpoint speaks POST only", http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, maxHTTPTestBody))
	var env struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"params"`
	}
	if json.Unmarshal(body, &env) != nil {
		f.record("unparseable", r, string(body))
		http.Error(w, "bad envelope", http.StatusBadRequest)
		return
	}
	f.record(env.Method, r, string(body))
	switch env.Method {
	case "server/discover":
		f.reply(w, env.ID, fakeHTTPDiscoverResult)
	case "tools/list":
		f.mu.Lock()
		list := f.toolsListJSON
		f.mu.Unlock()
		if list == "" {
			list = fakeHTTPToolsListResult
		}
		f.reply(w, env.ID, list)
	case "tools/call":
		f.mu.Lock()
		f.callCount++
		mode := f.callMode
		call := f.callCount
		fault := f.faultCode
		nullShot := f.nullIDReply && !f.nullIDServed
		f.mu.Unlock()
		if nullShot {
			f.mu.Lock()
			f.nullIDServed = true
			f.mu.Unlock()
			f.replyError(w, json.RawMessage("null"), map[string]any{"code": -32600, "message": "null-id reply mode"})
			return
		}
		// Knob faults bend the normal/SSE/batch reply shapes only; the
		// transport-level modes (500, broken stream) keep their behavior.
		if fault != 0 {
			switch mode {
			case toolsCallNormal:
				f.replyError(w, env.ID, map[string]any{"code": fault, "message": fmt.Sprintf("spec band error: %d", fault)})
				return
			case toolsCallSSE:
				f.replySSEError(w, env.ID, map[string]any{"code": fault, "message": fmt.Sprintf("spec band error: %d", fault)})
				return
			}
		}
		switch mode {
		case toolsCallNormal:
			f.reply(w, env.ID, `{"resultType":"complete","content":[{"type":"text","text":"echoed hi"}]}`)
		case toolsCallSSE:
			f.replySSE(w, env.ID)
		case toolsCallBreakStream:
			f.replySSEBreakStream(w, env.ID, call)
		case toolsCallStatus500:
			http.Error(w, "server exploded", http.StatusInternalServerError)
		case toolsCallBatch:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"jsonrpc":"2.0","id":` + string(env.ID) + `,"result":{}}]`))
		default:
			f.reply(w, env.ID, `{"resultType":"complete","content":[{"type":"text","text":"echoed hi"}]}`)
		}
	case "subscriptions/listen":
		// Reply immediately: the listen id is unknown to the fake, and the
		// envelope settles the subscribe POST (dropped client-side).
		f.reply(w, env.ID, `{}`)
	default:
		if len(env.ID) == 0 || string(env.ID) == "null" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		f.replyError(w, env.ID, map[string]any{"code": -32601, "message": "method not found"})
	}
}

// idFor re-encodes reply ids per idStyle: "number" emits integer ids as
// JSON numbers, exercising the client's numeric-id normalization end to
// end; every other style passes the id through unchanged.
func (f *mcpHTTPFake) idFor(id json.RawMessage) any {
	if f.idStyle == "number" {
		var n int64
		if err := json.Unmarshal(id, &n); err == nil {
			return n
		}
	}
	return rawOrNull(id)
}

func (f *mcpHTTPFake) reply(w http.ResponseWriter, id json.RawMessage, resultJSON string) {
	env := map[string]any{"jsonrpc": "2.0", "id": f.idFor(id), "result": json.RawMessage(resultJSON)}
	raw, _ := json.Marshal(env)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

func (f *mcpHTTPFake) replyError(w http.ResponseWriter, id json.RawMessage, errBody map[string]any) {
	env := map[string]any{"jsonrpc": "2.0", "id": f.idFor(id), "error": errBody}
	raw, _ := json.Marshal(env)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

// replySSE streams one tools/call response as SSE: the progress
// notification first (untokened; the bridge routes a sole in-flight
// execute), then the call's own result envelope.
func (f *mcpHTTPFake) replySSE(w http.ResponseWriter, id json.RawMessage) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	progress, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/progress", "params": map[string]any{"progress": 1, "total": 2}})
	fmt.Fprintf(w, "data: %s\n\n", progress)
	if flusher != nil {
		flusher.Flush()
	}
	complete, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": f.idFor(id), "result": json.RawMessage(`{"resultType":"complete","content":[{"type":"text","text":"streamed"}]}`)})
	fmt.Fprintf(w, "data: %s\n\n", complete)
	if flusher != nil {
		flusher.Flush()
	}
}

// replySSEError delivers an error envelope over the event stream: SSE
// frames carry the same full JSON-RPC envelope language as the JSON leg,
// errors included.
func (f *mcpHTTPFake) replySSEError(w http.ResponseWriter, id json.RawMessage, errBody map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": f.idFor(id), "error": errBody})
	fmt.Fprintf(w, "data: %s\n\n", raw)
	if flusher != nil {
		flusher.Flush()
	}
}

// replySSEBreakStream serves the first tools/call as an SSE stream that
// breaks before settling the call (SEP-2575: no resumability, the in-flight
// request is lost), and later calls normally.
func (f *mcpHTTPFake) replySSEBreakStream(w http.ResponseWriter, id json.RawMessage, call int) {
	if call > 1 {
		// A re-issued call is a brand-new request: with resumability removed
		// the server answers it like any other call.
		f.reply(w, id, `{"resultType":"complete","content":[{"type":"text","text":"recovered"}]}`)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	progress, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/progress", "params": map[string]any{"progress": 1}})
	fmt.Fprintf(w, "data: %s\n\n", progress)
	if flusher != nil {
		flusher.Flush()
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "no hijack", http.StatusInternalServerError)
		return
	}
	conn, _, err := hijacker.Hijack()
	if err != nil {
		return
	}
	_ = conn.Close() // drop the connection with the call still pending
}

func rawOrNull(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

const maxHTTPTestBody = 1 << 20

// openHTTPConformanceSession opens a bridge session with transport=http
// against the fake. Returns the bridge wired for teardown.
func openHTTPConformanceSession(t *testing.T, sessionID string, cfg map[string]string) *MCPBridge {
	t.Helper()
	bridge := &MCPBridge{sessions: map[string]*sessionState{}}
	if _, err := bridge.OpenSession(context.Background(), &v2.OpenSessionRequest{SessionId: sessionID, Config: cfg}); err != nil {
		t.Fatalf("OpenSession(transport=http): %v", err)
	}
	t.Cleanup(func() {
		_, _ = bridge.CloseSession(context.Background(), &v2.CloseSessionRequest{SessionId: sessionID})
	})
	return bridge
}

func TestHTTPConformance_OpenSessionDiscoverAndExecuteWithHeaders(t *testing.T) {
	fake := &mcpHTTPFake{}
	ts := httptest.NewServer(fake)
	t.Cleanup(ts.Close)

	b := openHTTPConformanceSession(t, "http-conf-1", map[string]string{
		"transport": "http",
		"url":       ts.URL,
		"headers":   "X-Tenant: acme",
	})

	// Discovery: the fake's tool surface is advertised by Info.
	info, err := b.Info(context.Background(), &v2.InfoRequest{})
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	names := make([]string, 0, len(info.GetTools()))
	for _, tool := range info.GetTools() {
		names = append(names, tool.GetName())
	}
	if len(names) == 0 || !strings.Contains(strings.Join(names, ","), "echo") {
		t.Fatalf("Info tools = %v, want the fake's discovered surface", names)
	}

	// discovery POSTs carried the MCP transport headers and the config header.
	// The probe carries the announced protocol version (equal to its body
	// _meta) and no Mcp-Name — only name-bearing methods carry one; the
	// client identity rides the body _meta[clientInfo].
	probeHdrs := fake.headers(t, "server/discover")
	if len(probeHdrs) != 1 || probeHdrs[0]["mcp-method"][0] != "server/discover" {
		t.Fatalf("server/discover probe transport headers = %+v", probeHdrs)
	}
	if _, ok := probeHdrs[0]["mcp-name"]; ok {
		t.Fatalf("server/discover must not carry Mcp-Name: %+v", probeHdrs[0])
	}
	if got := probeHdrs[0]["mcp-protocol-version"][0]; got != "2026-07-28" {
		t.Fatalf("the probe announces the requested protocol version, Mcp-Protocol-Version = %q", got)
	}
	probeBody := fake.bodies(t, "server/discover")[0]
	var probeEnv struct {
		Params struct {
			Meta map[string]json.RawMessage `json:"_meta"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(probeBody), &probeEnv); err != nil {
		t.Fatalf("decode server/discover body: %v", err)
	}
	if got := probeEnv.Params.Meta["io.modelcontextprotocol/protocolVersion"]; string(got) != `"2026-07-28"` {
		t.Fatalf("probe body _meta protocolVersion = %s, want the announced version in the Mcp-Protocol-Version header", got)
	}
	if len(probeEnv.Params.Meta["io.modelcontextprotocol/clientInfo"]) == 0 {
		t.Fatalf("the probe carries the client identity in _meta[clientInfo]: %s", probeBody)
	}
	listHdrs := fake.headers(t, "tools/list")
	if len(listHdrs) != 1 || listHdrs[0]["mcp-method"][0] != "tools/list" {
		t.Fatalf("tools/list transport headers = %+v", listHdrs)
	}
	if _, ok := listHdrs[0]["mcp-name"]; ok {
		t.Fatalf("tools/list must not carry Mcp-Name: %+v", listHdrs[0])
	}
	if got := listHdrs[0]["x-tenant"][0]; got != "acme" {
		t.Fatalf("tools/list config header X-Tenant = %q, want acme", got)
	}
	if listHdrs[0]["mcp-protocol-version"][0] != "2026-07-28" {
		t.Fatalf("post-adoption tools/list must carry Mcp-Protocol-Version: %+v", listHdrs[0])
	}

	// Execute: the per-call header rides the tools/call POST, colliding
	// config headers lose, and the header parameters are stripped from
	// the tool arguments.
	sender := &permittingEventSender{bridge: b}
	if err := b.Execute(context.Background(), &v2.ExecuteRequest{
		SessionId: "http-conf-1",
		Input: map[string]string{
			"tool":                  "echo",
			"message":               "hi",
			"success_outcome":       "success",
			"x-mcp-header-X-Trace":  "tok-1",
			"x-mcp-header-X-Tenant": "tenant2",
		},
	}, sender); err != nil {
		t.Fatalf("Execute(echo) over http: %v", err)
	}
	calls := fake.headers(t, "tools/call")
	if len(calls) != 1 {
		t.Fatalf("tools/call POSTs = %d, want 1", len(calls))
	}
	hdr := calls[0]
	if hdr["mcp-method"][0] != "tools/call" || hdr["mcp-name"][0] != "echo" || hdr["mcp-protocol-version"][0] != "2026-07-28" {
		t.Fatalf("tools/call transport headers = %+v", hdr)
	}
	if got := hdr["x-tenant"][0]; got != "tenant2" {
		t.Fatalf("per-call header must win on collision, X-Tenant = %q", got)
	}
	if got := hdr["x-trace"][0]; got != "tok-1" {
		t.Fatalf("per-call header X-Trace = %q, want tok-1", got)
	}
	var body struct {
		Params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	callsb := fake.bodies(t, "tools/call")
	if err := json.Unmarshal([]byte(callsb[len(callsb)-1]), &body); err != nil {
		t.Fatalf("decode tools/call body: %v", err)
	}
	if body.Params.Name != "echo" || body.Params.Arguments["message"] != "hi" {
		t.Fatalf("tools/call arguments = %+v, want echo with message", body.Params.Arguments)
	}
	for key := range body.Params.Arguments {
		if strings.EqualFold(key, "success_outcome") || strings.HasPrefix(strings.ToLower(key), "x-mcp-header") {
			t.Fatalf("tools/call arguments leaked %q", key)
		}
	}

	// Result: success with the echoed text.
	if text := kb155ContentTexts(t, sender.all()); !strings.Contains(text, "echoed hi") {
		t.Fatalf("content events %q missing echoed text", text)
	}
	last := sender.all()[len(sender.all())-1]
	if res := last.GetResult(); res == nil || res.GetOutcome() != "success" {
		t.Fatalf("terminal event = %s, want success result", describeEvents(sender.all()))
	}
}

func TestHTTPConformance_SSEResponseStreamsProgress(t *testing.T) {
	fake := &mcpHTTPFake{callMode: toolsCallSSE}
	ts := httptest.NewServer(fake)
	t.Cleanup(ts.Close)

	b := openHTTPConformanceSession(t, "http-conf-sse", map[string]string{"transport": "http", "url": ts.URL})

	sender := &permittingEventSender{bridge: b}
	if err := b.Execute(context.Background(), &v2.ExecuteRequest{
		SessionId: "http-conf-sse",
		Input:     map[string]string{"tool": "echo", "message": "hi"},
	}, sender); err != nil {
		t.Fatalf("Execute(echo) over SSE response: %v", err)
	}
	events := sender.all()
	sawProgress, sawContent := false, false
	for _, ev := range events {
		if a := ev.GetAdapter(); a != nil && a.GetEventKind() == "mcp.progress" {
			sawProgress = true
		}
		if a := ev.GetAdapter(); a != nil && a.GetEventKind() == "mcp.content" {
			sawContent = true
		}
	}
	if !sawProgress || !sawContent {
		t.Fatalf("SSE response events missing progress/content: %s", describeEvents(events))
	}
	last := events[len(events)-1]
	if res := last.GetResult(); res == nil || res.GetOutcome() != "success" {
		t.Fatalf("terminal event = %s, want success result", describeEvents(events))
	}
}

func TestHTTPConformance_BrokenStreamLosesCallNotSession(t *testing.T) {
	// The fake breaks the first tools/call response stream mid-call
	// (SEP-2575: no server-side resumability — the in-flight request is
	// lost); the bridge reports the per-call failure and the session keeps
	// working: the next execute re-issues a fresh POST and succeeds.
	fake := &mcpHTTPFake{callMode: toolsCallBreakStream}
	ts := httptest.NewServer(fake)
	t.Cleanup(ts.Close)

	b := openHTTPConformanceSession(t, "http-conf-break", map[string]string{"transport": "http", "url": ts.URL})

	sender := &permittingEventSender{bridge: b}
	err := b.Execute(context.Background(), &v2.ExecuteRequest{
		SessionId: "http-conf-break",
		Input:     map[string]string{"tool": "echo", "message": "lost"},
	}, sender)
	if err == nil {
		t.Fatal("the broken response stream must fail the in-flight call")
	}

	err = b.Execute(context.Background(), &v2.ExecuteRequest{
		SessionId: "http-conf-break",
		Input:     map[string]string{"tool": "echo", "message": "again"},
	}, sender)
	if err != nil {
		t.Fatalf("session must survive a lost response stream (second execute): %v", err)
	}
	calls := len(fake.bodies(t, "tools/call"))
	if calls < 2 {
		t.Fatalf("tools/call POSTs = %d, want the lost call re-issued", calls)
	}
	text := kb155ContentTexts(t, sender.all())
	if !strings.Contains(text, "recovered") {
		t.Fatalf("second execute content = %q, want the recovered echo", text)
	}
}

func TestHTTPConformance_StatusFailureIsPerCall(t *testing.T) {
	fake := &mcpHTTPFake{callMode: toolsCallStatus500}
	ts := httptest.NewServer(fake)
	t.Cleanup(ts.Close)

	b := openHTTPConformanceSession(t, "http-conf-500", map[string]string{"transport": "http", "url": ts.URL})

	sender := &permittingEventSender{bridge: b}
	err := b.Execute(context.Background(), &v2.ExecuteRequest{
		SessionId: "http-conf-500",
		Input:     map[string]string{"tool": "echo", "message": "hi"},
	}, sender)
	if err == nil || !strings.Contains(err.Error(), "unexpected status 500") {
		t.Fatalf("Execute over a 500 endpoint = %v, want the typed status failure", err)
	}
}

func TestHTTPConformance_ReceiveBatchClosesSession(t *testing.T) {
	// MCP peers never batch (one envelope per message in both directions):
	// a batched response is a protocol violation, not a multi-reply
	// container — the session closes and further executes fail.
	fake := &mcpHTTPFake{callMode: toolsCallBatch}
	ts := httptest.NewServer(fake)
	t.Cleanup(ts.Close)

	b := openHTTPConformanceSession(t, "http-conf-batch", map[string]string{"transport": "http", "url": ts.URL})

	sender := &permittingEventSender{bridge: b}
	first := b.Execute(context.Background(), &v2.ExecuteRequest{
		SessionId: "http-conf-batch",
		Input:     map[string]string{"tool": "echo", "message": "hi"},
	}, sender)
	if first == nil || !strings.Contains(first.Error(), "batched") {
		t.Fatalf("Execute over a batched response = %v, want the batch violation", first)
	}
	second := b.Execute(context.Background(), &v2.ExecuteRequest{
		SessionId: "http-conf-batch",
		Input:     map[string]string{"tool": "echo", "message": "hi"},
	}, sender)
	if second == nil {
		t.Fatal("executes after the batch violation must fail (session closed)")
	}
}

func TestHTTPConformance_ConfigMatrix(t *testing.T) {
	cases := []struct {
		name    string
		cfg     map[string]string
		wantErr string
	}{
		{"http requires url", map[string]string{"transport": "http"}, "config.url is required"},
		{"http rejects command", map[string]string{"transport": "http", "url": "http://mcp", "command": "x"}, "only valid with transport=stdio"},
		{"http rejects framing", map[string]string{"transport": "http", "url": "http://mcp", "framing": "ndjson"}, "only valid with transport=stdio"},
		{"stdio rejects url", map[string]string{"command": "x", "url": "http://mcp"}, "only valid with transport=http"},
		{"stdio rejects headers", map[string]string{"command": "x", "headers": "A: b"}, "only valid with transport=http"},
		{"stdio rejects timeout", map[string]string{"command": "x", "timeout": "5s"}, "only valid with transport=http"},
		{"initialize+http rejected", map[string]string{"transport": "http", "url": "http://mcp", "handshake": "initialize"}, "not valid with transport=http"},
		{"bad timeout", map[string]string{"transport": "http", "url": "http://mcp", "timeout": "soon"}, "parse timeout"},
		{"zero timeout", map[string]string{"transport": "http", "url": "http://mcp", "timeout": "0s"}, "must be positive"},
		{"malformed header", map[string]string{"transport": "http", "url": "http://mcp", "headers": "BearerBodies"}, "invalid header"},
		{"reserved header", map[string]string{"transport": "http", "url": "http://mcp", "headers": "Accept: text/event-stream"}, "under transport control"},
		{"unknown transport", map[string]string{"transport": "websocket", "command": "x"}, "unknown transport"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseServerConfig(tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("parseServerConfig = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}

	t.Run("stdio defaults", func(t *testing.T) {
		parsed, err := parseServerConfig(map[string]string{"command": "x"})
		if err != nil {
			t.Fatalf("default stdio config: %v", err)
		}
		if parsed.transport != mcpclient.TransportStdio || parsed.framingSet || parsed.negotiation != mcpclient.NegotiationProbeFirst {
			t.Fatalf("stdio defaults = %+v", parsed)
		}
	})
	t.Run("http knobs parse", func(t *testing.T) {
		parsed, err := parseServerConfig(map[string]string{
			"transport": "http", "url": "http://mcp", "headers": "A: b, X-Tenant: acme", "timeout": "250ms",
		})
		if err != nil {
			t.Fatalf("http config: %v", err)
		}
		if parsed.transport != mcpclient.TransportHTTP || parsed.endpoint != "http://mcp" || parsed.timeout != 250*time.Millisecond {
			t.Fatalf("http parse = %+v", parsed)
		}
		if len(parsed.headers) != 2 || parsed.headers[0].Name != "A" || parsed.headers[1].Name != "X-Tenant" {
			t.Fatalf("headers parse = %+v", parsed.headers)
		}
	})
}

func TestHTTPConformance_StdioExecuteRejectsHeaderParams(t *testing.T) {
	// The x-mcp-header seam is Streamable HTTP surface: a stdio session
	// rejects header parameters instead of leaking transport control into
	// tool semantics or silently dropping the intent. The rejection is the
	// typed invalid_header failure, not a bare execute error.
	b := kb158OpenConfig(t, "http-conf-stdio-headers", map[string]string{
		"command": testEchoBin,
		"env":     "MCP_PROTOCOL=2026-07-28",
	})
	sender := &permittingEventSender{bridge: b}
	err := b.Execute(context.Background(), &v2.ExecuteRequest{
		SessionId: "http-conf-stdio-headers",
		Input:     map[string]string{"tool": "echo", "message": "hi", "x-mcp-header-X-Trace": "tok-1"},
	}, sender)
	if err != nil {
		t.Fatalf("Execute with x-mcp-header on stdio = %v, want the typed failure event with nil error", err)
	}
	assertTypedCallError(t, sender, callErrorInvalidHeader, "transport=stdio")
}

func TestHTTPConformance_EmptyHeaderNameRejected(t *testing.T) {
	// A parameter named exactly the x-mcp-header- prefix names no header:
	// forwarded as a tool argument it would leak transport control into
	// tool semantics, contradicting the never-forwarded seam contract. The
	// rejection is the typed invalid_header failure, not a bare execute
	// error.
	fake := &mcpHTTPFake{}
	ts := httptest.NewServer(fake)
	t.Cleanup(ts.Close)

	b := openHTTPConformanceSession(t, "http-conf-empty-header", map[string]string{"transport": "http", "url": ts.URL})
	sender := &permittingEventSender{bridge: b}
	err := b.Execute(context.Background(), &v2.ExecuteRequest{
		SessionId: "http-conf-empty-header",
		Input:     map[string]string{"tool": "echo", "message": "hi", "x-mcp-header-": "v"},
	}, sender)
	if err != nil {
		t.Fatalf("Execute with a bare x-mcp-header- key = %v, want the typed failure event with nil error", err)
	}
	assertTypedCallError(t, sender, callErrorInvalidHeader, "requires a non-empty header name")
}

// TestHTTPConformance_SpecAnnotatedSurfaceAndMirror pins the bridge
// behavior against a spec-shaped annotated surface: the validly annotated
// tool is advertised by Info while the invalidly annotated one is dropped,
// and the mirror rides the bridge's own flat-string argument — one
// Mcp-Param-<Name> header per annotated property present in the arguments,
// the argument itself staying in the call body.
func TestHTTPConformance_SpecAnnotatedSurfaceAndMirror(t *testing.T) {
	fake := &mcpHTTPFake{toolsListJSON: `{"resultType":"complete","ttlMs":30000,"tools":[` +
		`{"name":"mirror","description":"header mirror",` +
		`"inputSchema":{"type":"object","properties":{"region":{"type":"string","x-mcp-header":"Region"}}}},` +
		`{"name":"broken","description":"invalid annotation",` +
		`"inputSchema":{"type":"object","properties":{"count":{"type":"number","x-mcp-header":"Region"}}}}]}`}
	ts := httptest.NewServer(fake)
	t.Cleanup(ts.Close)

	b := openHTTPConformanceSession(t, "http-conf-mirror", map[string]string{"transport": "http", "url": ts.URL})

	// Surface: the validly annotated tool advertised, the invalidly
	// annotated one dropped (an annotation whose declared type is not
	// permitted invalidates the tool; the rest of the list stays).
	info, err := b.Info(context.Background(), &v2.InfoRequest{})
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	names := make([]string, 0, len(info.GetTools()))
	for _, tool := range info.GetTools() {
		names = append(names, tool.GetName())
	}
	if len(names) != 1 || names[0] != "mirror" {
		t.Fatalf("Info tools = %v, want [mirror] (the invalidly annotated tool dropped)", names)
	}

	// The mirror reads the bridge's flat argument at the annotated
	// property: one Mcp-Param-Region carrying the instance value, the
	// argument itself retained in the body.
	sender := &permittingEventSender{bridge: b}
	if err := b.Execute(context.Background(), &v2.ExecuteRequest{
		SessionId: "http-conf-mirror",
		Input:     map[string]string{"tool": "mirror", "message": "hi", "region": "us-west-1"},
	}, sender); err != nil {
		t.Fatalf("Execute(mirror, region=us-west-1): %v", err)
	}
	calls := fake.headers(t, "tools/call")
	if len(calls) != 1 {
		t.Fatalf("tools/call POSTs = %d, want 1", len(calls))
	}
	if got := calls[0]["mcp-param-region"]; len(got) != 1 || got[0] != "us-west-1" {
		t.Fatalf("Mcp-Param-Region = %q, want [us-west-1]: %+v", got, calls[0])
	}
	if _, emitted := calls[0]["mcp-param-x-trace"]; emitted {
		t.Fatalf("no Mcp-Param-* outside the declared set may ride the call: %+v", calls[0])
	}
	bodies := fake.bodies(t, "tools/call")
	var body struct {
		Params struct {
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(bodies[len(bodies)-1]), &body); err != nil {
		t.Fatalf("decode tools/call body: %v", err)
	}
	if body.Params.Arguments["region"] != "us-west-1" {
		t.Fatalf("the mirrored argument must stay in the body, arguments = %+v", body.Params.Arguments)
	}

	// An absent annotated argument mirrors nothing: the call goes out with
	// no Mcp-Param-* and the rest of the arguments untouched.
	sender = &permittingEventSender{bridge: b}
	if err := b.Execute(context.Background(), &v2.ExecuteRequest{
		SessionId: "http-conf-mirror",
		Input:     map[string]string{"tool": "mirror", "message": "hi"},
	}, sender); err != nil {
		t.Fatalf("Execute(mirror, no region): %v", err)
	}
	calls = fake.headers(t, "tools/call")
	if len(calls) != 2 {
		t.Fatalf("tools/call POSTs = %d, want 2", len(calls))
	}
	if _, emitted := calls[1]["mcp-param-region"]; emitted {
		t.Fatalf("an absent annotated argument must emit no Mcp-Param-*: %+v", calls[1])
	}
}

// assertTypedCallError pins the typed invalid_header failure shape: the
// execute stream answered a failure result carrying the reserved call_error
// output with the expected code and a "message" detail (the validation
// message, never a header value).
func assertTypedCallError(t *testing.T, sender *permittingEventSender, wantCode, wantMessagePart string) {
	t.Helper()
	var failure *v2.ExecuteEvent
	for _, ev := range sender.all() {
		if res := ev.GetResult(); res != nil && res.GetOutcome() == "failure" {
			failure = ev
		}
	}
	if failure == nil {
		t.Fatalf("no failure result on the execute stream, want a typed %s failure", wantCode)
	}
	var outputs map[string]any
	if raw := failure.GetResult().GetOutputsJson(); len(raw) > 0 {
		if err := json.Unmarshal(raw, &outputs); err != nil {
			t.Fatalf("failure outputs_json = %q is not an object", raw)
		}
	}
	code, ok := outputs[callErrorOutputKey]
	if !ok || code != wantCode {
		t.Fatalf("failure outputs = %v, want call_error=%s", outputs, wantCode)
	}
	message, _ := outputs["message"].(string)
	if wantMessagePart != "" && !strings.Contains(message, wantMessagePart) {
		t.Fatalf("failure message = %q, want it to contain %q", message, wantMessagePart)
	}
}
