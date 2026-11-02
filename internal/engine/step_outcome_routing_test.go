package engine

// node_step_w15_test.go — W15 engine tests for outcome routing:
//   - outcome "default" mapping for unknown adapter outcomes
//   - next = step.return at top-level exits successfully
//   - next = step.return in a subworkflow scope bubbles to parent
//   - output projection stores projected keys in run vars for subsequent steps
//   - unknown outcome without outcome "default" block causes run failure
//   - event emission for defaulted/unknown outcomes
//   - subworkflow.* namespace accessible in outcome.output expressions

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"

	criteriav2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// outcomeSink embeds fakeSink and captures W15-specific events for assertion.
type outcomeSink struct {
	fakeSink
	defaulted []struct{ step, orig, mapped string }
	unknown   []struct{ step, outcome string }
	outputs   []map[string]string
}

func (s *outcomeSink) OnStepOutcomeDefaulted(step, orig, mapped string) {
	s.defaulted = append(s.defaulted, struct{ step, orig, mapped string }{step, orig, mapped})
}

func (s *outcomeSink) OnStepOutcomeUnknown(step, outcome string) {
	s.unknown = append(s.unknown, struct{ step, outcome string }{step, outcome})
}

func (s *outcomeSink) OnStepOutcomeInvalid(step, outcome string, issues []string, attempt int) {
	s.unknown = append(s.unknown, struct{ step, outcome string }{step, outcome})
}

func (s *outcomeSink) OnAgentPromptInjected(string, string, string, string, time.Time) {}

func (s *outcomeSink) OnRunOutputs(outputs []map[string]string) {
	s.outputs = append(s.outputs, outputs...)
}

// TestStep_DefaultOutcome_AppliedOnUnknownName verifies that when an adapter
// returns an outcome not in the declared set and outcome "default" is configured,
// the engine maps the outcome, the run completes successfully, and the
// OnStepOutcomeDefaulted event is emitted with the original and mapped names.
func TestStep_DefaultOutcome_AppliedOnUnknownName(t *testing.T) {
	g := compile(t, `
workflow {
  name = "t"
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}
step "work" {
  target          = adapter.fake
  outcome "success" { next = step.done }
  outcome "default" { next = step.done }
}
state "done" { terminal = true }`)
	sink := &outcomeSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake": &fakeAdapter{name: "fake", outcome: "unmapped_name"},
	}}
	if err := NewTestEngine(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal=%q ok=%v, want done/true", sink.terminal, sink.terminalOK)
	}
	// Verify OnStepOutcomeDefaulted was emitted with correct payloads.
	if len(sink.defaulted) != 1 {
		t.Fatalf("OnStepOutcomeDefaulted emitted %d times, want 1", len(sink.defaulted))
	}
	d := sink.defaulted[0]
	if d.step != "work" {
		t.Errorf("defaulted.step=%q want %q", d.step, "work")
	}
	if d.orig != "unmapped_name" {
		t.Errorf("defaulted.orig=%q want %q", d.orig, "unmapped_name")
	}
	if d.mapped != "default" {
		t.Errorf("defaulted.mapped=%q want %q", d.mapped, "default")
	}
}

// TestStep_DefaultOutcomeUnset_UnknownNameErrors verifies that an adapter
// returning an unknown outcome with no outcome "default" block set causes a run error
// and emits OnStepOutcomeUnknown.
func TestStep_DefaultOutcomeUnset_UnknownNameErrors(t *testing.T) {
	g := compile(t, `
workflow {
  name = "t"
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}
step "work" {
  target = adapter.fake
  outcome "success" { next = step.done }
  outcome "error" { next = step.done }
}
state "done" { terminal = true }`)
	sink := &outcomeSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake": &fakeAdapter{name: "fake", outcome: "not_declared"},
	}}
	err := NewTestEngine(g, loader, sink).Run(context.Background())
	if err == nil {
		t.Fatal("expected error for unknown outcome without outcome \"default\" block, got nil")
	}
	// Verify OnStepOutcomeUnknown was emitted.
	if len(sink.unknown) != 1 {
		t.Fatalf("OnStepOutcomeUnknown emitted %d times, want 1", len(sink.unknown))
	}
	u := sink.unknown[0]
	if u.step != "work" {
		t.Errorf("unknown.step=%q want %q", u.step, "work")
	}
	if u.outcome != "not_declared" {
		t.Errorf("unknown.outcome=%q want %q", u.outcome, "not_declared")
	}
}

// TestStep_OutcomeReturn_TopLevelTerminal verifies that a step with
// next = step.return at the top level completes the run as terminal-success.
func TestStep_OutcomeReturn_TopLevelTerminal(t *testing.T) {
	g := compile(t, `
workflow {
  name = "t"
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}
step "work" {
  target = adapter.fake
  outcome "success" { next = step.return }
  outcome "error" { next = step.return }
}
state "done" { terminal = true }`)
	sink := &fakeSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake": &fakeAdapter{name: "fake", outcome: "success"},
	}}
	if err := NewTestEngine(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	// Top-level return must complete the run as successful.
	if !sink.terminalOK {
		t.Errorf("terminalOK=false, want true for top-level return")
	}
}

