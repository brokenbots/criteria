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
// Verified against @modelcontextprotocol/server-everything (stdio &&
// streamableHttp): initialize/session, id echo (numeric && string), a
// non-empty tools/list, -32601 for unknown methods, tools/call, batch
// posture, parse-error handling, and the HTTP Accept/session-header rules.
// The stdio integration leg also drives the real mcpclient pipeline
// (probe-first open with legacy fallback) against the external server; the
// HTTP bridge leg is asserted as a clean deterministic failure because the
// in-tree Streamable HTTP dialect has no legacy initialize fallback
// (terminal discover probe) by design.

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
		status, envelopes, err := leg.rawPost(ctx, "{corrupt")
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
		status, envelopes, err := leg.rawPost(ctx, string(mustJSON(t, batch)))
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
// carries; per-call overrides ride on top (session id only exists post-open).
func (h *referenceHTTP) standardHeaders(method string) map[string]string {
	hdrs := map[string]string{
		"Content-Type":         "application/json",
		"Accept":               "application/json, text/event-stream",
		"Mcp-Name":             "criteria-conformance",
		"Mcp-Protocol-Version": refProtocolVersion,
	}
	if method != "" {
		hdrs["Mcp-Method"] = method
	}
	if h.session != "" {
		hdrs["Mcp-Session-Id"] = h.session
	}
	return hdrs
}

// post issues one exchange and returns the flattened response facets
// (status, content type, body text); the body is closed here so callers
// never hold a *http.Response. The Mcp-Session-Id response header is
// captured for replay on later POSTs.
func (h *referenceHTTP) post(ctx context.Context, body string, override map[string]string) (status int, contentType, text string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.endpoint, strings.NewReader(body))
	if err != nil {
		return 0, "", "", err
	}
	for k, v := range h.standardHeaders("") {
		req.Header.Set(k, v)
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

// rawPost issues one already-encoded POST and reports the status plus every
// JSON-RPC envelope parsed off the wire.
func (h *referenceHTTP) rawPost(ctx context.Context, body string) (int, []refEnvelope, error) {
	status, contentType, text, err := h.post(ctx, body, nil)
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
	status, envelopes, err := h.rawPost(cctx, string(raw))
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
	status, envelopes, err := h.rawPost(cctx, string(raw))
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
	status, contentType, text, postErr := h.post(cctx, string(raw), override)
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
		refProtocolVersion+`","capabilities":{},"clientInfo":{"name":"criteria-conformance","version":"1"}}}`, map[string]string{"Mcp-Method": "initialize"})
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

// TestExternalReferenceConformance_HTTPBridgeFailsCleanly pins the
// transport's terminal discover probe against an external legacy HTTP
// reference server: the 2026-07-28 in-tree dialect has no initialize
// fallback on http, so the open must fail with the typed probe error —
// deterministically, with no half-connected session and no hang.
func TestExternalReferenceConformance_HTTPBridgeFailsCleanly(t *testing.T) {
	url := os.Getenv(envRefHTTPURL)
	if url == "" {
		t.Skipf("set %s to run the external reference-server conformance leg", envRefHTTPURL)
	}

	bridge := &MCPBridge{sessions: map[string]*sessionState{}}
	ctx, cancel := context.WithTimeout(context.Background(), refProbeTimeout)
	defer cancel()
	_, err := bridge.OpenSession(ctx, &v2.OpenSessionRequest{
		SessionId: "ext-ref-http",
		Config:    map[string]string{"transport": "http", "url": url},
	})
	if err == nil {
		// An external server that speaks the in-tree stateless discover
		// dialect would legitimately open; nothing to pin in that case.
		_, _ = bridge.CloseSession(ctx, &v2.CloseSessionRequest{SessionId: "ext-ref-http"})
		t.Skip("external HTTP server adopted the discover probe; the clean-failure posture does not apply")
	}
	if !strings.Contains(err.Error(), "server/discover probe failed") {
		t.Fatalf("OpenSession against the external HTTP server = %v, want the typed discover-probe failure", err)
	}
	if openErr := bridge.Execute(ctx, &v2.ExecuteRequest{SessionId: "ext-ref-http", Input: map[string]string{"tool": "echo", "message": "hi"}},
		&permittingEventSender{bridge: bridge}); openErr == nil {
		t.Fatal("a failed open must leave no usable session")
	}
}
