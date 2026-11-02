// Tool resources, shared sessions, and child run supervision test suite.
// Unifies:
//   - KB-58: Host-local tool resource resolution & max_tool_calls budget enforcement (kb58_tool_resource_seam_test.go)
//   - KB-156: Shared adapter sessions across parallel subworkflow branches (kb156_shared_tool_resource_sessions_test.go)
//   - KB-160: Remote peer tool resource host routing (kb160_remote_tool_resource_test.go)
//   - KB-95: Workflow.v1 criteria-as-adapter child session in-flight re-execute guard & crash adoption (kb95_repro_test.go)
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	criteriav2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	peerpkg "github.com/brokenbots/criteria/internal/peer"
	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

// -----------------------------------------------------------------------------
// KB-58: Host-Local Tool-Resource Resolution and Budget Enforcement
// -----------------------------------------------------------------------------

const (
	kb58ProbeSecret = "s3cr3t-probe-token"
	kb58ProbeTarget = "adapter.mcp.probe.tools.probe"
	kb58CalleeRef   = "mcp.probe"
	kb58CalleeSess  = kb58CalleeRef
	kb58SecretVar   = "probe_token"
)

// kb58TestCallee is the mcp-like dynamic-tool callee fake: a host-local
// adapter with a dynamic tool surface.
type kb58TestCallee struct {
	rec *nestedEngineRecorder

	// concurrent makes the fake declare the concurrent_execute capability
	// (KB-155) so sibling nested calls fan out over one shared session
	// instead of serializing on the execute turn gate.
	concurrent bool

	mu       sync.Mutex
	openSess []string
	opens    []map[string]string
	closes   []string
}

func (a *kb58TestCallee) Info(context.Context) (adapterhost.Info, error) {
	caps := []string{"execute"}
	if a.concurrent {
		caps = append(caps, "concurrent_execute")
	}
	return adapterhost.Info{
		Name:         "mcp",
		Version:      "test",
		Capabilities: caps,
		AdapterInfo: workflow.AdapterInfo{
			OutputSchema: map[string]workflow.ConfigField{
				"report": {CtyType: cty.String},
			},
		},
	}, nil
}

func (a *kb58TestCallee) OpenSession(_ context.Context, sessionID string, _, secrets map[string]string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.openSess = append(a.openSess, sessionID)
	a.opens = append(a.opens, copySS(secrets))
	return nil
}

func (a *kb58TestCallee) Execute(_ context.Context, sessionID string, step *workflow.StepNode, _ adapter.EventSink, rejection *criteriav2.ExecutionRejection) (adapter.Result, error) {
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
func (a *kb58TestCallee) Inspect(context.Context, string) (*criteriav2.InspectResponse, error) {
	return &criteriav2.InspectResponse{}, nil
}
func (a *kb58TestCallee) Snapshot(context.Context, string) (*criteriav2.SnapshotResponse, error) {
	return &criteriav2.SnapshotResponse{}, nil
}
func (a *kb58TestCallee) Restore(context.Context, string, []byte, uint32) error {
	return nil
}

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

type kb58TestCaller struct {
	mu       sync.Mutex
	callIDs  []string
	requests map[string]<-chan *criteriav2.PermissionEvent
	results  map[string]*criteriav2.ToolCallResult
}

func newKB58TestCaller() *kb58TestCaller {
	return &kb58TestCaller{
		requests: map[string]<-chan *criteriav2.PermissionEvent{},
		results:  map[string]*criteriav2.ToolCallResult{},
	}
}

func (a *kb58TestCaller) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: "claude", Version: "test", Capabilities: []string{"adapter_tools", "execute"}}, nil
}

func (a *kb58TestCaller) OpenSession(context.Context, string, map[string]string, map[string]string) error {
	return nil
}

func (a *kb58TestCaller) StartPermissionStream(_ context.Context, sessionID string, requests <-chan *criteriav2.PermissionEvent) (func(), error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests[sessionID] = requests
	return func() {}, nil
}

func (a *kb58TestCaller) Execute(ctx context.Context, sessionID string, _ *workflow.StepNode, sink adapter.EventSink, rejection *criteriav2.ExecutionRejection) (adapter.Result, error) {
	a.mu.Lock()
	requests := a.requests[sessionID]
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
func (a *kb58TestCaller) Inspect(context.Context, string) (*criteriav2.InspectResponse, error) {
	return &criteriav2.InspectResponse{}, nil
}
func (a *kb58TestCaller) Snapshot(context.Context, string) (*criteriav2.SnapshotResponse, error) {
	return &criteriav2.SnapshotResponse{}, nil
}
func (a *kb58TestCaller) Restore(context.Context, string, []byte, uint32) error { return nil }

func (a *kb58TestCaller) gotResult(id string) *criteriav2.ToolCallResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.results[id]
}