// TestStep_OutcomeReturn_BubblesToParent verifies that a subworkflow step with
// next = step.return exits the subworkflow and maps the result to the parent step's
// declared outcome.
func TestStep_OutcomeReturn_BubblesToParent(t *testing.T) {
	// Build the callee graph: one adapter step that routes to "return".
	calleeStep := &workflow.StepNode{
		Name:       "inner",
		TargetKind: workflow.StepTargetAdapter,
		AdapterRef: "fake.default",
		Input:      map[string]string{},
		Outcomes: map[string]*workflow.CompiledOutcome{
			"success": {Next: workflow.ReturnSentinel},
		},
	}
	calleeGraph := &workflow.FSMGraph{
		Name:         "callee",
		InitialState: "inner",
		TargetState:  "done",
		Policy:       workflow.DefaultPolicy,
		Steps:        map[string]*workflow.StepNode{"inner": calleeStep},
		States: map[string]*workflow.StateNode{
			"done": {Name: "done", Terminal: true, Success: true},
		},
		Adapters:     map[string]*workflow.AdapterNode{"fake.default": {Type: "fake", Name: "default"}},
		AdapterOrder: []string{"fake.default"},
		Subworkflows: map[string]*workflow.SubworkflowNode{},
		Variables:    map[string]*workflow.VariableNode{},
		Environments: map[string]*workflow.EnvironmentNode{},
	}
	swNode := &workflow.SubworkflowNode{
		Name:         "callee",
		Body:         calleeGraph,
		BodyEntry:    "inner",
		Inputs:       map[string]hcl.Expression{},
		DeclaredVars: map[string]*workflow.VariableNode{},
	}
	parentGraph := &workflow.FSMGraph{
		Name:         "parent",
		InitialState: "call",
		TargetState:  "done",
		Policy:       workflow.DefaultPolicy,
		Steps: map[string]*workflow.StepNode{
			"call": {
				Name:           "call",
				TargetKind:     workflow.StepTargetSubworkflow,
				SubworkflowRef: "callee",
				Outcomes: map[string]*workflow.CompiledOutcome{
					"success": {Next: "done"},
				},
			},
		},
		States: map[string]*workflow.StateNode{
			"done": {Name: "done", Terminal: true, Success: true},
		},
		Adapters:     map[string]*workflow.AdapterNode{},
		Subworkflows: map[string]*workflow.SubworkflowNode{"callee": swNode},
		Variables:    map[string]*workflow.VariableNode{},
		Environments: map[string]*workflow.EnvironmentNode{},
	}

	sink := &fakeSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake":         &fakeAdapter{name: "fake", outcome: "success"},
		"fake.default": &fakeAdapter{name: "fake", outcome: "success"},
	}}
	if err := NewTestEngine(parentGraph, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal=%q ok=%v, want done/true", sink.terminal, sink.terminalOK)
	}
}

// TestStep_OutcomeOutputProjection_PassedToNextStep verifies that a step's
// outcome.output = { ... } projection is stored in the run vars and is
// accessible via steps.<name>.* by subsequent steps.
func TestStep_OutcomeOutputProjection_PassedToNextStep(t *testing.T) {
	// This test uses an HCL workflow where step "a" projects its output,
	// and step "b" references it in its input. The run must succeed end-to-end.
	g := compile(t, `
workflow {
  name = "t"
  version       = "0.1"
  initial_state = "a"
  target_state  = "done"
}
step "a" {
  target = adapter.fake
  outcome "success" {
    next = step.b
    output = { result = "projected" }
  }
  outcome "error" { next = step.b }
}
step "b" {
  target = adapter.fake
  outcome "success" { next = step.done }
  outcome "error" { next = step.done }
}
state "done" { terminal = true }`)
	sink := &fakeSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake": &fakeAdapter{name: "fake", outcome: "success"},
	}}
	if err := NewTestEngine(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal=%q ok=%v, want done/true", sink.terminal, sink.terminalOK)
	}
}

// TestStep_OutcomeReturnOutputOverridesOutputBlocks verifies that when a step
// exits via next = step.return with an output projection, the return path is
// treated as successful and the projected output values are emitted via
// OnRunOutputs with correct type encoding (not double-stringified).
func TestStep_OutcomeReturnOutputOverridesOutputBlocks(t *testing.T) {
	g := compile(t, `
workflow {
  name = "t"
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}
step "work" {
  target = adapter.fake
  outcome "success" {
    next = step.return
    output = { status = "from_return", count = 42 }
  }
  outcome "error" { next = step.done }
}
state "done" { terminal = true }`)
	sink := &outcomeSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake": &fakeAdapter{name: "fake", outcome: "success"},
	}}
	if err := NewTestEngine(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !sink.terminalOK {
		t.Error("expected terminalOK=true for return with output projection")
	}
	// Verify OnRunOutputs was called with projected values and correct encoding.
	outputMap := make(map[string]string)
	for _, o := range sink.outputs {
		outputMap[o["name"]] = o["value"]
	}
	if got, want := outputMap["status"], `"from_return"`; got != want {
		t.Errorf("output status: got %q want %q", got, want)
	}
	// count=42 is a number literal; it must be encoded as "42" (not "\"42\"").
	if got, want := outputMap["count"], "42"; got != want {
		t.Errorf("output count: got %q want %q (number must not be double-quoted)", got, want)
	}
}

