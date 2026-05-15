package engine_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/engine"
	"github.com/brokenbots/criteria/workflow"
)

// switchSink records branch-evaluated events (the proto event name is preserved)
// and terminal state.
type switchSink struct {
	mu             sync.Mutex
	branchEvents   []branchEvent
	terminal       string
	terminalOK     bool
	failure        string
	outputCaptured map[string]map[string]string
}

type branchEvent struct {
	node       string
	matchedArm string
	target     string
	condition  string
}

func (s *switchSink) OnRunStarted(string, string) {}
func (s *switchSink) OnRunCompleted(state string, ok bool) {
	s.mu.Lock()
	s.terminal = state
	s.terminalOK = ok
	s.mu.Unlock()
}
func (s *switchSink) OnRunFailed(reason, _ string) {
	s.mu.Lock()
	s.failure = reason
	s.mu.Unlock()
}
func (s *switchSink) OnStepEntered(string, string, int)                  {}
func (s *switchSink) OnStepOutcome(string, string, time.Duration, error) {}
func (s *switchSink) OnStepTransition(string, string, string)            {}
func (s *switchSink) OnStepResumed(string, int, string)                  {}
func (s *switchSink) OnVariableSet(string, string, string)               {}
func (s *switchSink) OnStepOutputCaptured(name string, outputs map[string]string) {
	s.mu.Lock()
	if s.outputCaptured == nil {
		s.outputCaptured = make(map[string]map[string]string)
	}
	s.outputCaptured[name] = outputs
	s.mu.Unlock()
}
func (s *switchSink) OnRunPaused(string, string, string)                           {}
func (s *switchSink) OnWaitEntered(string, string, string, string)                 {}
func (s *switchSink) OnWaitResumed(string, string, string, map[string]string)      {}
func (s *switchSink) OnApprovalRequested(string, []string, string)                 {}
func (s *switchSink) OnApprovalDecision(string, string, string, map[string]string) {}
func (s *switchSink) OnBranchEvaluated(node, matchedArm, target, condition string) {
	s.mu.Lock()
	s.branchEvents = append(s.branchEvents, branchEvent{node: node, matchedArm: matchedArm, target: target, condition: condition})
	s.mu.Unlock()
}
func (s *switchSink) OnForEachEntered(string, int)                      {}
func (s *switchSink) OnStepIterationStarted(string, int, string, bool)  {}
func (s *switchSink) OnStepIterationCompleted(string, string, string)   {}
func (s *switchSink) OnStepIterationItem(string, int, string)           {}
func (s *switchSink) OnScopeIterCursorSet(string)                       {}
func (s *switchSink) OnAdapterLifecycle(string, string, string, string) {}
func (s *switchSink) OnRunOutputs([]map[string]string)                  {}
func (s *switchSink) OnStepOutcomeDefaulted(string, string, string)     {}
func (s *switchSink) OnStepOutcomeUnknown(string, string)               {}
func (s *switchSink) StepEventSink(string) adapter.EventSink            { return noopAdapterSink{} }

// --- Unit tests for switchNode.Evaluate ---

// TestSwitch_FirstMatchWins verifies that the first matching condition wins.
func TestSwitch_FirstMatchWins(t *testing.T) {
	src := `
workflow "t" {
  version       = "0.1"
  initial_state = "decide"
  target_state  = "done"
}

variable "env" {
  type    = "string"
  default = "staging"
}

switch "decide" {
  condition {
    match = var.env == "prod"
    next  = state.deploy
  }
  condition {
    match = var.env == "staging"
    next  = state.staging_deploy
  }
  default {
    next = state.skip
  }
}

state "deploy"         { terminal = true }
state "staging_deploy" { terminal = true }
state "skip"           { terminal = true }
state "done"           { terminal = true }
`
	spec, diags := workflow.Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	vars := workflow.SeedVarsFromGraph(g)

	sink := &switchSink{}
	eng := engine.New(g, adapterhost.NewLoader(), sink,
		engine.WithResumedVars(vars))
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(sink.branchEvents) != 1 {
		t.Fatalf("expected 1 branch event, got %d", len(sink.branchEvents))
	}
	ev := sink.branchEvents[0]
	if ev.node != "decide" {
		t.Errorf("event.node = %q, want \"decide\"", ev.node)
	}
	if ev.matchedArm != "condition[1]" {
		t.Errorf("event.matchedArm = %q, want \"condition[1]\"", ev.matchedArm)
	}
	if ev.target != "staging_deploy" {
		t.Errorf("event.target = %q, want \"staging_deploy\"", ev.target)
	}
	if ev.condition == "" {
		t.Error("event.condition should be non-empty for a matched condition")
	}
	if !strings.Contains(ev.condition, "staging") {
		t.Errorf("event.condition = %q, expected it to contain \"staging\"", ev.condition)
	}
	if sink.terminal != "staging_deploy" {
		t.Errorf("terminal = %q, want \"staging_deploy\"", sink.terminal)
	}
}