func awaitToolCallResult(ctx context.Context, requests <-chan *criteriav2.PermissionEvent, timeout time.Duration) (*criteriav2.ToolCallResult, error) {
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

type kb58CallerProbe struct {
	mu    sync.Mutex
	ids   []string
	fakes []*kb58TestCaller
}

func (p *kb58CallerProbe) setCallIDs(ids []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ids = ids
	for _, f := range p.fakes {
		f.callIDs = ids
	}
}

func (p *kb58CallerProbe) newFake() *kb58TestCaller {
	p.mu.Lock()
	defer p.mu.Unlock()
	f := newKB58TestCaller()
	f.callIDs = p.ids
	p.fakes = append(p.fakes, f)
	return f
}

func (p *kb58CallerProbe) gotResult(id string) *criteriav2.ToolCallResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, f := range p.fakes {
		if r := f.gotResult(id); r != nil {
			return r
		}
	}
	return nil
}

func (p *kb58CallerProbe) idsList() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.ids...)
}

func (p *kb58CallerProbe) fakeCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.fakes)
}

type kb58CalleeProbe struct {
	mu         sync.Mutex
	fakes      []*kb58TestCallee
	concurrent bool
}

func (p *kb58CalleeProbe) newFake() *kb58TestCallee {
	p.mu.Lock()
	defer p.mu.Unlock()
	f := &kb58TestCallee{rec: &nestedEngineRecorder{}, concurrent: p.concurrent}
	p.fakes = append(p.fakes, f)
	return f
}

func (p *kb58CalleeProbe) openCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, f := range p.fakes {
		n += len(f.openSess)
	}
	return n
}

func (p *kb58CalleeProbe) secretsSeen(sessionID string) (map[string]string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, f := range p.fakes {
		if secs, ok := f.secretsSeen(sessionID); ok {
			return secs, true
		}
	}
	return nil, false
}

func (p *kb58CalleeProbe) closeCount(sessionID string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, f := range p.fakes {
		n += f.closeCount(sessionID)
	}
	return n
}

func (p *kb58CalleeProbe) executeCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, f := range p.fakes {
		n += len(f.rec.sessions)
	}
	return n
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

