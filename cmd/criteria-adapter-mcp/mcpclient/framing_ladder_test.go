package mcpclient

// framing_ladder_test.go — the framing probe of Connect (probe-first on a
// stdio session with no framing pinned): the first server/discover probe
// runs in the configured write framing (lsp default), and only an
// uncertain outcome — a -32700 parse-error reply or a transport failure —
// re-probes in the other framing. The fake peers here model the two real
// wire dialects: a newline-based JSON reader (the MCP stdio NDJSON shape,
// which answers unparseable lines with -32700 and never parses a header
// frame) and a strict header-framed reader (Content-Length only; unframed
// junk is skipped and a length-less blank line ends the session).

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"
)

// startLinePeerFakeServer serves one client over in-process pipes with a
// newline-based JSON reader (the MCP stdio NDJSON dialect): every non-blank
// line is one attemptable frame, unparseable lines answer -32700 with a
// null id (spec parse handling, exactly like the echo fixture), and clean
// JSON lines dispatch through handler. Replies go out as NDJSON lines; the
// client's reads auto-detect framing per frame, so the reply framing only
// mirrors the peer.
func startLinePeerFakeServer(t *testing.T, handler func(method string, params map[string]any) handlerResult) (*Client, *recordServer) {
	t.Helper()
	serverRead, clientWrite := io.Pipe()
	clientRead, serverWrite := io.Pipe()

	rec := &recordServer{}
	client := New(clientRead, clientWrite, nil)
	client.EnableFramingNegotiation()

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer serverWrite.Close()
		reader := bufio.NewReader(serverRead)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			text := strings.TrimRight(line, "\r\n")
			if strings.TrimSpace(text) == "" {
				continue
			}
			var req map[string]any
			if json.Unmarshal([]byte(text), &req) != nil {
				// Spec conformant parse handling: -32700 with a null id, then
				// keep reading (a line reader is stateless per line).
				_ = writeNDJSON(serverWrite, map[string]any{
					"jsonrpc": "2.0",
					"id":      nil,
					"error":   map[string]any{"code": -32700, "message": "parse error"},
				})
				continue
			}
			method, _ := req["method"].(string)
			if strings.HasPrefix(method, "notifications/") {
				// Notifications are never answered or recorded (the fixture's
				// dispatch posture): the cancelled-notification resync line
				// must not wedge the read loop with a reply.
				continue
			}
			id, _ := req["id"].(string)
			paramsRaw, _ := json.Marshal(req["params"])
			if method != "" {
				rec.record(method, paramsRaw)
			}
			params := map[string]any{}
			_ = json.Unmarshal(paramsRaw, &params)
			ans := handler(method, params)
			if ans.err != nil {
				errPayload := map[string]any{"code": ans.err.Code, "message": ans.err.Message}
				if len(ans.err.Data) > 0 {
					errPayload["data"] = ans.err.Data
				}
				_ = writeNDJSON(serverWrite, map[string]any{"jsonrpc": "2.0", "id": id, "error": errPayload})
				continue
			}
			_ = writeNDJSON(serverWrite, map[string]any{"jsonrpc": "2.0", "id": id, "result": ans.result})
		}
	}()

	t.Cleanup(func() {
		client.Close()
		clientWrite.Close()
		clientRead.Close()
		serverWrite.Close()
		serverRead.Close()
		<-done
	})
	return client, rec
}

// startStrictHeaderPeerFakeServer serves one client over in-process pipes
// with a header-framed reader (Content-Length only, like the echo fixture's
// default mode): unframed junk lines are skipped, and a blank line before
// any header ends the read (the frame was unframed) with a silent session
// end — the fixture answers such requests with nothing and the stream just
// ends for the client.
func startStrictHeaderPeerFakeServer(t *testing.T, handler func(method string, params map[string]any) handlerResult) (*Client, *recordServer) {
	t.Helper()
	serverRead, clientWrite := io.Pipe()
	clientRead, serverWrite := io.Pipe()

	rec := &recordServer{}
	client := NewWithFraming(clientRead, clientWrite, FramingNDJSON, nil)
	// The framing is pinned (no EnableFramingNegotiation): a pinned config
	// must not flip, even when the peer is unreadable.

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer serverWrite.Close()
		reader := bufio.NewReader(serverRead)
		for {
			payload, err := readStrictHeaderFrame(reader)
			if err != nil {
				return
			}
			var req map[string]any
			if json.Unmarshal(payload, &req) != nil {
				_ = writeJSON(serverWrite, map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32700, "message": "parse error"}})
				continue
			}
			method, _ := req["method"].(string)
			if strings.HasPrefix(method, "notifications/") {
				continue
			}
			id, _ := req["id"].(string)
			paramsRaw, _ := json.Marshal(req["params"])
			if method != "" {
				rec.record(method, paramsRaw)
			}
			params := map[string]any{}
			_ = json.Unmarshal(paramsRaw, &params)
			ans := handler(method, params)
			if ans.err != nil {
				errPayload := map[string]any{"code": ans.err.Code, "message": ans.err.Message}
				if len(ans.err.Data) > 0 {
					errPayload["data"] = ans.err.Data
				}
				_ = writeJSON(serverWrite, map[string]any{"jsonrpc": "2.0", "id": id, "error": errPayload})
				continue
			}
			_ = writeJSON(serverWrite, map[string]any{"jsonrpc": "2.0", "id": id, "result": ans.result})
		}
	}()

	t.Cleanup(func() {
		client.Close()
		clientWrite.Close()
		clientRead.Close()
		serverWrite.Close()
		serverRead.Close()
		<-done
	})
	return client, rec
}

