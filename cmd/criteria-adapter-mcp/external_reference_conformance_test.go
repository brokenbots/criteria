package main

// external_reference_conformance_test.go — JSON-RPC 2.0 conformance vectors
// against a REAL, non-fixture MCP server. Opt-in so the deterministic suite
// stays hermetic: skip cleanly when the environment is unset.
//
//	MCP_REFERENCE_STDIO_CMD  — command line (whitespace split) of a stdio
//	                           MCP server speaking newline-delimited JSON
//	                           (e.g. "node .../server-everything/dist/index.js stdio")
//	MCP_REFERENCE_HTTP_URL   — endpoint URL of a Streamable HTTP MCP server
//	                           (e.g. "http://127.0.0.1:3001/mcp" for
//	                           server-everything streamableHttp)
//	MCP_REFERENCE_TOOL / MCP_REFERENCE_TOOL_ARGS — optional tools/call
//	    vector: tool name plus a JSON object of call arguments
//
// The stdio integration leg also drives the real mcpclient pipeline
// (probe-first open with legacy fallback) against the external server.
//
// Revision split across the HTTP legs (each leg names the dialect it
// speaks): the raw-wire HTTP leg drives the reference server's published
// dialect — the legacy initialize handshake, @modelcontextprotocol/
// server-everything's 2025-06-18 — and pins wire vectors, not transport
// conformance. The transport HTTP leg
// (TestExternalReferenceConformance_HTTPTransport) drives mcpclient's
// Streamable HTTP path, which speaks the 2026-07-28 stateless core, so it
// runs only against a reference endpoint that answers the server/discover
// probe and skips naming the dialect mismatch otherwise; a legacy-dialect
// reference server is a skip, never a failure claim. Both HTTP legs build
// their standard header set with mcpclient.StandardHeaders so the
// hand-rolled wire leg cannot carry a divergent header shape.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"

	"github.com/brokenbots/criteria/cmd/criteria-adapter-mcp/mcpclient"
)

// refProtocolVersion is the published MCP protocol revision the external
// reference servers speak (@modelcontextprotocol/server-everything 1.0.0
// echoes 2025-06-18 through the initialize negotiation); raw-wire vectors
// pin that dialect, not the in-tree 2026-07-28 stateless one.
const refProtocolVersion = "2025-06-18"

const (
	envRefStdioCmd  = "MCP_REFERENCE_STDIO_CMD"
	envRefHTTPURL   = "MCP_REFERENCE_HTTP_URL"
	envRefTool      = "MCP_REFERENCE_TOOL"
	envRefToolArgs  = "MCP_REFERENCE_TOOL_ARGS"
	refProbeTimeout = 8 * time.Second
)

// refEnvelope is one raw JSON-RPC message parsed off a reference server.
// Field access stays key-based: reference SDKs vary the serialization
// order (result-first is common).
type refEnvelope map[string]any

func (e refEnvelope) hasError() (code float64, ok bool) {
	errObj, ok := e["error"].(map[string]any)
	if !ok {
		return 0, false
	}
	code, ok = errObj["code"].(float64)
	return code, ok
}

// refIDEqual compares a wire id against the sent id across the JSON number
// (float64) and string spellings.
func refIDEqual(want, got any) bool {
	switch w := want.(type) {
	case string:
		g, _ := got.(string)
		return w != "" && g == w
	case int:
		g, ok := got.(float64)
		return ok && float64(w) == g
	}
	return false
}

// callJSON encodes a request envelope; nil params are omitted entirely —
// some reference servers (server-everything tools/list) never answer a
// request whose params is explicit null.
func callJSON(id any, method string, params map[string]any) map[string]any {
	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	return msg
}

func notificationJSON(method string, params map[string]any) map[string]any {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	return msg
}

// referenceLeg is the raw-wire surface every universal vector runs on: a
// request envelope exchange bound to its id plus an id-less notification.
type referenceLeg interface {
	call(ctx context.Context, id any, method string, params map[string]any) (refEnvelope, error)
	notify(ctx context.Context, method string, params map[string]any) error
}