func kb58Graph(t *testing.T, callerAllow []string, policy workflow.Policy) (*workflow.FSMGraph, *kb58CallerProbe, *kb58CalleeProbe) {
	t.Helper()
	callers := &kb58CallerProbe{}
	callees := &kb58CalleeProbe{}

	body := &workflow.FSMGraph{
		Name:         "probe-iteration",
		InitialState: "call",
		Steps: map[string]*workflow.StepNode{
			"call": {
				Name:       "call",
				TargetKind: workflow.StepTargetAdapter,
				AdapterRef: "claude.agent",
				AllowTools: callerAllow,
				Tools:      []workflow.AdapterToolRef{{CalleeRef: kb58CalleeRef}},
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
	return g, callers, callees
}

func kb58RunGraph(t *testing.T, g *workflow.FSMGraph, callers *kb58CallerProbe, callees *kb58CalleeProbe) (*loopOutputSink, *engineAuditCollector) {
	t.Helper()
	loader := adapterhost.NewLoaderWithDiscovery(func(string) (string, error) { return "", os.ErrNotExist })
	loader.RegisterBuiltin("claude", func() adapterhost.Handle { return callers.newFake() })
	loader.RegisterBuiltin("mcp", func() adapterhost.Handle { return callees.newFake() })

	sink := &loopOutputSink{}
	audit := &engineAuditCollector{}
	if err := New(g, loader, sink, WithAuditWriter(audit)).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	return sink, audit
}

func auditReasons(entries []*adapterhost.DecisionLogEntry, substr string) []string {
	var out []string
	for _, e := range entries {
		if e != nil && strings.Contains(e.Reason, substr) {
			out = append(out, e.Reason)
		}
	}
	return out
}

// TestKB58_ParallelIterationResolvesHostLocalToolResource pins KB-58:
// a parallel per-scope iteration tool-calls a host-local dynamic adapter
// declared only in the root graph.
func TestKB58_ParallelIterationResolvesHostLocalToolResource(t *testing.T) {
	g, callers, callees := kb58Graph(t, []string{"adapter.mcp.probe.tools.*"}, workflow.DefaultPolicy)
	sink, audit := kb58RunGraph(t, g, callers, callees)

	for _, id := range []string{"call-0", "call-1"} {
		res := callers.gotResult(id)
		if res == nil {
			t.Fatalf("%s: caller never received a tool_call_result", id)
		}
		if res.CallError != "" {
			t.Errorf("%s: call_error = %q (outputs=%s); want empty (adapter must resolve)", id, res.CallError, string(res.OutputsJson))
			continue
		}
	}

	if secs, ok := callees.secretsSeen(kb58CalleeSess); !ok {
		t.Errorf("callee session %q was never opened on the local handle", kb58CalleeSess)
	} else if secs["probe_token"] != kb58ProbeSecret {
		t.Errorf("callee session opened without root adapter secret: got %v; want probe_token=%q", secs, kb58ProbeSecret)
	}

	for _, id := range []string{"call-0", "call-1"} {
		if res := callers.gotResult(id); res != nil && strings.Contains(string(res.OutputsJson), kb58ProbeSecret) {
			t.Errorf("%s: probe secret leaked into results returned to the caller", id)
		}
	}

	if got, want := callees.executeCount(), 4; got != want {
		t.Errorf("callee Execute count = %d; want %d (2 iterations × 2 calls)", got, want)
	}

	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal state: got %q (ok=%v); want \"done\" (true)", sink.terminal, sink.terminalOK)
	}

	if got, want := callees.closeCount(kb58CalleeSess), 1; got != want {
		t.Errorf("callee CloseSession count for %q = %d; want %d (one shared session, closed with the owner scope)", kb58CalleeSess, got, want)
	}
	if got, want := callees.openCount(), 1; got != want {
		t.Errorf("callee OpenSession count = %d; want %d (one shared session per environment)", got, want)
	}

	if reasons := auditReasons(audit.all(), "unknown_adapter"); len(reasons) != 0 {
		t.Errorf("audit recorded unknown_adapter denials: %v", reasons)
	}
}

// TestKB58_MaxToolCallsBudgetEnforcesCallCount pins policy.max_tool_calls.
func TestKB58_MaxToolCallsBudgetEnforcesCallCount(t *testing.T) {
	policy := workflow.DefaultPolicy
	policy.MaxToolCalls = 1

	g, callers, callees := kb58Graph(t, []string{"adapter.mcp.probe.tools.*"}, policy)
	callers.setCallIDs([]string{"call-0", "call-1", "call-2", "call-3"})
	sink, audit := kb58RunGraph(t, g, callers, callees)

	if callers.fakeCount() < 2 {
		t.Errorf("caller handles resolved = %d; want >= 2 (one per parallel iteration)", callers.fakeCount())
	}

	for _, id := range callers.idsList() {
		res := callers.gotResult(id)
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

	if got, want := callees.executeCount(), 2; got != want {
		t.Errorf("callee Execute count = %d; want %d (one per iteration)", got, want)
	}

	if got, want := len(auditReasons(audit.all(), "budget_exhausted")), 6; got != want {
		t.Errorf("budget_exhausted audit entries = %d; want %d (3 refusals × 2 iterations)", got, want)
	}

	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal state: got %q (ok=%v); want \"done\" (true) — the budget refusal must not kill the run", sink.terminal, sink.terminalOK)
	}
}

// -----------------------------------------------------------------------------
// KB-156: Shared Tool-Resource Sessions Across Parallel Branches
// -----------------------------------------------------------------------------

