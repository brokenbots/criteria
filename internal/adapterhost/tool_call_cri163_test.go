package adapterhost

// tool_call_cri163_test.go — CRI-163: nested tool-call observability at the
// adapterhost level: tool.call / tool.call_result events attributed under the
// caller step with their payload contract (target, tool, depth, request_id,
// outcome, duration), per-layer audit attribution (the caller's allow/deny
// decision vs the callee-side decisions under the callee's own allow_tools),
// and audit-side redaction of registered secret values.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zclconf/go-cty/cty"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapter/secrets"
	"github.com/brokenbots/criteria/workflow"
)

// cri163RedactedMask is the literal the redaction registry substitutes for
// registered sensitive values.
const cri163RedactedMask = "[REDACTED]"

// cri163Callee is the callee fake for the CRI-163 tests. It emits marker
// adapter events around its Execute (so event ordering is observable), delays
// a fixed duration (so the tool.call_result duration is measurable), emits
// configurable plain permission requests, and returns configurable outputs or
// an error.
type cri163Callee struct {
	rec       *nestedCalleeRecorder
	delay     time.Duration
	permTools []string
	outputs   map[string]cty.Value
	execErr   error
}

func (a *cri163Callee) Info(_ context.Context) (Info, error) {
	return Info{
		Capabilities: []string{"execute"},
		AdapterInfo: workflow.AdapterInfo{
			InputSchema: map[string]workflow.ConfigField{
				"task": {Required: true},
			},
			OutputSchema: map[string]workflow.ConfigField{
				"report": {CtyType: cty.String},
				"count":  {CtyType: cty.Number},
			},
		},
	}, nil
}
func (a *cri163Callee) OpenSession(_ context.Context, _ string, _, _ map[string]string) error {
	return nil
}
func (a *cri163Callee) CloseSession(_ context.Context, _ string) error { return nil }
func (a *cri163Callee) Kill()                                          {}
func (a *cri163Callee) Pause(context.Context, string) error            { return nil }
func (a *cri163Callee) Resume(context.Context, string) error           { return nil }
func (a *cri163Callee) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}
func (a *cri163Callee) Snapshot(context.Context, string) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{}, nil
}
func (a *cri163Callee) Restore(context.Context, string, []byte, uint32) error { return nil }

func (a *cri163Callee) Execute(ctx context.Context, sessionID string, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	a.rec.record(sessionID, step)
	sink.Adapter("callee.started", map[string]any{"task": step.Input["task"]})
	if a.delay > 0 {
		select {
		case <-time.After(a.delay):
		case <-ctx.Done():
			return adapter.Result{Outcome: "failure"}, ctx.Err()
		}
	}
	for i, tool := range a.permTools {
		sink.Adapter("permission.request", map[string]any{
			"request_id": "callee-perm-" + string(rune('0'+i)),
			"tool":       tool,
		})
	}
	sink.Adapter("callee.finished", map[string]any{})
	if a.execErr != nil {
		return adapter.Result{Outcome: "failure"}, a.execErr
	}
	outputs := a.outputs
	if outputs == nil {
		outputs = map[string]cty.Value{
			"report": cty.StringVal(step.Input["task"]),
			"count":  cty.NumberIntVal(1),
		}
	}
	return adapter.Result{Outcome: "success", Outputs: outputs}, nil
}

// cri163Caller is the caller fake with full control over the permission.request
// payload: request id, §2 target, and the optional tool field (for the §2
// bare-tools form whose tool name rides the tool field). It blocks until the
// host replies with the typed tool_call_result, which it records.
type cri163Caller struct {
	requestID string
	target    string
	tool      string

	mu       sync.Mutex
	requests <-chan *v2.PermissionEvent
	result   *v2.ToolCallResult
}

func (a *cri163Caller) Info(_ context.Context) (Info, error) {
	return Info{Capabilities: []string{"adapter_tools", "execute"}}, nil
}
func (a *cri163Caller) OpenSession(_ context.Context, _ string, _, _ map[string]string) error {
	return nil
}
func (a *cri163Caller) CloseSession(_ context.Context, _ string) error { return nil }
func (a *cri163Caller) Kill()                                          {}
func (a *cri163Caller) Pause(context.Context, string) error            { return nil }
func (a *cri163Caller) Resume(context.Context, string) error           { return nil }
func (a *cri163Caller) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}
func (a *cri163Caller) Snapshot(context.Context, string) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{}, nil
}
func (a *cri163Caller) Restore(context.Context, string, []byte, uint32) error { return nil }

