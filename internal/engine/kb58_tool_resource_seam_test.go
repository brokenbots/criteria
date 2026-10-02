package engine

// kb58_tool_resource_seam_test.go — KB-58: the adapter-tools seam must resolve
// host-LOCAL tool-resource adapters from borrowed/per-scope SessionManagers,
// and must bound the number of nested adapter tool calls (policy.max_tool_calls).
//
// The documented reproduction: a parallel subworkflow iteration runs on a fresh
// SessionManager that borrows only remote provisioning
// (BorrowRemoteProvisioningFrom, PR #458). The iteration's caller session
// tool-calls a host-local adapter declared in the ROOT graph only — never a
// step target — so only the root scope ever stored its verified record. The
// seam's nested-execute lazy bind misses on the borrowed manager
// (verified[name] == nil → ErrUnknownSession → typed unknown_adapter), even
// though VerifyGraph handshake-verified the adapter on the parent manager.
//
// Containment invariant asserted alongside: the callee is still bound and
// executed HOST-LOCAL (in this test process) with the root adapter's own
// secrets; the caller session never receives them.

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

const (
	kb58ProbeSecret = "s3cr3t-probe-token"
	kb58ProbeTarget = "adapter.mcp.probe.tools.probe"
	kb58CalleeRef   = "mcp.probe"
	kb58CalleeSess  = kb58CalleeRef
	kb58SecretVar   = "probe_token"
)

// kb58TestCallee is the mcp-like dynamic-tool callee fake: a host-local
// adapter with a dynamic tool surface. It records OpenSession config/secrets,
// CloseSession, and every executed session/step, and reports a typed
// `report` output so the caller can re-export callee.* outputs.
type kb58TestCallee struct {
	rec *nestedEngineRecorder

	mu       sync.Mutex
	openSess []string
	opens    []map[string]string
	closes   []string
}

func (a *kb58TestCallee) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{
		Name:         "mcp",
		Version:      "test",
		Capabilities: []string{"execute"},
		AdapterInfo: workflow.AdapterInfo{
			OutputSchema: map[string]workflow.ConfigField{
				"report": {CtyType: cty.String},
			},
		},
	}, nil
}

func (a *kb58TestCallee) OpenSession(_ context.Context, sessionID string, _ map[string]string, secrets map[string]string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.openSess = append(a.openSess, sessionID)
	a.opens = append(a.opens, copySS(secrets))
	return nil
}

func (a *kb58TestCallee) Execute(_ context.Context, sessionID string, step *workflow.StepNode, _ adapter.EventSink, rejection *v2.ExecutionRejection) (adapter.Result, error) {
	a.rec.record(sessionID, step)
	return adapter.Result{
		Outcome: "success",
		Outputs: map[string]cty.Value{
			"report": cty.StringVal("probed:" + step.Input["task"]),
		},
	}, nil
}

func (a *kb58TestCallee) CloseSession(_ context.Context, sessionID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closes = append(a.closes, sessionID)
	return nil
}

func (a *kb58TestCallee) Kill()                               {}
func (a *kb58TestCallee) Pause(context.Context, string) error { return nil }
func (a *kb58TestCallee) Resume(context.Context, string) error {
	return nil
}
func (a *kb58TestCallee) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}
func (a *kb58TestCallee) Snapshot(context.Context, string) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{}, nil
}
func (a *kb58TestCallee) Restore(context.Context, string, []byte, uint32) error {
	return nil
}

// secretsSeen returns the secrets the session identified by sessionID was
// opened with (the host passes the adapter instance id to OpenSession).
func (a *kb58TestCallee) secretsSeen(sessionID string) (map[string]string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, sess := range a.openSess {
		if sess == sessionID {
			return a.opens[i], true
		}
	}
	return nil, false
}

func (a *kb58TestCallee) closeCount(sessionID string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, c := range a.closes {
		if c == sessionID {
			n++
		}
	}
	return n
}

// kb58TestCaller is the claude-like caller fake. Each Execute issues its
// call sequence sequentially on the shared permission stream with unique
// request ids, records every tool_call_result, and re-exports the last
// successful callee result's outputs as callee.* step outputs
// (ADR-0004 §5 caller contract).
type kb58TestCaller struct {
	mu sync.Mutex
	// callIDs is the request-id sequence Execute issues; nil defaults to
	// ["call-0", "call-1"].
	callIDs  []string
	requests <-chan *v2.PermissionEvent
	results  map[string]*v2.ToolCallResult
}

func newKB58TestCaller() *kb58TestCaller {
	return &kb58TestCaller{results: map[string]*v2.ToolCallResult{}}
}

func (a *kb58TestCaller) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: "claude", Version: "test", Capabilities: []string{"adapter_tools", "execute"}}, nil
}