// TestStep_OutcomeReturn_EndToEnd verifies an end-to-end workflow that uses
// a subworkflow with next = step.return and checks that the parent run completes.
func TestStep_OutcomeReturn_EndToEnd(t *testing.T) {
	// Reuse the BubblesToParent test pattern but via compile() for coverage.
	calleeStep := &workflow.StepNode{
		Name:       "inner",
		TargetKind: workflow.StepTargetAdapter,
		AdapterRef: "fake.default",
		Input:      map[string]string{},
		Outcomes: map[string]*workflow.CompiledOutcome{
			"success": {Next: workflow.ReturnSentinel, Name: "success"},
		},
	}
	calleeGraph := &workflow.FSMGraph{
		Name:         "callee",
		InitialState: "inner",
		TargetState:  "done",
		Policy:       workflow.DefaultPolicy,
		Steps:        map[string]*workflow.StepNode{"inner": calleeStep},
		States: map[string]*workflow.StateNode{
			"done": {Name: "done", Terminal: true, Success: true},
		},
		Adapters:     map[string]*workflow.AdapterNode{"fake.default": {Type: "fake", Name: "default"}},
		AdapterOrder: []string{"fake.default"},
		Subworkflows: map[string]*workflow.SubworkflowNode{},
		Variables:    map[string]*workflow.VariableNode{},
		Environments: map[string]*workflow.EnvironmentNode{},
	}
	swNode := &workflow.SubworkflowNode{
		Name:         "callee",
		Body:         calleeGraph,
		BodyEntry:    "inner",
		Inputs:       map[string]hcl.Expression{},
		DeclaredVars: map[string]*workflow.VariableNode{},
	}
	// Parent calls the subworkflow twice to exercise the path thoroughly.
	parentGraph := &workflow.FSMGraph{
		Name:         "parent",
		InitialState: "first",
		TargetState:  "done",
		Policy:       workflow.DefaultPolicy,
		Steps: map[string]*workflow.StepNode{
			"first": {
				Name:           "first",
				TargetKind:     workflow.StepTargetSubworkflow,
				SubworkflowRef: "callee",
				Outcomes: map[string]*workflow.CompiledOutcome{
					"success": {Next: "second"},
				},
			},
			"second": {
				Name:           "second",
				TargetKind:     workflow.StepTargetSubworkflow,
				SubworkflowRef: "callee",
				Outcomes: map[string]*workflow.CompiledOutcome{
					"success": {Next: "done"},
				},
			},
		},
		States: map[string]*workflow.StateNode{
			"done": {Name: "done", Terminal: true, Success: true},
		},
		Adapters:     map[string]*workflow.AdapterNode{},
		Subworkflows: map[string]*workflow.SubworkflowNode{"callee": swNode},
		Variables:    map[string]*workflow.VariableNode{},
		Environments: map[string]*workflow.EnvironmentNode{},
	}

	sink := &fakeSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake":         &fakeAdapter{name: "fake", outcome: "success"},
		"fake.default": &fakeAdapter{name: "fake", outcome: "success"},
	}}
	if err := NewTestEngine(parentGraph, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal=%q ok=%v, want done/true", sink.terminal, sink.terminalOK)
	}
}

// parseExpr is a test helper that parses a single HCL expression from src.
func parseExpr(t *testing.T, src string) hcl.Expression {
	t.Helper()
	expr, diags := hclsyntax.ParseExpression([]byte(src), "test.hcl", hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		t.Fatalf("parseExpr(%q): %s", src, diags.Error())
	}
	return expr
}

