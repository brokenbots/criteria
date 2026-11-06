package main

// jsonrpc_http_conformance_test.go — KB-159: JSON-RPC 2.0 conformance over
// the Streamable HTTP transport.
//
// Matrix covered:
//   - Error-code band: tools/call answered with every spec-band error code
//     (-32020/-32021/-32022 and neighbors) in BOTH response shapes — the
//     application/json leg and the SSE leg (an SSE data frame carries the
//     same full JSON-RPC envelope language) — surfaced as a typed
//     *mcpclient.RPCError with the exact code and message, the session
//     surviving each protocol-level failure.
//   - Id handling: a reply id delivered as a JSON number settles against
//     the client's string request id (normalize both sides), and a null-id
//     parse-error reply binds to the sole outstanding request instead of
//     wedging the caller.
//   - Reference server: the echo fixture runs in Streamable HTTP mode as a
//     real subprocess — discover/tools-list discovery, an echoes round
//     trip over an SSE-delivered result with a progress event, and the
//     SEP-2575 no-resumability posture (a broken response stream loses the
//     in-flight call; the re-issued call is a brand-new request that the
//     unbroken session answers).

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"

	"github.com/brokenbots/criteria/cmd/criteria-adapter-mcp/mcpclient"
)

// TestJSONRPCHTTPConformance_ErrorCodeBand runs the JSON-RPC error-code
// band across both HTTP response shapes. Each code is surfaced as a typed
// *mcpclient.RPCError with the exact code and message preserved, and the
// session survives: the next execute on the same session answers again.
func TestJSONRPCHTTPConformance_ErrorCodeBand(t *testing.T) {
	for _, shape := range []struct {
		name string
		sse  bool
	}{{"json", false}, {"sse", true}} {
		t.Run(shape.name, func(t *testing.T) {
			for n, code := range []int{-32020, -32021, -32022, -32000, -32099, -32601, -32602, -32700} {
				t.Run(fmt.Sprintf("code_%d", -code), func(t *testing.T) {
					fake := &mcpHTTPFake{faultCode: code}
					if shape.sse {
						fake.callMode = toolsCallSSE
					}
					ts := httptest.NewServer(fake)
					t.Cleanup(ts.Close)
					sid := fmt.Sprintf("http-band-%s-%d", shape.name, n)
					b := openHTTPConformanceSession(t, sid, map[string]string{"transport": "http", "url": ts.URL})

					sender := &permittingEventSender{bridge: b}
					err := b.Execute(context.Background(), &v2.ExecuteRequest{
						SessionId: sid,
						Input:     map[string]string{"tool": "echo", "message": "band"},
					}, sender)
					if err == nil {
						t.Fatalf("fault with code %d returned nil error, want typed RPCError", code)
					}
					var rpcErr *mcpclient.RPCError
					if !errors.As(err, &rpcErr) {
						t.Fatalf("fault error not typed *mcpclient.RPCError: %v (%T)", err, err)
					}
					if rpcErr.Code != code {
						t.Errorf("rpc error code = %d, want %d", rpcErr.Code, code)
					}
					if want := fmt.Sprintf("spec band error: %d", code); rpcErr.Message != want {
						t.Errorf("rpc error message = %q, want %q", rpcErr.Message, want)
					}

					// The session survived the protocol-level error.
					err = b.Execute(context.Background(), &v2.ExecuteRequest{
						SessionId: sid,
						Input:     map[string]string{"tool": "echo", "message": "again"},
					}, sender)
					if err == nil {
						t.Fatalf("re-issue after a %d fault must still answer (session alive)", code)
					}
					if !errors.As(err, &rpcErr) || rpcErr.Code != code {
						t.Errorf("re-issue after fault = %v (%T), want the same typed %d", err, err, code)
					}
				})
			}
		})
	}
}

