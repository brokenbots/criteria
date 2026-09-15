package adapterhost

// tool_call_chain_test.go — tests for CRI-162: per-call tool-depth tracking
// and runtime call-chain checks in the host runtime. Covers the multi-hop
// chain (A calls B calls C) succeeding under the default cap, the depth gate
// rejecting a call beyond policy.max_tool_depth mid-chain while the run
// completes, runtime cycle detection over the caller adapter ref chain, and
// the cycle gate on a directly constructed sink.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/brokenbots/criteria/workflow"
)

const (
	chainAlphaSession = "alpha.host"
	chainBetaSession  = "beta.mid"
	chainGammaSession = "gamma.leaf"

	chainBetaToolsGlob  = "adapter.beta.mid.tools.*"
	chainAlphaToolsGlob = "adapter.alpha.host.tools.*"
	chainGammaToolsGlob = "adapter.gamma.leaf.tools.*"

	chainBetaTarget  = "adapter.beta.mid.tools.helper_task"
	chainGammaTarget = "adapter.gamma.leaf.tools.helper_task"
	chainAlphaTarget = "adapter.alpha.host.tools.helper_task"
)

// toolCallChainWorkflowHCL is the compiled-graph workflow for the chain
// fixtures: alpha is the engine-targeted caller, beta and gamma are nested
// callees (both dynamic, lazily declared tool surfaces). The step's
// allow_tools covers A's call to B; the workflow-level permissions.allow_tools
// covers the callee sessions' own call surfaces — gamma's leaf surface and
// (for the cycle test) alpha's surface, which is what a call back up the
// chain must clear at the policy gate before the cycle gate diagnoses it.
func toolCallChainWorkflowHCL(policyBlock string) string {
	policy := ""
	if policyBlock != "" {
		policy = "\n  " + policyBlock + "\n"
	}
	return `
workflow {
  name = "chain"
  version       = "0.1"
  initial_state = "call"
  target_state  = "done"` + policy + `
}

adapter "alpha" "host" { dynamic_tools = true }
adapter "beta" "mid"   { dynamic_tools = true }
adapter "gamma" "leaf" { dynamic_tools = true }

step "call" {
  target = adapter.alpha.host
  allow_tools = ["` + chainBetaToolsGlob + `"]
  outcome "success" { next = step.done }
  outcome "failure" { next = step.done }
}
state "done" { terminal = true }

permissions {
  allow_tools = ["` + chainGammaToolsGlob + `", "` + chainAlphaToolsGlob + `"]
}
`
}

func compileToolCallChainGraph(t *testing.T, maxToolDepth int) *workflow.FSMGraph {
	t.Helper()
	policyBlock := ""
	if maxToolDepth > 0 {
		policyBlock = fmt.Sprintf("policy { max_tool_depth = %d }", maxToolDepth)
	}
	spec, diags := workflow.Parse("chain.hcl", []byte(toolCallChainWorkflowHCL(policyBlock)))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	return g
}

// newToolCallChainManager builds a SessionManager with builtin alpha/beta/
// gamma adapters wired to the given fakes.
func newToolCallChainManager(t *testing.T, alpha, beta, gamma Handle) *SessionManager {
	t.Helper()
	loader := NewLoaderWithDiscovery(func(string) (string, error) { return "", nil })
	loader.RegisterBuiltin("alpha", func() Handle { return alpha })
	loader.RegisterBuiltin("beta", func() Handle { return beta })
	loader.RegisterBuiltin("gamma", func() Handle { return gamma })
	return NewSessionManager(loader)
}

// openToolCallChainSessions opens every session on the chain eagerly and
// closes them at test end. The chain tests run the full Execute flow (not the
// lazy-bind path), so eager opens keep session lifetimes deterministic.
func openToolCallChainSessions(t *testing.T, sm *SessionManager, ctx context.Context) {
	t.Helper()
	sessions := []struct{ name, adapterType string }{
		{chainAlphaSession, "alpha"},
		{chainBetaSession, "beta"},
		{chainGammaSession, "gamma"},
	}
	for _, s := range sessions {
		if err := sm.Open(ctx, s.name, s.adapterType, "", nil, nil); err != nil {
			t.Fatalf("Open %s: %v", s.name, err)
		}
	}
	t.Cleanup(func() {
		for _, s := range sessions {
			_ = sm.Close(ctx, s.name)
		}
	})
}