// readStrictHeaderFrame reads one Content-Length framed frame strictly:
// header lines are parsed for Content-Length, the blank separator ends the
// header block, and the payload follows by length. A blank line before any
// valid frame (i.e. the peer wrote unframed JSON lines) is a framing
// failure: the read side ends the session silently, exactly like the echo
// fixture's header mode.
func readStrictHeaderFrame(r *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) && line == "" {
				return nil, io.EOF
			}
			return nil, err
		}
		text := strings.TrimRight(line, "\r\n")
		if text == "" {
			break
		}
		name, value, found := strings.Cut(text, ":")
		if found && strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			n, parseErr := strconv.Atoi(strings.TrimSpace(value))
			if parseErr != nil {
				return nil, parseErr
			}
			length = n
		}
	}
	if length < 0 {
		return nil, errors.New("missing content-length")
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func TestConnectFramingLadderAdoptsNDJSONModern(t *testing.T) {
	// probe-first against a newline-based modern server: the lsp-framed
	// probe is unreadable to a line reader (-32700), the ladder re-probes
	// in ndjson, the discover reply adopts the stateless core, and the
	// session's write framing becomes ndjson for everything that follows.
	client, rec := startLinePeerFakeServer(t, func(method string, params map[string]any) handlerResult {
		switch method {
		case "server/discover":
			return handlerResult{result: modernDiscoverResult("1.0.0", 5000)}
		case "tools/list":
			return echoCallResult(ResultTypeComplete)
		case "tools/call":
			return handlerResult{result: map[string]any{"content": []map[string]any{{"type": "text", "text": "ok"}}}}
		default:
			return unknownMethodError()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Connect(ctx, NegotiationProbeFirst, "criteria-test", "v1"); err != nil {
		t.Fatalf("Connect: %v (dispatched: %v)", err, rec.snapshotMethods())
	}
	if got := client.stdio().currentFraming(); got != FramingNDJSON {
		t.Fatalf("session write framing = %q, want ndjson after the ladder", got)
	}
	if !client.stateless {
		t.Fatal("a discover reply adopted the stateless core, session stayed legacy")
	}

	// TWO server/discover probes are dispatched on the wire: the resync
	// newline re-terminates the lsp-framed probe so the line reader parses
	// it (its late reply carries the abandoned probe's id and is dropped
	// client-side), and the ndjson probe follows on its own line — which is
	// the one that adopts the stateless core. Never an initialize.
	methods := rec.snapshotMethods()
	if len(methods) < 1 || methods[0] != "server/discover" {
		t.Fatalf("expected the re-delivered probe as the first dispatched method, saw %v", methods)
	}
	if n := countOf(methods, "server/discover"); n != 2 {
		t.Fatalf("server/discover dispatched %d times, want the re-delivered lsp probe + the ndjson probe", n)
	}
	if n := countOf(methods, "initialize"); n != 0 {
		t.Fatalf("a modern peer adopted the stateless core but %d initialize requests ran", n)
	}

	page, err := client.ListToolsDetailed(ctx)
	if err != nil {
		t.Fatalf("ListToolsDetailed over ndjson: %v", err)
	}
	if len(page.Tools) != 1 || page.Tools[0].Name != "echo" {
		t.Fatalf("unexpected tools page: %+v", page.Tools)
	}
	if _, err := client.CallToolTracked(ctx, "echo", map[string]any{"message": "hi"}, "tok-1", nil); err != nil {
		t.Fatalf("CallToolTracked over ndjson: %v", err)
	}
	if got := client.stdio().currentFraming(); got != FramingNDJSON {
		t.Fatalf("framing reverted during traffic: %q", got)
	}
}

func TestConnectFramingLadderLegacyLinePeerFallsBack(t *testing.T) {
	// probe-first against a newline-based legacy server (-32601 to the
	// discover probe): the second probe's -32601 is a parsed reply, so the
	// ndjson dialect is proven and the legacy initialize fallback runs in
	// it. No lsp-framed request ever reached the peer.
	client, rec := startLinePeerFakeServer(t, func(method string, params map[string]any) handlerResult {
		switch method {
		case "server/discover":
			return unknownMethodError()
		case "initialize":
			return handlerResult{result: map[string]any{"protocolVersion": ProtocolVersion20250326}}
		case "tools/call":
			return handlerResult{result: map[string]any{"content": []map[string]any{{"type": "text", "text": "ok"}}}}
		default:
			return unknownMethodError()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Connect(ctx, NegotiationProbeFirst, "criteria-test", "v1"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if got := client.stdio().currentFraming(); got != FramingNDJSON {
		t.Fatalf("session write framing = %q, want ndjson", got)
	}
	if client.stateless || client.protocolVersion != ProtocolVersion20250326 {
		t.Fatalf("fallback must stay legacy: stateless=%v version=%q", client.stateless, client.protocolVersion)
	}

	methods := rec.snapshotMethods()
	if n := countOf(methods, "server/discover"); n != 2 {
		t.Fatalf("server/discover dispatched %d times, want the re-delivered lsp probe + the ndjson probe", n)
	}
	if n := countOf(methods, "initialize"); n != 1 {
		t.Fatalf("initialize dispatched %d times, want exactly the ndjson handshake", n)
	}
	if _, err := client.CallTool(ctx, "echo", map[string]any{"message": "hi"}); err != nil {
		t.Fatalf("CallTool over legacy+ndjson: %v", err)
	}
	if _, hasMeta := rec.lastParams(t, "tools/call")["_meta"]; hasMeta {
		t.Fatal("legacy sessions must not send request _meta")
	}
}

func TestConnectFramingLadderRejectsUnsupportedVersionOnSecondProbe(t *testing.T) {
	// -32022 on the second probe is still deterministic: an ndjson-only
	// modern peer that rejects this client's protocol version fails the
	// open with the no-mutual-version error and never runs the legacy
	// initialize fallback.
	client, rec := startLinePeerFakeServer(t, func(method string, params map[string]any) handlerResult {
		switch method {
		case "server/discover":
			return unsupportedVersionError([]string{"2036-07-28"})
		case "initialize":
			return handlerResult{result: map[string]any{"protocolVersion": ProtocolVersion20250326}}
		default:
			return unknownMethodError()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := client.Connect(ctx, NegotiationProbeFirst, "criteria-test", "v1")
	if err == nil {
		t.Fatal("Connect must fail when the modern peer rejects the requested version")
	}
	if !strings.Contains(err.Error(), "no mutually supported protocol version") {
		t.Fatalf("error = %v, want the deterministic no-mutual-version failure", err)
	}
	if n := countOf(rec.snapshotMethods(), "initialize"); n != 0 {
		t.Fatalf("deterministic -32022 must not fall back to initialize (saw %d calls)", n)
	}
	if n := countOf(rec.snapshotMethods(), "server/discover"); n != 2 {
		t.Fatalf("server/discover dispatched %d times, want the re-delivered probe + the ndjson probe", n)
	}
}

func TestConnectPinnedNDJSONAgainstHeaderOnlyPeerFails(t *testing.T) {
	// A pinned ndjson config against a header-framed-only legacy server:
	// the unframed probe is silently unreadable, the pinned framing may not
	// flip, and the fallback that runs on a session the peer cannot
	// understand fails the open instead of half-connecting.
	client, rec := startStrictHeaderPeerFakeServer(t, func(method string, params map[string]any) handlerResult {
		switch method {
		case "initialize":
			return handlerResult{result: map[string]any{"protocolVersion": ProtocolVersion20250326}}
		default:
			return unknownMethodError()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := client.Connect(ctx, NegotiationProbeFirst, "criteria-test", "v1"); err == nil {
		t.Fatal("Connect with a pinned ndjson framing against a header-only peer must fail")
	}
	if got := client.stdio().currentFraming(); got != FramingNDJSON {
		t.Fatalf("a pinned framing must not flip (now %q)", got)
	}
	if n := len(rec.snapshotMethods()); n != 0 {
		t.Fatalf("the peer parsed %v, want no dispatched request at all", rec.snapshotMethods())
	}
}

func countOf(list []string, want string) int {
	n := 0
	for _, v := range list {
		if v == want {
			n++
		}
	}
	return n
}