// TestJSONRPCHTTPConformance_IDHandling covers the id-shape vectors over
// HTTP: a server that echoes request ids as JSON numbers still settles
// every call (the client's normalize path makes "3" and 3 one key), and a
// null-id parse-error reply attributes to the sole outstanding request
// instead of wedging it, with the session surviving.
func TestJSONRPCHTTPConformance_IDHandling(t *testing.T) {
	t.Run("numeric_reply_ids_settle", func(t *testing.T) {
		fake := &mcpHTTPFake{idStyle: "number"}
		ts := httptest.NewServer(fake)
		t.Cleanup(ts.Close)
		b := openHTTPConformanceSession(t, "http-id-number", map[string]string{"transport": "http", "url": ts.URL})

		sender := &permittingEventSender{bridge: b}
		if err := b.Execute(context.Background(), &v2.ExecuteRequest{
			SessionId: "http-id-number",
			Input:     map[string]string{"tool": "echo", "message": "hi"},
		}, sender); err != nil {
			t.Fatalf("execute with numeric reply ids: %v", err)
		}
		if text := kb155ContentTexts(t, sender.all()); !strings.Contains(text, "echoed hi") {
			t.Fatalf("echo content = %q, want the fake's echo", text)
		}
	})

	t.Run("null_reply_id_attributed", func(t *testing.T) {
		fake := &mcpHTTPFake{nullIDReply: true}
		ts := httptest.NewServer(fake)
		t.Cleanup(ts.Close)
		b := openHTTPConformanceSession(t, "http-id-null", map[string]string{"transport": "http", "url": ts.URL})

		// The bend targets the first tools/call: the -32600 envelope carries
		// id null, which binds to no pending id — attributed to the sole
		// outstanding request and surfaced verbatim.
		sender := &permittingEventSender{bridge: b}
		err := b.Execute(context.Background(), &v2.ExecuteRequest{
			SessionId: "http-id-null",
			Input:     map[string]string{"tool": "echo", "message": "first"},
		}, sender)
		if err == nil {
			t.Fatal("first tools/call returned nil error, want the null-id reply")
		}
		var rpcErr *mcpclient.RPCError
		if !errors.As(err, &rpcErr) {
			t.Fatalf("null-id reply error not typed *mcpclient.RPCError: %v (%T)", err, err)
		}
		if rpcErr.Code != -32600 || rpcErr.Message != "null-id reply mode" {
			t.Errorf("null-id reply = %d/%q, want -32600/\"null-id reply mode\"", rpcErr.Code, rpcErr.Message)
		}

		// The session survived the global failure shape.
		if err := b.Execute(context.Background(), &v2.ExecuteRequest{
			SessionId: "http-id-null",
			Input:     map[string]string{"tool": "echo", "message": "second"},
		}, sender); err != nil {
			t.Errorf("execute after null-id reply: %v", err)
		}
	})
}

// startEchoHTTPFixture launches the reference echo fixture in Streamable
// HTTP mode as a real subprocess and waits for the pidlog's "http=<addr>"
// startup line, returning the server URL. Extra env knobs bend specific
// reply shapes (MCP_HTTP_SSE, MCP_REPLY_MODE, ...).
func startEchoHTTPFixture(t *testing.T, extraEnv ...string) string {
	t.Helper()
	dir := t.TempDir()
	pidlog := dir + "/pid.txt"
	stderr := dir + "/stderr.txt"
	cmd := exec.Command(testEchoBin)
	cmd.Env = append(os.Environ(), "MCP_HTTP_LISTEN=127.0.0.1:0", "MCP_PIDLOG="+pidlog)
	cmd.Env = append(cmd.Env, extraEnv...)
	if f, err := os.Create(stderr); err == nil {
		cmd.Stderr = f
		t.Cleanup(func() { _ = f.Close() })
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start echo fixture (http): %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		fd, err := os.Open(pidlog)
		if err == nil {
			scanner := bufio.NewScanner(fd)
			for scanner.Scan() {
				line := scanner.Text()
				if addr, ok := strings.CutPrefix(line, "http="); ok {
					_ = fd.Close()
					return "http://" + addr
				}
			}
			_ = fd.Close()
		}
		time.Sleep(20 * time.Millisecond)
	}
	raw, _ := os.ReadFile(stderr)
	t.Fatalf("echo fixture (http) never reported its address: stderr=%s", raw)
	return ""
}