func kb156Graph(t *testing.T, items []string, declaredCallee bool, policy workflow.Policy) (*workflow.FSMGraph, *kb58CallerProbe, *kb58CalleeProbe) {
	t.Helper()
	callers := &kb58CallerProbe{}
	callees := &kb58CalleeProbe{concurrent: true}

	body := &workflow.FSMGraph{
		Name:         "probe-iteration",
		InitialState: "call",
		Steps: map[string]*workflow.StepNode{
			"call": {
				Name:       "call",
				TargetKind: workflow.StepTargetAdapter,
				AdapterRef: "claude.agent",
				AllowTools: []string{"adapter.mcp.probe.tools.*"},
				Tools:      []workflow.AdapterToolRef{{CalleeRef: kb58CalleeRef}},
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
		Policy:       workflow.DefaultPolicy,
		Adapters:     map[string]*workflow.AdapterNode{"claude.agent": {Type: "claude", Name: "agent"}},
		AdapterOrder: []string{"claude.agent"},
		Environments: map[string]*workflow.EnvironmentNode{},
		Variables:    map[string]*workflow.VariableNode{},
	}

	rootAdapters := map[string]*workflow.AdapterNode{}
	rootOrder := []string{}
	if declaredCallee {
		rootAdapters[kb58CalleeRef] = &workflow.AdapterNode{
			Type:         "mcp",
			Name:         "probe",
			DynamicTools: true,
			Secrets: map[string]hcl.Expression{
				"probe_token": parseExpr(t, "var."+kb58SecretVar),
			},
		}
		rootOrder = append(rootOrder, kb58CalleeRef)
	}

	quoted := make([]string, len(items))
	for i, it := range items {
		quoted[i] = fmt.Sprintf("%q", it)
	}
	sw := &workflow.SubworkflowNode{
		Name:         "probe",
		SourcePath:   "/test/probe",
		Body:         body,
		BodyEntry:    body.InitialState,
		Inputs:       map[string]hcl.Expression{},
		DeclaredVars: map[string]*workflow.VariableNode{},
	}
	g := &workflow.FSMGraph{
		Name:         "kb156-root",
		InitialState: "call",
		TargetState:  "done",
		Policy:       policy,
		Steps: map[string]*workflow.StepNode{
			"call": {
				Name:           "call",
				TargetKind:     workflow.StepTargetSubworkflow,
				SubworkflowRef: "probe",
				Parallel:       parseExpr(t, "["+strings.Join(quoted, ", ")+"]"),
				ParallelMax:    len(items),
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
		Adapters:     rootAdapters,
		AdapterOrder: rootOrder,
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
	return g, callers, callees
}

// TestKB156_ParallelIterationsShareOneToolResourceSession pins the core
// KB-156 contract: N parallel branches against an mcp tool-resource run
// against ONE shared session per environment.
func TestKB156_ParallelIterationsShareOneToolResourceSession(t *testing.T) {
	const iterations = 4
	g, callers, callees := kb156Graph(t, []string{"a", "b", "c", "d"}, true, workflow.DefaultPolicy)
	callers.setCallIDs([]string{"call-0"})
	sink, audit := kb58RunGraph(t, g, callers, callees)

	if got, want := callees.openCount(), 1; got != want {
		t.Errorf("callee OpenSession count = %d; want %d (one shared session per environment)", got, want)
	}
	if got, want := callees.executeCount(), iterations; got != want {
		t.Errorf("callee Execute count = %d; want %d (every branch's call still delivered)", got, want)
	}
	if got, want := callees.closeCount(kb58CalleeSess), 1; got != want {
		t.Errorf("callee CloseSession count for %q = %d; want %d (teardown anchored to the owner scope, after the last lease drops)", kb58CalleeSess, got, want)
	}
	for i := 0; i < iterations; i++ {
		res := callers.gotResult("call-0")
		if res == nil {
			t.Fatalf("branch %d: caller never received a tool_call_result", i)
		}
		if res.CallError != "" {
			t.Errorf("branch %d: call_error = %q; want empty", i, res.CallError)
		}
	}
	if reasons := auditReasons(audit.all(), "unknown_adapter"); len(reasons) != 0 {
		t.Errorf("audit recorded unknown_adapter denials: %v", reasons)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal state: got %q (ok=%v); want \"done\" (true)", sink.terminal, sink.terminalOK)
	}
}

// TestKB156_LeaseOnlyHostOwnedCalleesPreservesUnknownAdapter pins the
// preserved error signature: a tool resource granted in an iteration body
// that the owner (host-of-record) manager cannot host is never leased.
func TestKB156_LeaseOnlyHostOwnedCalleesPreservesUnknownAdapter(t *testing.T) {
	g, callers, callees := kb156Graph(t, []string{"a", "b"}, false, workflow.DefaultPolicy)
	callers.setCallIDs([]string{"call-0"})
	sink, audit := kb58RunGraph(t, g, callers, callees)

	if got, want := callees.openCount(), 0; got != want {
		t.Errorf("callee OpenSession count = %d; want %d (nothing to host)", got, want)
	}
	for _, item := range []string{"a", "b"} {
		res := callers.gotResult("call-0")
		if res == nil {
			t.Fatalf("branch %s: caller never received a tool_call_result", item)
		}
		if res.CallError != "unknown_adapter" {
			t.Errorf("branch %s: call_error = %q; want unknown_adapter (preserved KB-58 signature)", item, res.CallError)
		}
	}
	if reasons := auditReasons(audit.all(), "unknown_adapter"); len(reasons) == 0 {
		t.Errorf("audit recorded no unknown_adapter denials; want one per refused call")
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal state: got %q (ok=%v); want \"done\" (true) — the typed refusal must not kill the run", sink.terminal, sink.terminalOK)
	}
}

// -----------------------------------------------------------------------------
// KB-160: Remote Peer Tool Resource Routing
// -----------------------------------------------------------------------------

const (
	kb160RemoteEnvKey = "remote.cell"
	kb160PeerDigest   = "sha256:abcd1234"
)

func kb160PinSet() *lockfile.Lockfile {
	return &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{Type: "mcp", Name: "probe", ResolvedDigest: kb160PeerDigest},
		},
	}
}

func kb160ReservePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve loopback port: %v", err)
	}
	listen := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("release reserved port: %v", err)
	}
	return listen
}