func (a *kb58TestCaller) OpenSession(context.Context, string, map[string]string, map[string]string) error {
	return nil
}

func (a *kb58TestCaller) StartPermissionStream(_ context.Context, _ string, requests <-chan *v2.PermissionEvent) (func(), error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests = requests
	return func() {}, nil
}

func (a *kb58TestCaller) Execute(ctx context.Context, _ string, _ *workflow.StepNode, sink adapter.EventSink, rejection *v2.ExecutionRejection) (adapter.Result, error) {
	a.mu.Lock()
	requests := a.requests
	a.mu.Unlock()
	if requests == nil {
		return adapter.Result{Outcome: "failure"}, errors.New("permission stream not started")
	}

	var lastOutputs map[string]cty.Value
	ids := a.callIDs
	if ids == nil {
		ids = []string{"call-0", "call-1"}
	}
	for _, id := range ids {
		sink.Adapter("permission.request", map[string]any{
			"request_id": id,
			"target":     kb58ProbeTarget,
			"args":       map[string]any{"task": "probe-thing"},
		})

		tcr, err := awaitToolCallResult(ctx, requests, 5*time.Second)
		if err != nil {
			return adapter.Result{Outcome: "failure"}, err
		}
		a.mu.Lock()
		a.results[id] = tcr
		a.mu.Unlock()
		if tcr.CallError != "" {
			continue
		}
		if out := kb58DecodeOutputs(tcr.OutputsJson); out != nil {
			lastOutputs = out
		}
	}

	if lastOutputs == nil {
		return adapter.Result{Outcome: "success"}, nil
	}
	outputs := map[string]cty.Value{}
	for k, v := range lastOutputs {
		outputs["callee."+k] = v
	}
	return adapter.Result{Outcome: "success", Outputs: outputs}, nil
}

func (a *kb58TestCaller) CloseSession(context.Context, string) error { return nil }
func (a *kb58TestCaller) Kill()                                      {}
func (a *kb58TestCaller) Pause(context.Context, string) error        { return nil }
func (a *kb58TestCaller) Resume(context.Context, string) error       { return nil }
func (a *kb58TestCaller) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}
func (a *kb58TestCaller) Snapshot(context.Context, string) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{}, nil
}
func (a *kb58TestCaller) Restore(context.Context, string, []byte, uint32) error { return nil }

func (a *kb58TestCaller) gotResult(id string) *v2.ToolCallResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.results[id]
}

func awaitToolCallResult(ctx context.Context, requests <-chan *v2.PermissionEvent, timeout time.Duration) (*v2.ToolCallResult, error) {
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-requests:
			if !ok {
				return nil, errors.New("permission stream closed")
			}
			if tcr := ev.GetToolCallResult(); tcr != nil {
				return tcr, nil
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline:
			return nil, errors.New("timed out waiting for tool_call_result")
		}
	}
}

func kb58DecodeOutputs(raw []byte) map[string]cty.Value {
	if len(raw) == 0 {
		return nil
	}
	typed, err := ctyjson.Unmarshal(raw, cty.Object(map[string]cty.Type{"report": cty.String}))
	if err != nil {
		return nil
	}
	return map[string]cty.Value{"report": typed.GetAttr("report")}
}