// TestJSONRPCHTTPConformance_ReferenceServer exercises the conformance
// surface end to end against the real reference MCP server (the echo
// fixture, not the in-process fake) in Streamable HTTP mode: discovery
// over the same envelope core as stdio, an SSE-delivered result with a
// progress event, and the SEP-2575 behavior for a response stream that
// breaks mid-call — the in-flight call fails and the re-issued call is an
// ordinary new request on the unbroken session.
func TestJSONRPCHTTPConformance_ReferenceServer(t *testing.T) {
	t.Run("sse_progress_and_result", func(t *testing.T) {
		url := startEchoHTTPFixture(t, "MCP_HTTP_SSE=1")
		sid := "http-ref-sse"
		b := openHTTPConformanceSession(t, sid, map[string]string{"transport": "http", "url": url})

		// Discovery: the fixture's real tool surface, spoken by the same
		// routeEnvelope core the stdio dialect serves.
		info, err := b.Info(context.Background(), &v2.InfoRequest{})
		if err != nil {
			t.Fatalf("Info over HTTP: %v", err)
		}
		names := make([]string, 0, len(info.GetTools()))
		for _, tool := range info.GetTools() {
			names = append(names, tool.GetName())
		}
		if len(names) == 0 || !strings.Contains(strings.Join(names, ","), "echo") {
			t.Fatalf("Info tools = %v, want the fixture's surfaced tools", names)
		}

		// One echo round trip: the fixture's SSE leg streams a progress
		// event, then the call's own result envelope.
		sender := &permittingEventSender{bridge: b}
		if err := b.Execute(context.Background(), &v2.ExecuteRequest{
			SessionId: sid,
			Input:     map[string]string{"tool": "echo", "message": "sse"},
		}, sender); err != nil {
			t.Fatalf("Execute(echo) over the fixture's SSE leg: %v", err)
		}
		events := sender.all()
		sawProgress, sawContent := false, false
		for _, ev := range events {
			if a := ev.GetAdapter(); a != nil {
				switch a.GetEventKind() {
				case "mcp.progress":
					sawProgress = true
				case "mcp.content":
					sawContent = true
				}
			}
		}
		if !sawProgress || !sawContent {
			t.Fatalf("SSE-delivered execute events missing progress/content: %s", describeEvents(events))
		}
		last := events[len(events)-1]
		if res := last.GetResult(); res == nil || res.GetOutcome() != "success" {
			t.Fatalf("terminal event = %s, want success result", describeEvents(events))
		}
	})

	t.Run("broken_stream_loses_call_not_session", func(t *testing.T) {
		// SEP-2575: resumability was removed from the revision, so a broken
		// response stream loses the in-flight request — no client-side
		// resumability state exists to recover it from. The re-issued call
		// is a brand-new request that the unbroken session answers.
		url := startEchoHTTPFixture(t, "MCP_HTTP_SSE=break_once")
		sid := "http-ref-break"
		b := openHTTPConformanceSession(t, sid, map[string]string{"transport": "http", "url": url})

		sender := &permittingEventSender{bridge: b}
		if err := b.Execute(context.Background(), &v2.ExecuteRequest{
			SessionId: sid,
			Input:     map[string]string{"tool": "echo", "message": "lost"},
		}, sender); err == nil {
			t.Fatal("the broken response stream must fail the in-flight call")
		}
		if err := b.Execute(context.Background(), &v2.ExecuteRequest{
			SessionId: sid,
			Input:     map[string]string{"tool": "echo", "message": "reissued"},
		}, sender); err != nil {
			t.Fatalf("re-issued execute on the surviving session: %v", err)
		}
		last := sender.all()[len(sender.all())-1]
		if res := last.GetResult(); res == nil || res.GetOutcome() != "success" {
			t.Fatalf("re-issued terminal event = %s, want success result", describeEvents(sender.all()))
		}
	})
}
