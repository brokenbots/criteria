package mcpclient

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordServer records every request the client sends so tests can assert
// wire shape (methods, order, and _meta fields).
type recordServer struct {
	mu      sync.Mutex
	methods []string
	params  []json.RawMessage
}

func (s *recordServer) record(method string, raw json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.methods = append(s.methods, method)
	s.params = append(s.params, raw)
}

func (s *recordServer) snapshotMethods() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.methods...)
}

func (s *recordServer) lastParams(t *testing.T, method string) map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.methods) - 1; i >= 0; i-- {
		if s.methods[i] == method {
			out := map[string]any{}
			if err := json.Unmarshal(s.params[i], &out); err != nil {
				t.Fatalf("decode recorded %s params: %v", method, err)
			}
			return out
		}
	}
	t.Fatalf("no recorded request for method %s (have %v)", method, s.snapshotMethods())
	return nil
}

// handlerResult is what a fake server answers per request.
type handlerResult struct {
	result any
	err    *RPCError
}

// startFakeServer serves one client over in-process pipes, answering each
// request via handler (mirrors the harness of client_test.go).
func startFakeServer(t *testing.T, handler func(method string, params map[string]any) handlerResult) (*Client, *recordServer, func()) {
	t.Helper()
	serverRead, clientWrite := io.Pipe()
	clientRead, serverWrite := io.Pipe()

	rec := &recordServer{}
	client := New(clientRead, clientWrite, nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer serverWrite.Close()
		reader := bufio.NewReader(serverRead)
		for {
			payload, err := readFrame(reader)
			if err != nil {
				return
			}
			var req map[string]any
			if err := json.Unmarshal(payload, &req); err != nil {
				continue
			}
			method, _ := req["method"].(string)
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
					errPayload["data"] = json.RawMessage(ans.err.Data)
				}
				_ = writeJSON(serverWrite, map[string]any{"jsonrpc": "2.0", "id": id, "error": errPayload})
				continue
			}
			_ = writeJSON(serverWrite, map[string]any{"jsonrpc": "2.0", "id": id, "result": ans.result})
		}
	}()

	cleanup := func() {
		client.Close()
		clientWrite.Close()
		clientRead.Close()
		serverWrite.Close()
		serverRead.Close()
		<-done
	}
	t.Cleanup(cleanup)
	return client, rec, cleanup
}

// modernDiscoverResult is a spec-shaped server/discover result.
func modernDiscoverResult(name, version string, ttlMs int64) map[string]any {
	return map[string]any{
		"resultType":        "complete",
		"supportedVersions": []string{ProtocolVersion20260728},
		"capabilities":      map[string]any{"tools": map[string]any{}},
		"instructions":      "echo server",
		"ttlMs":             ttlMs,
		"cacheScope":        "public",
		"_meta": map[string]any{
			MetaServerInfo: map[string]any{"name": name, "version": version},
		},
	}
}

// unsupportedVersionError is the spec-shaped -32022 error.
func unsupportedVersionError(supported []string) handlerResult {
	data, _ := json.Marshal(map[string]any{"supported": supported, "requested": ProtocolVersion20260728})
	return handlerResult{err: &RPCError{Code: ErrorCodeUnsupportedProtocolVersion, Message: "requested version unsupported", Data: data}}
}

func unknownMethodError() handlerResult {
	return handlerResult{err: &RPCError{Code: -32601, Message: "Method not found"}}
}

func echoCallResult(resultType string) handlerResult {
	tools := map[string]any{
		"tools": []map[string]any{{"name": "echo", "description": "echo"}},
	}
	if resultType != "" {
		tools["resultType"] = resultType
	}
	return handlerResult{result: tools}
}

func TestParseNegotiation(t *testing.T) {
	cases := []struct {
		in   string
		want Negotiation
		ok   bool
	}{
		{"", NegotiationProbeFirst, true},
		{"probe-first", NegotiationProbeFirst, true},
		{" PROBE-FIRST ", NegotiationProbeFirst, true},
		{"initialize", NegotiationInitialize, true},
		{"discover", NegotiationDiscover, true},
		{"handshake", "", false},
		{"probe first", "", false},
	}
	for _, tc := range cases {
		got, err := ParseNegotiation(tc.in)
		if tc.ok && (err != nil || got != tc.want) {
			t.Errorf("ParseNegotiation(%q) = (%q, %v), want %q with no error", tc.in, got, err, tc.want)
		}
		if !tc.ok && err == nil {
			t.Errorf("ParseNegotiation(%q) accepted an unknown value", tc.in)
		}
	}
}