// TestStep_OutcomeOutput_SubworkflowOutputAvailable verifies that when a
// subworkflow step projects its outcome via subworkflow.*, the keys from the
// callee's output map are accessible and the projected values are emitted as
// top-level run outputs (via OnRunOutputs) with the correct encoding.
func TestStep_OutcomeOutput_SubworkflowOutputAvailable(t *testing.T) {
	// Callee: single step returns next = step.return with output = { val = "hello" }.
	// This puts val="hello" in the callee's ReturnOutputs, which runSubworkflow
	// returns as map[string]cty.Value{"val": cty.StringVal("hello")}.
	calleeStep := &workflow.StepNode{
		Name:       "inner",
		TargetKind: workflow.StepTargetAdapter,
		AdapterRef: "fake.default",
		Input:      map[string]string{},
		Outcomes: map[string]*workflow.CompiledOutcome{
			"success": {
				Name:       "success",
				Next:       workflow.ReturnSentinel,
				OutputExpr: parseExpr(t, `{ val = "hello" }`),
			},
		},
	}
	calleeGraph := &workflow.FSMGraph{
		Name:         "callee",
		InitialState: "inner",
		TargetState:  "done",
		Policy:       workflow.DefaultPolicy,
		Steps:        map[string]*workflow.StepNode{"inner": calleeStep},
		States: map[string]*workflow.StateNode{
			"done": {Name: "done", Terminal: true, Success: true},
		},
		Adapters:     map[string]*workflow.AdapterNode{"fake.default": {Type: "fake", Name: "default"}},
		AdapterOrder: []string{"fake.default"},
		Subworkflows: map[string]*workflow.SubworkflowNode{},
		Variables:    map[string]*workflow.VariableNode{},
		Environments: map[string]*workflow.EnvironmentNode{},
	}

	// Parent: subworkflow step projects via subworkflow.val, then returns.
	// next = step.return makes the projected output the top-level run output set,
	// which is emitted via OnRunOutputs and can be cleanly asserted.
	callStep := &workflow.StepNode{
		Name:           "call",
		TargetKind:     workflow.StepTargetSubworkflow,
		SubworkflowRef: "callee",
		Input:          map[string]string{},
		Outcomes: map[string]*workflow.CompiledOutcome{
			"success": {
				Name:       "success",
				Next:       workflow.ReturnSentinel,
				OutputExpr: parseExpr(t, `{ result = subworkflow.val }`),
			},
		},
	}
	swNode := &workflow.SubworkflowNode{
		Name: "callee",
		Body: calleeGraph,
	}
	parentGraph := &workflow.FSMGraph{
		Name:         "t",
		InitialState: "call",
		TargetState:  "done",
		Policy:       workflow.DefaultPolicy,
		Steps:        map[string]*workflow.StepNode{"call": callStep},
		States: map[string]*workflow.StateNode{
			"done": {Name: "done", Terminal: true, Success: true},
		},
		Adapters:     map[string]*workflow.AdapterNode{},
		Subworkflows: map[string]*workflow.SubworkflowNode{"callee": swNode},
		Variables:    map[string]*workflow.VariableNode{},
		Environments: map[string]*workflow.EnvironmentNode{},
	}

	sink := &outcomeSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake":         &fakeAdapter{name: "fake", outcome: "success"},
		"fake.default": &fakeAdapter{name: "fake", outcome: "success"},
	}}
	if err := NewTestEngine(parentGraph, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !sink.terminalOK {
		t.Error("expected terminalOK=true")
	}

	// OnRunOutputs should contain result = "\"hello\"" (JSON-encoded string).
	outputMap := make(map[string]string)
	for _, o := range sink.outputs {
		outputMap[o["name"]] = o["value"]
	}
	if got, want := outputMap["result"], `"hello"`; got != want {
		t.Errorf("subworkflow output projection: result = %q, want %q", got, want)
	}
}

// -----------------------------------------------------------------------------
// Regression tests for CRI-130: step outcome routing integrity
//
// The engine must route step outcomes exclusively through the routing the
// workflow declares. The engine is step-name-agnostic, and routing is preserved
// regardless of step name prefix or position.
// -----------------------------------------------------------------------------

const cri130CrashErr = "rpc error: code = Canceled desc = grpc: the client connection is closing"

type cri130Adapter struct {
	*fakeAdapter
	failStep  string
	failMode  string
	crashStep string
	mu        sync.Mutex
	crashed   bool
}

func (p *cri130Adapter) Execute(ctx context.Context, name string, step *workflow.StepNode, sink adapter.EventSink, rejection *criteriav2.ExecutionRejection) (adapter.Result, error) {
	p.mu.Lock()
	crashed := p.crashed
	stepName := ""
	if step != nil {
		stepName = step.Name
	}
	if !crashed && ((stepName == p.failStep && p.failMode == "crash") || (p.crashStep != "" && stepName == p.crashStep)) {
		p.crashed = true
		crashed = true
	}
	p.mu.Unlock()
	if crashed {
		return adapter.Result{Outcome: "failure"}, errors.New(cri130CrashErr)
	}
	if stepName == p.failStep {
		return adapter.Result{Outcome: "failure"}, nil
	}
	return p.fakeAdapter.Execute(ctx, name, step, sink, nil)
}

func cri130NewLoader(p *cri130Adapter) *fakeLoader {
	return &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake":         p,
		"fake.default": p,
	}}
}

type cri130ViaSink struct {
	*fakeSink

	mu   sync.Mutex
	vias []string
}

func (s *cri130ViaSink) OnStepTransition(from, to, via string) {
	s.fakeSink.OnStepTransition(from, to, via)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vias = append(s.vias, from+"->"+to+" via "+via)
}

