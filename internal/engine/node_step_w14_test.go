package engine

// node_step_w14_test.go — W14 universal step target engine tests.
//
// Covers: TestStep_Evaluate_AdapterTarget, TestStep_Evaluate_SubworkflowTarget,
// TestStep_EnvironmentOverride_AppliesToSubprocess,
// TestStep_SubworkflowStepInput_ReachesCallee,
// TestStep_EnvironmentOverride_InjectedIntoAdapter.

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/internal/plugin"
	"github.com/brokenbots/criteria/workflow"
)

// TestStep_Evaluate_AdapterTarget verifies that a step compiled with
// TargetKind=StepTargetAdapter runs via the adapter plugin path and
// produces the expected outcome.
func TestStep_Evaluate_AdapterTarget(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "do"
  target_state  = "done"
  step "do" {
    target = adapter.fake
    outcome "success" { transition_to = "done" }
  }
  state "done" { terminal = true }
}`)

	step, ok := g.Steps["do"]
	if !ok {
		t.Fatal("step 'do' not in compiled graph")
	}
	if step.TargetKind != workflow.StepTargetAdapter {
		t.Errorf("TargetKind = %v, want StepTargetAdapter", step.TargetKind)
	}
	if step.AdapterRef != "fake.default" {
		t.Errorf("AdapterRef = %q, want %q", step.AdapterRef, "fake.default")
	}

	sink := &fakeSink{}
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{
		"fake": &fakePlugin{name: "fake", outcome: "success"},
	}}
	if err := NewTestEngine(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal=%q ok=%v, want done/true", sink.terminal, sink.terminalOK)
	}
}

// TestStep_Evaluate_SubworkflowTarget verifies that a step compiled with
// TargetKind=StepTargetSubworkflow runs via the subworkflow execution path
// and completes successfully. Uses direct struct construction to avoid
// requiring filesystem setup.
func TestStep_Evaluate_SubworkflowTarget(t *testing.T) {
	swNode := minimalSubworkflowNode("callee")
	g := &workflow.FSMGraph{
		Name:         "t",
		InitialState: "do",
		TargetState:  "done",
		Policy:       workflow.DefaultPolicy,
		Steps: map[string]*workflow.StepNode{
			"do": {
				Name:           "do",
				TargetKind:     workflow.StepTargetSubworkflow,
				SubworkflowRef: "callee",
				Outcomes:       map[string]string{"success": "done"},
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

	step := g.Steps["do"]
	if step.TargetKind != workflow.StepTargetSubworkflow {
		t.Errorf("TargetKind = %v, want StepTargetSubworkflow", step.TargetKind)
	}
	if step.SubworkflowRef != "callee" {
		t.Errorf("SubworkflowRef = %q, want %q", step.SubworkflowRef, "callee")
	}

	sink := &fakeSink{}
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{}}
	if err := NewTestEngine(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal=%q ok=%v, want done/true", sink.terminal, sink.terminalOK)
	}
}

// TestStep_EnvironmentOverride_AppliesToSubprocess verifies that a per-step
// environment override takes precedence over the workflow default environment.
// The test uses getStepEnvironment directly to inspect the resolved env without
// requiring a live subprocess.
func TestStep_EnvironmentOverride_AppliesToSubprocess(t *testing.T) {
	g := &workflow.FSMGraph{
		Environments: map[string]*workflow.EnvironmentNode{
			"shell.default": {
				Type:      "shell",
				Name:      "default",
				Variables: map[string]string{"FROM_DEFAULT": "1"},
			},
			"shell.override": {
				Type:      "shell",
				Name:      "override",
				Variables: map[string]string{"FROM_OVERRIDE": "1"},
			},
		},
		DefaultEnvironment: "shell.default",
		Adapters: map[string]*workflow.AdapterNode{
			"noop.default": {Type: "noop", Name: "default"},
		},
	}
	n := &stepNode{
		graph: g,
		step: &workflow.StepNode{
			Name:        "s",
			TargetKind:  workflow.StepTargetAdapter,
			AdapterRef:  "noop.default",
			Environment: "shell.override",
			Input:       map[string]string{},
		},
	}

	env := n.getStepEnvironment()
	if env == nil {
		t.Fatal("getStepEnvironment returned nil")
	}
	if env.Name != "override" {
		t.Errorf("environment Name = %q, want %q; per-step override did not take precedence", env.Name, "override")
	}
	if _, ok := env.Variables["FROM_OVERRIDE"]; !ok {
		t.Error("expected FROM_OVERRIDE in override environment variables")
	}
	if _, ok := env.Variables["FROM_DEFAULT"]; ok {
		t.Error("default environment variable leaked into override environment")
	}
}

// TestStep_EnvironmentOverride_InjectedIntoAdapter verifies that a per-step
// environment override is injected into the adapter's Input["env"] field before
// the adapter receives the step. The existing captureInputPlugin records all
// Input maps from Execute calls so we can inspect the resolved env vars.
func TestStep_EnvironmentOverride_InjectedIntoAdapter(t *testing.T) {
	var captured []map[string]string
	capPlugin := &captureInputPlugin{outcome: "success", capture: &captured}
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{"noop": capPlugin}}
	sink := &fakeSink{}

	g := &workflow.FSMGraph{
		Name:         "t",
		InitialState: "do",
		TargetState:  "done",
		Policy:       workflow.DefaultPolicy,
		Steps: map[string]*workflow.StepNode{
			"do": {
				Name:        "do",
				TargetKind:  workflow.StepTargetAdapter,
				AdapterRef:  "noop.default",
				Environment: "shell.override",
				Input:       map[string]string{},
				Outcomes:    map[string]string{"success": "done"},
			},
		},
		States: map[string]*workflow.StateNode{
			"done": {Name: "done", Terminal: true, Success: true},
		},
		Adapters: map[string]*workflow.AdapterNode{
			"noop.default": {Type: "noop", Name: "default"},
		},
		AdapterOrder: []string{"noop.default"},
		Subworkflows: map[string]*workflow.SubworkflowNode{},
		Variables:    map[string]*workflow.VariableNode{},
		Environments: map[string]*workflow.EnvironmentNode{
			"shell.override": {
				Type:      "shell",
				Name:      "override",
				Variables: map[string]string{"INJECTED_VAR": "injected-value"},
			},
		},
	}

	if err := NewTestEngine(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(captured) == 0 {
		t.Fatal("captureInputPlugin was never called; adapter not reached")
	}
	rawEnv, ok := captured[0]["env"]
	if !ok {
		t.Fatal("adapter did not receive 'env' key in step Input; environment was not injected")
	}
	var envVars map[string]string
	if err := json.Unmarshal([]byte(rawEnv), &envVars); err != nil {
		t.Fatalf("failed to unmarshal env JSON %q: %v", rawEnv, err)
	}
	if envVars["INJECTED_VAR"] != "injected-value" {
		t.Errorf("INJECTED_VAR = %q, want %q; environment override was not injected", envVars["INJECTED_VAR"], "injected-value")
	}
}

// captureOutputSink extends fakeSink to record per-step output maps from OnStepOutputCaptured.
type captureOutputSink struct {
	fakeSink
	mu      sync.Mutex
	outputs map[string]map[string]string
}

func (s *captureOutputSink) OnStepOutputCaptured(step string, outs map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.outputs == nil {
		s.outputs = make(map[string]map[string]string)
	}
	s.outputs[step] = outs
}

// TestStep_SubworkflowStepInput_ReachesCallee verifies that input expressions on a
// subworkflow-targeted step are evaluated in the parent scope and reach the callee's
// variable namespace. The callee binds the input to a variable and returns it as an
// output; the test asserts that the step's output contains the step-supplied value.
func TestStep_SubworkflowStepInput_ReachesCallee(t *testing.T) {
	body := &workflow.FSMGraph{
		InitialState: "done",
		States:       map[string]*workflow.StateNode{"done": {Name: "done", Terminal: true, Success: true}},
		Variables:    map[string]*workflow.VariableNode{"msg": {Name: "msg", Type: cty.String}},
		Outputs: map[string]*workflow.OutputNode{
			"echo": {Name: "echo", Value: traversalExpr("var", "msg")},
		},
		OutputOrder: []string{"echo"},
		Adapters:    map[string]*workflow.AdapterNode{},
	}
	swNode := &workflow.SubworkflowNode{
		Name:         "callee",
		SourcePath:   t.TempDir(),
		Body:         body,
		BodyEntry:    "done",
		Inputs:       map[string]hcl.Expression{},
		DeclaredVars: map[string]*workflow.VariableNode{"msg": {Name: "msg", Type: cty.String}},
	}

	stepInputExpr := map[string]hcl.Expression{
		"msg": &hclsyntax.LiteralValueExpr{Val: cty.StringVal("from-step")},
	}

	g := &workflow.FSMGraph{
		Name:         "t",
		InitialState: "call",
		TargetState:  "done",
		Policy:       workflow.DefaultPolicy,
		Steps: map[string]*workflow.StepNode{
			"call": {
				Name:           "call",
				TargetKind:     workflow.StepTargetSubworkflow,
				SubworkflowRef: "callee",
				InputExprs:     stepInputExpr,
				Outcomes:       map[string]string{"success": "done"},
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

	sink := &captureOutputSink{}
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{}}
	if err := NewTestEngine(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal=%q ok=%v, want done/true", sink.terminal, sink.terminalOK)
	}
	sink.mu.Lock()
	got := sink.outputs["call"]
	sink.mu.Unlock()
	if got == nil {
		t.Fatal("step 'call' outputs not captured; OnStepOutputCaptured was not called")
	}
	if got["echo"] != "from-step" {
		t.Errorf("step output echo = %q, want %q; step input did not reach callee", got["echo"], "from-step")
	}
}