func TestConnectProbeFirstAdoptsStatelessCore(t *testing.T) {
	client, rec, _ := startFakeServer(t, func(method string, params map[string]any) handlerResult {
		switch method {
		case "server/discover":
			return handlerResult{result: modernDiscoverResult("echo", "1.0.0", 5000)}
		case "tools/list", "tools/call":
			return echoCallResult(ResultTypeComplete)
		default:
			return unknownMethodError()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := client.Connect(ctx, NegotiationProbeFirst, "criteria-test", "v1"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if !client.stateless || client.protocolVersion != ProtocolVersion20260728 {
		t.Fatalf("session did not adopt the stateless core: stateless=%v version=%q", client.stateless, client.protocolVersion)
	}

	// The probe itself advertises the protocol version + client capabilities
	// + client info in _meta (the stateless core has no handshake).
	dmeta, _ := rec.lastParams(t, "server/discover")["_meta"].(map[string]any)
	if dmeta == nil || dmeta[MetaProtocolVersion] != ProtocolVersion20260728 {
		t.Fatalf("server/discover _meta missing %s: %v", MetaProtocolVersion, dmeta)
	}
	if _, ok := dmeta[MetaClientCapabilities].(map[string]any); !ok {
		t.Fatalf("server/discover _meta missing %s object: %v", MetaClientCapabilities, dmeta)
	}
	info, _ := dmeta[MetaClientInfo].(map[string]any)
	if info == nil || info["name"] != "criteria-test" || info["version"] != "v1" {
		t.Fatalf("server/discover _meta missing client info: %v", dmeta)
	}

	page, err := client.ListToolsDetailed(ctx)
	if err != nil {
		t.Fatalf("ListToolsDetailed: %v", err)
	}
	if len(page.Tools) != 1 || page.Tools[0].Name != "echo" {
		t.Fatalf("unexpected tools page: %+v", page.Tools)
	}
	if page.TTLms != 0 {
		t.Fatalf("tools/list is not the discover result: ttlMs=%d", page.TTLms)
	}

	// tools/call on a stateless session carries the request _meta and merges
	// the KB-155 progress token into it.
	res, err := client.CallToolTracked(ctx, "echo", map[string]any{"message": "hi"}, "tok-1")
	if err != nil {
		t.Fatalf("CallToolTracked: %v", err)
	}
	if res.IsInputRequired() {
		t.Fatal("a resultType-complete result must not read as input_required")
	}
	cmeta, _ := rec.lastParams(t, "tools/call")["_meta"].(map[string]any)
	if cmeta == nil {
		t.Fatal("stateless tools/call carried no _meta")
	}
	if cmeta["progressToken"] != "tok-1" || cmeta[MetaProtocolVersion] != ProtocolVersion20260728 {
		t.Fatalf("tools/call _meta missing progressToken or protocolVersion: %v", cmeta)
	}
	if _, ok := cmeta[MetaClientCapabilities]; !ok {
		t.Fatalf("tools/call _meta missing client capabilities: %v", cmeta)
	}

	lmeta, _ := rec.lastParams(t, "tools/list")["_meta"].(map[string]any)
	if lmeta == nil || lmeta[MetaProtocolVersion] != ProtocolVersion20260728 {
		t.Fatalf("stateless tools/list carried no request _meta: %v", lmeta)
	}
}

func TestConnectProbeFirstFallsBackToLegacyHandshake(t *testing.T) {
	client, rec, _ := startFakeServer(t, func(method string, params map[string]any) handlerResult {
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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := client.Connect(ctx, NegotiationProbeFirst, "criteria-test", "v1"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	methods := rec.snapshotMethods()
	if len(methods) != 2 || methods[0] != "server/discover" || methods[1] != "initialize" {
		t.Fatalf("probe-first must probe then fall back to initialize, saw %v", methods)
	}
	iparams := rec.lastParams(t, "initialize")
	if iparams["protocolVersion"] != ProtocolVersion20250326 {
		t.Fatalf("legacy handshake must pin %s, got %v", ProtocolVersion20250326, iparams["protocolVersion"])
	}
	if client.stateless || client.protocolVersion != ProtocolVersion20250326 {
		t.Fatalf("fallback session must stay legacy: stateless=%v version=%q", client.stateless, client.protocolVersion)
	}

	// Legacy sessions keep the historical bare request shape: no _meta.
	if _, err := client.CallTool(ctx, "echo", nil); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if _, hasMeta := rec.lastParams(t, "tools/call")["_meta"]; hasMeta {
		t.Fatal("legacy sessions must not send request _meta")
	}
}

func TestConnectLegacyParityForFreshClient(t *testing.T) {
	// A client used without Connect/Initialize (direct New -> CallTool) must
	// ship the exact historical wire shape: empty-params tools/list and a
	// tools/call without _meta.
	client, rec, _ := startFakeServer(t, func(method string, params map[string]any) handlerResult {
		if method == "tools/call" {
			return handlerResult{result: map[string]any{"content": []map[string]any{{"type": "text", "text": "ok"}}}}
		}
		return unknownMethodError()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := client.CallTool(ctx, "echo", map[string]any{"message": "hi"}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if _, hasMeta := rec.lastParams(t, "tools/call")["_meta"]; hasMeta {
		t.Fatal("a fresh client must not send request _meta")
	}
}

func TestConnectUnsupportedProtocolVersionDoesNotFallBack(t *testing.T) {
	// Spec stdio rule: an UnsupportedProtocolVersionError (-32022) means the
	// peer answered server/discover and rejected the requested version —
	// that is a modern peer, so the fallback must NOT run; negotiation fails
	// deterministically naming the advertised versions.
	client, rec, _ := startFakeServer(t, func(method string, params map[string]any) handlerResult {
		switch method {
		case "server/discover":
			return unsupportedVersionError([]string{"2036-07-28"})
		case "initialize":
			return handlerResult{result: map[string]any{"protocolVersion": ProtocolVersion20250326}}
		default:
			return unknownMethodError()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := client.Connect(ctx, NegotiationProbeFirst, "criteria-test", "v1")
	if err == nil {
		t.Fatal("Connect must fail when no protocol version is mutually supported")
	}
	if !IsUnsupportedProtocolVersion(err) {
		t.Fatalf("negotiation failure lost the -32022 typing: %v", err)
	}
	if versions := UnsupportedVersions(err); len(versions) != 1 || versions[0] != "2036-07-28" {
		t.Fatalf("advertised versions not surfaced: %v", versions)
	}
	if msg := err.Error(); !strings.Contains(msg, "2036-07-28") {
		t.Fatalf("failure message should name the advertised versions: %s", msg)
	}
	for _, m := range rec.snapshotMethods() {
		if m == "initialize" {
			t.Fatal("UnsupportedProtocolVersion must not fall back to the initialize handshake")
		}
	}
}

func TestConnectDiscoverModeHasNoLegacyFallback(t *testing.T) {
	client, rec, _ := startFakeServer(t, func(method string, params map[string]any) handlerResult {
		if method == "server/discover" {
			return unknownMethodError()
		}
		return unknownMethodError()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := client.Connect(ctx, NegotiationDiscover, "criteria-test", "v1")
	if err == nil || !strings.Contains(err.Error(), "legacy fallback is disabled") {
		t.Fatalf("discover mode must fail terminally on a non-modern peer, got %v", err)
	}
	if methods := rec.snapshotMethods(); len(methods) != 1 || methods[0] != "server/discover" {
		t.Fatalf("discover mode must not send anything but the probe, saw %v", methods)
	}
}

func TestConnectInitializeModeSkipsProbe(t *testing.T) {
	client, rec, _ := startFakeServer(t, func(method string, params map[string]any) handlerResult {
		if method == "initialize" {
			return handlerResult{result: map[string]any{"protocolVersion": ProtocolVersion20250326}}
		}
		return unknownMethodError()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := client.Connect(ctx, NegotiationInitialize, "criteria-test", "v1"); err != nil {
		t.Fatalf("Connect(initialize): %v", err)
	}
	if methods := rec.snapshotMethods(); len(methods) != 1 || methods[0] != "initialize" {
		t.Fatalf("initialize mode must not probe, saw %v", methods)
	}
	iparams := rec.lastParams(t, "initialize")
	if iparams["protocolVersion"] != ProtocolVersion20250326 {
		t.Fatalf("legacy handshake must pin %s, got %v", ProtocolVersion20250326, iparams["protocolVersion"])
	}
	_ = client
}

func TestDiscoverParsesResultAndDoesNotMutateSession(t *testing.T) {
	client, _, _ := startFakeServer(t, func(method string, params map[string]any) handlerResult {
		return handlerResult{result: modernDiscoverResult("echo", "2.0.0", 4200)}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	res, err := client.Discover(ctx, ProtocolVersion20260728)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if res.ResultType != ResultTypeComplete {
		t.Fatalf("unexpected resultType %q", res.ResultType)
	}
	if len(res.SupportedVersions) != 1 || res.SupportedVersions[0] != ProtocolVersion20260728 {
		t.Fatalf("unexpected supported versions %v", res.SupportedVersions)
	}
	if res.TTLms != 4200 || res.CacheScope != "public" {
		t.Fatalf("CacheableResult contract not parsed: ttlMs=%d cacheScope=%q", res.TTLms, res.CacheScope)
	}
	if res.ServerInfo.Name != "echo" || res.ServerInfo.Version != "2.0.0" {
		t.Fatalf("_meta serverInfo not parsed: %+v", res.ServerInfo)
	}
	if res.Instructions != "echo server" {
		t.Fatalf("instructions not parsed: %q", res.Instructions)
	}
	if client.stateless || client.protocolVersion != "" {
		t.Fatal("Discover is a probe: it must not mutate session state")
	}
}

func TestCallToolResultTypes(t *testing.T) {
	client, _, _ := startFakeServer(t, func(method string, params map[string]any) handlerResult {
		switch method {
		case "server/discover":
			return handlerResult{result: modernDiscoverResult("echo", "1.0.0", 0)}
		case "tools/call":
			if len(params) > 0 {
				if args, ok := params["arguments"].(map[string]any); ok && args["kind"] == "mrtr" {
					return handlerResult{result: map[string]any{
						"resultType": ResultTypeInputRequired,
						"inputRequests": map[string]any{
							"req-1": map[string]any{"method": "sampling/createMessage", "params": map[string]any{}},
						},
						"requestState": "st-42",
					}}
				}
			}
			return handlerResult{result: map[string]any{
				"resultType": ResultTypeComplete,
				"content":    []map[string]any{{"type": "text", "text": "ok"}},
			}}
		default:
			return unknownMethodError()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Connect(ctx, NegotiationDiscover, "criteria-test", "v1"); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	res, err := client.CallTool(ctx, "mrtr", map[string]any{"kind": "mrtr"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsInputRequired() {
		t.Fatal("input_required result not recognized")
	}
	enc, ok := res.InputRequests["req-1"]
	if !ok {
		t.Fatalf("inputRequests not decoded: %v", res.InputRequests)
	}
	var envelope map[string]any
	if err := json.Unmarshal(enc, &envelope); err != nil || envelope["method"] != "sampling/createMessage" {
		t.Fatalf("input request envelope not readable: %v (%v)", envelope, err)
	}
	if res.RequestState != "st-42" {
		t.Fatalf("requestState not parsed: %q", res.RequestState)
	}

	plain, err := client.CallTool(ctx, "echo", nil)
	if err != nil {
		t.Fatalf("CallTool(complete): %v", err)
	}
	if plain.IsInputRequired() {
		t.Fatal("complete result misread as input_required")
	}

	// Legacy-shaped result (no resultType) is complete by definition.
	client.stateless = false
	legacy, err := client.CallTool(ctx, "echo", nil)
	if err != nil {
		t.Fatalf("CallTool(legacy shape): %v", err)
	}
	if legacy.IsInputRequired() {
		t.Fatal("a result without resultType must be treated as complete")
	}
}

func TestToolsListInputRequiredRejected(t *testing.T) {
	client, _, _ := startFakeServer(t, func(method string, params map[string]any) handlerResult {
		if method == "tools/list" {
			return handlerResult{result: map[string]any{
				"resultType": ResultTypeInputRequired,
				"inputRequests": map[string]any{
					"req-1": map[string]any{"method": "elicitation/create", "params": map[string]any{}},
				},
			}}
		}
		return unknownMethodError()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := client.ListToolsDetailed(ctx); err == nil || !strings.Contains(err.Error(), "SEP-2322") {
		t.Fatalf("tools/list must reject interim input_required results, got %v", err)
	}
}

func TestReservedErrorHelpers(t *testing.T) {
	if IsUnsupportedProtocolVersion(io.EOF) {
		t.Fatal("a transport error is not UnsupportedProtocolVersion")
	}
	mismatch := &RPCError{Code: ErrorCodeHeaderMismatch, Message: "header mismatch"}
	if IsUnsupportedProtocolVersion(mismatch) {
		t.Fatal("-32020 is not -32022")
	}
	empty := &RPCError{Code: ErrorCodeUnsupportedProtocolVersion, Message: "unsupported"}
	if !IsUnsupportedProtocolVersion(empty) {
		t.Fatal("-32022 without data still types as UnsupportedProtocolVersion")
	}
	if versions := UnsupportedVersions(empty); versions != nil {
		t.Fatalf("no data must yield no versions: %v", versions)
	}
	wrapped := fmt.Errorf("dial: %w", unsupportedVersionError([]string{"2036-07-28"}).err)
	if versions := UnsupportedVersions(wrapped); len(versions) != 1 || versions[0] != "2036-07-28" {
		t.Fatalf("errors.As chain must still surface versions: %v", versions)
	}
}