func kb160RemoteEnv(t *testing.T, listen string) *workflow.EnvironmentNode {
	t.Helper()
	parser := hclparse.NewParser()
	file, diags := parser.ParseHCL([]byte(fmt.Sprintf("listen_address = %q\ninsecure = true\n", listen)), "remote.hcl")
	if diags.HasErrors() {
		t.Fatalf("parse remote body: %s", diags)
	}
	return &workflow.EnvironmentNode{
		Type:    "remote",
		Name:    "cell",
		Process: &workflow.ProcessPolicy{Exec: []string{"*"}},
		RawBody: file.Body,
	}
}

func kb160RemoteGraph(t *testing.T, listen string) *workflow.FSMGraph {
	t.Helper()
	g, _, _ := kb58Graph(t, []string{"adapter.mcp.probe.tools.*"}, workflow.DefaultPolicy)
	g.Environments = map[string]*workflow.EnvironmentNode{
		kb160RemoteEnvKey: kb160RemoteEnv(t, listen),
	}
	g.PinSet = kb160PinSet()
	g.Adapters[kb58CalleeRef].Environment = kb160RemoteEnvKey
	return g
}

type kb160PeerCallee struct {
	mu       sync.Mutex
	opens    []string
	openSecs []map[string]string
	closes   []string
	executes []kb160Execute
}

type kb160Execute struct {
	session string
	step    string
	task    string
}

func (c *kb160PeerCallee) Info(context.Context, *criteriav2.InfoRequest) (*criteriav2.InfoResponse, error) {
	return &criteriav2.InfoResponse{Name: "mcp", Version: "test", Capabilities: []string{"execute"}}, nil
}

func (c *kb160PeerCallee) OpenSession(_ context.Context, req *criteriav2.OpenSessionRequest) (*criteriav2.OpenSessionResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.opens = append(c.opens, req.GetSessionId())
	secs := map[string]string{}
	for k, v := range req.GetSecrets() {
		secs[k] = v
	}
	c.openSecs = append(c.openSecs, secs)
	return &criteriav2.OpenSessionResponse{}, nil
}

func (c *kb160PeerCallee) Execute(_ context.Context, req *criteriav2.ExecuteRequest, sink adapterhost.ExecuteEventSink) error {
	c.mu.Lock()
	c.executes = append(c.executes, kb160Execute{
		session: req.GetSessionId(),
		step:    req.GetStepName(),
		task:    req.GetInput()["task"],
	})
	c.mu.Unlock()
	out, err := json.Marshal(map[string]any{"report": "probed:" + req.GetInput()["task"]})
	if err != nil {
		return err
	}
	return sink.Emit(&criteriav2.ExecuteEvent{
		Event: &criteriav2.ExecuteEvent_Result{Result: &criteriav2.ExecuteResult{
			Outcome:     "success",
			OutputsJson: out,
		}},
	})
}

func (c *kb160PeerCallee) CloseSession(_ context.Context, req *criteriav2.CloseSessionRequest) (*criteriav2.CloseSessionResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closes = append(c.closes, req.GetSessionId())
	return &criteriav2.CloseSessionResponse{}, nil
}

func (c *kb160PeerCallee) Log(_ context.Context, _ *criteriav2.LogRequest, sink adapterhost.LogEventSink) error {
	return sink.Emit(&criteriav2.LogEvent{StreamName: "stdout", Line: []byte("kb160 callee log")})
}

func (c *kb160PeerCallee) Permissions(_ context.Context, requests <-chan *criteriav2.PermissionEvent) error {
	for range requests {
	}
	return nil
}

func (c *kb160PeerCallee) Pause(context.Context, *criteriav2.PauseRequest) (*criteriav2.PauseResponse, error) {
	return &criteriav2.PauseResponse{}, nil
}

func (c *kb160PeerCallee) Resume(context.Context, *criteriav2.ResumeRequest) (*criteriav2.ResumeResponse, error) {
	return &criteriav2.ResumeResponse{}, nil
}

func (c *kb160PeerCallee) Snapshot(context.Context, *criteriav2.SnapshotRequest) (*criteriav2.SnapshotResponse, error) {
	return &criteriav2.SnapshotResponse{}, nil
}

func (c *kb160PeerCallee) Restore(context.Context, *criteriav2.RestoreRequest) (*criteriav2.RestoreResponse, error) {
	return &criteriav2.RestoreResponse{}, nil
}

func (c *kb160PeerCallee) Inspect(context.Context, *criteriav2.InspectRequest) (*criteriav2.InspectResponse, error) {
	return &criteriav2.InspectResponse{}, nil
}