// runReferenceUniversalVectors is the transport-independent JSON-RPC
// conformance core: id handling, capability shape, and the error code band.
func runReferenceUniversalVectors(t *testing.T, leg referenceLeg, opts map[string]bool) {
	t.Helper()

	t.Run("id_echo_numeric", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), refProbeTimeout)
		defer cancel()
		env, err := leg.call(ctx, 4400, "tools/list", nil)
		if err != nil {
			t.Fatalf("tools/list id=4400: %v", err)
		}
		if !refIDEqual(4400, env["id"]) {
			t.Fatalf("reply id = %v (%T), want the numeric request id echoed", env["id"], env["id"])
		}
	})

	t.Run("id_echo_string", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), refProbeTimeout)
		defer cancel()
		env, err := leg.call(ctx, "ref-string-id", "tools/list", nil)
		if err != nil {
			t.Fatalf("tools/list id=ref-string-id: %v", err)
		}
		if !refIDEqual("ref-string-id", env["id"]) {
			t.Fatalf("reply id = %v (%T), want the string request id echoed", env["id"], env["id"])
		}
	})

	if !opts["skipCapability"] {
		t.Run("tools_list_nonempty", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), refProbeTimeout)
			defer cancel()
			env, err := leg.call(ctx, "cap", "tools/list", nil)
			if err != nil {
				t.Fatalf("tools/list: %v", err)
			}
			result, _ := env["result"].(map[string]any)
			tools, _ := result["tools"].([]any)
			if len(tools) == 0 {
				t.Fatalf("tools/list returned %d tools, want a non-empty reference surface", len(tools))
			}
		})
	}

	t.Run("unknown_method_not_found", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), refProbeTimeout)
		defer cancel()
		env, err := leg.call(ctx, "nf", "server/nonexistent_reference_method", nil)
		if err != nil {
			t.Fatalf("unknown method call: %v", err)
		}
		code, hasErr := env.hasError()
		if !hasErr || code != -32601 {
			t.Fatalf("unknown method error = %v, want -32601 method-not-found", env["error"])
		}
	})

	t.Run("notification_roundtrip", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), refProbeTimeout)
		defer cancel()
		if err := leg.notify(ctx, "notifications/cancelled", map[string]any{
			"requestId": 12345,
		}); err != nil {
			t.Fatalf("notification accepted: %v", err)
		}
		// The session must keep working after the id-less notification.
		env, err := leg.call(ctx, "after-note", "tools/list", nil)
		if err != nil {
			t.Fatalf("tools/list after notification: %v", err)
		}
		if !refIDEqual("after-note", env["id"]) {
			t.Fatalf("post-notification reply id = %v, want the request id echoed", env["id"])
		}
	})

	if tool := os.Getenv(envRefTool); tool != "" {
		t.Run("tool_call", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), refProbeTimeout)
			defer cancel()
			args := map[string]any{}
			if raw := os.Getenv(envRefToolArgs); raw != "" {
				if err := json.Unmarshal([]byte(raw), &args); err != nil {
					t.Fatalf("parse %s: %v", envRefToolArgs, err)
				}
			}
			env, err := leg.call(ctx, "call-tool", "tools/call", map[string]any{"name": tool, "arguments": args})
			if err != nil {
				t.Fatalf("tools/call(%s): %v", tool, err)
			}
			if _, hasErr := env.hasError(); hasErr {
				t.Fatalf("tools/call(%s) error = %v", tool, env["error"])
			}
			if _, hasResult := env["result"]; !hasResult {
				t.Fatalf("tools/call(%s) missing result: %v", tool, env)
			}
		})
	}
}

// TestExternalReferenceConformance_Stdio drives the raw NDJSON stdio wire
// of a real reference MCP server: newline-delimited requests, id matching
// that tolerates server-initiated notifications between replies, the
// stateless-per-line read posture (a corrupt line loses only itself), and
// the batch posture of the 2025-06-18 spec (batching removed: members
// stay unanswered, the session keeps working).
func TestExternalReferenceConformance_Stdio(t *testing.T) {
	cmdline := os.Getenv(envRefStdioCmd)
	if cmdline == "" {
		t.Skipf("set %s to run the external reference-server conformance leg", envRefStdioCmd)
	}

	leg, closeServer := startReferenceStdio(t, cmdline)
	defer closeServer()

	ctx, cancel := context.WithTimeout(context.Background(), refProbeTimeout)
	defer cancel()
	if err := leg.initialize(ctx); err != nil {
		t.Fatalf("initialize %q: %v", cmdline, err)
	}

	runReferenceUniversalVectors(t, leg, nil)

	t.Run("batch_stays_unanswered", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), refProbeTimeout)
		defer cancel()
		if err := leg.raw([]any{
			map[string]any{"jsonrpc": "2.0", "id": 31, "method": "tools/list"},
			map[string]any{"jsonrpc": "2.0", "id": "32", "method": "tools/list"},
		}); err != nil {
			t.Fatalf("send batch: %v", err)
		}
		// Batch members must never be answered on the stdio wire (the
		// 2025-06-18 revision removed batching); tolerate unrelated noise
		// (server-initiated notifications, null-id error replies) while
		// the window is open, then prove the session still works.
		for elapsed := time.Duration(0); elapsed < 1500*time.Millisecond; elapsed += 25 * time.Millisecond {
			select {
			case env := <-leg.lines:
				if refIDEqual(31, env["id"]) || refIDEqual("32", env["id"]) {
					t.Fatalf("batch member answered on the stdio wire: %v", env)
				}
			default:
				time.Sleep(25 * time.Millisecond)
			}
		}
		env, err := leg.call(ctx, "after-batch", "tools/list", nil)
		if err != nil {
			t.Fatalf("tools/list after batch: %v", err)
		}
		if !refIDEqual("after-batch", env["id"]) {
			t.Fatalf("post-batch reply id = %v, want the request id echoed", env["id"])
		}
	})

	t.Run("corrupt_line_loses_only_itself", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), refProbeTimeout)
		defer cancel()
		if err := leg.raw("{corrupt"); err != nil {
			t.Fatalf("send corrupt line: %v", err)
		}
		env, err := leg.call(ctx, "post-corrupt", "tools/list", nil)
		if err != nil {
			t.Fatalf("tools/list after a corrupt line: %v", err)
		}
		if !refIDEqual("post-corrupt", env["id"]) {
			t.Fatalf("post-corrupt reply id = %v, want the request id echoed", env["id"])
		}
	})
}

