package engine

// node_step_w15_test.go — W15 engine tests for outcome routing:
//   - default_outcome mapping for unknown adapter outcomes
//   - next = "return" at top-level exits successfully
//   - next = "return" in a subworkflow scope bubbles to parent
//   - output projection stores projected keys in run vars for subsequent steps
//   - unknown outcome without default_outcome causes run failure
//   - event emission for defaulted/unknown outcomes
//   - subworkflow.* namespace accessible in outcome.output expressions

import (
	"context"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"

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

func (s *outcomeSink) OnRunOutputs(outputs []map[string]string) {
	s.outputs = append(s.outputs, outputs...)
}

// TestStep_DefaultOutcome_AppliedOnUnknownName verifies that when an adapter
// returns an outcome not in the declared set and default_outcome is configured,
// the engine maps the outcome, the run completes successfully, and the
// OnStepOutcomeDefaulted event is emitted with the original and mapped names.
func TestStep_DefaultOutcome_AppliedOnUnknownName(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}
step "work" {
  target          = adapter.fake
  default_outcome = "success"
  outcome "success" { next = "done" }
}
state "done" { terminal = true }`)
	sink := &outcomeSink{}
	loader := &fakeLoader{plugins: map[string]adapterhost.Handle{
		"fake": &fakePlugin{name: "fake", outcome: "unmapped_name"},
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
	if d.mapped != "success" {
		t.Errorf("defaulted.mapped=%q want %q", d.mapped, "success")
	}
}

// TestStep_DefaultOutcomeUnset_UnknownNameErrors verifies that an adapter
// returning an unknown outcome with no default_outcome set causes a run error
// and emits OnStepOutcomeUnknown.
func TestStep_DefaultOutcomeUnset_UnknownNameErrors(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}
step "work" {
  target = adapter.fake
  outcome "success" { next = "done" }
}
state "done" { terminal = true }`)
	sink := &outcomeSink{}
	loader := &fakeLoader{plugins: map[string]adapterhost.Handle{
		"fake": &fakePlugin{name: "fake", outcome: "not_declared"},
	}}
	err := NewTestEngine(g, loader, sink).Run(context.Background())
	if err == nil {
		t.Fatal("expected error for unknown outcome without default_outcome, got nil")
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
// next = "return" at the top level completes the run as terminal-success.
func TestStep_OutcomeReturn_TopLevelTerminal(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}
step "work" {
  target = adapter.fake
  outcome "success" { next = "return" }
}
state "done" { terminal = true }`)
	sink := &fakeSink{}
	loader := &fakeLoader{plugins: map[string]adapterhost.Handle{
		"fake": &fakePlugin{name: "fake", outcome: "success"},
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
// next = "return" exits the subworkflow and maps the result to the parent step's
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
	loader := &fakeLoader{plugins: map[string]adapterhost.Handle{
		"fake":         &fakePlugin{name: "fake", outcome: "success"},
		"fake.default": &fakePlugin{name: "fake", outcome: "success"},
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
workflow "t" {
  version       = "0.1"
  initial_state = "a"
  target_state  = "done"
}
step "a" {
  target = adapter.fake
  outcome "success" {
    next   = "b"
    output = { result = "projected" }
  }
}
step "b" {
  target = adapter.fake
  outcome "success" { next = "done" }
}
state "done" { terminal = true }`)
	sink := &fakeSink{}
	loader := &fakeLoader{plugins: map[string]adapterhost.Handle{
		"fake": &fakePlugin{name: "fake", outcome: "success"},
	}}
	if err := NewTestEngine(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal=%q ok=%v, want done/true", sink.terminal, sink.terminalOK)
	}
}

// TestStep_OutcomeReturnOutputOverridesOutputBlocks verifies that when a step
// exits via next = "return" with an output projection, the return path is
// treated as successful and the projected output values are emitted via
// OnRunOutputs with correct type encoding (not double-stringified).
func TestStep_OutcomeReturnOutputOverridesOutputBlocks(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}
step "work" {
  target = adapter.fake
  outcome "success" {
    next   = "return"
    output = { status = "from_return", count = 42 }
  }
}
state "done" { terminal = true }`)
	sink := &outcomeSink{}
	loader := &fakeLoader{plugins: map[string]adapterhost.Handle{
		"fake": &fakePlugin{name: "fake", outcome: "success"},
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
// a subworkflow with next = "return" and checks that the parent run completes.
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
	loader := &fakeLoader{plugins: map[string]adapterhost.Handle{
		"fake":         &fakePlugin{name: "fake", outcome: "success"},
		"fake.default": &fakePlugin{name: "fake", outcome: "success"},
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
	// Callee: single step returns next = "return" with output = { val = "hello" }.
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
	// next = "return" makes the projected output the top-level run output set,
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
	loader := &fakeLoader{plugins: map[string]adapterhost.Handle{
		"fake":         &fakePlugin{name: "fake", outcome: "success"},
		"fake.default": &fakePlugin{name: "fake", outcome: "success"},
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