func (c *kb160PeerCallee) Prompt(context.Context, *adapterhost.PromptRequest) (*adapterhost.PromptResponse, error) {
	return &adapterhost.PromptResponse{}, nil
}

func (c *kb160PeerCallee) openCount(session string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, id := range c.opens {
		if id == session {
			n++
		}
	}
	return n
}

func (c *kb160PeerCallee) secretsSeen(session string) (map[string]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, id := range c.opens {
		if id == session {
			return c.openSecs[i], true
		}
	}
	return nil, false
}

func (c *kb160PeerCallee) executeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.executes)
}

func (c *kb160PeerCallee) closeCount(session string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, id := range c.closes {
		if id == session {
			n++
		}
	}
	return n
}

func kb160StartPeer(t *testing.T, listen string, callee *kb160PeerCallee) *peerpkg.Server {
	t.Helper()
	cfg := &peerpkg.Config{
		Host:        listen,
		AdapterName: "mcp.probe",
		Digest:      kb160PeerDigest,
		BackoffMin:  10 * time.Millisecond,
		BackoffMax:  50 * time.Millisecond,
	}
	srv := peerpkg.NewServeAdapterServer(cfg, callee, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Serve(ctx) }()
	return srv
}

func kb160StartRun(t *testing.T, g *workflow.FSMGraph, callers *kb58CallerProbe) (*loopOutputSink, *engineAuditCollector, chan error) {
	t.Helper()
	loader := adapterhost.NewLoaderWithDiscovery(func(string) (string, error) { return "", os.ErrNotExist })
	loader.RegisterBuiltin("claude", func() adapterhost.Handle { return callers.newFake() })

	sink := &loopOutputSink{}
	audit := &engineAuditCollector{}
	done := make(chan error, 1)
	go func() {
		done <- New(g, loader, sink, WithAuditWriter(audit)).Run(context.Background())
	}()
	return sink, audit, done
}

func kb160AwaitRun(t *testing.T, sink *loopOutputSink, done chan error, timeout time.Duration) error {
	t.Helper()
	deadline := time.After(timeout)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			return err
		case <-ticker.C:
			if state, _ := sink.terminalState(); state == "" {
				continue
			}
			select {
			case err := <-done:
				return err
			case <-time.After(250 * time.Millisecond):
				return nil
			}
		case <-deadline:
			state, _ := sink.terminalState()
			t.Fatalf("run never reached a terminal state (sink.terminal=%q)", state)
		}
	}
}

// TestKB160_ParallelIterationRoutesRemoteToolResourceThroughPeer is the KB-160
// reproduction: parallel iterations tool-call a REMOTE-environment adapter
// declared only in the root graph.
func TestKB160_ParallelIterationRoutesRemoteToolResourceThroughPeer(t *testing.T) {
	if !canDialLoopbackTCP() {
		t.Skip("skipping test: loopback TCP dialing is restricted in current environment")
	}
	listen := kb160ReservePort(t)
	g := kb160RemoteGraph(t, listen)
	callers := &kb58CallerProbe{}
	callee := &kb160PeerCallee{}
	kb160StartPeer(t, listen, callee)
	sink, audit, done := kb160StartRun(t, g, callers)

	if err := kb160AwaitRun(t, sink, done, 60*time.Second); err != nil {
		t.Fatalf("run: %v", err)
	}

	for _, id := range []string{"call-0", "call-1"} {
		res := callers.gotResult(id)
		if res == nil {
			t.Fatalf("%s: caller never received a tool_call_result", id)
		}
		if res.CallError != "" {
			t.Fatalf("%s: call_error = %q (outputs=%s); want empty (the remote callee must resolve through the peer)", id, res.CallError, string(res.OutputsJson))
		}
		if !strings.Contains(string(res.OutputsJson), "probed:probe-thing") {
			t.Errorf("%s: outputs %s missing the callee report", id, string(res.OutputsJson))
		}
	}

	if got, want := callee.openCount(kb58CalleeSess), 1; got != want {
		t.Errorf("peer session opens for %q = %d; want %d (one shared session per environment)", kb58CalleeSess, got, want)
	}

	if secs, ok := callee.secretsSeen(kb58CalleeSess); !ok {
		t.Errorf("peer session %q never opened", kb58CalleeSess)
	} else if secs[kb58SecretVar] != kb58ProbeSecret {
		t.Errorf("peer session opened without the root adapter secret: got %v; want %s=%q", secs, kb58SecretVar, kb58ProbeSecret)
	}

	for _, id := range []string{"call-0", "call-1"} {
		if res := callers.gotResult(id); res != nil && strings.Contains(string(res.OutputsJson), kb58ProbeSecret) {
			t.Errorf("%s: probe secret leaked into results returned to the caller", id)
		}
	}

	if got, want := callee.executeCount(), 4; got != want {
		t.Errorf("peer executes = %d; want %d (2 iterations × 2 calls over the shared session)", got, want)
	}

	if got, want := callee.closeCount(kb58CalleeSess), 1; got != want {
		t.Errorf("peer closes for %q = %d; want %d (owner closes the shared session once)", kb58CalleeSess, got, want)
	}

	if state, ok := sink.terminalState(); state != "done" || !ok {
		t.Errorf("terminal state: got %q (ok=%v); want \"done\" (true)", state, ok)
	}

	if reasons := auditReasons(audit.all(), "unknown_adapter"); len(reasons) != 0 {
		t.Errorf("audit recorded unknown_adapter denials: %v", reasons)
	}
}

