package engine_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/engine"
	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/workflowtest"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

func compileHCL(t *testing.T, src string) *workflow.FSMGraph {
	t.Helper()
	spec, diags := workflow.Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s\nHCL:\n%s", diags.Error(), src)
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s\nHCL:\n%s", diags.Error(), src)
	}
	return g
}

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
func (s *switchSink) OnAuditLoss(*pb.AuditLoss) {}
func (s *switchSink) OnStepEntered(string, string, int)                             {}
func (s *switchSink) OnStepOutcome(_, _ string, _ time.Duration, _ error, _ string) {}
func (s *switchSink) OnStepTransition(string, string, string)                       {}
func (s *switchSink) OnStepResumed(string, int, string)                             {}
func (s *switchSink) OnVariableSet(string, string, string)                          {}
func (s *switchSink) OnStepOutputCaptured(name string, outputs map[string]string) {
	s.mu.Lock()
	if s.outputCaptured == nil {
		s.outputCaptured = make(map[string]map[string]string)
	}
	s.outputCaptured[name] = outputs
	s.mu.Unlock()
}
func (s *switchSink) OnRunPaused(string, string, string)                           {}
func (s *switchSink) OnRunResumed(string)                                          {}
func (s *switchSink) OnWaitEntered(string, string, string, string)                 {}
func (s *switchSink) OnWaitResumed(string, string, string, map[string]string)      {}
func (s *switchSink) OnApprovalRequested(string, []string, string)                 {}
func (s *switchSink) OnApprovalDecision(string, string, string, map[string]string) {}
func (s *switchSink) OnBranchEvaluated(node, matchedArm, target, condition string) {
	s.mu.Lock()
	s.branchEvents = append(s.branchEvents, branchEvent{node: node, matchedArm: matchedArm, target: target, condition: condition})
	s.mu.Unlock()
}
func (s *switchSink) OnForEachEntered(string, int)                                {}
func (s *switchSink) OnStepIterationStarted(string, int, string, bool)            {}
func (s *switchSink) OnStepIterationCompleted(string, string, string)             {}
func (s *switchSink) OnStepIterationItem(string, int, string)                     {}
func (s *switchSink) OnScopeIterCursorSet(string)                                 {}
func (s *switchSink) OnAdapterLifecycle(string, string, string, string)           {}
func (s *switchSink) OnAdapterLifecycleEvent(event *engine.AdapterLifecycleEvent) {}
func (s *switchSink) OnRunOutputs([]*pb.RunOutputs_Output)                        {}
func (s *switchSink) OnStepOutcomeDefaulted(string, string, string)               {}
func (s *switchSink) OnStepOutcomeUnknown(string, string)                         {}
func (s *switchSink) OnStepOutcomeInvalid(string, string, []string, int)          {}

func (s *switchSink) OnAgentPromptInjected(string, string, string, string, time.Time) {}
func (s *switchSink) OnCheckpointPointer(*engine.CheckpointPointerEvent)              {}
func (s *switchSink) StepEventSink(string) adapter.EventSink                          { return noopAdapterSink{} }

// --- Unit tests for switchNode.Evaluate ---

// TestSwitch_FirstMatchWins verifies that the first matching condition wins.
func TestSwitch_FirstMatchWins(t *testing.T) {
	src := workflowtest.New().
		WithHeader("t", "0.1", "decide", "deploy").
		WithVariable("env", "string", `"staging"`).
		WithSwitch("decide",
			workflowtest.Match(`var.env == "prod"`, "state.deploy"),
			workflowtest.Match(`var.env == "staging"`, "state.staging_deploy"),
			workflowtest.DefaultNext("state.skip"),
		).
		WithState("deploy", true, nil).
		WithState("staging_deploy", true, nil).
		WithState("skip", true, nil).
		Build()
	g := compileHCL(t, src)
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
	if ev.matchedArm != "match[1]" {
		t.Errorf("event.matchedArm = %q, want \"match[1]\"", ev.matchedArm)
	}
	if ev.target != "staging_deploy" {
		t.Errorf("event.target = %q, want \"staging_deploy\"", ev.target)
	}
	want := `var.env == "staging"`
	if ev.condition != want {
		t.Errorf("event.condition = %q, want %q", ev.condition, want)
	}
	if sink.terminal != "staging_deploy" {
		t.Errorf("terminal = %q, want \"staging_deploy\"", sink.terminal)
	}
}