// TestExternalReferenceConformance_StdioBridge drives the real mcpclient
// pipeline (bridge config parse, probe-first open with the legacy
// initialize fallback, tools/list discovery, an Execute round trip) end to
// end against the external reference stdio server.
func TestExternalReferenceConformance_StdioBridge(t *testing.T) {
	cmdline := os.Getenv(envRefStdioCmd)
	if cmdline == "" {
		t.Skipf("set %s to run the external reference-server conformance leg", envRefStdioCmd)
	}

	// The bridge config carries the server binary and its CSV args
	// separately; a multi-token reference command line splits across them.
	fields := strings.Fields(cmdline)
	cfg := map[string]string{"command": fields[0]}
	if len(fields) > 1 {
		cfg["args"] = strings.Join(fields[1:], ",")
	}
	b := kb158OpenConfig(t, "ext-ref-stdio", cfg)
	info, err := b.Info(context.Background(), &v2.InfoRequest{})
	if err != nil {
		t.Fatalf("Info over the external stdio reference server: %v", err)
	}
	if len(info.GetTools()) == 0 {
		t.Fatal("Info tools empty, want the external reference server's discovered surface")
	}
	if tool := os.Getenv(envRefTool); tool != "" {
		sender := &permittingEventSender{bridge: b}
		input := map[string]string{"tool": tool}
		if raw := os.Getenv(envRefToolArgs); raw != "" {
			var args map[string]any
			if json.Unmarshal([]byte(raw), &args) != nil {
				t.Fatalf("parse %s: %v", envRefToolArgs, err)
			}
			for k, v := range args {
				input[k] = fmt.Sprint(v)
			}
		}
		if err := b.Execute(context.Background(), &v2.ExecuteRequest{
			SessionId: "ext-ref-stdio",
			Input:     input,
		}, sender); err != nil {
			t.Fatalf("Execute(%s) over the external stdio reference server: %v", tool, err)
		}
	}
}

// referenceStdio is the raw line-dialect leg: a spawned stdio MCP server
// whose wire is newline-delimited JSON in both directions.
type referenceStdio struct {
	cmdline string
	proc    *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	lines   chan refEnvelope
	stderr  bytes.Buffer
}

// startReferenceStdio spawns a stdio reference server and hands back the
// raw line-dialect leg plus its teardown closer.
func startReferenceStdio(t *testing.T, cmdline string) (leg *referenceStdio, closeServer func()) {
	t.Helper()
	fields := strings.Fields(cmdline)
	if len(fields) == 0 {
		t.Fatalf("%s is empty", envRefStdioCmd)
	}
	leg = &referenceStdio{cmdline: cmdline, lines: make(chan refEnvelope, 64)}
	leg.proc = exec.Command(fields[0], fields[1:]...)
	leg.proc.Stderr = &leg.stderr
	in, err := leg.proc.StdinPipe()
	if err != nil {
		t.Fatalf("stdio stdin pipe: %v", err)
	}
	leg.stdin = in
	out, err := leg.proc.StdoutPipe()
	if err != nil {
		t.Fatalf("stdio stdout pipe: %v", err)
	}
	leg.stdout = out
	if err := leg.proc.Start(); err != nil {
		t.Fatalf("start %s: %v", cmdline, err)
	}
	go leg.readLines()
	done := false
	return leg, func() {
		if done {
			return
		}
		done = true
		_ = leg.stdin.Close()
		_ = leg.proc.Wait()
		_ = leg.stdout.Close()
		close(leg.lines)
	}
}