// -----------------------------------------------------------------------------
// KB-95: Criteria-as-Adapter (Workflow.v1) Child Session In-Flight Guard & Crash Adoption
// -----------------------------------------------------------------------------

const kb95ChildLossErr = "connection lost: criteria peer transport lost (child run evidence pending adoption)"

const kb95Workflow = `
workflow {
  name = "kb95"
  version = "0.1"
  initial_state = "run_child"
  target_state  = "done"
}
step "run_child" {
  target = adapter.fake
  outcome "success" { next = state.done }
  outcome "failure" { next = step.collect_outcome }
}
step "collect_outcome" {
  target = adapter.fake
  outcome "success" { next = state.done }
  outcome "error" { next = state.done }
}
state "done" {
  terminal = true
  success  = true
}`

type kb95Shared struct {
	engine *Engine

	mu            sync.Mutex
	guardErr      error
	guardResultOk bool
}

func (s *kb95Shared) liveManager() *adapterhost.SessionManager {
	if s == nil {
		return nil
	}
	s.engine.mu.RLock()
	sessions := s.engine.liveSessions
	s.engine.mu.RUnlock()
	return sessions
}

func (s *kb95Shared) reExecuteProbe(ctx context.Context, sessionName string, step *workflow.StepNode) {
	mgr := s.liveManager()
	if mgr == nil {
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err := mgr.Execute(probeCtx, sessionName, step, noopSink{}, nil)
	s.mu.Lock()
	s.guardErr = err
	s.guardResultOk = err != nil && errors.As(err, new(*adapterhost.ErrChildRunInFlight))
	s.mu.Unlock()
}

func (s *kb95Shared) guard() (error, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.guardErr, s.guardResultOk
}

type kb95ChildAdapter struct {
	shared *kb95Shared

	mu       sync.Mutex
	executes []string
	opens    []string
}

func (a *kb95ChildAdapter) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{
		Name:         "fake",
		Version:      "test",
		Capabilities: []string{"execute", "workflow.v1"},
	}, nil
}

func (a *kb95ChildAdapter) OpenSession(_ context.Context, name string, _, _ map[string]string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.opens = append(a.opens, name)
	return nil
}

func (a *kb95ChildAdapter) Execute(ctx context.Context, name string, step *workflow.StepNode, _ adapter.EventSink, rejection *criteriav2.ExecutionRejection) (adapter.Result, error) {
	a.mu.Lock()
	stepName := ""
	if step != nil {
		stepName = step.Name
	}
	a.executes = append(a.executes, stepName)
	a.mu.Unlock()
	if stepName == "run_child" {
		a.shared.reExecuteProbe(ctx, name, step)
		return adapter.Result{}, errors.New(kb95ChildLossErr)
	}
	return adapter.Result{Outcome: "success"}, nil
}

func (a *kb95ChildAdapter) SupervisionCrashReason() (string, bool) {
	return adapterhost.CrashReasonChildRunLost, true
}

func (a *kb95ChildAdapter) Permit(context.Context, string, string, bool, string) error { return nil }
func (a *kb95ChildAdapter) CloseSession(context.Context, string) error                 { return nil }
func (a *kb95ChildAdapter) Kill()                                                      {}
func (a *kb95ChildAdapter) Pause(context.Context, string) error                        { return nil }
func (a *kb95ChildAdapter) Resume(context.Context, string) error                       { return nil }
func (a *kb95ChildAdapter) Inspect(context.Context, string) (*criteriav2.InspectResponse, error) {
	return &criteriav2.InspectResponse{}, nil
}
func (a *kb95ChildAdapter) Snapshot(context.Context, string) (*criteriav2.SnapshotResponse, error) {
	return &criteriav2.SnapshotResponse{}, nil
}
func (a *kb95ChildAdapter) Restore(context.Context, string, []byte, uint32) error { return nil }