func cri130CommentTailWorkflow() string {
	return `
workflow {
  name = "linear_intake_cri130"
  version = "0.1"
  initial_state = "merge_pr"
  target_state  = "handler_complete"
}
step "merge_pr" {
  target = adapter.fake
  outcome "success" { next = step.set_review_state }
  outcome "failure" { next = step.set_review_state }
}
step "set_review_state" {
  target = adapter.fake
  outcome "success" { next = step.comment_handler_done }
  outcome "failure" { next = step.comment_handler_done }
}
step "comment_handler_done" {
  target = adapter.fake
  outcome "success" { next = step.set_done_state }
  outcome "failure" { next = state.failed }
}
step "set_done_state" {
  target = adapter.fake
  outcome "success" { next = state.handler_complete }
  outcome "failure" { next = state.failed }
}
state "handler_complete" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}`
}

func cri130RecoveryWorkflow() string {
	return `
workflow {
  name = "linear_intake_cri130"
  version = "0.1"
  initial_state = "merge_pr"
  target_state  = "done"
}
step "merge_pr" {
  target = adapter.fake
  outcome "success" { next = step.comment_handler_done }
  outcome "failure" { next = step.comment_handler_done }
}
step "comment_handler_done" {
  target = adapter.fake
  outcome "success" { next = state.done }
  outcome "failure" { next = step.restate_ticket }
}
step "restate_ticket" {
  target = adapter.fake
  outcome "success" { next = state.recovered }
  outcome "failure" { next = state.failed }
}
state "done" { terminal = true }
state "recovered" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}`
}

func TestStepOutcomeRouting_DeclaredFailureRoutesToFailedTerminal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failMode string
	}{
		{"clean_failure_outcome", "failure"},
		{"session_crash", "crash"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := compile(t, cri130CommentTailWorkflow())
			p := &cri130Adapter{
				fakeAdapter: &fakeAdapter{name: "fake", outcome: "success"},
				failStep:    "comment_handler_done",
				failMode:    tc.failMode,
			}
			sink := &cri130ViaSink{fakeSink: &fakeSink{}}
			if err := NewTestEngine(g, cri130NewLoader(p), sink).Run(context.Background()); err != nil {
				t.Fatalf("run: %v (a declared failure arm routes; it does not fail the run at the engine level)", err)
			}
			if sink.terminal != "failed" || sink.terminalOK {
				t.Errorf("terminal state %q success=%v; want failed/false (declared failure arm)", sink.terminal, sink.terminalOK)
			}
			if sink.failure != "" {
				t.Errorf("unexpected run failure: %s", sink.failure)
			}
			joined := strings.Join(sink.vias, ",")
			if !strings.Contains(joined, "comment_handler_done->failed via failure") {
				t.Errorf("transitions %q; want the declared failure arm taken with via=failure", joined)
			}
			if strings.Contains(joined, "comment_handler_done->set_review_state via success") {
				t.Errorf("transitions %q; the success arm must not be taken for a failed step (the old viaOutcome=success laundering)", joined)
			}
			ran := strings.Join(sink.stepsRun, ",")
			if strings.Contains(ran, "set_done_state") {
				t.Errorf("steps run %q; the success-arm bookkeeping must not execute after the failure", ran)
			}
		})
	}
}