func (s *referenceStdio) readLines() {
	reader := bufio.NewReader(s.stdout)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		text := strings.TrimSpace(strings.TrimRight(line, "\r\n"))
		if text == "" {
			continue
		}
		env := refEnvelope{}
		if json.Unmarshal([]byte(text), &env) != nil {
			// Unparseable server output: hold nothing, keep reading.
			continue
		}
		s.lines <- env
	}
}

func (s *referenceStdio) send(payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	_, err = s.stdin.Write(raw)
	return err
}

// replyServerRequests answers any server-initiated request with a spec
// method-not-found reply so the reference server never wedges on a
// request expecting a client answer; id-less notices are skipped.
func (s *referenceStdio) drainUntil(ctx context.Context, match func(refEnvelope) bool) (refEnvelope, error) {
	for {
		select {
		case env := <-s.lines:
			if match(env) {
				return env, nil
			}
			_, hasMethod := env["method"]
			if _, isReply := env["id"]; isReply && hasMethod && env["id"] != nil {
				_ = s.send(map[string]any{
					"jsonrpc": "2.0",
					"id":      env["id"],
					"error":   map[string]any{"code": -32601, "message": "method not found"},
				})
			}
		case <-ctx.Done():
			return nil, fmt.Errorf("timed out waiting for a bound reply (stderr: %s)", s.stderr.String())
		}
	}
}

func (s *referenceStdio) call(ctx context.Context, id any, method string, params map[string]any) (refEnvelope, error) {
	if err := s.send(callJSON(id, method, params)); err != nil {
		return nil, fmt.Errorf("send %s: %w", method, err)
	}
	cctx, cancel := context.WithTimeout(ctx, refProbeTimeout)
	defer cancel()
	return s.drainUntil(cctx, func(env refEnvelope) bool {
		got, isReply := env["id"]
		return isReply && got != nil && (env["result"] != nil || env["error"] != nil) && refIDEqual(id, got)
	})
}

func (s *referenceStdio) notify(_ context.Context, method string, params map[string]any) error {
	return s.send(notificationJSON(method, params))
}

// initStdioSession performs the initialize handshake plus the
// notifications/initialized notice and pins the negotiated protocolVersion.
func (s *referenceStdio) initialize(ctx context.Context) error {
	sendCtx, cancel := context.WithTimeout(ctx, refProbeTimeout)
	defer cancel()
	_ = s.send(callJSON("init", "initialize", map[string]any{
		"protocolVersion": refProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "criteria-conformance", "version": "1"},
	}))
	env, err := s.drainUntil(sendCtx, func(e refEnvelope) bool {
		got, isReply := e["id"]
		return isReply && refIDEqual("init", got)
	})
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	if _, hasErr := env.hasError(); hasErr {
		return fmt.Errorf("initialize rejected: %v (server may not speak %s)", env["error"], refProtocolVersion)
	}
	if err := s.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}); err != nil {
		return fmt.Errorf("notifications/initialized: %w", err)
	}
	return nil
}

func (s *referenceStdio) raw(payload any) error { return s.send(payload) }

