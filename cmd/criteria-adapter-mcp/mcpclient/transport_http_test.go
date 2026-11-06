package mcpclient

// transport_http_test.go — KB-159: the Streamable HTTP transport (HTTP/SSE)
// per the 2026-07-28 revision. Pinned at the client seam: the request POST
// shape (transport headers, client identity, no session id, POST-only),
// SSE response streaming, the per-call failure of a lost response stream,
// the batched envelope as a session-killing protocol violation, the POST
// notification shape, request-timeout scoping (requests bounded, listen
// not), the subscriptions/listen lifecycle, the SSE parser's line shapes,
// and header validation.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// httpFake is a minimal Streamable HTTP MCP server for transport tests: one
// muxed endpoint answering POSTs per method, optionally in SSE mode, with
// knobs that observe the request shape and inject stream breakage, status
// errors, and protocol errors.
type httpFake struct {
	mu sync.Mutex

	// lastHeaders is the last request's header map.
	lastHeaders http.Header
	// allHeaders is the header map of every request, in arrival order.
	allHeaders []http.Header
	// gotMethods is every request's method ("BATCH" for framed arrays,
	// "BYPASS:<method>" for non-POST traffic), in arrival order.
	gotMethods []string
	// bodies is the raw body of every request, in arrival order.
	bodies [][]byte
	// notes collects notifications delivered to the client callback.
	notes []Notification

	// sse answers tools/call with an SSE stream (progress + result) when
	// set.
	sse bool
	// streamBreak is called mid-SSE-stream for tools/call instead of the
	// final result event when set.
	streamBreak func(w http.ResponseWriter)
	// status forces a non-2xx answer for the methods it names.
	status map[string]int
	// rpcErr forces a JSON-RPC error envelope for the methods it names.
	rpcErr map[string]*RPCError
	// batchPayload answers every request with a JSON-RPC batch envelope.
	batchPayload string
	// holdListen keeps a subscriptions/listen stream open until the
	// client hangs up; without it the stream closes right after open.
	holdListen bool
	// sleepPerCall delays each request response by this long.
	sleepPerCall time.Duration
	// toolsListJSON overrides the canned tools/list answer (for
	// schema-carrying tool lists).
	toolsListJSON string

	notifications      atomic.Int64
	listenAttemptsSeen atomic.Int64
	listenActive       atomic.Bool
}

func (f *httpFake) record(r *http.Request, method string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastHeaders = r.Header.Clone()
	f.allHeaders = append(f.allHeaders, r.Header.Clone())
	f.gotMethods = append(f.gotMethods, method)
	f.bodies = append(f.bodies, append([]byte(nil), body...))
}

// headerBody pairs one captured request's wire shape: its method (as read
// from the envelope), its raw body and its headers.
type headerBody struct {
	method  string
	body    []byte
	headers http.Header
}

// captured returns every captured request's wire shape in arrival order.
func (f *httpFake) captured() []headerBody {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]headerBody, len(f.gotMethods))
	for i := range f.gotMethods {
		out[i] = headerBody{method: f.gotMethods[i], body: append([]byte(nil), f.bodies[i]...), headers: f.allHeaders[i].Clone()}
	}
	return out
}

func (f *httpFake) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func (f *httpFake) body(i int) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.bodies) {
		return nil
	}
	return append([]byte(nil), f.bodies[i]...)
}

func (f *httpFake) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.gotMethods...)
}

func (f *httpFake) sawMethod(method string) bool {
	for _, m := range f.methods() {
		if m == method {
			return true
		}
	}
	return false
}

// lastRequestHeaders returns the headers of the most recent request.
func (f *httpFake) lastRequestHeaders() http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lastHeaders == nil {
		return nil
	}
	return f.lastHeaders.Clone()
}