// toolCallChainStep is the engine step that targets alpha: its allow_tools
// covers the chain's first hop (A's call to B).
func toolCallChainStep() *workflow.StepNode {
	return &workflow.StepNode{
		Name:       "call",
		AdapterRef: chainAlphaSession,
		AllowTools: []string{chainBetaToolsGlob},
		Outcomes: map[string]*workflow.CompiledOutcome{
			"success": {Name: "success"},
			"failure": {Name: "failure"},
		},
	}
}

// toolCallChain holds the handles startToolCallChain wires up; alpha and beta
// expose their recorded replies (per-hop observations), gammaRec records the
// leaf callee's synthetic step, and audit collects the run's audit entries.
type toolCallChain struct {
	sm       *SessionManager
	audit    *sliceAuditWriter
	alpha    *nestedCallerAdapter
	beta     *nestedCallerAdapter
	gammaRec *nestedCalleeRecorder
}

// startToolCallChain wires the A→B→(C) chain: alpha tool-calls beta, beta
// tool-calls the given target (gamma for the plain chain, alpha for the
// cycle), and the graph is compiled and verified.
func startToolCallChain(t *testing.T, betaTarget string, maxToolDepth int) *toolCallChain {
	t.Helper()
	audit := &sliceAuditWriter{}
	gammaRec := &nestedCalleeRecorder{}
	alpha := &nestedCallerAdapter{target: chainBetaTarget, args: map[string]any{"task": "chain"}}
	beta := &nestedCallerAdapter{target: betaTarget, args: map[string]any{"task": "leaf"}}
	gamma := &nestedCalleeAdapter{rec: gammaRec}
	sm := newToolCallChainManager(t, alpha, beta, gamma)
	sm.Audit = audit
	graph := compileToolCallChainGraph(t, maxToolDepth)
	sm.SetGraph(graph)
	ctx := context.Background()
	if err := sm.VerifyGraph(ctx, graph, nil); err != nil {
		t.Fatalf("VerifyGraph: %v", err)
	}
	openToolCallChainSessions(t, sm, ctx)
	return &toolCallChain{sm: sm, audit: audit, alpha: alpha, beta: beta, gammaRec: gammaRec}
}

// denyAuditReason returns the deny audit entry's reason for a session and
// typed code, or "" when no matching entry exists.
func denyAuditReason(entries []DecisionLogEntry, sessionID, code string) string {
	for i := range entries {
		e := &entries[i]
		if e.Decision == "deny" && e.SessionID == sessionID && strings.Contains(e.Reason, code) {
			return e.Reason
		}
	}
	return ""
}