// TestExternalReferenceConformance_HTTP drives the raw Streamable HTTP wire
// of a real reference MCP server: the initialize/session dance (the
// mcp-session-id response header is captured and replayed), the dual-Accept
// rule, JSON and SSE response shapes, -32601, -32700 with a 400 status,
// the id-less notification round-trip, and two-member batch replies that
// may arrive out of order.
func TestExternalReferenceConformance_HTTP(t *testing.T) {
	url := os.Getenv(envRefHTTPURL)
	if url == "" {
		t.Skipf("set %s to run the external reference-server conformance leg", envRefHTTPURL)
	}

	leg, err := newReferenceHTTP(t, url)
	if err != nil {
		t.Fatalf("open %s: %v", url, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), refProbeTimeout)
	defer cancel()
	if err := leg.initialize(ctx); err != nil {
		t.Fatalf("initialize %s: %v", url, err)
	}

	t.Run("accept_requires_event_stream", func(t *testing.T) {
		body, status, code, err := leg.exceptCall(ctx, 5, "tools/list", map[string]string{
			"Accept": "application/json",
		})
		if err != nil {
			t.Fatalf("single-Accept POST: %v", err)
		}
		if status < 400 || code != -32000 {
			t.Fatalf("status=%d error=%v, want a >=400 rejection with -32000 (body=%s)", status, code, body)
		}
	})

	t.Run("parse_error_32700", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), refProbeTimeout)
		defer cancel()
		status, envelopes, err := leg.rawPost(ctx, "{corrupt", "")
		if err != nil {
			t.Fatalf("corrupt POST: %v", err)
		}
		if status != http.StatusBadRequest {
			t.Fatalf("corrupt POST status = %d, want 400", status)
		}
		if len(envelopes) != 1 {
			t.Fatalf("corrupt POST produced %d envelopes, want one -32700 reply", len(envelopes))
		}
		code, hasErr := envelopes[0].hasError()
		if !hasErr || code != -32700 {
			t.Fatalf("corrupt POST error = %v, want -32700 parse error", envelopes[0]["error"])
		}
		if envelopes[0]["id"] != nil {
			t.Fatalf("parse error reply id = %v, want null", envelopes[0]["id"])
		}
	})

	t.Run("batch_two_members_any_order", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), refProbeTimeout)
		defer cancel()
		batch := []map[string]any{
			{"jsonrpc": "2.0", "id": 21, "method": "tools/list"},
			{"jsonrpc": "2.0", "id": "22", "method": "tools/list"},
		}
		status, envelopes, err := leg.rawPost(ctx, string(mustJSON(t, batch)), "")
		if err != nil {
			t.Fatalf("batch POST: %v", err)
		}
		if status != http.StatusOK {
			t.Fatalf("batch POST status = %d, want 200 (body=%v)", status, envelopes)
		}
		got := map[any]bool{}
		for _, env := range envelopes {
			switch {
			case refIDEqual(21, env["id"]):
				got[21] = true
			case refIDEqual("22", env["id"]):
				got["22"] = true
			}
		}
		if len(envelopes) < 2 || !got[21] || !got["22"] {
			t.Fatalf("batch replies: %v — member replies missing or rebound (%v)", envelopes, got)
		}
	})

	runReferenceUniversalVectors(t, leg, nil)
}

// referenceHTTP is the raw Streamable HTTP leg: one POST per envelope, the
// standard headers on every call, session id replay after initialize.
type referenceHTTP struct {
	endpoint string
	client   *http.Client
	session  string
}

func newReferenceHTTP(t *testing.T, endpoint string) (*referenceHTTP, error) {
	t.Helper()
	if !strings.Contains(endpoint, "://") {
		return nil, fmt.Errorf("%s must be a URL (got %q)", envRefHTTPURL, endpoint)
	}
	return &referenceHTTP{endpoint: endpoint, client: &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// standardHeaders renders the transport-controlled header set every POST
// carries, built from mcpclient.StandardHeaders so this hand-rolled wire
// leg shares the transport's header source and cannot diverge from it: Mcp-Method on
// every POST, Mcp-Name only for a name-bearing method (carrying the
// request's params.name/params.uri), Mcp-Protocol-Version announcing the
// dialect the leg speaks (the reference server's 2025-06-18). Per-call
// overrides (session replay, the Accept single-value vector) ride on top.
func (h *referenceHTTP) standardHeaders(method, name string) map[string]string {
	hdrs := map[string]string{}
	for _, entry := range mcpclient.StandardHeaders(method, name, refProtocolVersion) {
		hdrs[entry.Name] = entry.Value
	}
	return hdrs
}

// post issues one exchange and returns the flattened response facets
// (status, content type, body text); the body is closed here so callers
// never hold a *http.Response. name carries the request's params.name/
// params.uri for name-bearing methods (shared Mcp-Name rule); the
// Mcp-Session-Id response header is captured for replay on later POSTs.
func (h *referenceHTTP) post(ctx context.Context, body, name string, override map[string]string) (status int, contentType, text string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.endpoint, strings.NewReader(body))
	if err != nil {
		return 0, "", "", err
	}
	for k, v := range h.standardHeaders("", name) {
		req.Header.Set(k, v)
	}
	if h.session != "" {
		req.Header.Set("Mcp-Session-Id", h.session)
	}
	for k, v := range override {
		if v == "" {
			req.Header.Del(k)
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, "", "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPTestBody))
	if err != nil {
		return 0, "", "", err
	}
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		h.session = sid
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), string(raw), nil
}

// parseRefWire splits one HTTP response body into JSON-RPC envelopes: a
// bare JSON value (object, a single message; array, member envelopes) or
// an SSE stream of data frames.
func parseRefWire(contentType, body string) ([]refEnvelope, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, nil
	}
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		var out []refEnvelope
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimRight(strings.TrimPrefix(line, "data:"), "\r")
			if strings.TrimSpace(line) == "" {
				continue
			}
			env := refEnvelope{}
			if json.Unmarshal([]byte(strings.TrimSpace(line)), &env) == nil && (env["jsonrpc"] != nil || env["result"] != nil || env["error"] != nil) {
				out = append(out, env)
			}
		}
		return out, nil
	}
	var one refEnvelope
	if err := json.Unmarshal([]byte(body), &one); err == nil {
		return []refEnvelope{one}, nil
	}
	var many []refEnvelope
	if err := json.Unmarshal([]byte(body), &many); err == nil {
		return many, nil
	}
	return nil, fmt.Errorf("unparseable reference response (content-type %s): %.200s", contentType, body)
}

