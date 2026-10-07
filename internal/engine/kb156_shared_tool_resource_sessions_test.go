package engine

// kb156_shared_tool_resource_sessions_test.go — KB-156: ONE shared adapter
// session per environment for mcp/tool-resources. The host-of-record manager
// binds the tool-resource session once; every caller — root graph, parallel
// iterations, subworkflows — routes nested tool calls through that session
// instead of binding a copy per iteration (the KB-58 borrow).
//
// The documented mechanism being fixed: a for_each with N parallel branches
// against an mcp tool-resource used to spawn N adapter processes with N
// initialize+tools/list handshakes, and the adapter's Info surface (its
// discovered tools) clobbered itself across concurrent sessions. After the
// fix there is exactly one OpenSession for the environment no matter how
// many branches run, and the session's close anchors to the owner scope.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// kb156Graph builds the KB-156 topology: the root graph declares a
// host-local tool-resource callee (unless declaredCallee is false) and a
// parallel subworkflow step with one iteration per item; each iteration body
// declares one claude caller granted the callee's tool surface. The fake
// callees declare the KB-155 concurrent_execute capability so sibling
// branches genuinely multiplex the shared session.
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
		// Root adapter "mcp.probe" declares a host-local secret: the probe
		// credentials live and die in the runner host (containment
		// invariant).
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

// kb156RunGraph wires the loader, runs the graph, and returns the engine sink
// and audit collector for assertions. Mirrors kb58RunGraph.
func kb156RunGraph(t *testing.T, g *workflow.FSMGraph, callers *kb58CallerProbe, callees *kb58CalleeProbe) (*loopOutputSink, *engineAuditCollector) {
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

// TestKB156_ParallelIterationsShareOneToolResourceSession pins the core
// KB-156 contract: N parallel branches against an mcp tool-resource run
// against ONE shared session per environment. Before the fix each per-
// iteration SessionManager lazily bound its own callee handle — N processes,
// N initialize+tools/list handshakes, and the adapter's Info surface
// clobbered itself across concurrent sessions. After the fix every branch
// delegates to the root (host-of-record) manager: one OpenSession, one
// CloseSession (anchored to the owner scope), and every issued tool call
// still delivered on the multiplexed (KB-155 concurrent_execute) session.
func TestKB156_ParallelIterationsShareOneToolResourceSession(t *testing.T) {
	const iterations = 4
	g, callers, callees := kb156Graph(t, []string{"a", "b", "c", "d"}, true, workflow.DefaultPolicy)
	callers.setCallIDs([]string{"call-0"})
	sink, audit := kb156RunGraph(t, g, callers, callees)

	if got, want := callees.openCount(), 1; got != want {
		t.Errorf("callee OpenSession count = %d; want %d (one shared session per environment)", got, want)
	}
	if got, want := callees.executeCount(), iterations; got != want {
		t.Errorf("callee Execute count = %d; want %d (every branch's call still delivered)", got, want)
	}
	if got, want := callees.closeCount(kb58CalleeSess), 1; got != want {
		t.Errorf("callee CloseSession count = %d; want %d (teardown anchored to the owner scope, after the last lease drops)", got, want)
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
// that the owner (host-of-record) manager cannot host is never leased, so
// the nested dispatch fails closed with the typed unknown_adapter error and
// the run keeps going (the caller still reports every call, and the deny is
// audited) — the KB-58 seam's documented pre-fix signature.
func TestKB156_LeaseOnlyHostOwnedCalleesPreservesUnknownAdapter(t *testing.T) {
	g, callers, callees := kb156Graph(t, []string{"a", "b"}, false, workflow.DefaultPolicy)
	callers.setCallIDs([]string{"call-0"})
	sink, audit := kb156RunGraph(t, g, callers, callees)

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