func TestStepOutcomeRouting_DeclaredFailureTakesRecoveryBranch(t *testing.T) {
	g := compile(t, cri130RecoveryWorkflow())
	p := &cri130Adapter{
		fakeAdapter: &fakeAdapter{name: "fake", outcome: "success"},
		failStep:    "comment_handler_done",
		failMode:    "failure",
	}
	sink := &fakeSink{}
	if err := NewTestEngine(g, cri130NewLoader(p), sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "recovered" || !sink.terminalOK {
		t.Errorf("terminal state %q success=%v; want recovered/true (declared recovery branch)", sink.terminal, sink.terminalOK)
	}
	joined := strings.Join(sink.transitions, ",")
	if !strings.Contains(joined, "comment_handler_done->restate_ticket") {
		t.Errorf("transitions %q; want the declared failure arm to the recovery branch", joined)
	}
	if strings.Contains(joined, "comment_handler_done->done") {
		t.Errorf("transitions %q; the success arm must not be taken for a failed comment step", joined)
	}
	ran := strings.Join(sink.stepsRun, ",")
	if !strings.Contains(ran, "restate_ticket") {
		t.Errorf("steps run %q; the recovery branch must execute", ran)
	}
}

func TestStepOutcomeRouting_IndependentOfStepName(t *testing.T) {
	const tpl = `
workflow {
  name = "linear_intake_cri130"
  version = "0.1"
  initial_state = "merge_pr"
  target_state  = "handler_complete"
}
step "merge_pr" {
  target = adapter.fake
  outcome "success" { next = step.%[1]s }
  outcome "failure" { next = state.failed }
}
step "%[1]s" {
  target = adapter.fake
  outcome "success" { next = step.set_review_state }
  outcome "failure" { next = state.failed }
}
step "set_review_state" {
  target = adapter.fake
  outcome "success" { next = state.handler_complete }
  outcome "failure" { next = state.failed }
}
state "handler_complete" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}`
	byName := map[string]string{}
	for _, stepName := range []string{"comment_handler_done", "finalize_bookkeeping"} {
		t.Run(stepName, func(t *testing.T) {
			g := compile(t, fmt.Sprintf(tpl, stepName))
			p := &cri130Adapter{
				fakeAdapter: &fakeAdapter{name: "fake", outcome: "success"},
				failStep:    stepName,
				failMode:    "failure",
			}
			sink := &fakeSink{}
			if err := NewTestEngine(g, cri130NewLoader(p), sink).Run(context.Background()); err != nil {
				t.Fatalf("run: %v", err)
			}
			if sink.terminal != "failed" || sink.terminalOK {
				t.Errorf("terminal state %q success=%v; want failed/false (declared failure arm, name-independent)", sink.terminal, sink.terminalOK)
			}
			got := make([]string, 0, len(sink.transitions))
			for _, tr := range sink.transitions {
				got = append(got, strings.Replace(tr, stepName, "FAILSTEP", 1))
			}
			byName[stepName] = strings.Join(got, ",")
		})
	}
	if byName["comment_handler_done"] != byName["finalize_bookkeeping"] {
		t.Errorf("routing depends on the step name: comment-prefixed %q vs plain %q", byName["comment_handler_done"], byName["finalize_bookkeeping"])
	}
}

func TestStepOutcomeRouting_CommentCrashRecoveryRunsOnReopenedSession(t *testing.T) {
	g := compile(t, cri130RecoveryWorkflow())
	p := newCri271Adapter("comment_handler_done")
	sink := &fakeSink{}
	if err := NewTestEngine(g, cri271NewLoader(p), sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "recovered" || !sink.terminalOK {
		t.Errorf("terminal state %q success=%v; want recovered/true (declared recovery branch on the re-opened session)", sink.terminal, sink.terminalOK)
	}
	if sink.failure != "" {
		t.Errorf("unexpected run failure: %s", sink.failure)
	}
	opens, executes := p.callLog()
	wantOpens := []string{"fake.default", "fake.default"}
	if len(opens) != len(wantOpens) {
		t.Fatalf("OpenSession calls: %v; want %v (the re-open is name-agnostic)", opens, wantOpens)
	}
	wantExecutes := []string{"merge_pr", "comment_handler_done", "restate_ticket"}
	if len(executes) != len(wantExecutes) {
		t.Fatalf("Execute calls: %v; want %v", executes, wantExecutes)
	}
	for i, want := range wantExecutes {
		if executes[i] != want {
			t.Errorf("Execute calls[%d] = %q; want %q", i, executes[i], want)
		}
	}
	joined := strings.Join(sink.transitions, ",")
	if !strings.Contains(joined, "comment_handler_done->restate_ticket") {
		t.Errorf("transitions %q; want the declared failure arm taken to the recovery branch", joined)
	}
	if strings.Contains(joined, "comment_handler_done->done") {
		t.Errorf("transitions %q; the success arm must not be taken (no laundered continuation)", joined)
	}
	if !strings.Contains(joined, "restate_ticket->recovered") {
		t.Errorf("transitions %q; want the recovery step to complete on the re-opened session", joined)
	}
}

func TestStepOutcomeRouting_CommentCrashRecoveryWithReopenDisabledFailsRun(t *testing.T) {
	t.Setenv("CRITERIA_SESSION_CRASH_REOPEN", "0")
	g := compile(t, cri130RecoveryWorkflow())
	p := newCri271Adapter("comment_handler_done")
	sink := &fakeSink{}
	if err := NewTestEngine(g, cri271NewLoader(p), sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v (the run ends at the failed terminal, not with an engine error)", err)
	}
	if sink.terminal != "failed" || sink.terminalOK {
		t.Errorf("terminal state %q success=%v; want failed/false (restate_ticket's declared failure arm on the dead session)", sink.terminal, sink.terminalOK)
	}
	opens, executes := p.callLog()
	if len(opens) != 1 {
		t.Errorf("OpenSession calls: %v; want exactly the initial open (re-open disabled)", opens)
	}
	wantExecutes := []string{"merge_pr", "comment_handler_done", "restate_ticket"}
	if len(executes) != len(wantExecutes) {
		t.Fatalf("Execute calls: %v; want %v", executes, wantExecutes)
	}
	joined := strings.Join(sink.transitions, ",")
	if !strings.Contains(joined, "comment_handler_done->restate_ticket") {
		t.Errorf("transitions %q; want the comment step's declared failure arm taken", joined)
	}
	if !strings.Contains(joined, "restate_ticket->failed") {
		t.Errorf("transitions %q; want the recovery step routed via its declared failure outcome", joined)
	}
	if strings.Contains(joined, "restate_ticket->recovered") || strings.Contains(joined, "comment_handler_done->done") {
		t.Errorf("transitions %q; no step may reach a success arm after the crash replay", joined)
	}
}

func TestStepOutcomeRouting_CrashExhaustionFailsRunWhenNoFailureArm(t *testing.T) {
	g := compile(t, `
workflow {
  name = "linear_intake_cri130"
  version = "0.1"
  initial_state = "merge_pr"
  target_state  = "done"
}
step "merge_pr" {
  target = adapter.fake
  outcome "success" { next = step.comment_handler_done }
  outcome "failure" { next = step.comment_handler_done }
}
step "comment_handler_done" {
  target = adapter.fake
  outcome "success" { next = state.done }
  outcome "error"   { next = state.done }
}
state "done" { terminal = true }`)
	p := &cri130Adapter{
		fakeAdapter: &fakeAdapter{name: "fake", outcome: "success"},
		failStep:    "comment_handler_done",
		failMode:    "crash",
	}
	sink := &fakeSink{}
	err := NewTestEngine(g, cri130NewLoader(p), sink).Run(context.Background())
	if err == nil {
		t.Fatal("expected the run to fail when the crashed step has no declared failure outcome")
	}
	if !strings.Contains(err.Error(), "step \"comment_handler_done\" failed after 1 attempts") {
		t.Errorf("unexpected error: %v", err)
	}
	if !strings.Contains(err.Error(), cri130CrashErr) {
		t.Errorf("error must carry the observed crash signature; got: %v", err)
	}
	if sink.failure == "" {
		t.Error("expected OnRunFailed for the crashed step with no declared failure outcome")
	}
	if sink.terminal != "" {
		t.Errorf("terminal %q; the failed run must not report a terminal", sink.terminal)
	}
}

func TestStepOutcomeRouting_UnroutedFailureOutcomeFailsRun(t *testing.T) {
	g := compile(t, `
workflow {
  name = "linear_intake_cri130"
  version = "0.1"
  initial_state = "merge_pr"
  target_state  = "done"
}
step "merge_pr" {
  target = adapter.fake
  outcome "success" { next = step.comment_handler_done }
  outcome "failure" { next = step.comment_handler_done }
}
step "comment_handler_done" {
  target = adapter.fake
  outcome "success" { next = state.done }
  outcome "error"   { next = state.done }
}
state "done" { terminal = true }`)
	p := &cri130Adapter{
		fakeAdapter: &fakeAdapter{name: "fake", outcome: "success"},
		failStep:    "comment_handler_done",
		failMode:    "failure",
	}
	sink := &fakeSink{}
	err := NewTestEngine(g, cri130NewLoader(p), sink).Run(context.Background())
	if err == nil {
		t.Fatal("expected the run to fail on the unmapped failure outcome")
	}
	if !strings.Contains(err.Error(), `produced unmapped outcome "failure"`) {
		t.Errorf("unexpected error: %v", err)
	}
	if sink.failure == "" {
		t.Error("expected OnRunFailed for the unmapped failure outcome")
	}
}

func TestStepOutcomeRouting_CommentStepWithoutSuccessTransitionKeepsRouting(t *testing.T) {
	g := compile(t, `
workflow {
  name = "linear_intake_cri130"
  version = "0.1"
  initial_state = "merge_pr"
  target_state  = "done"
}
step "merge_pr" {
  target = adapter.fake
  outcome "success" { next = step.comment_invalid }
  outcome "done"    { next = state.done }
  outcome "failure" { next = state.failed }
}
step "comment_invalid" {
  target = adapter.fake
  outcome "failure" { next = state.failed }
  outcome "success" { next = state.failed }
}
state "done" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}`)
	p := &cri130Adapter{
		fakeAdapter: &fakeAdapter{name: "fake", outcome: "success"},
		failStep:    "comment_invalid",
		failMode:    "crash",
	}
	sink := &fakeSink{}
	if err := NewTestEngine(g, cri130NewLoader(p), sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "failed" || sink.terminalOK {
		t.Errorf("terminal state %q success=%v; want failed/false (declared failure routing preserved)", sink.terminal, sink.terminalOK)
	}
}

func TestStepOutcomeRouting_CommentStepAbortRunStillFatal(t *testing.T) {
	g := compile(t, `
workflow {
  name = "linear_intake_cri130"
  version = "0.1"
  initial_state = "merge_pr"
  target_state  = "done"
}
step "merge_pr" {
  target = adapter.fake
  outcome "success" { next = step.comment_handler_done }
  outcome "failure" { next = step.comment_handler_done }
}
step "comment_handler_done" {
  target = adapter.fake
  on_crash = "abort_run"
  outcome "success" { next = state.done }
  outcome "failure" { next = state.failed }
}
state "done" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}`)
	p := &cri130Adapter{
		fakeAdapter: &fakeAdapter{name: "fake", outcome: "success"},
		failStep:    "comment_handler_done",
		failMode:    "crash",
	}
	sink := &fakeSink{}
	if err := NewTestEngine(g, cri130NewLoader(p), sink).Run(context.Background()); err == nil {
		t.Fatal("expected run failure for on_crash=abort_run")
	}
	if sink.failure == "" {
		t.Error("expected OnRunFailed for on_crash=abort_run")
	}
}

func TestStepOutcomeRouting_NonCommentStepCrashStillFailsRun(t *testing.T) {
	g := compile(t, `
workflow {
  name = "t"
  version = "0.1"
  initial_state = "merge_pr"
  target_state  = "done"
}
step "merge_pr" {
  target = adapter.fake
  outcome "success" { next = step.set_done_state }
  outcome "failure" { next = state.failed }
}
step "set_done_state" {
  target = adapter.fake
  outcome "success" { next = state.done }
  outcome "failure" { next = state.done }
}
state "done" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}`)
	p := &cri130Adapter{
		fakeAdapter: &fakeAdapter{name: "fake", outcome: "success"},
		failStep:    "merge_pr",
		failMode:    "crash",
	}
	sink := &fakeSink{}
	if err := NewTestEngine(g, cri130NewLoader(p), sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "failed" || sink.terminalOK {
		t.Errorf("terminal state %q success=%v; want failed/false", sink.terminal, sink.terminalOK)
	}
}

func TestStepOutcomeRouting_FunctionalFailureBranchNotReportedSuccess(t *testing.T) {
	g := compile(t, `
workflow {
  name = "linear_intake_cri130"
  version = "0.1"
  initial_state = "run_handler"
  target_state  = "awaiting_human"
}
step "run_handler" {
  target = adapter.fake
  outcome "success" { next = state.handler_complete }
  outcome "failure" { next = step.comment_handler_failed }
}
step "comment_handler_failed" {
  target = adapter.fake
  outcome "success" { next = step.set_review_state }
  outcome "failure" { next = state.failed }
}
step "set_review_state" {
  target = adapter.fake
  outcome "success" { next = state.awaiting_human }
  outcome "failure" { next = state.failed }
}
state "handler_complete" { terminal = true }
state "awaiting_human" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}`)
	p := &cri130Adapter{
		fakeAdapter: &fakeAdapter{name: "fake", outcome: "success"},
		failStep:    "run_handler",
		failMode:    "failure",
		crashStep:   "comment_handler_failed",
	}
	sink := &fakeSink{}
	if err := NewTestEngine(g, cri130NewLoader(p), sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "failed" || sink.terminalOK {
		t.Errorf("terminal state %q success=%v; want failed/false (a functionally failed run must not reach a success terminal)", sink.terminal, sink.terminalOK)
	}
	joined := strings.Join(sink.transitions, ",")
	if !strings.Contains(joined, "run_handler->comment_handler_failed") {
		t.Errorf("transitions %q; want run_handler's declared failure arm taken", joined)
	}
	if !strings.Contains(joined, "comment_handler_failed->failed") {
		t.Errorf("transitions %q; want the comment step's declared failure arm taken after its crash", joined)
	}
	if strings.Contains(joined, "comment_handler_failed->set_review_state") {
		t.Errorf("transitions %q; the crashed comment step's success arm must not be taken (no viaOutcome=success laundering)", joined)
	}
	ran := strings.Join(sink.stepsRun, ",")
	if strings.Contains(ran, "set_review_state") {
		t.Errorf("steps run %q; the success-arm bookkeeping must not execute after the comment crash", ran)
	}
}

func TestStepOutcomeRouting_NonTailFollowOnStillFailsRun(t *testing.T) {
	g := compile(t, `
workflow {
  name = "linear_intake_cri130"
  version = "0.1"
  initial_state = "merge_pr"
  target_state  = "handler_complete"
}
step "merge_pr" {
  target = adapter.fake
  outcome "success" { next = step.comment_progress }
  outcome "failure" { next = step.comment_progress }
}
step "comment_progress" {
  target = adapter.fake
  outcome "success" { next = step.set_done_state }
  outcome "failure" { next = state.failed }
}
step "set_done_state" {
  target = adapter.fake
  outcome "success" { next = step.finalize_report }
  outcome "failure" { next = state.failed }
}
step "finalize_report" {
  target = adapter.fake
  outcome "success" { next = state.handler_complete }
  outcome "failure" { next = state.failed }
}
state "handler_complete" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}`)
	p := &cri130Adapter{
		fakeAdapter: &fakeAdapter{name: "fake", outcome: "success"},
		failStep:    "comment_progress",
		failMode:    "crash",
	}
	sink := &fakeSink{}
	if err := NewTestEngine(g, cri130NewLoader(p), sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "failed" || sink.terminalOK {
		t.Errorf("terminal state %q success=%v; want failed/false (follow-on failure must keep genuine routing)", sink.terminal, sink.terminalOK)
	}
	if got := strings.Join(sink.transitions, ","); !strings.Contains(got, "comment_progress->failed") {
		t.Errorf("transitions %q; want comment_progress to route via its failure outcome (no silent downgrade)", got)
	}
	if ran := strings.Join(sink.stepsRun, ","); strings.Contains(ran, "set_done_state") {
		t.Errorf("steps run %q; the success-arm follow-ons must not execute after the declared failure routing", ran)
	}
}