// rawPost issues one already-encoded POST (name = request name/uri, "" on
// name-free methods) and reports the status plus every JSON-RPC envelope
// parsed off the wire.
func (h *referenceHTTP) rawPost(ctx context.Context, body, name string) (int, []refEnvelope, error) {
	status, contentType, text, err := h.post(ctx, body, name, nil)
	if err != nil {
		return 0, nil, err
	}
	if status < 200 || status > 299 {
		env, perr := parseRefWire(contentType, text)
		if perr == nil && len(env) == 1 {
			return status, env, nil
		}
		return status, nil, fmt.Errorf("unexpected status %d: %.200s", status, text)
	}
	envelopes, err := parseRefWire(contentType, text)
	if err != nil {
		return status, nil, err
	}
	return status, envelopes, nil
}

func (h *referenceHTTP) call(ctx context.Context, id any, method string, params map[string]any) (refEnvelope, error) {
	raw, err := json.Marshal(callJSON(id, method, params))
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, refProbeTimeout)
	defer cancel()
	status, envelopes, err := h.rawPost(cctx, string(raw), paramNameFromParams(params))
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", method, err)
	}
	if status < 200 || status > 299 {
		return nil, fmt.Errorf("POST %s: unexpected status %d", method, status)
	}
	for _, env := range envelopes {
		if env["id"] != nil && (env["result"] != nil || env["error"] != nil) && refIDEqual(id, env["id"]) {
			return env, nil
		}
	}
	return nil, fmt.Errorf("POST %s: no reply bound to the request id (status %d, envelopes %v)", method, status, envelopes)
}

func (h *referenceHTTP) notify(ctx context.Context, method string, params map[string]any) error {
	raw, err := json.Marshal(notificationJSON(method, params))
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, refProbeTimeout)
	defer cancel()
	status, envelopes, err := h.rawPost(cctx, string(raw), "")
	if err != nil {
		return fmt.Errorf("POST %s: %w", method, err)
	}
	if status < 200 || status > 299 || len(envelopes) != 0 {
		return fmt.Errorf("POST %s: status %d with %d envelopes, want a 2xx with no reply body", method, status, len(envelopes))
	}
	return nil
}

// mustJSON encodes an envelope or fails the test (callers with a *testing.T).
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode envelope: %v", err)
	}
	return raw
}

// exceptCall makes one call while overriding headers (the Accept
// single-value vector) and returns the body text, status, and the last
// wire error code parsed off the response.
func (h *referenceHTTP) exceptCall(ctx context.Context, id int, method string, override map[string]string) (bodyText string, statusCode int, errCode float64, err error) {
	raw, merr := json.Marshal(callJSON(id, method, nil))
	if merr != nil {
		return "", 0, 0, merr
	}
	cctx, cancel := context.WithTimeout(ctx, refProbeTimeout)
	defer cancel()
	status, contentType, text, postErr := h.post(cctx, string(raw), "", override)
	if postErr != nil {
		return "", 0, 0, postErr
	}
	responseErr := float64(0)
	envs, perr := parseRefWire(contentType, text)
	if perr == nil && len(envs) > 0 {
		responseErr, _ = envs[len(envs)-1].hasError()
	}
	return text, status, responseErr, nil
}