func (a *kb95ChildAdapter) callLog() (opens, executes []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.opens...), append([]string(nil), a.executes...)
}

func (a *kb95ChildAdapter) executeCount(stepName string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, s := range a.executes {
		if s == stepName {
			n++
		}
	}
	return n
}

type kb95ReproSink struct {
	*fakeSink

	mu       sync.Mutex
	events   []cri271Event
	outcomes []kb95Outcome
}

type kb95Outcome struct {
	step    string
	outcome string
	err     string
}

func (s *kb95ReproSink) OnStepOutcome(step, outcome string, _ time.Duration, err error, comment string) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	s.mu.Lock()
	s.outcomes = append(s.outcomes, kb95Outcome{step: step, outcome: outcome, err: msg})
	s.mu.Unlock()
	s.fakeSink.OnStepOutcome(step, outcome, 0, nil, "")
}

func (s *kb95ReproSink) StepEventSink(string) adapter.EventSink {
	return &kb95EventRecorder{parent: s}
}

type kb95EventRecorder struct {
	parent *kb95ReproSink
}

func (r *kb95EventRecorder) Log(string, []byte) {}
func (r *kb95EventRecorder) Adapter(kind string, data any) {
	payload, _ := data.(map[string]any)
	r.parent.mu.Lock()
	defer r.parent.mu.Unlock()
	r.parent.events = append(r.parent.events, cri271Event{kind: kind, data: payload})
}

func (s *kb95ReproSink) recorded(kind string) (map[string]any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, evt := range s.events {
		if evt.kind == kind {
			return evt.data, true
		}
	}
	return nil, false
}

func (s *kb95ReproSink) lastOutcome(step string) (kb95Outcome, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.outcomes) - 1; i >= 0; i-- {
		if s.outcomes[i].step == step {
			return s.outcomes[i], true
		}
	}
	return kb95Outcome{}, false
}

// TestKB95_WorkflowV1ReExecuteGuardFailClosedAndCrashAdoption pins KB-95:
// during the child run's Execute a second (overlapping) Execute on the same session
// is rejected fail-closed with the typed ErrChildRunInFlight, and when the
// phone-home transport to the child dies mid-run the step fails with the child's
// classified evidence (CrashReasonChildRunLost).
func TestKB95_WorkflowV1ReExecuteGuardFailClosedAndCrashAdoption(t *testing.T) {
	g := compile(t, kb95Workflow)
	shared := &kb95Shared{}
	p := &kb95ChildAdapter{shared: shared}
	sink := &kb95ReproSink{fakeSink: &fakeSink{}}
	eng := NewTestEngine(g, cri271NewLoader(p), sink)
	shared.engine = eng
	err := eng.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal state %q success=%v; want done/true", sink.terminal, sink.terminalOK)
	}

	guardErr, guardTyped := shared.guard()
	if guardErr == nil {
		t.Fatal("re-Execute probe was not rejected; want the typed ErrChildRunInFlight")
	}
	if !guardTyped {
		t.Errorf("guard err = %v; want *adapterhost.ErrChildRunInFlight", guardErr)
	}
	var guard *adapterhost.ErrChildRunInFlight
	if errors.As(guardErr, &guard) && guard.Session != "fake.default" {
		t.Errorf("guard.Session = %q; want fake.default", guard.Session)
	}

	if got := p.executeCount("run_child"); got != 1 {
		t.Errorf("run_child Execute count = %d; want exactly 1 (no queued/no double run)", got)
	}

	out, ok := sink.lastOutcome("run_child")
	if !ok || out.outcome != "failure" {
		t.Fatalf("run_child outcome = %+v ok=%v; want a failure outcome", out, ok)
	}
	if !strings.Contains(out.err, "connection lost") || !strings.Contains(out.err, kb95ChildLossErr) {
		t.Errorf("step failure error = %q; want the child-run-loss evidence", out.err)
	}
	data, ok := sink.recorded("session.crash")
	if !ok {
		t.Fatal("expected a session.crash event for the lost child run")
	}
	if reason, _ := data["crash_reason"].(string); reason != adapterhost.CrashReasonChildRunLost {
		t.Errorf("crash_reason = %q; want CrashReasonChildRunLost", reason)
	}
	if data["session"] != "fake.default" {
		t.Errorf("session.crash session = %v; want fake.default", data["session"])
	}

	_, executes := p.callLog()
	foundBookkeeping := false
	for _, s := range executes {
		if s == "collect_outcome" {
			foundBookkeeping = true
		}
	}
	if !foundBookkeeping {
		t.Errorf("collect_outcome did not execute after the adopted crash; executes = %v", executes)
	}
}