func (a *cri163Caller) StartPermissionStream(_ context.Context, _ string, requests <-chan *v2.PermissionEvent) (func(), error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests = requests
	return func() {}, nil
}

func (a *cri163Caller) Execute(ctx context.Context, _ string, _ *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	payload := map[string]any{"request_id": a.requestID, "target": a.target}
	if a.tool != "" {
		payload["tool"] = a.tool
	}
	payload["args"] = map[string]any{"task": "do-thing"}
	sink.Adapter("permission.request", payload)

	a.mu.Lock()
	requests := a.requests
	a.mu.Unlock()
	if requests == nil {
		return adapter.Result{Outcome: "failure"}, errors.New("permission stream not started")
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-requests:
			if !ok {
				return adapter.Result{Outcome: "failure"}, errors.New("permission stream closed before tool_call_result")
			}
			if tcr := ev.GetToolCallResult(); tcr != nil {
				a.mu.Lock()
				a.result = tcr
				a.mu.Unlock()
				return adapter.Result{Outcome: "success"}, nil
			}
		case <-ctx.Done():
			return adapter.Result{Outcome: "failure"}, ctx.Err()
		case <-deadline:
			return adapter.Result{Outcome: "failure"}, errors.New("timed out waiting for tool_call_result")
		}
	}
}

func (a *cri163Caller) gotResult() *v2.ToolCallResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.result
}

// collectAllEvents snapshots the collector's captured event stream.
func collectAllEvents(c *adapterEventCollector) []adapterEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]adapterEvent(nil), c.events...)
}

// eventIndexes returns the index of the first event of each kind in the
// collector's stream (or -1) so relative ordering can be asserted.
func eventIndexes(t *testing.T, c *adapterEventCollector, kinds ...string) []int {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]int, len(kinds))
	for i, kind := range kinds {
		out[i] = -1
		for idx, evt := range c.events {
			if evt.kind == kind {
				out[i] = idx
				break
			}
		}
	}
	return out
}

// assertEventPayload checks the shared CRI-163 payload fields of a nested
// tool-call event.
func assertEventPayload(t *testing.T, kind string, data map[string]any, reqID, wantTarget, wantTool string) {
	t.Helper()
	if data["request_id"] != reqID {
		t.Errorf("%s request_id = %v, want %q", kind, data["request_id"], reqID)
	}
	if data["target"] != wantTarget {
		t.Errorf("%s target = %v, want %q", kind, data["target"], wantTarget)
	}
	if data["tool"] != wantTool {
		t.Errorf("%s tool = %v, want %q", kind, data["tool"], wantTool)
	}
	if depth, ok := data["depth"].(int); !ok || depth != 1 {
		t.Errorf("%s depth = %v (%T), want int 1 (first-level nested call)", kind, data["depth"], data["depth"])
	}
}

// runCri163Call opens both sessions and executes the caller step, returning
// the collector, audit writer, and the caller result. A non-nil registry is
// installed on the SessionManager as the run's redaction registry.
func runCri163Call(t *testing.T, caller *cri163Caller, callee *cri163Callee, step *workflow.StepNode, registry *secrets.Registry) (*adapterEventCollector, *sliceAuditWriter, adapter.Result, error) {
	t.Helper()
	audit := &sliceAuditWriter{}
	loader := NewLoaderWithDiscovery(func(string) (string, error) { return "", nil })
	loader.RegisterBuiltin("caller", func() Handle { return caller })
	loader.RegisterBuiltin("callee", func() Handle { return callee })
	sm := NewSessionManager(loader)
	sm.Audit = audit
	if registry != nil {
		sm.RedactionRegistry = registry
	}

	graph := compileNestedToolCallGraph(t)
	sm.SetGraph(graph)
	ctx := context.Background()
	if err := sm.VerifyGraph(ctx, graph, nil); err != nil {
		t.Fatalf("VerifyGraph: %v", err)
	}
	if err := sm.Open(ctx, nestedCallerSession, "caller", "", nil, nil); err != nil {
		t.Fatalf("Open caller: %v", err)
	}
	defer func() { _ = sm.Close(ctx, nestedCallerSession) }()
	if err := sm.Open(ctx, nestedCalleeSession, "callee", "", nil, nil); err != nil {
		t.Fatalf("Open callee: %v", err)
	}
	defer func() { _ = sm.Close(ctx, nestedCalleeSession) }()

	inner := &adapterEventCollector{}
	res, err := sm.Execute(ctx, nestedCallerSession, step, inner)
	return inner, audit, res, err
}