func copySS(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

// kb58Graph builds the KB-58 reproduction topology:
// root declares the host-local dynamic callee "mcp.probe" (with adapter
// secrets) and a parallel subworkflow step; each iteration body declares its
// own claude caller whose step allowlists the probe's tool surface.
func kb58Graph(t *testing.T, callerAllow []string, policy workflow.Policy) (*workflow.FSMGraph, *kb58TestCaller) {
	t.Helper()

	body := &workflow.FSMGraph{
		Name:         "probe-iteration",
		InitialState: "call",
		Steps: map[string]*workflow.StepNode{
			"call": {
				Name:       "call",
				TargetKind: workflow.StepTargetAdapter,
				AdapterRef: "claude.agent",
				AllowTools: callerAllow,
				// CRI-157 grant shape: the caller is granted a tool
				// resource (the mcp probe), never a caller-is-self grant
				// and never a step target anywhere in the body tree.
				Tools: []workflow.AdapterToolRef{{CalleeRef: kb58CalleeRef}},
				Outcomes: map[string]*workflow.CompiledOutcome{
					"success": {Next: "done"},
					"failure": {Next: "failed"},
				},
			},
		},
		States: map[string]*workflow.StateNode{
			"done":   {Name: "done", Terminal: true, Success: true},
			"failed": {Name: "failed", Terminal: true, Success: false},
		},
		Policy: workflow.DefaultPolicy,
		Adapters: map[string]*workflow.AdapterNode{
			"claude.agent": {Type: "claude", Name: "agent"},
		},
		AdapterOrder: []string{"claude.agent"},
		Environments: map[string]*workflow.EnvironmentNode{},
		Variables:    map[string]*workflow.VariableNode{},
	}

	sw := &workflow.SubworkflowNode{
		Name:         "probe",
		SourcePath:   "/test/probe",
		Body:         body,
		BodyEntry:    body.InitialState,
		Inputs:       map[string]hcl.Expression{},
		DeclaredVars: map[string]*workflow.VariableNode{},
	}

	caller := newKB58TestCaller()
	g := &workflow.FSMGraph{
		Name:         "kb58-root",
		InitialState: "call",
		TargetState:  "done",
		Policy:       policy,
		Steps: map[string]*workflow.StepNode{
			"call": {
				Name:           "call",
				TargetKind:     workflow.StepTargetSubworkflow,
				SubworkflowRef: "probe",
				Parallel:       parseExpr(t, `["a", "b"]`),
				ParallelMax:    2,
				Outcomes: map[string]*workflow.CompiledOutcome{
					"all_succeeded": {Next: "done"},
					"any_failed":    {Next: "failed"},
				},
			},
		},
		States: map[string]*workflow.StateNode{
			"done":   {Name: "done", Terminal: true, Success: true},
			"failed": {Name: "failed", Terminal: true, Success: false},
		},
		// Root adapter "mcp.probe" declares a host-local secret: the probe credentials
		// live and die in the runner host (containment invariant). The secret is a
		// declared sensitive workflow variable bound directly in the adapter block.
		Adapters: map[string]*workflow.AdapterNode{
			kb58CalleeRef: {
				Type:         "mcp",
				Name:         "probe",
				DynamicTools: true,
				Secrets: map[string]hcl.Expression{
					"probe_token": parseExpr(t, "var."+kb58SecretVar),
				},
			},
		},
		AdapterOrder: []string{kb58CalleeRef},
		Environments: map[string]*workflow.EnvironmentNode{},
		Variables: map[string]*workflow.VariableNode{
			kb58SecretVar: {
				Name:    kb58SecretVar,
				Type:    cty.String,
				Secret:  true,
				Default: cty.StringVal(kb58ProbeSecret),
			},
		},
		Subworkflows: map[string]*workflow.SubworkflowNode{"probe": sw},
	}
	return g, caller
}

// kb58RunGraph wires the loader, runs the graph, and returns the callee,
// sink, and audit collector for assertions.
func kb58RunGraph(t *testing.T, g *workflow.FSMGraph, caller *kb58TestCaller) (*kb58TestCallee, *loopOutputSink, *engineAuditCollector) {
	t.Helper()
	callee := &kb58TestCallee{rec: &nestedEngineRecorder{}}
	sink, audit := kb58RunGraphCallee(t, g, caller, callee)
	return callee, sink, audit
}

// kb58RunGraphCallee runs the graph with an explicit callee instance and
// returns the engine sink and audit collector for assertions.
func kb58RunGraphCallee(t *testing.T, g *workflow.FSMGraph, caller *kb58TestCaller, callee *kb58TestCallee) (*loopOutputSink, *engineAuditCollector) {
	t.Helper()
	loader := adapterhost.NewLoaderWithDiscovery(func(string) (string, error) { return "", os.ErrNotExist })
	loader.RegisterBuiltin("claude", func() adapterhost.Handle { return caller })
	loader.RegisterBuiltin("mcp", func() adapterhost.Handle { return callee })

	sink := &loopOutputSink{}
	audit := &engineAuditCollector{}
	if err := New(g, loader, sink, WithAuditWriter(audit)).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	return sink, audit
}

// auditReasons returns all audit decision reasons containing substr.
func auditReasons(entries []*adapterhost.DecisionLogEntry, substr string) []string {
	var out []string
	for _, e := range entries {
		if e != nil && strings.Contains(e.Reason, substr) {
			out = append(out, e.Reason)
		}
	}
	return out
}

// TestKB58_ParallelIterationResolvesHostLocalToolResource is the documented
// reproduction: a parallel per-scope iteration tool-calls a host-local
// dynamic adapter declared only in the root graph. Before the fix the seam
// fails with typed unknown_adapter; after the fix both iterations resolve the
// callee from the borrowed root verified record, the callee runs host-local
// with the root adapter secrets, the caller never sees them, and the
// borrowed callee sessions are closed at scope teardown.
func TestKB58_ParallelIterationResolvesHostLocalToolResource(t *testing.T) {
	g, caller := kb58Graph(t, []string{"adapter.mcp.probe.tools.*"}, workflow.DefaultPolicy)
	callee, sink, audit := kb58RunGraph(t, g, caller)

	// Every issued call must be resolved and succeed (no typed errors).
	for _, id := range []string{"call-0", "call-1"} {
		res := caller.gotResult(id)
		if res == nil {
			t.Fatalf("%s: caller never received a tool_call_result", id)
		}
		if res.CallError != "" {
			t.Errorf("%s: call_error = %q (outputs=%s); want empty (adapter must resolve)", id, res.CallError, string(res.OutputsJson))
			continue
		}
	}

	// Containment: the callee was opened host-local WITH the root adapter's
	// secrets.
	if secs, ok := callee.secretsSeen(kb58CalleeSess); !ok {
		t.Errorf("callee session %q was never opened on the local handle", kb58CalleeSess)
	} else if secs["probe_token"] != kb58ProbeSecret {
		t.Errorf("callee session opened without root adapter secret: got %v; want probe_token=%q", secs, kb58ProbeSecret)
	}

	// Containment: the secret never leaked into callee.* outputs returned to
	// the caller.
	for _, id := range []string{"call-0", "call-1"} {
		if res := caller.gotResult(id); res != nil && strings.Contains(string(res.OutputsJson), kb58ProbeSecret) {
			t.Errorf("%s: probe secret leaked into results returned to the caller", id)
		}
	}

	// The callee must be executed once per tool call: two parallel iterations
	// × two calls made by each caller step = 4 host-local executions.
	if got, want := len(callee.rec.allSessions()), 4; got != want {
		t.Errorf("callee Execute count = %d; want %d (2 iterations × 2 calls)", got, want)
	}

	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal state: got %q (ok=%v); want \"done\" (true)", sink.terminal, sink.terminalOK)
	}

	// Lifecycle hygiene: the borrowed callee session is closed when the
	// borrowed manager tears down (each iteration scope closes its own copy).
	if n := callee.closeCount(kb58CalleeSess); n < 2 {
		t.Errorf("callee CloseSession count for %q = %d; want >= 2 (one per iteration scope)", kb58CalleeSess, n)
	}

	// Audit hygiene: a pre-fix deny (unknown_adapter) must not be recorded.
	if reasons := auditReasons(audit.all(), "unknown_adapter"); len(reasons) != 0 {
		t.Errorf("audit recorded unknown_adapter denials: %v", reasons)
	}
}