// TestToolCallChain_ThreeAdapters: the three-adapter chain A calls B calls C
// succeeds end to end under the default cap — C executes in its own session
// with its synthetic step, B receives C's result as its tool result, and A's
// outcome routing is unaffected.
func TestToolCallChain_ThreeAdapters(t *testing.T) {
	chain := startToolCallChain(t, chainGammaTarget, 0)

	inner := &adapterEventCollector{}
	res, err := chain.sm.Execute(context.Background(), chainAlphaSession, toolCallChainStep(), inner)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Outcome != "success" {
		t.Errorf("caller outcome = %q, want success", res.Outcome)
	}

	// C executed in its own session with the synthetic step carrying its
	// declared input from B's call args.
	if got := chain.gammaRec.calleeSession(); got != chainGammaSession {
		t.Fatalf("C executed in session %q, want %q", got, chainGammaSession)
	}
	if step := chain.gammaRec.calleeStep(); step == nil || step.Input["task"] != "leaf" {
		t.Errorf("C step input = %+v, want task=leaf", step)
	}

	// B received C's typed result as its tool result.
	betaTCR := chain.beta.gotResult()
	if betaTCR == nil || betaTCR.CallError != "" || betaTCR.Outcome != "success" {
		t.Fatalf("B's tool_call_result = %+v, want success with no error", betaTCR)
	}
	// A received B's own success result as its tool result (the chain's
	// outcomes flow back up the whole chain).
	alphaTCR := chain.alpha.gotResult()
	if alphaTCR == nil || alphaTCR.CallError != "" || alphaTCR.Outcome != "success" {
		t.Fatalf("A's tool_call_result = %+v, want success with no error", alphaTCR)
	}
	objType := cty.Object(map[string]cty.Type{"report": cty.String, "count": cty.Number})
	vals, decErr := ctyjson.Unmarshal(betaTCR.OutputsJson, objType)
	if decErr != nil {
		t.Fatalf("decode B's outputs_json %q: %v", betaTCR.OutputsJson, decErr)
	}
	if got := vals.GetAttr("report").AsString(); got != "leaf" {
		t.Errorf("B's tool result report = %q, want leaf (C's derived output)", got)
	}

	// No enforcement deny was written: the chain stayed within bounds.
	for _, e := range chain.audit.all() {
		if e.Decision == "deny" {
			t.Errorf("unexpected deny audit entry: session=%s reason=%q", e.SessionID, e.Reason)
		}
	}
}

// TestToolCallChain_DepthExceeded: with policy.max_tool_depth = 1, A's call
// to B nests at depth 1 (allowed) and B's call to C would nest at depth 2 —
// a typed depth_exceeded failure returned to B. The run still completes: B
// finishes after its failed call, A receives B's success result, and an audit
// entry records the enforcement.
func TestToolCallChain_DepthExceeded(t *testing.T) {
	chain := startToolCallChain(t, chainGammaTarget, 1)

	inner := &adapterEventCollector{}
	res, err := chain.sm.Execute(context.Background(), chainAlphaSession, toolCallChainStep(), inner)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Outcome != "success" {
		t.Errorf("caller outcome = %q, want success (routing unaffected by the rejected nested call)", res.Outcome)
	}

	// B's call to C failed typed; C never executed.
	betaTCR := chain.beta.gotResult()
	if betaTCR == nil || betaTCR.CallError != callErrorDepthExceeded {
		t.Fatalf("B's tool_call_result = %+v, want call_error %q", betaTCR, callErrorDepthExceeded)
	}
	if got := chain.gammaRec.calleeSession(); got != "" {
		t.Errorf("C unexpectedly executed in session %q", got)
	}

	// B's step still completed successfully after its failed call, so A
	// received a clean result — the outer outcome routing is unaffected.
	alphaTCR := chain.alpha.gotResult()
	if alphaTCR == nil || alphaTCR.CallError != "" || alphaTCR.Outcome != "success" {
		t.Fatalf("A's tool_call_result = %+v, want success with no error", alphaTCR)
	}

	// The enforcement is audited on B's session; A's call was allowed.
	if reason := denyAuditReason(chain.audit.all(), chainBetaSession, callErrorDepthExceeded); reason == "" {
		t.Errorf("audit missing depth_exceeded deny entry for session %q; entries = %+v", chainBetaSession, chain.audit.all())
	}
	if reason := denyAuditReason(chain.audit.all(), chainAlphaSession, callErrorDepthExceeded); reason != "" {
		t.Errorf("unexpected depth_exceeded deny entry for A's session: %q", reason)
	}
}

