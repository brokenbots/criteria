package adapterhost

// tool_call_test.go — tests for the CRI-159 host seam: §2 target parsing and
// detection, the full tool-call decision matrix (capability-missing, policy
// deny, unknown adapter, unknown static tool, malformed target, self-call,
// nil-manager fallback, grant union), and the plain-request regression that
// non-adapter-root dotted tool names keep flowing through the plain
// permission path. Nested execution against a real SessionManager lives in
// tool_call_exec_test.go.

import (
	"testing"
	"time"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/workflow"
)

// spyPolicy counts Decide calls so tests can assert that the capability gate
// (and earlier failure paths) make no policy call.
type spyPolicy struct {
	calls  int
	allow  bool
	reason string
}

func (p *spyPolicy) Decide(req PermissionRequest) (allow bool, reason string) {
	p.calls++
	return p.allow, p.reason
}

// newToolCallState builds a PermissionState with an active stream so events
// can be collected from ps.Requests().
func newToolCallState(t *testing.T, audit *sliceAuditWriter) *permissionState {
	t.Helper()
	ps := NewPermissionState("sess-1", audit)
	ps.SetStreamCancel(func() {})
	return ps
}

// readStreamEvent pops one PermissionEvent off the stream with a timeout.
// The pointer is returned as-is (protobuf messages are non-copyable).
func readStreamEvent(t *testing.T, ps *permissionState) *v2.PermissionEvent {
	t.Helper()
	select {
	case ev := <-ps.Requests():
		return ev
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stream event")
		return nil
	}
}

// readToolCallResult drains stream events until a tool_call_result arrives,
// skipping the interleaved permission request/cancel grant events.
func readToolCallResult(t *testing.T, ps *permissionState) *v2.ToolCallResult {
	t.Helper()
	for i := 0; i < 3; i++ {
		if tcr := readStreamEvent(t, ps).GetToolCallResult(); tcr != nil {
			return tcr
		}
	}
	t.Fatal("expected tool_call_result event within 3 stream events")
	return nil
}

// toolCallGraph builds a graph with the given adapter nodes, plus the
// caller-side toolGrantRef wiring for steps.
func toolCallGraph(nodes map[string]*workflow.AdapterNode) *workflow.FSMGraph {
	return &workflow.FSMGraph{Adapters: nodes}
}

func TestParseToolCallTarget(t *testing.T) {
	cases := []struct {
		name       string
		target     string
		wantRef    string
		wantTool   string
		wantParsed bool
	}{
		{name: "bare whole-surface", target: "adapter.shell.runner.tools", wantRef: "shell.runner", wantTool: "", wantParsed: true},
		{name: "named tool", target: "adapter.shell.runner.tools.git_status", wantRef: "shell.runner", wantTool: "git_status", wantParsed: true},
		{name: "hyphenated tool", target: "adapter.shell.runner.tools.git-status", wantRef: "shell.runner", wantTool: "git-status", wantParsed: true},
		{name: "underscored type", target: "adapter.mcp_server.fs.tools.read", wantRef: "mcp_server.fs", wantTool: "read", wantParsed: true},
		{name: "adapter ref only", target: "adapter.shell.runner", wantParsed: false},
		{name: "too few segments", target: "adapter.shell", wantParsed: false},
		{name: "two segments", target: "adapter.tools", wantParsed: false},
		{name: "too many segments", target: "adapter.shell.runner.tools.git_status.extra", wantParsed: false},
		{name: "wrong marker segment", target: "adapter.shell.runner.tool.git_status", wantParsed: false},
		{name: "wrong root", target: "shell.runner.tools", wantParsed: false},
		{name: "empty label", target: "adapter..runner.tools", wantParsed: false},
		{name: "trailing dot", target: "adapter.shell.runner.tools.", wantParsed: false},
		{name: "leading digit label", target: "adapter.shell.2fast.tools", wantParsed: false},
		{name: "punctuation label", target: "adapter.shell.runner.tools.read$", wantParsed: false},
		{name: "empty target", target: "", wantParsed: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseToolCallTarget(tc.target)
			if ok != tc.wantParsed {
				t.Fatalf("parsed = %v, want %v", ok, tc.wantParsed)
			}
			if !tc.wantParsed {
				return
			}
			if got.AdapterRef != tc.wantRef || got.Tool != tc.wantTool {
				t.Errorf("parsed = %+v, want ref=%q tool=%q", got, tc.wantRef, tc.wantTool)
			}
			if round := got.String(); round != tc.target {
				t.Errorf("String() = %q, want %q", round, tc.target)
			}
		})
	}
}