// TestKB58_MaxToolCallsBudgetEnforcesCallCount pins policy.max_tool_calls
// (KB-58): the seam counts every nested adapter tool call a caller session
// makes per iteration and refuses calls beyond the budget with the typed
// call_error `budget_exhausted` (ADR-0004 SS8 registry) plus an audited deny,
// while the run keeps going to terminal. Each iteration's caller session gets
// its own budget, so one successful call survives per iteration. Depth
// (default 8) is untouched — the loop is sequential, not nested.
func TestKB58_MaxToolCallsBudgetEnforcesCallCount(t *testing.T) {
	policy := workflow.DefaultPolicy
	policy.MaxToolCalls = 1

	g, caller := kb58Graph(t, []string{"adapter.mcp.probe.tools.*"}, policy)
	// Two parallel iterations, each issuing 4 sequential calls: with a budget
	// of 1 per caller session, call-0 executes and call-1..3 are refused
	// typed — the callee never sees them.
	caller.callIDs = []string{"call-0", "call-1", "call-2", "call-3"}
	callee, sink, audit := kb58RunGraph(t, g, caller)

	for _, id := range caller.callIDs {
		res := caller.gotResult(id)
		if res == nil {
			t.Fatalf("%s: caller never received a tool_call_result", id)
		}
		switch id {
		case "call-0":
			if res.CallError != "" {
				t.Errorf("%s: call_error = %q; want empty (within budget)", id, res.CallError)
			}
		default:
			if res.CallError != "budget_exhausted" {
				t.Errorf("%s: call_error = %q; want budget_exhausted (budget exhausted)", id, res.CallError)
			}
		}
	}

	// The refused calls never reach the callee: exactly one host-local
	// execution per iteration makes it through.
	if got, want := len(callee.rec.allSessions()), 2; got != want {
		t.Errorf("callee Execute count = %d; want %d (one per iteration)", got, want)
	}

	// Every refused call is audited as a deny naming the typed code.
	if got, want := len(auditReasons(audit.all(), "budget_exhausted")), 6; got != want {
		t.Errorf("budget_exhausted audit entries = %d; want %d (3 refusals × 2 iterations)", got, want)
	}

	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal state: got %q (ok=%v); want \"done\" (true) — the budget refusal must not kill the run", sink.terminal, sink.terminalOK)
	}
}