func (f *httpFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		f.record(r, "BYPASS:"+r.Method, nil)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(r.Body)

	method := "BATCH"
	var id json.RawMessage
	if isJSONBody(body) && body[0] == '{' {
		var envelope struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(body, &envelope)
		id = envelope.ID
		method = envelope.Method
	}
	f.record(r, method, body)

	if code, ok := f.status[method]; ok {
		w.WriteHeader(code)
		return
	}
	if errRPC, ok := f.rpcErr[method]; ok {
		f.replyError(w, id, errRPC)
		return
	}
	if f.batchPayload != "" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(f.batchPayload))
		return
	}

	switch method {
	case "server/discover":
		f.reply(w, id, `{"resultType":"complete","supportedVersions":["2026-07-28"]}`)
	case "tools/list":
		if f.toolsListJSON != "" {
			f.reply(w, id, f.toolsListJSON)
			return
		}
		f.reply(w, id, `{"tools":[{"name":"echo"}]}`)
	case "tools/call":
		if f.sleepPerCall > 0 {
			select {
			case <-time.After(f.sleepPerCall):
			case <-r.Context().Done():
				return
			}
		}
		if !f.sse {
			f.reply(w, id, `{"content":[{"type":"text","text":"ok"}],"isError":false}`)
			return
		}
		f.replySSECall(w, id)
	case "subscriptions/listen":
		f.listenAttemptsSeen.Add(1)
		f.listenActive.Store(true)
		defer f.listenActive.Store(false)
		w.Header().Set("Content-Type", "text/event-stream")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		if f.holdListen {
			<-r.Context().Done()
		}
	default:
		if id == nil {
			// Notifications: any 2xx settles an id-less POST.
			w.WriteHeader(http.StatusAccepted)
			f.notifications.Add(1)
			return
		}
		f.replyError(w, id, &RPCError{Code: -32601, Message: "method not found"})
	}
}

func (f *httpFake) reply(w http.ResponseWriter, id json.RawMessage, resultJSON string) {
	env, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": json.RawMessage(resultJSON)})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(env)
}

func (f *httpFake) replyError(w http.ResponseWriter, id json.RawMessage, errRPC *RPCError) {
	errBody := map[string]any{"code": errRPC.Code, "message": errRPC.Message}
	if len(errRPC.Data) > 0 {
		errBody["data"] = errRPC.Data
	}
	env, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "error": errBody})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(env)
}

func (f *httpFake) replySSECall(w http.ResponseWriter, id json.RawMessage) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	progress, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/progress", "params": map[string]any{"progressToken": "t1", "progress": 1}})
	fmt.Fprintf(w, "data: %s\n\n", progress)
	if flusher != nil {
		flusher.Flush()
	}
	if f.streamBreak != nil {
		f.streamBreak(w)
		return
	}
	complete, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": json.RawMessage(`{"content":[{"type":"text","text":"ok"}],"isError":false}`)})
	fmt.Fprintf(w, "data: %s\n\n", complete)
	if flusher != nil {
		flusher.Flush()
	}
}

// newHTTPFakeClient wires a client to a running fake; mutate adjusts knobs
// (all before the first request) and the config.
func newHTTPFakeClient(t *testing.T, mutate func(fake *httpFake, cfg *HTTPConfig)) (*Client, *httpFake) {
	t.Helper()
	fake := &httpFake{}
	cfg := HTTPConfig{Listen: false}
	if mutate != nil {
		mutate(fake, &cfg)
	}
	ts := httptest.NewServer(fake)
	cfg.Endpoint = ts.URL
	t.Cleanup(ts.Close)
	client := NewHTTPClient(cfg, func(n Notification) {
		fake.mu.Lock()
		fake.notes = append(fake.notes, n)
		fake.mu.Unlock()
	})
	return client, fake
}

// isJSONBody reports whether body begins with a JSON token start.
func isJSONBody(body []byte) bool {
	for _, b := range body {
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		case '{', '[':
			return true
		default:
			return false
		}
	}
	return false
}