// initHTTPSession performs the initialize exchange that establishes the
// reference server's session, capturing the Mcp-Session-Id response header,
// then sends notifications/initialized like any conformant client.
func (h *referenceHTTP) initialize(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, refProbeTimeout)
	defer cancel()
	status, ct, text, err := h.post(cctx, `{"jsonrpc":"2.0","id":"init","method":"initialize","params":{"protocolVersion":"`+
		refProtocolVersion+`","capabilities":{},"clientInfo":{"name":"criteria-conformance","version":"1"}}}`, "", map[string]string{"Mcp-Method": "initialize"})
	if err != nil {
		return fmt.Errorf("initialize POST: %w", err)
	}
	if status < 200 || status > 299 {
		return fmt.Errorf("initialize status %d: %.200s", status, text)
	}
	if h.session == "" {
		return fmt.Errorf("initialize response carried no Mcp-Session-Id (status %d, ct=%s)", status, ct)
	}
	envs, err := parseRefWire(ct, text)
	if err != nil {
		return fmt.Errorf("initialize reply unparseable: %w (%.200s)", err, text)
	}
	if len(envs) == 0 {
		return fmt.Errorf("initialize reply carried no envelope: %.200s", text)
	}
	if _, hasErr := envs[0].hasError(); hasErr {
		return fmt.Errorf("initialize rejected: %v (server may not speak %s)", envs[0]["error"], refProtocolVersion)
	}
	nctx, ncancel := context.WithTimeout(ctx, refProbeTimeout)
	defer ncancel()
	if err := h.notify(nctx, "notifications/initialized", nil); err != nil {
		return fmt.Errorf("notifications/initialized: %w", err)
	}
	return nil
}

// paramNameFromParams extracts the name/uri a name-bearing request carries,
// mirroring the transport's Mcp-Name rule: params.name for tools/call and
// prompts/get, params.uri for resources/read, and "" for every other method
// (the vectors here only exercise the tools family plus the name-free
// universal set).
func paramNameFromParams(params map[string]any) string {
	if params == nil {
		return ""
	}
	if s, ok := params["name"].(string); ok && s != "" {
		return s
	}
	if s, ok := params["uri"].(string); ok && s != "" {
		return s
	}
	return ""
}

// TestExternalReferenceConformance_HTTPTransport drives the transport under
// test — mcpclient's Streamable HTTP path (NewHTTPClient, the
// server/discover Connect, tools/list, tools/call) — against a real external
// reference HTTP MCP server, asserting the green path end to end: the
// stateless-core open, a non-empty discovered surface, the configured tool
// call when MCP_REFERENCE_TOOL is set, and the SSE delivery shape when the
// endpoint answers text/event-stream. This leg speaks the 2026-07-28
// stateless core (unlike the initialize-era raw-wire HTTP leg above). A
// reference server that only speaks the legacy initialize handshake is not
// a conformance failure for this transport: the leg skips with the dialect
// mismatch named. The deterministic header-vs-body vector set lives in the
// hermetic fixture leg (http_conformance_test.go, transport_http_test.go),
// which CI runs unconditionally.
func TestExternalReferenceConformance_HTTPTransport(t *testing.T) {
	url := os.Getenv(envRefHTTPURL)
	if url == "" {
		t.Skipf("set %s to run the external reference-server conformance leg", envRefHTTPURL)
	}

	// Dialect gate, raw, before the transport drives: a POST of the
	// stateless-core server/discover probe announcing 2026-07-28. A
	// reachable endpoint that rejects the probe speaks a different dialect
	// (legacy initialize-era servers answer unknown-method rejections) —
	// skip naming that mismatch; unreachable endpoints are a harness
	// configuration failure and stay fatal.
	speaks, reason, err := referenceHTTPStatelessDialect(url)
	if err != nil {
		t.Fatalf("stateless dialect probe %s: %v", url, err)
	}
	if !speaks {
		t.Skipf("reference HTTP server does not speak the 2026-07-28 stateless-core dialect (MCP_REFERENCE_HTTP_URL dialect mismatch): %s", reason)
	}

	client := mcpclient.NewHTTPClient(mcpclient.HTTPConfig{
		Endpoint:       url,
		RequestTimeout: refProbeTimeout,
	}, func(mcpclient.Notification) {})
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*refProbeTimeout)
	defer cancel()

	t.Run("discover_connect", func(t *testing.T) {
		if err := client.Connect(ctx, mcpclient.NegotiationDiscover, "criteria-conformance", "1"); err != nil {
			t.Fatalf("Connect (server/discover probe) %s: %v", url, err)
		}
	})

	t.Run("surface_nonempty", func(t *testing.T) {
		tools, err := client.ListTools(ctx)
		if err != nil {
			t.Fatalf("tools/list: %v", err)
		}
		if len(tools) == 0 {
			t.Fatal("tools/list returned an empty surface, want the reference server's tools")
		}
	})

	if tool := os.Getenv(envRefTool); tool != "" {
		t.Run("tool_call", func(t *testing.T) {
			args := map[string]any{}
			if raw := os.Getenv(envRefToolArgs); raw != "" {
				if err := json.Unmarshal([]byte(raw), &args); err != nil {
					t.Fatalf("parse %s: %v", envRefToolArgs, err)
				}
			}
			result, err := client.CallTool(ctx, tool, args)
			if err != nil {
				t.Fatalf("tools/call(%s): %v", tool, err)
			}
			if result.IsError {
				t.Fatalf("tools/call(%s) errored: %v", tool, result.Content)
			}
			if len(result.Content) == 0 {
				t.Fatalf("tools/call(%s) returned no content", tool)
			}
		})
	}

	t.Run("sse_delivery", func(t *testing.T) {
		// Delivery shape, straight off the reference wire: this raw POST is
		// built exactly like the transport's own tools/list POST (the shared
		// StandardHeaders builder, the same announced _meta version), so when
		// the reference answers text/event-stream the transport's results
		// were SSE-delivered too — pin the SSE frame parse against that wire.
		raw, err := json.Marshal(callJSON("sse-delivery", "tools/list", map[string]any{
			"_meta": map[string]any{mcpclient.MetaProtocolVersion: mcpclient.ProtocolVersion20260728},
		}))
		if err != nil {
			t.Fatalf("encode tools/list: %v", err)
		}
		cctx, cancel := context.WithTimeout(ctx, refProbeTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(cctx, http.MethodPost, url, bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("build POST: %v", err)
		}
		for _, entry := range mcpclient.StandardHeaders("tools/list", "", mcpclient.ProtocolVersion20260728) {
			req.Header.Set(entry.Name, entry.Value)
		}
		resp, err := (&http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}).Do(req)
		if err != nil {
			t.Fatalf("tools/list POST: %v", err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPTestBody))
		if err != nil {
			t.Fatalf("read reply: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("tools/list status = %d, want 200", resp.StatusCode)
		}
		contentType := resp.Header.Get("Content-Type")
		if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
			envs, err := parseRefWire(contentType, string(body))
			if err != nil {
				t.Fatalf("SSE frame parse: %v (%.200s)", err, body)
			}
			matched := false
			for _, env := range envs {
				if refIDEqual("sse-delivery", env["id"]) && env["result"] != nil {
					matched = true
				}
			}
			if !matched {
				t.Fatalf("SSE stream %v carried no reply bound to the probe id", envs)
			}
			t.Logf("SSE delivery confirmed: %d frame(s) parsed off text/event-stream", len(envs))
		} else {
			t.Logf("endpoint answered %q for tools/list; the SSE leg only asserts when the reference delivers text/event-stream", contentType)
		}
	})
}