func TestToolCallRequestDetected(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
		want    bool
	}{
		{name: "kind marker only", payload: map[string]any{"kind": "adapter_tool"}, want: true},
		{name: "kind marker with bad target", payload: map[string]any{"kind": "adapter_tool", "target": "garbage"}, want: true},
		{name: "adapter ref target", payload: map[string]any{"target": "adapter.shell.runner"}, want: true},
		{name: "bare tool target", payload: map[string]any{"target": "adapter.shell.runner.tools"}, want: true},
		{name: "named tool target", payload: map[string]any{"target": "adapter.shell.runner.tools.git_status"}, want: true},
		{name: "two-segment target", payload: map[string]any{"target": "shell.runner"}, want: false},
		{name: "six-segment target", payload: map[string]any{"target": "adapter.a.b.tools.c.d"}, want: false},
		{name: "fourth segment not tools", payload: map[string]any{"target": "adapter.shell.runner.tool.git_status"}, want: false},
		{name: "non-adapter root dotted tool", payload: map[string]any{"target": "mcp.filesystem.read_file"}, want: false},
		{name: "plain permission request", payload: map[string]any{"request_id": "r1", "tool": "read_file"}, want: false},
		{name: "non-string target", payload: map[string]any{"target": 42}, want: false},
		{name: "empty target", payload: map[string]any{"target": ""}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := toolCallRequestDetected(tc.payload); got != tc.want {
				t.Errorf("detected = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestToolGrantAllows(t *testing.T) {
	// A bare whole-surface grant covers every call to that callee.
	refsBare := []workflow.AdapterToolRef{{CalleeRef: "shell.runner", Tool: ""}}
	// A named grant covers exactly that named call.
	refsNamed := []workflow.AdapterToolRef{{CalleeRef: "shell.runner", Tool: "git_status"}}
	cases := []struct {
		name      string
		refs      []workflow.AdapterToolRef
		calleeRef string
		tool      string
		want      bool
	}{
		{name: "bare grant covers bare call", refs: refsBare, calleeRef: "shell.runner", tool: "", want: true},
		{name: "bare grant covers named call", refs: refsBare, calleeRef: "shell.runner", tool: "run", want: true},
		{name: "named grant covers named call", refs: refsNamed, calleeRef: "shell.runner", tool: "git_status", want: true},
		{name: "named grant does not cover other tool", refs: refsNamed, calleeRef: "shell.runner", tool: "missing", want: false},
		{name: "named grant does not cover bare call", refs: refsNamed, calleeRef: "shell.runner", tool: "", want: false},
		{name: "other callee grant ignored", refs: refsNamed, calleeRef: "other.instance", tool: "git_status", want: false},
		{name: "no grants deny", refs: nil, calleeRef: "shell.runner", tool: "run", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := toolGrantAllows(tc.refs, tc.calleeRef, tc.tool); got != tc.want {
				t.Errorf("toolGrantAllows(%q, %q) = %v, want %v", tc.calleeRef, tc.tool, got, tc.want)
			}
		})
	}
}

// runToolCall builds a session/state/sink wired for the decision-matrix tests
// and dispatches a single tool-call payload through the intercept sink.
type toolCallFixture struct {
	inner  *adapterEventCollector
	sink   *permissionInterceptSink
	ps     *permissionState
	audit  *sliceAuditWriter
	policy *spyPolicy
}

func runToolCall(t *testing.T, sess *Session, ps *permissionState, policy *spyPolicy, step *workflow.StepNode, graph *workflow.FSMGraph, payload map[string]any) *toolCallFixture {
	t.Helper()
	inner := &adapterEventCollector{}
	sink := &permissionInterceptSink{
		inner:     inner,
		permState: ps,
		session:   sess,
		step:      step,
		graph:     graph,
	}
	if policy != nil {
		ps.SetPolicy(policy)
	}
	sink.Adapter("permission.request", payload)
	return &toolCallFixture{inner: inner, sink: sink, ps: ps, audit: ps.audit.(*sliceAuditWriter), policy: policy}
}

// fixturePolicy is the default allow-all policy for otherwise-allowed calls.
func allowAllPolicy() *spyPolicy {
	return &spyPolicy{allow: true, reason: "matched: adapter.shell.runner.*"}
}

func TestToolCall_CapabilityMissing(t *testing.T) {
	audit := &sliceAuditWriter{}
	ps := newToolCallState(t, audit)
	policy := allowAllPolicy()
	sess := &Session{Adapter: "shell", Name: "shell.local"} // no adapter_tools capability
	fx := runToolCall(t, sess, ps, policy, nil, toolCallGraph(map[string]*workflow.AdapterNode{
		"shell.runner": {Type: "shell", Name: "runner", StaticTools: []string{"run"}},
	}), map[string]any{
		"request_id": "req-1",
		"target":     "adapter.shell.runner.tools.run",
	})

	ev := readStreamEvent(t, ps)
	tcr := ev.GetToolCallResult()
	if tcr == nil {
		t.Fatalf("expected tool_call_result event, got %T", ev.Event)
	}
	if tcr.RequestId != "req-1" || tcr.CallError != callErrorCapabilityMissing {
		t.Errorf("tool_call_result = %+v, want req-1/capability_missing", tcr)
	}
	if fx.inner.saw("permission.granted") || fx.inner.saw("permission.denied") {
		t.Error("expected no permission grant/deny events on capability gate")
	}
	if policy.calls != 0 {
		t.Errorf("policy called %d times, want 0", policy.calls)
	}
	if fx.sink.anyDenied {
		t.Error("expected anyDenied=false for typed capability failure")
	}
	entries := audit.all()
	if len(entries) != 1 || entries[0].Reason != "adapter tool call rejected: "+callErrorCapabilityMissing {
		t.Errorf("audit entries = %+v, want single capability_missing rejection", entries)
	}
}

func TestToolCall_PolicyDeny(t *testing.T) {
	audit := &sliceAuditWriter{}
	ps := newToolCallState(t, audit)
	policy := &spyPolicy{allow: false, reason: "no matching allow_tools entry"}
	sess := &Session{Adapter: "shell", Name: "shell.local", Capabilities: []string{adapterToolsCapability}}
	fx := runToolCall(t, sess, ps, policy, nil, toolCallGraph(map[string]*workflow.AdapterNode{
		"shell.runner": {Type: "shell", Name: "runner", StaticTools: []string{"run"}},
	}), map[string]any{
		"request_id": "req-1",
		"target":     "adapter.shell.runner.tools.run",
		"tool":       "run",
	})

	if policy.calls != 1 {
		t.Fatalf("policy called %d times, want 1", policy.calls)
	}
	ev := readStreamEvent(t, ps)
	cancel := ev.GetCancel()
	if cancel == nil {
		t.Fatalf("expected cancel event, got %T", ev.Event)
	}
	if cancel.RequestId != "req-1" || cancel.Reason != "no matching allow_tools entry" {
		t.Errorf("cancel = %+v, want req-1 with policy reason", cancel)
	}
	if fx.inner.saw("permission.granted") {
		t.Error("expected no permission.granted on deny")
	}
	denied, ok := fx.inner.first("permission.denied")
	if !ok {
		t.Fatal("expected permission.denied event")
	}
	if denied["request_id"] != "req-1" || denied["tool"] != "run" || denied["reason"] != "no matching allow_tools entry" {
		t.Errorf("permission.denied = %+v", denied)
	}
	if !fx.sink.anyDenied {
		t.Error("expected anyDenied=true after policy denial")
	}
}

func TestToolCall_MalformedTarget(t *testing.T) {
	for _, target := range []string{"adapter.shell.runner", "garbage", "adapter.shell.runner.tool.run"} {
		audit := &sliceAuditWriter{}
		ps := newToolCallState(t, audit)
		policy := allowAllPolicy()
		sess := &Session{Adapter: "shell", Name: "shell.local", Capabilities: []string{adapterToolsCapability}}
		runToolCall(t, sess, ps, policy, nil, nil, map[string]any{
			"request_id": "req-1",
			"kind":       "adapter_tool",
			"target":     target,
		})

		ev := readStreamEvent(t, ps)
		tcr := ev.GetToolCallResult()
		if tcr == nil {
			t.Fatalf("target %q: expected tool_call_result, got %T", target, ev.Event)
		}
		if tcr.CallError != callErrorMalformedTarget {
			t.Errorf("target %q: call_error = %q, want %q", target, tcr.CallError, callErrorMalformedTarget)
		}
		if policy.calls != 0 {
			t.Errorf("target %q: policy called %d times, want 0", target, policy.calls)
		}
	}
}

func TestToolCall_UnknownAdapter(t *testing.T) {
	audit := &sliceAuditWriter{}
	ps := newToolCallState(t, audit)
	policy := allowAllPolicy()
	sess := &Session{Adapter: "shell", Name: "shell.local", Capabilities: []string{adapterToolsCapability}}
	fx := runToolCall(t, sess, ps, policy, nil, toolCallGraph(map[string]*workflow.AdapterNode{
		"other.instance": {Type: "other", Name: "instance", StaticTools: []string{"run"}},
	}), map[string]any{
		"request_id": "req-1",
		"target":     "adapter.shell.runner.tools.run",
	})

	tcr := readToolCallResult(t, ps)
	if tcr.CallError != callErrorUnknownAdapter {
		t.Errorf("call_error = %q, want %q", tcr.CallError, callErrorUnknownAdapter)
	}
	if fx.inner.saw("permission.denied") {
		t.Error("expected no permission.denied for graph failure after allow")
	}
	if fx.sink.anyDenied {
		t.Error("expected anyDenied=false for typed graph failure")
	}
}

func TestToolCall_UnknownStaticTool(t *testing.T) {
	audit := &sliceAuditWriter{}
	ps := newToolCallState(t, audit)
	policy := allowAllPolicy()
	sess := &Session{Adapter: "shell", Name: "shell.local", Capabilities: []string{adapterToolsCapability}}
	fx := runToolCall(t, sess, ps, policy, nil, toolCallGraph(map[string]*workflow.AdapterNode{
		"shell.runner": {Type: "shell", Name: "runner", StaticTools: []string{"run"}},
	}), map[string]any{
		"request_id": "req-1",
		"target":     "adapter.shell.runner.tools.missing",
	})

	tcr := readToolCallResult(t, ps)
	if tcr.CallError != callErrorUnknownTool {
		t.Errorf("call_error = %q, want %q", tcr.CallError, callErrorUnknownTool)
	}
	if fx.sink.anyDenied {
		t.Error("expected anyDenied=false for typed graph failure")
	}
}

func TestToolCall_BareCallOnEmptyStaticSurface(t *testing.T) {
	audit := &sliceAuditWriter{}
	ps := newToolCallState(t, audit)
	policy := allowAllPolicy()
	sess := &Session{Adapter: "shell", Name: "shell.local", Capabilities: []string{adapterToolsCapability}}
	// Bare whole-surface call on an adapter that declares neither tool blocks
	// nor dynamic tools: the call resolves to nothing (CRI-156 mode 7).
	runToolCall(t, sess, ps, policy, nil, toolCallGraph(map[string]*workflow.AdapterNode{
		"shell.runner": {Type: "shell", Name: "runner"},
	}), map[string]any{
		"request_id": "req-1",
		"target":     "adapter.shell.runner.tools",
	})

	tcr := readToolCallResult(t, ps)
	if tcr.CallError != callErrorUnknownTool {
		t.Errorf("call_error = %q, want %q", tcr.CallError, callErrorUnknownTool)
	}
}

func TestToolCall_DynamicAdapterSkipsToolCheck(t *testing.T) {
	audit := &sliceAuditWriter{}
	ps := newToolCallState(t, audit)
	policy := allowAllPolicy()
	sess := &Session{Adapter: "shell", Name: "shell.local", Capabilities: []string{adapterToolsCapability}}
	runToolCall(t, sess, ps, policy, nil, toolCallGraph(map[string]*workflow.AdapterNode{
		"mcp.fs": {Type: "mcp", Name: "fs", DynamicTools: true},
	}), map[string]any{
		"request_id": "req-1",
		"target":     "adapter.mcp.fs.tools.anything",
	})

	// Allow decision first, then the typed stub reply.
	ev := readStreamEvent(t, ps)
	if ev.GetRequest() == nil {
		t.Fatalf("expected permission request grant event, got %T", ev.Event)
	}
	ev = readStreamEvent(t, ps)
	tcr := ev.GetToolCallResult()
	if tcr == nil {
		t.Fatalf("expected tool_call_result, got %T", ev.Event)
	}
	if tcr.CallError != callErrorNotYetSupported {
		t.Errorf("call_error = %q, want %q", tcr.CallError, callErrorNotYetSupported)
	}
}

func TestToolCall_SelfCall(t *testing.T) {
	audit := &sliceAuditWriter{}
	ps := newToolCallState(t, audit)
	policy := allowAllPolicy()
	sess := &Session{Adapter: "shell", Name: "shell.local", Capabilities: []string{adapterToolsCapability}}
	runToolCall(t, sess, ps, policy, nil, toolCallGraph(map[string]*workflow.AdapterNode{
		"shell.local": {Type: "shell", Name: "local", StaticTools: []string{"run"}},
	}), map[string]any{
		"request_id": "req-1",
		"target":     "adapter.shell.local.tools.run",
	})

	tcr := readToolCallResult(t, ps)
	if tcr.CallError != callErrorSelfCall {
		t.Errorf("call_error = %q, want %q", tcr.CallError, callErrorSelfCall)
	}
}

// TestToolCall_NilManagerFallback pins the defensive nil-manager fallback: a
// sink constructed without a wired SessionManager (directly built test
// fixtures) cannot execute a callee, so the allowed call is rejected with the
// typed not_yet_supported call_error. The real host always wires the manager
// (see tool_call_exec_test.go for the nested-execution path).
func TestToolCall_NilManagerFallback(t *testing.T) {
	audit := &sliceAuditWriter{}
	ps := newToolCallState(t, audit)
	policy := allowAllPolicy()
	sess := &Session{Adapter: "shell", Name: "shell.local", Capabilities: []string{adapterToolsCapability}}
	fx := runToolCall(t, sess, ps, policy, nil, toolCallGraph(map[string]*workflow.AdapterNode{
		"shell.runner": {Type: "shell", Name: "runner", StaticTools: []string{"run"}},
	}), map[string]any{
		"request_id": "req-1",
		"target":     "adapter.shell.runner.tools.run",
		"tool":       "run",
	})

	// First stream event: the permission allow grant (host-to-adapter).
	ev := readStreamEvent(t, ps)
	req := ev.GetRequest()
	if req == nil || req.RequestId != "req-1" {
		t.Fatalf("expected permission request grant event, got %+v", ev.Event)
	}
	// Second stream event: the typed stub reply.
	ev = readStreamEvent(t, ps)
	tcr := ev.GetToolCallResult()
	if tcr == nil {
		t.Fatalf("expected tool_call_result, got %T", ev.Event)
	}
	if tcr.RequestId != "req-1" || tcr.CallError != callErrorNotYetSupported {
		t.Errorf("tool_call_result = %+v, want req-1/not_yet_supported", tcr)
	}

	granted, ok := fx.inner.first("permission.granted")
	if !ok {
		t.Fatal("expected permission.granted event on execute sink")
	}
	if granted["request_id"] != "req-1" || granted["tool"] != "adapter.shell.runner.tools.run" || granted["pattern"] != "adapter.shell.runner.*" {
		t.Errorf("permission.granted = %+v", granted)
	}
	if fx.sink.anyDenied {
		t.Error("expected anyDenied=false for stub success path")
	}
	entries := audit.all()
	if len(entries) != 2 {
		t.Fatalf("expected 2 audit entries (policy allow + stub rejection), got %d", len(entries))
	}
	if entries[0].Decision != "allow" || entries[1].Decision != "deny" {
		t.Errorf("audit decisions = %q/%q, want allow/deny", entries[0].Decision, entries[1].Decision)
	}
	if entries[1].Reason != "adapter tool call rejected: "+callErrorNotYetSupported {
		t.Errorf("audit rejection reason = %q", entries[1].Reason)
	}
}

func TestToolCall_MissingRequestID(t *testing.T) {
	audit := &sliceAuditWriter{}
	ps := newToolCallState(t, audit)
	policy := allowAllPolicy()
	sess := &Session{Adapter: "shell", Name: "shell.local", Capabilities: []string{adapterToolsCapability}}
	fx := runToolCall(t, sess, ps, policy, nil, nil, map[string]any{
		"target": "adapter.shell.runner.tools.run",
	})

	if fx.inner.saw("permission.granted") {
		t.Error("expected no grant without request_id")
	}
	denied, ok := fx.inner.first("permission.denied")
	if !ok {
		t.Fatal("expected permission.denied for missing request_id")
	}
	if denied["reason"] != "malformed permission.request payload: missing request_id" {
		t.Errorf("permission.denied = %+v", denied)
	}
	if !fx.sink.anyDenied {
		t.Error("expected anyDenied=true for malformed request")
	}
}

func TestToolCall_GrantAllowsWhenPolicyDenies(t *testing.T) {
	audit := &sliceAuditWriter{}
	ps := newToolCallState(t, audit)
	policy := &spyPolicy{allow: false, reason: "no matching allow_tools entry"}
	sess := &Session{Adapter: "shell", Name: "shell.local", Capabilities: []string{adapterToolsCapability}}
	step := &workflow.StepNode{Tools: []workflow.AdapterToolRef{
		{CallerIsSelf: true, CalleeRef: "shell.runner", Tool: ""},
	}}
	runToolCall(t, sess, ps, policy, step, toolCallGraph(map[string]*workflow.AdapterNode{
		"shell.runner": {Type: "shell", Name: "runner", StaticTools: []string{"run", "other"}},
	}), map[string]any{
		"request_id": "req-1",
		"target":     "adapter.shell.runner.tools.run",
	})

	// The step's bare tools grant covers the named call despite the policy
	// deny: grants are literals unioned into the effective allow set.
	if policy.calls != 0 {
		t.Errorf("policy called %d times, want 0 (grant short-circuits)", policy.calls)
	}
	ev := readStreamEvent(t, ps)
	if ev.GetRequest() == nil {
		t.Fatalf("expected permission request grant event, got %T", ev.Event)
	}
	ev = readStreamEvent(t, ps)
	tcr := ev.GetToolCallResult()
	if tcr == nil {
		t.Fatalf("expected tool_call_result, got %T", ev.Event)
	}
	if tcr.CallError != callErrorNotYetSupported {
		t.Errorf("call_error = %q, want %q", tcr.CallError, callErrorNotYetSupported)
	}
}

func TestToolCall_NamedGrantOnlyCoversNamedCall(t *testing.T) {
	audit := &sliceAuditWriter{}
	ps := newToolCallState(t, audit)
	policy := &spyPolicy{allow: false, reason: "no matching allow_tools entry"}
	sess := &Session{Adapter: "shell", Name: "shell.local", Capabilities: []string{adapterToolsCapability}}
	step := &workflow.StepNode{Tools: []workflow.AdapterToolRef{
		{CallerIsSelf: true, CalleeRef: "shell.runner", Tool: "run"},
	}}
	// Bare whole-surface call: the named grant does not cover it, so the
	// policy deny stands and the existing deny path applies.
	fx := runToolCall(t, sess, ps, policy, step, toolCallGraph(map[string]*workflow.AdapterNode{
		"shell.runner": {Type: "shell", Name: "runner", StaticTools: []string{"run"}},
	}), map[string]any{
		"request_id": "req-1",
		"target":     "adapter.shell.runner.tools",
	})

	if policy.calls != 1 {
		t.Fatalf("policy called %d times, want 1", policy.calls)
	}
	if !fx.sink.anyDenied {
		t.Error("expected anyDenied=true when only policy denies")
	}
}

// TestToolCall_PlainRequestRegression pins the byte-identical plain flow: a
// payload with a dotted tool name whose root is not "adapter" is a plain
// permission request, evaluated through the untouched path with no typed
// reply.
func TestToolCall_PlainRequestRegression(t *testing.T) {
	audit := &sliceAuditWriter{}
	ps := NewPermissionState("sess-1", audit)
	ps.SetStreamCancel(func() {})
	ps.SetPolicy(NewPolicy([]string{"mcp.filesystem.read_file"}))
	sess := &Session{Adapter: "mcp", Name: "mcp.fs", Capabilities: []string{adapterToolsCapability}}
	inner := &adapterEventCollector{}
	sink := &permissionInterceptSink{
		inner:     inner,
		permState: ps,
		session:   sess,
	}

	sink.Adapter("permission.request", map[string]any{
		"request_id": "req-1",
		"tool":       "mcp.filesystem.read_file",
	})

	if !inner.saw("permission.granted") {
		t.Fatal("expected plain permission.granted for dotted non-adapter tool name")
	}
	if inner.saw("permission.denied") {
		t.Error("expected no denial for allowlisted plain tool")
	}
	select {
	case ev := <-ps.Requests():
		if ev.GetRequest() == nil {
			t.Fatalf("expected plain request event, got %T", ev.Event)
		}
		if ev.GetToolCallResult() != nil {
			t.Fatal("expected no tool_call_result on plain path")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for request event")
	}
}