func TestParseSSEEventsShapes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"lf separated", "data: one\n\ndata: two\n\n", []string{"one", "two"}},
		{"crlf separated", "data: one\r\n\r\ndata: two\r\n\r\n", []string{"one", "two"}},
		{"cr only separated", "data: one\r\rdata: two\r", []string{"one", "two"}},
		{"multi data joined", "data: one\ndata: two\n\n", []string{"one\ntwo"}},
		{"comment and unknown field ignored", ": ping\nx-custom: hi\ndata: one\n\n", []string{"one"}},
		{"one space stripped", "data:  value\n\n", []string{" value"}},
		{"event id retry dropped", "event: message\nid: 42\nretry: 3000\ndata: one\n\n", []string{"one"}},
		{"no space form", "data:one\n\n", []string{"one"}},
		{"eof flushes trailing event", "data: trailing", []string{"trailing"}},
		{"blank stream", "", nil},
		{"terminated data without blank flushes at eof", "data: one\n", []string{"one"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := []string{}
			err := parseSSEEvents(strings.NewReader(tc.in), func(data []byte) error {
				got = append(got, string(data))
				return nil
			})
			if err != nil {
				t.Fatalf("parseSSEEvents: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("events = %v want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("event[%d] = %q want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestParseSSEEventsAbortsOnFnError(t *testing.T) {
	abort := errors.New("abort")
	err := parseSSEEvents(strings.NewReader("data: one\n\ndata: two\n\n"), func([]byte) error {
		return abort
	})
	if !errors.Is(err, abort) {
		t.Fatalf("err = %v want abort", err)
	}
}

func TestNewHeaderEntryValidation(t *testing.T) {
	valid := []struct{ name, value string }{
		{"X-Trace-Id", "abc123"},
		{"x_lower", "-.value~"},
		{"X-Tab", "a\tb"},
		{"X-Obs", "café"}, // obs-text is allowed
	}
	for _, tc := range valid {
		t.Run(fmt.Sprintf("valid:%s", tc.name), func(t *testing.T) {
			entry, err := NewHeaderEntry(tc.name, tc.value)
			if err != nil {
				t.Fatalf("NewHeaderEntry(%q,%q): %v", tc.name, tc.value, err)
			}
			if entry.Name != tc.name || entry.Value != tc.value {
				t.Errorf("entry = %+v", entry)
			}
		})
	}
	invalid := []struct{ name, value, want string }{
		{"", "v", "empty"},
		{"   ", "v", "empty"},
		{"bad name", "v", "token"},
		{"bad(name)", "v", "token"},
		{"X-Ok", "line\nbreak", "line breaks"},
		{"X-Ok", "line\rbreak", "line breaks"},
		{"X-Ok", "\x01", "control"},
		{"X-Ok", "\x7f", "control"},
		{"Accept", "*/*", "transport control"},
		{"content-type", "text/html", "transport control"},
		{"Content-Length", "9", "transport control"},
		{"host", "evil.example", "transport control"},
		{"Last-Event-ID", "42", "transport control"},
		{"Mcp-Session-Id", "sess", "transport control"},
		{"mcp-method", "tools/call", "transport control"},
		{"MCP-NAME", "evil", "transport control"},
		{"Mcp-Protocol-Version", "2025-01-01", "transport control"},
	}
	for _, tc := range invalid {
		t.Run(fmt.Sprintf("invalid:%s", tc.name), func(t *testing.T) {
			_, err := NewHeaderEntry(tc.name, tc.value)
			if err == nil {
				t.Fatalf("NewHeaderEntry(%q,%q): want error", tc.name, tc.value)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v want mention %q", err, tc.want)
			}
		})
	}
}

func TestHTTPTransportRefusesRedirects(t *testing.T) {
	// A 3xx is an MCP error surface, not a follow instruction: replaying
	// the POST cross-host would forward custom header values to another
	// server and turn the POST into a GET, so the client keeps the 3xx as
	// the final response and the caller sees the status error.
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the redirect target must never receive a replayed request")
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(target.Close)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	t.Cleanup(ts.Close)

	client := NewHTTPClient(HTTPConfig{Endpoint: ts.URL, Listen: false}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := client.Connect(ctx, NegotiationDiscover, "criteria-test", "v1")
	if err == nil || !strings.Contains(err.Error(), "unexpected status 302") {
		t.Fatalf("Connect over a redirecting endpoint = %v, want the 302 kept as the final response", err)
	}
}

func TestHTTPTransportRequestShape(t *testing.T) {
	client, fake := newHTTPFakeClient(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Connect(ctx, NegotiationDiscover, "criteria", "0.1.0"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	result, err := client.CallToolTracked(ctx, "echo", map[string]any{"message": "hi"}, "t1", []HeaderEntry{{Name: "X-Trace-Id", Value: "abc"}})
	if err != nil {
		t.Fatalf("CallToolTracked: %v", err)
	}
	if result.IsError {
		t.Fatal("CallToolTracked: isError result")
	}

	// Every exchange is a POST to the single endpoint; a GET is a
	// 2026-07-28-removed shape this transport never issues.
	if fake.sawMethod("BYPASS:GET") {
		t.Fatalf("client issued a GET: %v", fake.methods())
	}
	if fake.requestCount() == 0 {
		t.Fatal("no requests captured")
	}

	// Last request = tools/call: the standard headers carry the tool name
	// (the request's params.name) in Mcp-Name — never the client identity —
	// and the negotiated protocol version.
	callHeaders := fake.lastRequestHeaders() // last request = tools/call
	for name, want := range map[string]string{
		"Content-Type":         "application/json",
		"Accept":               "application/json, text/event-stream",
		"Mcp-Method":           "tools/call",
		"Mcp-Name":             "echo",
		"Mcp-Protocol-Version": ProtocolVersion20260728,
		"X-Trace-Id":           "abc",
	} {
		if got := callHeaders.Get(name); got != want {
			t.Errorf("%s = %q want %q", name, got, want)
		}
	}
	// The 2026-07-28 revision removed the session id and resumability
	// headers; this client must never send them.
	if got := callHeaders.Get("Mcp-Session-Id"); got != "" {
		t.Errorf("Mcp-Session-Id = %q, must never be sent", got)
	}
	if got := callHeaders.Get("Last-Event-ID"); got != "" {
		t.Errorf("Last-Event-ID = %q, must never be sent", got)
	}
}

// capturedMetaProtocolVersion extracts _meta[MetaProtocolVersion] from a
// captured request body when the body carries it.
func capturedMetaProtocolVersion(t *testing.T, body []byte) (string, bool) {
	t.Helper()
	var env struct {
		Params struct {
			Meta map[string]json.RawMessage `json:"_meta"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode captured body %q: %v", body, err)
	}
	raw, ok := env.Params.Meta[MetaProtocolVersion]
	if !ok {
		return "", false
	}
	var version string
	if err := json.Unmarshal(raw, &version); err != nil {
		t.Fatalf("decode captured _meta protocolVersion %q: %v", raw, err)
	}
	return version, true
}

func TestHTTPTransportHeaderInvariantsOnEveryPost(t *testing.T) {
	// One POST, one header posture, every time: Mcp-Protocol-Version from
	// the announced/negotiated version — present on every POST including
	// the server/discover probe, and equal to the body's _meta version so
	// header and body cannot diverge; Accept with both media types;
	// Mcp-Name only on name-bearing methods, carrying the request's
	// params.name (the tool name), never the client identity.
	client, fake := newHTTPFakeClient(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Connect(ctx, NegotiationDiscover, "criteria", "0.1.0"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if _, err := client.ListTools(ctx); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if _, err := client.CallToolTracked(ctx, "echo", map[string]any{"message": "hi"}, "t1", nil); err != nil {
		t.Fatalf("CallToolTracked: %v", err)
	}
	if err := client.Notification(ctx, "notifications/initialized", nil); err != nil {
		t.Fatalf("Notification: %v", err)
	}

	reached := map[string]int{}
	for i, rec := range fake.captured() {
		reached[rec.method]++
		if got := rec.headers.Get("Accept"); got != "application/json, text/event-stream" {
			t.Fatalf("POST %d (%s) Accept = %q, want both media types", i, rec.method, got)
		}
		version := rec.headers.Get("Mcp-Protocol-Version")
		if version != ProtocolVersion20260728 {
			t.Fatalf("POST %d (%s) Mcp-Protocol-Version = %q, want %q on every POST (probe included)", i, rec.method, version, ProtocolVersion20260728)
		}
		if bodyVersion, ok := capturedMetaProtocolVersion(t, rec.body); ok && bodyVersion != version {
			t.Fatalf("POST %d (%s) header version %q != body _meta version %q", i, rec.method, version, bodyVersion)
		}
		if rec.method == "tools/call" {
			if got := rec.headers.Get("Mcp-Name"); got != "echo" {
				t.Fatalf("POST %d (tools/call) Mcp-Name = %q, want the tool name %q", i, got, "echo")
			}
			continue
		}
		if got := rec.headers.Get("Mcp-Name"); got != "" {
			t.Fatalf("POST %d (%s) carried Mcp-Name %q; a non-name-bearing method must omit it", i, rec.method, got)
		}
	}
	for _, method := range []string{"server/discover", "tools/list", "tools/call", "notifications/initialized"} {
		if reached[method] == 0 {
			t.Fatalf("no POST captured for %s; the invariant sweep never exercised it", method)
		}
	}
}

func TestHTTPTransportUnsafeToolNameSentinelHeader(t *testing.T) {
	// A tool name outside the header-safe set travels in the Base64
	// sentinel in Mcp-Name while the body keeps the raw name: the header
	// encoding never alters the request itself.
	client, fake := newHTTPFakeClient(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Connect(ctx, NegotiationDiscover, "criteria-test", "0.1.0"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if _, err := client.CallToolTracked(ctx, "a\tb", map[string]any{"message": "hi"}, "t1", nil); err != nil {
		t.Fatalf("CallToolTracked: %v", err)
	}
	recs := fake.captured()
	var call headerBody
	found := false
	for _, rec := range recs {
		if rec.method == "tools/call" {
			call = rec
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no tools/call POST captured: %v", fake.methods())
	}
	if got := call.headers.Get("Mcp-Name"); got != "=?base64?YQli?=" {
		t.Fatalf("Mcp-Name = %q, want the Base64 sentinel of the unsafe name", got)
	}
	var body struct {
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if err := json.Unmarshal(call.body, &body); err != nil {
		t.Fatalf("decode tools/call body: %v", err)
	}
	if body.Params.Name != "a\tb" {
		t.Fatalf("body params.name = %q, want the raw unencoded tool name", body.Params.Name)
	}
}

func TestHTTPTransportPreConnectRequestRejected(t *testing.T) {
	// A call before Connect has no negotiated protocol version, and the
	// mandatory Mcp-Protocol-Version header and the body _meta must share
	// one source (the announced version) — so the transport rejects the
	// pre-Connect POST instead of sending a request whose headers say
	// nothing about the dialect. The client identity is not the gate: it
	// rides _meta[clientInfo] and must not gate any request.
	client, fake := newHTTPFakeClient(t, nil)
	_, err := client.CallTool(context.Background(), "echo", nil)
	if err == nil || !strings.Contains(err.Error(), "no negotiated protocol version") {
		t.Fatalf("pre-Connect CallTool err = %v, want the negotiated-version rejection", err)
	}
	if got := fake.requestCount(); got != 0 {
		t.Fatalf("a pre-Connect call issued %d POSTs, want none", got)
	}
	if name := fake.lastRequestHeaders().Get("Mcp-Name"); name == "criteria" {
		t.Fatalf("a pre-Connect POST carried the client identity as Mcp-Name")
	}
}

func TestHTTPTransportCancelSendsNoCancelledNotification(t *testing.T) {
	// On Streamable HTTP the abandoned call's response stream close is the
	// cancellation signal; the revision expects no notifications/cancelled
	// after it. The client must not follow a cancelled http call with a
	// notifications/cancelled POST (stdio keeps sending one).
	client, fake := newHTTPFakeClient(t, func(fake *httpFake, cfg *HTTPConfig) {
		fake.sleepPerCall = 300 * time.Millisecond
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := client.Connect(ctx, NegotiationDiscover, "criteria-test", "0.1.0"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	callCtx, callCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer callCancel()
	if _, err := client.CallTool(callCtx, "echo", nil); err == nil {
		t.Fatal("the abandoned call must fail with its context error")
	}
	time.Sleep(100 * time.Millisecond)
	if fake.sawMethod("notifications/cancelled") {
		t.Fatalf("a cancelled http call POSTed notifications/cancelled: %v", fake.methods())
	}
	if !fake.sawMethod("tools/call") {
		t.Fatalf("the call never reached the peer: %v", fake.methods())
	}
}

func TestHTTPTransportStatusErrorIsPerCall(t *testing.T) {
	client, _ := newHTTPFakeClient(t, func(fake *httpFake, cfg *HTTPConfig) {
		fake.status = map[string]int{"tools/call": http.StatusInternalServerError}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Connect(ctx, NegotiationDiscover, "criteria", "0.1.0"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	_, err := client.CallTool(ctx, "echo", nil)
	if err == nil || !strings.Contains(err.Error(), "unexpected status 500") {
		t.Fatalf("CallTool err = %v, want status-500 per-call error", err)
	}
	// The failure is per-call: the session still works.
	if _, err := client.ListTools(ctx); err != nil {
		t.Fatalf("ListTools after status failure: %v", err)
	}
}

func TestHTTPTransportSSEResponseStreamsProgress(t *testing.T) {
	client, fake := newHTTPFakeClient(t, func(fake *httpFake, cfg *HTTPConfig) {
		fake.sse = true
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Connect(ctx, NegotiationDiscover, "criteria", "0.1.0"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	result, err := client.CallToolTracked(ctx, "echo", nil, "t1", nil)
	if err != nil {
		t.Fatalf("CallToolTracked: %v", err)
	}
	if result.IsError {
		t.Fatal("CallToolTracked: isError result")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	found := false
	for _, n := range fake.notes {
		if n.Method == "notifications/progress" {
			found = true
		}
	}
	if !found {
		t.Fatalf("progress notification lost streaming the SSE result: %+v", fake.notes)
	}
}

// TestHTTPTransportLostStreamKillsCallNotSession pins the SEP-2575
// posture: a response stream that breaks before settling the call fails
// that call alone — the request is lost and must be re-issued as a new
// request with a new id — and the session remains usable afterwards.
func TestHTTPTransportLostStreamKillsCallNotSession(t *testing.T) {
	client, _ := newHTTPFakeClient(t, func(fake *httpFake, cfg *HTTPConfig) {
		fake.sse = true
		fake.streamBreak = func(w http.ResponseWriter) {
			// Break exactly once: the retry rides a healthy stream.
			fake.streamBreak = nil
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, _ := hj.Hijack()
				_ = conn.Close()
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Connect(ctx, NegotiationDiscover, "criteria", "0.1.0"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if _, err := client.CallTool(ctx, "echo", nil); err == nil {
		t.Fatal("CallTool on broken stream: want error")
	} else if !strings.Contains(err.Error(), "response stream ended without a response") {
		t.Errorf("broken-stream error = %v, want stream-ended-or-read-error shape", err)
	}
	// The session survives: a fresh request re-issues with a new id.
	result, err := client.CallTool(ctx, "echo", nil)
	if err != nil {
		t.Fatalf("CallTool after lost stream: %v", err)
	}
	if result.IsError {
		t.Fatal("CallTool after lost stream: isError result")
	}
}

func TestHTTPTransportBatchEnvelopeKillsSession(t *testing.T) {
	client, _ := newHTTPFakeClient(t, func(fake *httpFake, cfg *HTTPConfig) {
		fake.batchPayload = `[{"jsonrpc":"2.0","id":7,"method":"ping"}]`
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The probe answer is a batch: a protocol violation that closes the
	// session.
	if err := client.Connect(ctx, NegotiationDiscover, "criteria", "0.1.0"); err == nil {
		t.Fatal("Connect against batch peer: want error")
	} else if !errors.Is(err, errBatchedTraffic) {
		t.Errorf("Connect err = %v, want errBatchedTraffic", err)
	}
	// A closed session fails every later request.
	if _, err := client.CallTool(ctx, "echo", nil); !errors.Is(err, io.EOF) {
		t.Errorf("post-batch CallTool err = %v, want EOF (closed session)", err)
	}
}

func TestHTTPTransportNotificationIsIDlessPOST(t *testing.T) {
	client, fake := newHTTPFakeClient(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Connect(ctx, NegotiationDiscover, "criteria", "0.1.0"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	before := fake.requestCount()
	if err := client.Notification(ctx, "notifications/cancelled", map[string]any{"requestId": "7"}); err != nil {
		t.Fatalf("Notification: %v", err)
	}
	if got := fake.notifications.Load(); got != 1 {
		t.Fatalf("notification POSTs = %d want 1", got)
	}
	envBody := fake.body(before)
	if len(envBody) == 0 {
		t.Fatal("notification body not captured")
	}
	var env map[string]any
	if err := json.Unmarshal(envBody, &env); err != nil {
		t.Fatalf("decode notification: %v", err)
	}
	if _, hasID := env["id"]; hasID {
		t.Errorf("notification envelope carries id: %v", env)
	}
	if env["method"] != "notifications/cancelled" {
		t.Errorf("notification method = %v", env["method"])
	}
}

// TestHTTPTransportRequestTimeoutBoundsCallNotListen pins the scoping of
// the timeout knob: every request and notification POST is bounded, the
// subscriptions/listen stream's lifetime is the session's, not one call's.
func TestHTTPTransportRequestTimeoutBoundsCallNotListen(t *testing.T) {
	client, fake := newHTTPFakeClient(t, func(fake *httpFake, cfg *HTTPConfig) {
		fake.holdListen = true
		fake.sleepPerCall = time.Second
		cfg.Listen = true
		cfg.RequestTimeout = 100 * time.Millisecond
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Connect(ctx, NegotiationDiscover, "criteria", "0.1.0"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	_, err := client.CallTool(ctx, "echo", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("slow CallTool err = %v, want request deadline", err)
	}
	if fake.listenAttemptsSeen.Load() != 1 {
		t.Errorf("listen attempts seen = %d want 1 (timed-out requests must not reopen the listen stream)", fake.listenAttemptsSeen.Load())
	}
	// The listen stream must still be held open.
	waitFor(t, 2*time.Second, func() bool { return fake.listenActive.Load() },
		"listen stream not held open (bounded by the request timeout)")
	client.Close()
	waitFor(t, 2*time.Second, func() bool { return !fake.listenActive.Load() },
		"listen stream still held after Close")
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}

// TestHTTPTransportListenGivesUpWithoutResumability pins the bounded
// listen reopen posture of the 2026-07-28 revision: each stream break
// re-opens subscriptions/listen, but only listenAttempts times and without
// any Last-Event-ID carry-over — notifications stop when the attempts run
// out while requests keep working.
func TestHTTPTransportListenGivesUpWithoutResumability(t *testing.T) {
	client, fake := newHTTPFakeClient(t, func(fake *httpFake, cfg *HTTPConfig) {
		cfg.Listen = true
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Connect(ctx, NegotiationDiscover, "criteria", "0.1.0"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fake.listenAttemptsSeen.Load() >= listenAttempts {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := fake.listenAttemptsSeen.Load(); got < listenAttempts {
		t.Fatalf("listen attempts seen = %d want >= %d", got, listenAttempts)
	}
	if _, err := client.ListTools(ctx); err != nil {
		t.Fatalf("ListTools after listen gave up: %v", err)
	}
}

func TestConnectHTTPNegotiationSurface(t *testing.T) {
	t.Run("initialize rejected on http", func(t *testing.T) {
		client, _ := newHTTPFakeClient(t, nil)
		err := client.Connect(context.Background(), NegotiationInitialize, "criteria", "0.1.0")
		if err == nil || !strings.Contains(err.Error(), "initialize handshake is not valid") {
			t.Fatalf("err = %v, want initialize-invalid-on-http error", err)
		}
	})
	t.Run("unsupported version is deterministic", func(t *testing.T) {
		client, _ := newHTTPFakeClient(t, func(fake *httpFake, cfg *HTTPConfig) {
			fake.rpcErr = map[string]*RPCError{
				"server/discover": {Code: ErrorCodeUnsupportedProtocolVersion, Message: "unsupported", Data: []byte(`{"supported":["2025-03-26"]}`)},
			}
		})
		err := client.Connect(context.Background(), NegotiationDiscover, "criteria", "0.1.0")
		if err == nil || !strings.Contains(err.Error(), "no mutually supported protocol version") {
			t.Fatalf("err = %v, want deterministic version failure", err)
		}
	})
	t.Run("legacy answer is terminal over http", func(t *testing.T) {
		client, fake := newHTTPFakeClient(t, func(fake *httpFake, cfg *HTTPConfig) {
			fake.rpcErr = map[string]*RPCError{
				"server/discover": {Code: -32601, Message: "method not found"},
			}
		})
		err := client.Connect(context.Background(), NegotiationProbeFirst, "criteria", "0.1.0")
		if err == nil || !strings.Contains(err.Error(), "terminal on Streamable HTTP") {
			t.Fatalf("err = %v, want terminal probe failure", err)
		}
		// No legacy fallback: the initialize handshake is never POSTed.
		if fake.sawMethod("initialize") {
			t.Errorf("client fell back to initialize over http: %v", fake.methods())
		}
	})
}

// TestStdioTransportRejectsPerCallHeaders pins the x-mcp-header boundary
// at the client seam too: per-request HTTP headers are Streamable HTTP
// surface, and the stdio transport refuses them rather than smuggling
// transport control into a framed pipe.
func TestStdioTransportRejectsPerCallHeaders(t *testing.T) {
	serverRead, clientWrite := io.Pipe()
	clientRead, serverWrite := io.Pipe()
	defer clientWrite.Close()
	defer clientRead.Close()
	client := New(clientRead, clientWrite, nil)
	defer client.Close()

	// Minimal legacy server: answer initialize so the session opens.
	go func() {
		payload, err := readFrame(bufio.NewReader(serverRead))
		if err != nil {
			return
		}
		var req map[string]any
		_ = json.Unmarshal(payload, &req)
		if req["method"] == "initialize" {
			_ = writeJSON(serverWrite, map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": map[string]any{"protocolVersion": "2025-03-26"}})
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Initialize(ctx, "criteria", "0.1.0"); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	_, err := client.CallToolTracked(ctx, "echo", nil, "", []HeaderEntry{{Name: "X-Trace-Id", Value: "abc"}})
	if err == nil || !strings.Contains(err.Error(), "not valid on the stdio transport") {
		t.Fatalf("stdio CallToolTracked with headers err = %v, want rejection", err)
	}
}

const annotatedToolListJSON = `{"tools":` +
	`[{"name":"echo","inputSchema":{"type":"object","properties":{` +
	`"trace":{"type":"object","x-mcp-header":{},"additionalProperties":false,` +
	`"required":["X-Trace"],"properties":{"X-Trace":{"type":"string"}}}}}}]}`

func TestHTTPTransportParamHeadersMirroredOnCall(t *testing.T) {
	// The tool's x-mcp-header schema annotation binds the X-Trace header to
	// the trace argument property: the transport mirrors it as
	// Mcp-Param-X-Trace on the tools/call POST, with the caller carrying no
	// explicit header.
	client, fake := newHTTPFakeClient(t, func(fake *httpFake, cfg *HTTPConfig) {
		fake.toolsListJSON = annotatedToolListJSON
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Connect(ctx, NegotiationDiscover, "criteria-test", "0.1.0"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	list, err := client.ListToolsDetailed(ctx)
	if err != nil {
		t.Fatalf("ListToolsDetailed: %v", err)
	}
	if len(list.Tools) != 1 || list.Tools[0].Name != "echo" {
		t.Fatalf("ListToolsDetailed = %+v, want the advertised echo tool", list.Tools)
	}

	if _, err := client.CallToolTracked(ctx, "echo", map[string]any{
		"trace": map[string]any{"X-Trace": "present"},
	}, "", nil); err != nil {
		t.Fatalf("CallToolTracked: %v", err)
	}
	recs := fake.captured()
	var call headerBody
	found := false
	for _, rec := range recs {
		if rec.method == "tools/call" {
			call = rec
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no tools/call captured; requests: %v", fake.methods())
	}
	if got := call.headers.Get("Mcp-Param-X-Trace"); got != "present" {
		t.Fatalf("Mcp-Param-X-Trace = %q, want %q", got, "present")
	}
	if got := call.headers.Values("Mcp-Param-X-Trace"); len(got) != 1 {
		t.Fatalf("Mcp-Param-X-Trace emitted %d times, want exactly one", len(got))
	}

	// A member absent from the arguments is not emitted: the server's own
	// required-Mcp-Param-* rule governs missing mirrors.
	if _, err := client.CallToolTracked(ctx, "echo", map[string]any{}, "", nil); err != nil {
		t.Fatalf("CallToolTracked (absent member): %v", err)
	}
	last := fake.lastRequestHeaders()
	if vals := last.Values("Mcp-Param-X-Trace"); len(vals) != 0 {
		t.Fatalf("absent member emitted Mcp-Param-X-Trace %v, want none", vals)
	}
}

func TestHTTPTransportInvalidAnnotatedToolDroppedFromSurface(t *testing.T) {
	// A tool whose annotation is not statically resolvable has no
	// mirrorable path: it is dropped from the advertised surface while the
	// rest of the list stays.
	const list = `{"tools":[` +
		`{"name":"broken","inputSchema":{"type":"object","properties":{` +
		`"trace":{"type":"object","oneOf":[{"type":"object","x-mcp-header":{},` +
		`"properties":{"X-T":{"type":"string"}}}]}}}},` +
		`{"name":"echo"}]}`
	client, fake := newHTTPFakeClient(t, func(fake *httpFake, cfg *HTTPConfig) {
		fake.toolsListJSON = list
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Connect(ctx, NegotiationDiscover, "criteria-test", "0.1.0"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	listing, err := client.ListToolsDetailed(ctx)
	if err != nil {
		t.Fatalf("ListToolsDetailed: %v", err)
	}
	names := make([]string, 0, len(listing.Tools))
	for _, tool := range listing.Tools {
		names = append(names, tool.Name)
	}
	if len(names) != 1 || names[0] != "echo" {
		t.Fatalf("surface = %v, want [echo] (the invalidly annotated tool dropped)", names)
	}
	if fake.sawMethod("tools/call") {
		t.Fatalf("unexpected tools/call during surface validation")
	}
}