// TestNestedToolCallEvents_Success drives the happy path: tool.call strictly
// precedes the callee's own events, tool.call_result follows them, and both
// carry the full payload contract.
func TestNestedToolCallEvents_Success(t *testing.T) {
	callee := &cri163Callee{
		rec:     &nestedCalleeRecorder{},
		delay:   100 * time.Millisecond,
		outputs: map[string]cty.Value{"report": cty.StringVal("ok"), "count": cty.NumberIntVal(1)},
	}
	caller := &cri163Caller{requestID: "call-1", target: nestedCallTarget}

	inner, audit, res, err := runCri163Call(t, caller, callee, nestedCallerStep(), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Outcome != "success" {
		t.Errorf("caller outcome = %q, want success", res.Outcome)
	}

	// Ordering: the call start precedes the callee's first event, and the
	// result follows the callee's last.
	idx := eventIndexes(t, inner, "tool.call", "callee.started", "callee.finished", "tool.call_result")
	for i, kind := range []string{"tool.call", "callee.started", "callee.finished", "tool.call_result"} {
		if idx[i] < 0 {
			t.Fatalf("no %q event in the stream", kind)
		}
	}
	if !(idx[0] < idx[1] && idx[1] < idx[2] && idx[2] < idx[3]) {
		t.Errorf("event order = tool.call@%d, callee.started@%d, callee.finished@%d, tool.call_result@%d; want strictly increasing", idx[0], idx[1], idx[2], idx[3])
	}

	// tool.call payload.
	callData, ok := inner.first("tool.call")
	if !ok {
		t.Fatal("no tool.call event")
	}
	assertEventPayload(t, "tool.call", callData, "call-1", nestedCallTarget, "helper_task")
	if _, has := callData["outcome"]; has {
		t.Error("tool.call payload must not carry outcome")
	}

	// tool.call_result payload: outcome + duration (integer milliseconds) on
	// top of the shared fields.
	resData, ok := inner.first("tool.call_result")
	if !ok {
		t.Fatal("no tool.call_result event")
	}
	assertEventPayload(t, "tool.call_result", resData, "call-1", nestedCallTarget, "helper_task")
	if resData["outcome"] != "success" {
		t.Errorf("tool.call_result outcome = %v, want success", resData["outcome"])
	}
	dur, ok := resData["duration"].(int64)
	if !ok {
		t.Fatalf("tool.call_result duration = %v (%T), want int64 milliseconds", resData["duration"], resData["duration"])
	}
	if dur < 90 {
		t.Errorf("tool.call_result duration = %dms, want >= 90ms (callee slept 100ms)", dur)
	}
	if _, has := resData["call_error"]; has {
		t.Error("successful tool.call_result must not carry call_error")
	}

	// Audit: the caller-layer allow decision is attributed to the caller
	// session at layer 0.
	entries := audit.entries
	var callAllow *DecisionLogEntry
	for _, e := range entries {
		if e.RequestID == "call-1" && e.Decision == "allow" && e.SessionID == nestedCallerSession {
			callAllow = e
			break
		}
	}
	if callAllow == nil {
		t.Fatalf("no caller-layer allow audit entry; entries: %+v", entries)
	}
	if callAllow.Layer != 0 {
		t.Errorf("caller-layer allow entry layer = %d, want 0", callAllow.Layer)
	}
	if callAllow.Tool != nestedCallTarget {
		t.Errorf("caller-layer allow entry tool = %q, want %q", callAllow.Tool, nestedCallTarget)
	}
}

// TestNestedToolCallEvents_ToolFieldFallback checks the resolved tool name of
// the §2 bare-tools form: the target carries no tool segment, so the payload's
// tool field is the resolved name in the events.
func TestNestedToolCallEvents_ToolFieldFallback(t *testing.T) {
	bareTarget := "adapter.callee.helper.tools"
	step := &workflow.StepNode{
		Name:       "call",
		AdapterRef: nestedCallerSession,
		AllowTools: []string{bareTarget},
		Outcomes: map[string]*workflow.CompiledOutcome{
			"success": {Name: "success"},
			"failure": {Name: "failure"},
		},
	}
	callee := &cri163Callee{rec: &nestedCalleeRecorder{}}
	caller := &cri163Caller{requestID: "call-2", target: bareTarget, tool: "fallback_tool"}

	inner, _, res, err := runCri163Call(t, caller, callee, step, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Outcome != "success" {
		t.Fatalf("caller outcome = %q, want success", res.Outcome)
	}
	callData, ok := inner.first("tool.call")
	if !ok {
		t.Fatal("no tool.call event")
	}
	assertEventPayload(t, "tool.call", callData, "call-2", bareTarget, "fallback_tool")
}

// TestNestedToolCallEvents_Failure checks the failed-call event: outcome is
// absent, call_error carries the typed code, duration is still measured, and
// the audit deny entry is attributed to the caller's layer.
func TestNestedToolCallEvents_Failure(t *testing.T) {
	callee := &cri163Callee{rec: &nestedCalleeRecorder{}, execErr: errors.New("boom inside callee")}
	caller := &cri163Caller{requestID: "call-3", target: nestedCallTarget}

	inner, audit, res, err := runCri163Call(t, caller, callee, nestedCallerStep(), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// ADR-0004 §5: a failed tool call is data for the caller, not a run
	// failure — outcome routing is unaffected.
	if res.Outcome != "success" {
		t.Fatalf("caller outcome = %q, want success (failed call is data)", res.Outcome)
	}
	tcr := caller.gotResult()
	if tcr == nil || tcr.CallError != callErrorCalleeCrash {
		t.Fatalf("tool_call_result = %+v, want callee_crash failure", tcr)
	}

	resData, ok := inner.first("tool.call_result")
	if !ok {
		t.Fatal("no tool.call_result event on the failure path")
	}
	assertEventPayload(t, "tool.call_result", resData, "call-3", nestedCallTarget, "helper_task")
	if _, has := resData["outcome"]; has {
		t.Errorf("failed tool.call_result must not carry outcome, got %v", resData["outcome"])
	}
	if resData["call_error"] != callErrorCalleeCrash {
		t.Errorf("tool.call_result call_error = %v, want callee_crash", resData["call_error"])
	}
	if dur, ok := resData["duration"].(int64); !ok || dur < 0 {
		t.Errorf("tool.call_result duration = %v (%T), want non-negative int64", resData["duration"], resData["duration"])
	}

	var deny *DecisionLogEntry
	for _, e := range audit.entries {
		if e.RequestID == "call-3" && e.Decision == "deny" && strings.Contains(e.Reason, "callee_crash") {
			deny = e
			break
		}
	}
	if deny == nil {
		t.Fatalf("no callee-crash deny audit entry; entries: %+v", audit.entries)
	}
	if deny.SessionID != nestedCallerSession || deny.Layer != 0 {
		t.Errorf("crash deny entry session/layer = %q/%d, want %q/0 (caller layer)", deny.SessionID, deny.Layer, nestedCallerSession)
	}
}

// TestNestedToolCallEvents_AuditLayers proves the two decision surfaces are
// separately distinguishable in the audit log: the caller's layer-0 allow of
// the call itself, and the callee-side layer-1 decisions (one allowed, one
// denied) evaluated under the callee's own allow_tools.
func TestNestedToolCallEvents_AuditLayers(t *testing.T) {
	callee := &cri163Callee{
		rec: &nestedCalleeRecorder{},
		permTools: []string{
			"callee.helpers.read_file", // allowed by the callee's own policy
			"caller.only.tool",         // allowed by the CALLER's step policy, denied on the callee
		},
	}
	caller := &cri163Caller{requestID: "call-4", target: nestedCallTarget}

	_, audit, res, err := runCri163Call(t, caller, callee, nestedCallerStep(), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Outcome != "success" {
		t.Fatalf("caller outcome = %q, want success (callee denials ride as data)", res.Outcome)
	}

	// Caller layer: the tool-call allow at layer 0.
	var callAllow *DecisionLogEntry
	for _, e := range audit.entries {
		if e.RequestID == "call-4" && e.SessionID == nestedCallerSession && e.Decision == "allow" {
			callAllow = e
			break
		}
	}
	if callAllow == nil {
		t.Fatalf("no caller-layer allow entry; entries: %+v", audit.entries)
	}
	if callAllow.Layer != 0 {
		t.Errorf("caller-layer allow layer = %d, want 0", callAllow.Layer)
	}

	// Callee layer: both plain requests evaluated under the callee's own
	// allow_tools land at layer 1, session callee.
	var calleeAllow, calleeDeny *DecisionLogEntry
	for _, e := range audit.entries {
		if e.SessionID != nestedCalleeSession || e.Layer != 1 {
			continue
		}
		switch {
		case e.Tool == "callee.helpers.read_file" && e.Decision == "allow":
			calleeAllow = e
		case e.Tool == "caller.only.tool" && e.Decision == "deny":
			calleeDeny = e
		}
	}
	if calleeAllow == nil || calleeDeny == nil {
		t.Fatalf("callee-side audit entries missing (allow=%v deny=%v); entries: %+v", calleeAllow, calleeDeny, audit.entries)
	}

	// The three entries are distinguishable by session and layer: the
	// caller's decision shares the caller's session at layer 0; the
	// callee-side decisions carry the callee session at layer 1.
	if callAllow.SessionID == calleeDeny.SessionID || callAllow.Layer == calleeDeny.Layer {
		t.Errorf("caller and callee entries not distinguishable: caller=%+v callee=%+v", callAllow, calleeDeny)
	}
}

// TestNestedToolCallEvents_TargetRedacted proves the event-side redaction of
// adapter-supplied target/tool values (CRI-163): a value registered in the
// run's redaction registry that rides the nested call's §2 target as the
// parsed tool label (bareword labels admit real token formats such as
// sk_live_…) is masked in both the tool.call and tool.call_result payloads
// and in the permission.granted decision event, while the host-computed
// depth/request_id/duration/outcome fields stay untouched. The audit entry
// for the same call is masked by the redacting audit writer.
func TestNestedToolCallEvents_TargetRedacted(t *testing.T) {
	secret := "sk_live_abcdefgh123456"
	secretTarget := "adapter.callee.helper.tools." + secret

	callee := &cri163Callee{rec: &nestedCalleeRecorder{}}
	caller := &cri163Caller{requestID: "call-5", target: secretTarget}
	registry := secrets.NewRegistry()
	registry.Register(secret)

	inner, audit, res, err := runCri163Call(t, caller, callee, nestedCallerStep(), registry)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Outcome != "success" {
		t.Fatalf("caller outcome = %q, want success", res.Outcome)
	}

	maskedTarget := "adapter.callee.helper.tools." + cri163RedactedMask
	for _, kind := range []string{"tool.call", "tool.call_result"} {
		data, ok := inner.first(kind)
		if !ok {
			t.Fatalf("no %s event", kind)
		}
		assertEventPayload(t, kind, data, "call-5", maskedTarget, cri163RedactedMask)
	}
	// Result-side extras stay host-computed.
	resData, _ := inner.first("tool.call_result")
	if resData["outcome"] != "success" {
		t.Errorf("tool.call_result outcome = %v, want success (redaction must not touch it)", resData["outcome"])
	}
	if _, ok := resData["duration"].(int64); !ok {
		t.Errorf("tool.call_result duration = %v (%T), want int64 milliseconds", resData["duration"], resData["duration"])
	}

	// The tool-call granted decision carries the target too; it must be
	// masked the same way.
	granted, ok := inner.first("permission.granted")
	if !ok {
		t.Fatal("no permission.granted event for the tool call")
	}
	if granted["tool"] != maskedTarget {
		t.Errorf("permission.granted tool = %v, want %q", granted["tool"], maskedTarget)
	}

	// No registered value may appear anywhere in the captured event stream.
	for _, evt := range collectAllEvents(inner) {
		encoded, err := json.Marshal(evt.data)
		if err != nil {
			t.Fatalf("marshal %s payload: %v", evt.kind, err)
		}
		if strings.Contains(string(encoded), secret) {
			t.Errorf("%s payload carries the raw registered value: %s", evt.kind, encoded)
		}
	}

	// The audit entry for the same call is masked by the redacting audit
	// writer (same registry, same run).
	var callAllow *DecisionLogEntry
	for _, e := range audit.entries {
		if e.RequestID == "call-5" && e.Decision == "allow" && e.SessionID == nestedCallerSession {
			callAllow = e
			break
		}
	}
	if callAllow == nil {
		t.Fatalf("no caller-layer allow audit entry; entries: %+v", audit.entries)
	}
	if callAllow.Tool != maskedTarget {
		t.Errorf("audit entry tool = %q, want %q", callAllow.Tool, maskedTarget)
	}
}

// TestRedactingAuditWriter verifies audit-side redaction: registered values
// are masked out of Tool and Reason, while correlation identifiers and the
// one-way args digest pass through untouched.
func TestRedactingAuditWriter(t *testing.T) {
	secret := "s3cr3t-token-value"
	reg := secrets.NewRegistry()
	reg.Register(secret)

	inner := &sliceAuditWriter{}
	writer := NewRedactingAuditWriter(inner, reg)
	writer.Write(&DecisionLogEntry{
		SessionID:  "sess-1",
		RequestID:  "req-1",
		Tool:       "shell:echo " + secret,
		ArgsDigest: "digest-1",
		Decision:   "allow",
		Reason:     "failure mentioning " + secret,
	})

	if len(inner.entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(inner.entries))
	}
	got := inner.entries[0]
	if got.Tool != "shell:echo [REDACTED]" {
		t.Errorf("Tool = %q, want masked (no %q)", got.Tool, secret)
	}
	if got.Reason != "failure mentioning [REDACTED]" {
		t.Errorf("Reason = %q, want masked", got.Reason)
	}
	if got.ArgsDigest != "digest-1" {
		t.Errorf("ArgsDigest = %q, want untouched (one-way digest)", got.ArgsDigest)
	}
	if got.SessionID != "sess-1" || got.RequestID != "req-1" {
		t.Errorf("SessionID/RequestID = %q/%q, want untouched correlation ids", got.SessionID, got.RequestID)
	}

	// A value never registered passes through.
	writer.Write(&DecisionLogEntry{Tool: "plain.tool", Reason: "plain reason"})
	if inner.entries[1].Tool != "plain.tool" || inner.entries[1].Reason != "plain reason" {
		t.Errorf("unregistered entry mutated: %+v", inner.entries[1])
	}

	// Nil-preserving wiring: no registry or no writer returns the inner as-is.
	if NewRedactingAuditWriter(inner, nil) != AuditWriter(inner) {
		t.Error("nil registry must return the inner writer unchanged")
	}
	if NewRedactingAuditWriter(nil, reg) != nil {
		t.Error("nil inner must return nil")
	}
}

// TestCalleeReportedCallError_NullString guards against a host-level panic
// (CRI-172 review): a failure result decoded from a hostile callee's
// outputs_json may carry the reserved call_error key as a typed cty null
// string — Type().Equals(cty.String) is true for a null value, so the guard
// must reject null (and unknown) values before calling AsString.
func TestCalleeReportedCallError_NullString(t *testing.T) {
	got := calleeReportedCallError(adapter.Result{
		Outcome: "failure",
		Outputs: map[string]cty.Value{
			calleeReportedCallErrorCode: cty.NullVal(cty.String),
		},
	})
	if got != "" {
		t.Fatalf("calleeReportedCallError with null call_error = %q, want \"\"", got)
	}
}

// A known-but-dynamic value is likewise not a usable string code.
func TestCalleeReportedCallError_UnknownValue(t *testing.T) {
	got := calleeReportedCallError(adapter.Result{
		Outcome: "failure",
		Outputs: map[string]cty.Value{
			calleeReportedCallErrorCode: cty.UnknownVal(cty.String),
		},
	})
	if got != "" {
		t.Fatalf("calleeReportedCallError with unknown call_error = %q, want \"\"", got)
	}
}
