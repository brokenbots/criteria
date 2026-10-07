package main

// jsonrpc_conformance_test.go — KB-161: JSON-RPC 2.0 conformance suite for
// the MCP stdio transport.
//
// Matrix covered:
//   - Framing: lsp (Content-Length header dialect, default) vs ndjson
//     (newline-delimited JSON). Matching pairs run full sessions through the
//     real bridge; both mismatch pairs fail the session open deterministically
//     inside a bounded context instead of hanging.
//   - Parse errors: an unparseable payload answers -32700 with the
//     spec-mandated null id in both framings, without tearing the session.
//     Valid JSON that is not a request answers -32600, id preserved when
//     present.
//   - Error-code boundary band: the fixture's fault tool answers tools/call
//     with a JSON-RPC error response carrying MCP_FAIL_CODE; the typed
//     *mcpclient.RPCError preserves code and message end to end across the
//     spec band (-32020/-32021/-32022) and neighbors, and the session
//     survives every protocol-level error code.
//   - Resiliency: one garbage frame ahead of the first real reply does not
//     break the session in either framing; truncated headers fail cleanly.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"

	"github.com/brokenbots/criteria/cmd/criteria-adapter-mcp/mcpclient"
)

// rawEcho drives the echo fixture directly over os.Pipes, bypassing the
// bridge, so framing and parse behavior can be asserted byte-level.
type rawEcho struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   *bufio.Reader
	waitExit func(time.Duration) error
}

func startRawEcho(t *testing.T, ndjson bool, extraEnv ...string) *rawEcho {
	t.Helper()
	cmd := exec.Command(testEchoBin)
	cmd.Env = append(os.Environ(), extraEnv...)
	if ndjson {
		cmd.Env = append(cmd.Env, "MCP_FRAMING=ndjson")
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", testEchoBin, err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	// waitExit reaps the fixture exactly once (cmd.Wait may be called only
	// once, regardless of how many callers want the result) bounded by bound.
	var waitOnce sync.Once
	waitErr := io.EOF // non-nil sentinel until reaped or timed out
	waitExit := func(bound time.Duration) error {
		waitOnce.Do(func() {
			select {
			case waitErr = <-waitCh:
			case <-time.After(bound):
				waitErr = errors.New("fixture did not exit within bound")
			}
		})
		return waitErr
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		if err := waitExit(5 * time.Second); err != nil {
			t.Logf("raw fixture cleanup: %v", err)
		}
	})
	return &rawEcho{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout), waitExit: waitExit}
}

// rawTimeout bounds every raw read; a missing reply aborts the test instead
// of hanging the suite.
const rawTimeout = 10 * time.Second

func readRaw(t *testing.T, read func() (string, error)) string {
	t.Helper()
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := read()
		ch <- result{line, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("raw read: %v", r.err)
		}
		return r.line
	case <-time.After(rawTimeout):
		t.Fatalf("raw read timed out after %s", rawTimeout)
	}
	return ""
}

// readLSPFrame reads one Content-Length header frame from the fixture.
func (f *rawEcho) readLSPFrame(t *testing.T) string {
	t.Helper()
	length := -1
	for {
		line := readRaw(t, func() (string, error) { return f.stdout.ReadString('\n') })
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 && strings.EqualFold(strings.TrimSpace(parts[0]), "Content-Length") {
			if _, err := fmt.Sscanf(strings.TrimSpace(parts[1]), "%d", &length); err != nil {
				t.Fatalf("bad content-length %q: %v", parts[1], err)
			}
		}
	}
	if length < 0 {
		t.Fatal("readLSPFrame: no Content-Length header")
	}
	body := readRaw(t, func() (string, error) {
		buf := make([]byte, length)
		if _, err := io.ReadFull(f.stdout, buf); err != nil {
			return "", err
		}
		return string(buf), nil
	})
	return body
}

// readNDJSONLine reads one non-blank ndjson line from the fixture.
func (f *rawEcho) readNDJSONLine(t *testing.T) string {
	t.Helper()
	for {
		line := readRaw(t, func() (string, error) { return f.stdout.ReadString('\n') })
		line = strings.TrimRight(line, "\r\n")
		if strings.TrimSpace(line) != "" {
			return line
		}
	}
}

func (f *rawEcho) writeLSP(t *testing.T, payload string) {
	t.Helper()
	if _, err := fmt.Fprintf(f.stdin, "Content-Length: %d\r\n\r\n%s", len(payload), payload); err != nil {
		t.Fatalf("write lsp frame: %v", err)
	}
}