// referenceHTTPStatelessDialect probes the endpoint with the 2026-07-28
// server/discover request and classifies the raw answer: speaks=true for a
// stateless-core discover result; speaks=false plus a reason naming the
// mismatch when the endpoint rejects the probe (unknown-method rejections,
// bare method errors, session-required postures); an error result means the
// endpoint was unreachable — a harness problem, not a dialect mismatch.
func referenceHTTPStatelessDialect(endpoint string) (speaks bool, reason string, err error) {
	// The probe body mirrors the transport's own Discover shape: the
	// announced version rides params._meta (MetaProtocolVersion) alongside
	// the Mcp-Protocol-Version header — a dialect probe built the same way
	// mcpclient.Discover builds its POST.
	raw, err := json.Marshal(callJSON("probe", "server/discover", map[string]any{
		"_meta": map[string]any{
			mcpclient.MetaProtocolVersion:    mcpclient.ProtocolVersion20260728,
			mcpclient.MetaClientCapabilities: map[string]any{},
		},
	}))
	if err != nil {
		return false, "", err
	}
	cctx, cancel := context.WithTimeout(context.Background(), refProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, endpoint, strings.NewReader(string(raw)))
	if err != nil {
		return false, "", err
	}
	for _, entry := range mcpclient.StandardHeaders("server/discover", "", mcpclient.ProtocolVersion20260728) {
		req.Header.Set(entry.Name, entry.Value)
	}
	resp, err := (&http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}).Do(req)
	if err != nil {
		return false, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPTestBody))
	if err != nil {
		return false, "", err
	}
	envelopes, perr := parseRefWire(resp.Header.Get("Content-Type"), string(body))
	if perr == nil && len(envelopes) > 0 {
		env := envelopes[0]
		if _, isResult := env["result"]; isResult {
			return true, "", nil
		}
		if _, hasErr := env.hasError(); hasErr {
			return false, fmt.Sprintf("server/discover answered error %v (HTTP %d), not a stateless-core discover result", env["error"], resp.StatusCode), nil
		}
	}
	return false, fmt.Sprintf("server/discover answered HTTP %d %q: %.200s", resp.StatusCode, resp.Header.Get("Content-Type"), string(body)), nil
}