// TestToolCallChain_CycleDetected: a transitive cycle (A calls B, B calls A)
// is diagnosed by the runtime cycle gate — B's call fails typed
// cycle_detected, the run still completes, and the enforcement is audited.
// The compile-time cycle warning stays advisory; this is the enforcement
// point.
func TestToolCallChain_CycleDetected(t *testing.T) {
	chain := startToolCallChain(t, chainAlphaTarget, 0)

	inner := &adapterEventCollector{}
	res, err := chain.sm.Execute(context.Background(), chainAlphaSession, toolCallChainStep(), inner)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Outcome != "success" {
		t.Errorf("caller outcome = %q, want success (routing unaffected by the rejected nested call)", res.Outcome)
	}

	betaTCR := chain.beta.gotResult()
	if betaTCR == nil || betaTCR.CallError != callErrorCycleDetected {
		t.Fatalf("B's tool_call_result = %+v, want call_error %q", betaTCR, callErrorCycleDetected)
	}

	// B's step still completed successfully after its failed call, so A
	// received a clean result — the outer outcome routing is unaffected.
	alphaTCR := chain.alpha.gotResult()
	if alphaTCR == nil || alphaTCR.CallError != "" || alphaTCR.Outcome != "success" {
		t.Fatalf("A's tool_call_result = %+v, want success with no error", alphaTCR)
	}

	if reason := denyAuditReason(chain.audit.all(), chainBetaSession, callErrorCycleDetected); reason == "" {
		t.Errorf("audit missing cycle_detected deny entry for session %q; entries = %+v", chainBetaSession, chain.audit.all())
	}
}

// TestToolCallChain_CycleDetectedDirectSink: the cycle gate fires on a
// directly constructed sink whose call chain already contains the callee —
// the gate itself, independent of the full nested execution path.
func TestToolCallChain_CycleDetectedDirectSink(t *testing.T) {
	audit := &sliceAuditWriter{}
	calleeRec := &nestedCalleeRecorder{}
	sm := newNestedToolCallManager(t,
		&nestedCallerAdapter{target: nestedCallTarget},
		&nestedCalleeAdapter{rec: calleeRec},
	)
	sink, ps := directToolCallSink(t, sm, audit, nestedCallerStep(), nestedToolCallGraph(), 0)
	sink.nesting = toolCallNesting{
		depth: 0,
		chain: []string{nestedCalleeSession, nestedCallerSession},
	}

	sink.Adapter("permission.request", map[string]any{
		"request_id": "call-1",
		"target":     nestedCallTarget,
	})
	tcr := readToolCallResult(t, ps)
	if tcr.CallError != callErrorCycleDetected {
		t.Errorf("call_error = %q, want %q", tcr.CallError, callErrorCycleDetected)
	}
	if calleeRec.calleeSession() != "" {
		t.Errorf("callee unexpectedly executed in session %q", calleeRec.calleeSession())
	}
}

// TestToolCallNesting: unit coverage for the nesting state — the depth
// baseline, the cycle check, and the copy-on-descend contract that keeps
// concurrently dispatched sibling calls from sharing a chain backing array.
func TestToolCallNesting(t *testing.T) {
	seed := toolCallNesting{depth: 0, chain: []string{"alpha.host"}}
	if !seed.enters("alpha.host") {
		t.Error("seed chain must contain the executing step's own ref")
	}
	if seed.enters("beta.mid") {
		t.Error("seed chain must not contain unrelated refs")
	}

	// Descending appends the callee ref and bumps the depth on a fresh chain;
	// the seed chain is left untouched.
	next := seed.descends("beta.mid")
	if next.depth != 1 {
		t.Errorf("descends depth = %d, want 1", next.depth)
	}
	if len(seed.chain) != 1 || seed.chain[0] != "alpha.host" {
		t.Errorf("seed chain mutated by descends: %v", seed.chain)
	}
	if !next.enters("beta.mid") || !next.enters("alpha.host") {
		t.Error("descended chain must contain every ref on the call chain")
	}

	// Two siblings descending from the same state get independent chains.
	left := seed.descends("beta.mid")
	right := seed.descends("gamma.leaf")
	left.descends("delta.side")
	if right.enters("delta.side") {
		t.Error("sibling chain shares state after descends")
	}
	if right.enters("beta.mid") {
		t.Error("sibling chain picked up the other branch's ref")
	}

	// The zero value is a valid baseline: no chain, no depth, no cycles.
	var zero toolCallNesting
	if zero.enters("alpha.host") || zero.depth != 0 {
		t.Errorf("zero nesting = %+v, want depth 0 and empty chain", zero)
	}
	one := zero.descends("beta.mid")
	if one.depth != 1 || !one.enters("beta.mid") {
		t.Errorf("descends from zero = %+v, want depth 1 chain [beta.mid]", one)
	}
}