func (f *rawEcho) writeNDJSON(t *testing.T, payload string) {
	t.Helper()
	if _, err := fmt.Fprintf(f.stdin, "%s\n", payload); err != nil {
		t.Fatalf("write ndjson line: %v", err)
	}
}

// closeStdinAndExit closes stdin and requires the fixture process to exit
// within bound: a silent or hung shutdown is a conformance failure.
func (f *rawEcho) closeStdinAndExit(t *testing.T) {
	t.Helper()
	_ = f.stdin.Close()
	if err := f.waitExit(rawTimeout); err != nil {
		t.Fatal(err)
	}
}

func initializeReq(id int) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`, id)
}

// assertJSONRPCError parses one reply payload and asserts the error envelope:
// the id is whatever arrived (json.RawMessage), the code matches exactly, and
// the message contains fragment. When rawID is "null" the envelope must carry
// a literal null id (a dropped id field is a spec violation).
func assertJSONRPCError(t *testing.T, payload, rawID string, wantCode int, wantMessage string) {
	t.Helper()
	var env struct {
		JSONRPC string `json:"jsonrpc"`
		ID      json.RawMessage
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		t.Fatalf("reply not JSON: %v (payload %q)", err, payload)
	}
	if env.JSONRPC != "2.0" {
		t.Errorf("reply jsonrpc = %q, want 2.0 (payload %q)", env.JSONRPC, payload)
	}
	if rawID == "null" {
		if strings.TrimSpace(string(env.ID)) != "null" {
			t.Errorf("reply id = %s, want literal null (payload %q)", env.ID, payload)
		}
	} else if string(env.ID) != rawID {
		t.Errorf("reply id = %s, want %s (payload %q)", env.ID, rawID, payload)
	}
	if env.Error.Code != wantCode {
		t.Errorf("reply error code = %d, want %d (payload %q)", env.Error.Code, wantCode, payload)
	}
	if !strings.Contains(env.Error.Message, wantMessage) {
		t.Errorf("reply error message = %q, want containing %q (payload %q)", env.Error.Message, wantMessage, payload)
	}
}

func assertInitializeResult(t *testing.T, payload string) {
	t.Helper()
	var env struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      float64         `json:"id"`
		Result  map[string]any  `json:"result"`
		Error   *map[string]any `json:"error"`
	}
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		t.Fatalf("reply not JSON: %v (payload %q)", err, payload)
	}
	if env.JSONRPC != "2.0" || env.ID != 1 {
		t.Errorf("initialize reply envelope off: jsonrpc=%q id=%v payload=%q", env.JSONRPC, env.ID, payload)
	}
	if env.Error != nil {
		t.Fatalf("initialize failed: %v (payload %q)", env.Error, payload)
	}
	if _, ok := env.Result["protocolVersion"]; !ok {
		t.Errorf("initialize result missing protocolVersion (payload %q)", payload)
	}
}

// TestJSONRPCConformance_ParseErrors runs the parse-error boundary in both
// framings through the raw fixture: misframed bytes (-32700 null id) are
// answered without tearing the session, valid-JSON-non-requests get -32600
// with the id preserved, and blank lines are frame-separator noise.
func TestJSONRPCConformance_ParseErrors(t *testing.T) {
	t.Run("lsp", func(t *testing.T) {
		f := startRawEcho(t, false)
		f.writeLSP(t, "{@oops")
		assertJSONRPCError(t, f.readLSPFrame(t), "null", -32700, "parse error")
		f.writeLSP(t, initializeReq(1))
		assertInitializeResult(t, f.readLSPFrame(t))
		f.writeLSP(t, `{"jsonrpc":"2.0","id":9}`)
		assertJSONRPCError(t, f.readLSPFrame(t), "9", -32600, "invalid request")
		f.closeStdinAndExit(t)
	})
	t.Run("ndjson", func(t *testing.T) {
		f := startRawEcho(t, true)
		f.writeNDJSON(t, "{@oops")
		assertJSONRPCError(t, f.readNDJSONLine(t), "null", -32700, "parse error")
		// Blank lines are separators, not garbage: the session keeps working.
		f.writeNDJSON(t, "")
		f.writeNDJSON(t, initializeReq(1))
		assertInitializeResult(t, f.readNDJSONLine(t))
		f.writeNDJSON(t, `{"jsonrpc":"2.0","id":9}`)
		assertJSONRPCError(t, f.readNDJSONLine(t), "9", -32600, "invalid request")
		f.closeStdinAndExit(t)
	})
}

// TestJSONRPCConformance_MisframedClient asserts the NDJSON-vs-framing probe
// byte-level in both directions: a peer writing the wrong framing shape gets
// deterministic behavior — ndjson fixtures answer misframed header lines with
// -32700 and stay alive; header fixtures ignore non-header lines and shut
// down cleanly on EOF instead of hanging.
func TestJSONRPCConformance_MisframedClient(t *testing.T) {
	t.Run("lsp_client_into_ndjson_fixture", func(t *testing.T) {
		f := startRawEcho(t, true)
		f.writeLSP(t, initializeReq(1))
		// The header line is the first ndjson frame the fixture reads:
		// unparseable header text answers -32700 with a null id.
		assertJSONRPCError(t, f.readNDJSONLine(t), "null", -32700, "parse error")
		// The lsp payload bytes remain an unterminated line in the fixture's
		// reader; terminating it makes the embedded (valid) request complete,
		// proving the fixture survived the misframed frame.
		f.writeNDJSON(t, "")
		assertInitializeResult(t, f.readNDJSONLine(t))
		f.closeStdinAndExit(t)
	})
	t.Run("ndjson_client_into_lsp_fixture", func(t *testing.T) {
		f := startRawEcho(t, false)
		f.writeNDJSON(t, initializeReq(1))
		// No reply is possible (the line is not a header), and on stdin close
		// the fixture exits instead of hanging.
		f.closeStdinAndExit(t)
	})
}

// TestJSONRPCConformance_TruncatedFrame requires a frame that promises more
// payload bytes than it delivers to fail cleanly on EOF rather than hang.
func TestJSONRPCConformance_TruncatedFrame(t *testing.T) {
	f := startRawEcho(t, false)
	if _, err := fmt.Fprintf(f.stdin, "Content-Length: 100\r\n\r\n{"); err != nil {
		t.Fatalf("write truncated frame: %v", err)
	}
	_ = f.stdin.Close()
	if err := f.waitExit(rawTimeout); err != nil {
		t.Fatal("fixture hung on truncated frame")
	}
}

// bridgeEnvSession opens an MCP bridge session against the echo fixture with
// extra env pairs (comma-separated K=V) and the given framing. The client
// framing only configures the bridge's write side; the fixture itself must
// also be told to speak it, so ndjson requests append MCP_FRAMING=ndjson to
// the fixture env (the fixture defaults to Content-Length framing).
func bridgeEnvSession(t *testing.T, ctx context.Context, sessionID, env, framing string) *MCPBridge {
	t.Helper()
	if framing == "ndjson" {
		if env == "" {
			env = "MCP_FRAMING=ndjson"
		} else {
			env += ",MCP_FRAMING=ndjson"
		}
	}
	bridge := &MCPBridge{sessions: map[string]*sessionState{}}
	cfg := map[string]string{"command": testEchoBin}
	if env != "" {
		cfg["env"] = env
	}
	if framing != "" {
		cfg["framing"] = framing
	}
	if _, err := bridge.OpenSession(ctx, &v2.OpenSessionRequest{SessionId: sessionID, Config: cfg}); err != nil {
		t.Fatalf("OpenSession %s: %v", sessionID, err)
	}
	t.Cleanup(func() {
		_, _ = bridge.CloseSession(context.Background(), &v2.CloseSessionRequest{SessionId: sessionID})
	})
	return bridge
}

// callTool runs one bridge Execute tool call and returns the result event's
// outputs (nil when Execute fails before producing a result).
func callTool(bridge *MCPBridge, sessionID, tool string, extra map[string]string) (map[string]any, error) {
	input := map[string]string{"tool": tool}
	for k, v := range extra {
		input[k] = v
	}
	// The bridge's permission gate is unconditional per Execute; without a
	// Permissions stream the unit-level sender itself resolves the pending
	// request as an allow (mirroring the stream's host-allow path).
	sender := &permittingEventSender{bridge: bridge}
	err := bridge.Execute(context.Background(), &v2.ExecuteRequest{SessionId: sessionID, Input: input}, sender)
	for _, ev := range sender.all() {
		if res := ev.GetResult(); res != nil && len(res.GetOutputsJson()) > 0 {
			var outs map[string]any
			if jsonErr := json.Unmarshal(res.GetOutputsJson(), &outs); jsonErr == nil {
				return outs, err
			}
		}
	}
	return nil, err
}

// TestJSONRPCConformance_ErrorCodeBand asserts the wave-specific spec-band
// boundary codes end to end: the bridge surfaces every MCP error RESPONSE as
// a typed *mcpclient.RPCError with the exact code and message preserved, and
// the shared stdio session survives each protocol-level failure.
func TestJSONRPCConformance_ErrorCodeBand(t *testing.T) {
	for n, code := range []int{-32020, -32021, -32022, -32000, -32099, -32601, -32602, -32700} {
		t.Run(fmt.Sprintf("code_%d", -code), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			env := fmt.Sprintf("MCP_FAIL_CODE=%d", code)
			bridge := bridgeEnvSession(t, ctx, fmt.Sprintf("band-%d", n), env, "ndjson")
			wantMsg := fmt.Sprintf("spec band error: %d", code)

			// Baseline: a good call round-trips before the fault.
			if _, err := callTool(bridge, fmt.Sprintf("band-%d", n), "echo", map[string]string{"message": "before"}); err != nil {
				t.Fatalf("echo before fault: %v", err)
			}
			_, err := callTool(bridge, fmt.Sprintf("band-%d", n), "fault", nil)
			if err == nil {
				t.Fatalf("fault with MCP_FAIL_CODE=%d returned nil error, want typed RPCError", code)
			}
			var rpcErr *mcpclient.RPCError
			if !errors.As(err, &rpcErr) {
				t.Fatalf("fault error not typed *mcpclient.RPCError: %v (%T)", err, err)
			}
			if rpcErr.Code != code {
				t.Errorf("rpc error code = %d, want %d", rpcErr.Code, code)
			}
			if rpcErr.Message != wantMsg {
				t.Errorf("rpc error message = %q, want %q", rpcErr.Message, wantMsg)
			}
			// The session survived the protocol-level error: echo works after.
			if _, err := callTool(bridge, fmt.Sprintf("band-%d", n), "echo", map[string]string{"message": "after"}); err != nil {
				t.Errorf("echo after fault (MCP_FAIL_CODE=%d): %v", code, err)
			}
		})
	}
}

// TestJSONRPCConformance_MismatchedFramingFailsOpen proves both mismatched
// client/fixture framing pairs fail the session open deterministically inside
// a bounded context instead of hanging the caller.
func TestJSONRPCConformance_MismatchedFramingFailsOpen(t *testing.T) {
	for _, tc := range []struct {
		name          string
		clientFraming string
		fixtureEnv    string
	}{{
		name:          "client_lsp_fixture_ndjson",
		clientFraming: "lsp",
		fixtureEnv:    "MCP_FRAMING=ndjson",
	}, {
		name:          "client_ndjson_fixture_lsp",
		clientFraming: "ndjson",
		fixtureEnv:    "",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			// The raw-path probes above assert what the mismatch looks like
			// byte-level; here the contract is the bound: the bridge session
			// must fail before its context deadline elapses, never hang.
			deadline := time.Now().Add(6 * time.Second)
			ctx, cancel := context.WithDeadline(context.Background(), deadline)
			defer cancel()
			bridge := &MCPBridge{sessions: map[string]*sessionState{}}
			cfg := map[string]string{"command": testEchoBin, "framing": tc.clientFraming}
			if tc.fixtureEnv != "" {
				cfg["env"] = tc.fixtureEnv
			}
			_, err := bridge.OpenSession(ctx, &v2.OpenSessionRequest{SessionId: "mismatch-" + tc.name, Config: cfg})
			if err == nil {
				t.Fatal("mismatched framing opened a session, want bounded failure")
			}
			if time.Now().After(deadline) {
				t.Fatalf("session open outlived its own deadline: %v", err)
			}
		})
	}
}

// TestJSONRPCConformance_GarbageOnceResiliency asserts frame-detection
// resiliency: one garbage frame ahead of the first reply is skipped by the
// auto-detecting reader in both framings and the session completes normally.
func TestJSONRPCConformance_GarbageOnceResiliency(t *testing.T) {
	for _, framing := range []string{"lsp", "ndjson"} {
		t.Run(framing, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			bridge := bridgeEnvSession(t, ctx, "garbage-"+framing, "MCP_REPLY_MODE=garbage_once", framing)
			out, err := callTool(bridge, "garbage-"+framing, "echo", map[string]string{"message": "post-garbage"})
			if err != nil {
				t.Fatalf("echo after garbage frame (%s framing): %v", framing, err)
			}
			if text, _ := out["text"].(string); !strings.Contains(text, "post-garbage") {
				t.Errorf("echo output = %v, want echoed message", out)
			}
		})
	}
}