// TestSwitch_NoMatchFallsToDefault verifies that when no condition matches,
// the default branch is taken.
func TestSwitch_NoMatchFallsToDefault(t *testing.T) {
	src := `
workflow "t" {
  version       = "0.1"
  initial_state = "decide"
  target_state  = "done"
}

variable "env" {
  type    = "string"
  default = "dev"
}

switch "decide" {
  condition {
    match = var.env == "prod"
    next  = state.deploy
  }
  condition {
    match = var.env == "staging"
    next  = state.staging_deploy
  }
  default {
    next = state.done
  }
}

state "deploy"         { terminal = true }
state "staging_deploy" { terminal = true }
state "done"           { terminal = true }
`
	spec, diags := workflow.Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	vars := workflow.SeedVarsFromGraph(g)

	sink := &switchSink{}
	eng := engine.New(g, adapterhost.NewLoader(), sink, engine.WithResumedVars(vars))
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(sink.branchEvents) != 1 {
		t.Fatalf("expected 1 branch event, got %d", len(sink.branchEvents))
	}
	ev := sink.branchEvents[0]
	if ev.matchedArm != "default" {
		t.Errorf("matchedArm = %q, want \"default\"", ev.matchedArm)
	}
	if ev.target != "done" {
		t.Errorf("target = %q, want \"done\"", ev.target)
	}
	if ev.condition != "" {
		t.Errorf("condition = %q, want empty string for default arm", ev.condition)
	}
	if sink.terminal != "done" {
		t.Errorf("terminal = %q, want \"done\"", sink.terminal)
	}
}

// TestSwitch_NonBoolConditionErrors verifies that a non-bool match expression
// results in a run failure.
func TestSwitch_NonBoolConditionErrors(t *testing.T) {
	src := `
workflow "t" {
  version       = "0.1"
  initial_state = "decide"
  target_state  = "done"
}

variable "env" {
  type    = "string"
  default = "dev"
}

switch "decide" {
  condition {
    match = var.env
    next  = state.done
  }
  default {
    next = state.done
  }
}

state "done" { terminal = true }
`
	spec, diags := workflow.Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	vars := workflow.SeedVarsFromGraph(g)

	sink := &switchSink{}
	eng := engine.New(g, adapterhost.NewLoader(), sink, engine.WithResumedVars(vars))
	err := eng.Run(context.Background())
	if err == nil {
		t.Fatal("expected error for non-bool switch condition, got nil")
	}
	if sink.failure == "" {
		t.Error("expected OnRunFailed to be called")
	}
}

// TestSwitch_OutputProjection_AppliedBeforeNext verifies that output projection
// from a switch condition is available to the immediately following node.
//
// The test wires two switches in sequence: "decide" projects
// { tier = "production" } when env == "prod", then routes to "check_tier".
// "check_tier" reads steps.decide.tier in its match expression; if projection
// happened correctly, the condition matches and routes to "tier_ok". If
// projection were missing or delayed, "check_tier" would fall through to its
// default branch ("tier_fail") and the terminal assertion would catch the bug.
func TestSwitch_OutputProjection_AppliedBeforeNext(t *testing.T) {
	src := `
workflow "t" {
  version       = "0.1"
  initial_state = "decide"
  target_state  = "tier_ok"
}

variable "env" {
  type    = "string"
  default = "prod"
}

switch "decide" {
  condition {
    match  = var.env == "prod"
    next   = switch.check_tier
    output = { tier = "production" }
  }
  default {
    next = state.tier_fail
  }
}

switch "check_tier" {
  condition {
    match = steps.decide.tier == "production"
    next  = state.tier_ok
  }
  default {
    next = state.tier_fail
  }
}

state "tier_ok"   { terminal = true }
state "tier_fail" {
  terminal = true
  success  = false
}
`
	spec, diags := workflow.Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	vars := workflow.SeedVarsFromGraph(g)

	sink := &switchSink{}
	eng := engine.New(g, adapterhost.NewLoader(), sink, engine.WithResumedVars(vars))
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	// If output projection worked, check_tier routed to tier_ok.
	// If projection was missing, check_tier would route to tier_fail.
	if sink.terminal != "tier_ok" {
		t.Errorf("terminal = %q, want \"tier_ok\"; output projection may not have fired before check_tier evaluated", sink.terminal)
	}
	if !sink.terminalOK {
		t.Error("terminalOK = false; check_tier routed to tier_fail, meaning steps.decide.tier was not visible")
	}
	// Verify capture event fired.
	outputs, ok := sink.outputCaptured["decide"]
	if !ok {
		t.Fatal("expected OnStepOutputCaptured for \"decide\"")
	}
	if got := outputs["tier"]; got != "production" {
		t.Errorf("steps.decide.tier = %q, want \"production\"", got)
	}
	if len(sink.branchEvents) != 2 {
		t.Errorf("expected 2 branch events (decide + check_tier), got %d", len(sink.branchEvents))
	}
}