// TestSwitch_NoMatchFallsToDefault verifies that when no condition matches,
// the default branch is taken.
func TestSwitch_NoMatchFallsToDefault(t *testing.T) {
	src := workflowtest.New().
		WithHeader("t", "0.1", "decide", "done").
		WithVariable("env", "string", `"dev"`).
		WithSwitch("decide",
			workflowtest.Match(`var.env == "prod"`, "state.deploy"),
			workflowtest.Match(`var.env == "staging"`, "state.staging_deploy"),
			workflowtest.DefaultNext("state.done"),
		).
		WithState("deploy", true, nil).
		WithState("staging_deploy", true, nil).
		WithState("done", true, nil).
		Build()
	g := compileHCL(t, src)
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
	src := workflowtest.New().
		WithHeader("t", "0.1", "decide", "done").
		WithVariable("env", "string", `"dev"`).
		WithSwitch("decide",
			workflowtest.Match("var.env", "state.done"),
			workflowtest.DefaultNext("state.done"),
		).
		WithState("done", true, nil).
		Build()
	g := compileHCL(t, src)
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
	falseVal := false
	src := workflowtest.New().
		WithHeader("t", "0.1", "decide", "tier_ok").
		WithVariable("env", "string", `"prod"`).
		WithSwitch("decide",
			workflowtest.MatchWithOutput(`var.env == "prod"`, "switch.check_tier", map[string]string{"tier": `"production"`}),
			workflowtest.DefaultNext("state.tier_fail"),
		).
		WithSwitch("check_tier",
			workflowtest.Match(`steps.decide.tier == "production"`, "state.tier_ok"),
			workflowtest.DefaultNext("state.tier_fail"),
		).
		WithState("tier_ok", true, nil).
		WithState("tier_fail", true, &falseVal).
		Build()
	g := compileHCL(t, src)
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

// TestSwitch_ReturnFromCondition_BubblesToCaller verifies that next = step.return
// in a switch condition causes the run to complete at the top level with
// empty terminal state and success = true.
func TestSwitch_ReturnFromCondition_BubblesToCaller(t *testing.T) {
	src := workflowtest.New().
		WithHeader("t", "0.1", "decide", "done").
		WithSwitch("decide",
			workflowtest.Match("true", "step.return"),
			workflowtest.DefaultNext("state.done"),
		).
		WithState("done", true, nil).
		Build()
	g := compileHCL(t, src)
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
	falseVal := false
	src := workflowtest.New().
		WithHeader("t", "0.1", "decide", "succeeded").
		WithVariable("result", "string", `"pass"`).
		WithSwitch("decide",
			workflowtest.Match(`var.result == "pass"`, "state.succeeded"),
			workflowtest.Match(`var.result == "fail"`, "state.failed"),
			workflowtest.DefaultNext("state.failed"),
		).
		WithState("succeeded", true, nil).
		WithState("failed", true, &falseVal).
		Build()
	g := compileHCL(t, src)
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
// a switch with next = step.return terminates the full workflow execution cleanly.
//
// This validates the complete contract boundary: parse → compile → engine run
// → OnRunCompleted, with the return sentinel propagating through the engine's
// handleReturnExit path correctly.
func TestSwitch_EndToEnd_ReturnExitsWorkflow(t *testing.T) {
	src := workflowtest.New().
		WithHeader("t", "0.1", "gate", "done").
		WithVariable("early_exit", "string", `"yes"`).
		WithSwitch("gate",
			workflowtest.Match(`var.early_exit == "yes"`, "step.return"),
			workflowtest.DefaultNext("state.done"),
		).
		WithState("done", true, nil).
		Build()
	g := compileHCL(t, src)
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
	if ev.matchedArm != "match[0]" {
		t.Errorf("branch event matchedArm = %q, want \"match[0]\"", ev.matchedArm)
	}
}

// writeHCLFile writes content to dir/name.hcl and fails the test on error.
func writeHCLFile(t *testing.T, dir, name, content string) {
	t.Helper()
	workflowtest.WriteFile(t, dir, name+".hcl", content)
}

// TestSwitch_MultiFile_BranchEvaluatedCondition is an end-to-end regression test
// verifying that BranchEvaluated events for a switch declared in a non-first file
// report the actual condition source text, not an unrelated fragment of the merged
// SourceBytes buffer.
func TestSwitch_MultiFile_BranchEvaluatedCondition(t *testing.T) {
	dir := t.TempDir()
	// Keep the switch in a file that sorts after the header so it is parsed from a
	// non-first file; the bug only reproduced in that arrangement.
	writeHCLFile(t, dir, "01_workflow", workflowtest.New().
		WithHeader("multi", "0.1", "route", "done").
		WithVariable("pick", "string", `"b"`).
		WithState("done", true, nil).
		Build(),
	)
	writeHCLFile(t, dir, "99_routing", workflowtest.Switch("route",
		workflowtest.Match(`var.pick == "a"`, "state.done"),
		workflowtest.Match(`var.pick == "b"`, "state.done"),
		workflowtest.DefaultNext("state.done"),
	))

	spec, diags := workflow.ParseDir(dir)
	if diags.HasErrors() {
		t.Fatalf("ParseDir: %s", diags.Error())
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("Compile: %s", diags.Error())
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
	if ev.node != "route" {
		t.Errorf("event.node = %q, want \"route\"", ev.node)
	}
	if ev.matchedArm != "match[1]" {
		t.Errorf("event.matchedArm = %q, want \"match[1]\"", ev.matchedArm)
	}
	if ev.target != "done" {
		t.Errorf("event.target = %q, want \"done\"", ev.target)
	}
	want := `var.pick == "b"`
	if ev.condition != want {
		t.Errorf("event.condition = %q, want %q", ev.condition, want)
	}
}

// TestSwitch_MultiFile_DefaultOmitsCondition verifies that default arms in a
// multi-file workflow continue to omit the condition field.
func TestSwitch_MultiFile_DefaultOmitsCondition(t *testing.T) {
	dir := t.TempDir()
	writeHCLFile(t, dir, "01_workflow", workflowtest.New().
		WithHeader("multi", "0.1", "route", "done").
		WithVariable("pick", "string", `"c"`).
		WithState("done", true, nil).
		Build(),
	)
	writeHCLFile(t, dir, "99_routing", workflowtest.Switch("route",
		workflowtest.Match(`var.pick == "a"`, "state.done"),
		workflowtest.DefaultNext("state.done"),
	))

	spec, diags := workflow.ParseDir(dir)
	if diags.HasErrors() {
		t.Fatalf("ParseDir: %s", diags.Error())
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("Compile: %s", diags.Error())
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
		t.Errorf("event.matchedArm = %q, want \"default\"", ev.matchedArm)
	}
	if ev.condition != "" {
		t.Errorf("event.condition = %q, want empty string for default arm", ev.condition)
	}
}

// compileParentWithSubworkflow writes a child subworkflow into parentDir/child
// and parses/compiles the parent HCL located in parentDir.
func compileParentWithSubworkflow(t *testing.T, parentHCL, parentDir string) *workflow.FSMGraph {
	t.Helper()
	childDir := filepath.Join(parentDir, "child")
	if err := os.Mkdir(childDir, 0o755); err != nil {
		t.Fatalf("create child dir: %v", err)
	}
	childHCL := `
workflow {
  name = "child"
  version       = "0.1"
  initial_state = "done"
  target_state  = "done"
}

output "verdict" {
  value = "reproduced"
}

state "done" {
  terminal = true
  success  = true
}
`
	if err := os.WriteFile(filepath.Join(childDir, "main.hcl"), []byte(childHCL), 0o644); err != nil {
		t.Fatalf("write child main.hcl: %v", err)
	}

	spec, diags := workflow.Parse("parent.hcl", []byte(parentHCL))
	if diags.HasErrors() {
		t.Fatalf("parse parent: %s", diags.Error())
	}
	g, diags := workflow.CompileWithOpts(spec, nil, workflow.CompileOpts{
		WorkflowDir:         parentDir,
		SubWorkflowResolver: &workflow.LocalSubWorkflowResolver{},
	})
	if diags.HasErrors() {
		t.Fatalf("compile parent: %s", diags.Error())
	}
	return g
}

// TestSwitch_ReadsSubworkflowStepWrite verifies that a switch node evaluates its
// match condition against updated data written by a preceding subworkflow step (CRI-37).
func TestSwitch_ReadsSubworkflowStepWrite(t *testing.T) {
	parentHCL := `
workflow {
  name = "cri37_subworkflow"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}

data "internal" "verdict" {
  type  = string
  value = "initial"
}

subworkflow "child" {
  source = "./child"
}

step "run" {
  target = subworkflow.child

  outcome "success" {
    next = switch.route
    write {
      target = data.internal.verdict.value
      value  = subworkflow.verdict
    }
  }
  outcome "failure" {
    next = state.fail
  }
}

switch "route" {
  match {
    condition = data.internal.verdict.value == "reproduced"
    next      = state.done
  }
  default {
    next = state.fail
  }
}

state "done" {
  terminal = true
  success  = true
}
state "fail" {
  terminal = true
  success  = false
}
`
	parentDir := t.TempDir()
	g := compileParentWithSubworkflow(t, parentHCL, parentDir)

	vars := workflow.SeedVarsFromGraph(g)
	sink := &switchSink{}
	eng := engine.New(g, adapterhost.NewLoader(), sink, engine.WithResumedVars(vars))
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if sink.terminal != "done" {
		t.Errorf("terminal = %q, want \"done\"; switch saw stale data and fell through", sink.terminal)
	}
	if !sink.terminalOK {
		t.Errorf("terminalOK = false, want true; switch routed to failure branch")
	}
	if sink.failure != "" {
		t.Errorf("unexpected run failure: %s", sink.failure)
	}
}

// TestSwitch_ReadsAdapterStepWrite verifies that a switch node reads updated data
// written by a preceding adapter step (CRI-37).
func TestSwitch_ReadsAdapterStepWrite(t *testing.T) {
	const src = `
workflow {
  name = "cri37_adapter"
  version       = "0.1"
  initial_state = "set"
  target_state  = "done"
}

data "internal" "verdict" {
  type  = string
  value = "initial"
}

adapter "noop" "default" {}

step "set" {
  target = adapter.noop.default
  outcome "success" {
    next = switch.route
    write {
      target = data.internal.verdict.value
      value  = "reproduced"
    }
  }
  outcome "failure" {
    next = state.fail
  }
}

switch "route" {
  match {
    condition = data.internal.verdict.value == "reproduced"
    next      = state.done
  }
  default {
    next = state.fail
  }
}

state "done" {
  terminal = true
  success  = true
}
state "fail" {
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

	loader := &fakeSwitchLoader{
		adapters: map[string]adapterhost.Handle{
			"noop":         &fakeSwitchAdapter{name: "noop", outcome: "success"},
			"noop.default": &fakeSwitchAdapter{name: "noop", outcome: "success"},
		},
	}
	vars := workflow.SeedVarsFromGraph(g)
	sink := &switchSink{}
	eng := engine.New(g, loader, sink, engine.WithResumedVars(vars))
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if sink.terminal != "done" {
		t.Errorf("terminal = %q, want \"done\"; switch saw stale data and fell through", sink.terminal)
	}
	if !sink.terminalOK {
		t.Errorf("terminalOK = false, want true; switch routed to failure branch")
	}
	if sink.failure != "" {
		t.Errorf("unexpected run failure: %s", sink.failure)
	}
}

type fakeSwitchLoader struct {
	adapters map[string]adapterhost.Handle
}

func (l *fakeSwitchLoader) Resolve(_ context.Context, name string) (adapterhost.Handle, error) {
	if h, ok := l.adapters[name]; ok {
		return h, nil
	}
	return nil, fmt.Errorf("adapter %q not found", name)
}

func (l *fakeSwitchLoader) Shutdown(_ context.Context) error {
	return nil
}

type fakeSwitchAdapter struct {
	name    string
	outcome string
}

func (a *fakeSwitchAdapter) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: a.name, Version: "test"}, nil
}

func (a *fakeSwitchAdapter) OpenSession(context.Context, string, map[string]string, map[string]string) error {
	return nil
}

func (a *fakeSwitchAdapter) Execute(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink, _ *v2.ExecutionRejection) (adapter.Result, error) {
	return adapter.Result{Outcome: a.outcome}, nil
}

func (a *fakeSwitchAdapter) Permit(context.Context, string, string, bool, string) error { return nil }
func (a *fakeSwitchAdapter) CloseSession(context.Context, string) error                 { return nil }
func (a *fakeSwitchAdapter) Kill()                                                      {}
func (a *fakeSwitchAdapter) Pause(context.Context, string) error                        { return nil }
func (a *fakeSwitchAdapter) Resume(context.Context, string) error                       { return nil }
func (a *fakeSwitchAdapter) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}
func (a *fakeSwitchAdapter) Snapshot(context.Context, string) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{}, nil
}
func (a *fakeSwitchAdapter) Restore(context.Context, string, []byte, uint32) error { return nil }