// TestSwitch_ReturnFromCondition_BubblesToCaller verifies that next = "return"
// in a switch condition causes the run to complete at the top level with
// empty terminal state and success = true.
func TestSwitch_ReturnFromCondition_BubblesToCaller(t *testing.T) {
	src := `
workflow "t" {
  version       = "0.1"
  initial_state = "decide"
  target_state  = "done"
}

switch "decide" {
  condition {
    match = true
    next  = "return"
  }
  default {
    next = state.done
  }
}

state "done" { terminal = true }
`
	spec, diags := workflow.Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	vars := workflow.SeedVarsFromGraph(g)

	sink := &switchSink{}
	eng := engine.New(g, adapterhost.NewLoader(), sink, engine.WithResumedVars(vars))
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	// "return" from a top-level workflow completes with empty terminal and success=true.
	if sink.terminal != "" {
		t.Errorf("terminal = %q, want \"\" (return from top-level)", sink.terminal)
	}
	if !sink.terminalOK {
		t.Error("terminalOK = false, want true for successful return")
	}
}

// TestSwitch_EndToEnd exercises a switch choosing between two terminal states.
func TestSwitch_EndToEnd_StepOutputSwitch(t *testing.T) {
	src := `
workflow "t" {
  version       = "0.1"
  initial_state = "decide"
  target_state  = "succeeded"
}

variable "result" {
  type    = "string"
  default = "pass"
}

switch "decide" {
  condition {
    match = var.result == "pass"
    next  = state.succeeded
  }
  condition {
    match = var.result == "fail"
    next  = state.failed
  }
  default {
    next = state.failed
  }
}

state "succeeded" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}
`
	spec, diags := workflow.Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	vars := workflow.SeedVarsFromGraph(g)

	sink := &switchSink{}
	eng := engine.New(g, adapterhost.NewLoader(), sink, engine.WithResumedVars(vars))
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "succeeded" {
		t.Errorf("terminal = %q, want \"succeeded\"", sink.terminal)
	}
	if !sink.terminalOK {
		t.Error("expected success=true")
	}
	if len(sink.branchEvents) == 0 {
		t.Error("expected at least one BranchEvaluated event")
	}
}

// TestSwitch_EndToEnd_ReturnExitsWorkflow is an end-to-end test verifying that
// a switch with next = "return" terminates the full workflow execution cleanly.
//
// This validates the complete contract boundary: parse → compile → engine run
// → OnRunCompleted, with the return sentinel propagating through the engine's
// handleReturnExit path correctly.
func TestSwitch_EndToEnd_ReturnExitsWorkflow(t *testing.T) {
	src := `
workflow "t" {
  version       = "0.1"
  initial_state = "gate"
  target_state  = "done"
}

variable "early_exit" {
  type    = "string"
  default = "yes"
}

switch "gate" {
  condition {
    match = var.early_exit == "yes"
    next  = "return"
  }
  default {
    next = state.done
  }
}

state "done" { terminal = true }
`
	spec, diags := workflow.Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	vars := workflow.SeedVarsFromGraph(g)

	sink := &switchSink{}
	eng := engine.New(g, adapterhost.NewLoader(), sink, engine.WithResumedVars(vars))
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	// A top-level return must: complete with empty terminal state and success=true,
	// fire OnBranchEvaluated for "gate", and never reach "done".
	if sink.terminal != "" {
		t.Errorf("terminal = %q, want \"\" (early return should not reach done)", sink.terminal)
	}
	if !sink.terminalOK {
		t.Error("terminalOK = false, want true for successful early return")
	}
	if sink.failure != "" {
		t.Errorf("failure = %q, want empty (return is not a failure)", sink.failure)
	}
	if len(sink.branchEvents) != 1 {
		t.Fatalf("expected 1 branch event for gate, got %d", len(sink.branchEvents))
	}
	ev := sink.branchEvents[0]
	if ev.node != "gate" {
		t.Errorf("branch event node = %q, want \"gate\"", ev.node)
	}
	if ev.target != "return" {
		t.Errorf("branch event target = %q, want \"return\"", ev.target)
	}
	if ev.matchedArm != "condition[0]" {
		t.Errorf("branch event matchedArm = %q, want \"condition[0]\"", ev.matchedArm)
	}
}
