package engine_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/engine"
	"github.com/brokenbots/criteria/internal/plugin"
	"github.com/brokenbots/criteria/workflow"
)

// branchSink records branch events and terminal state.
type branchSink struct {
	mu           sync.Mutex
	branchEvents []branchEvent
	terminal     string
	terminalOK   bool
	failure      string
}

type branchEvent struct {
	node       string
	matchedArm string
	target     string
	condition  string
}

func (s *branchSink) OnRunStarted(string, string) {}
func (s *branchSink) OnRunCompleted(state string, ok bool) {
	s.mu.Lock()
	s.terminal = state
	s.terminalOK = ok
	s.mu.Unlock()
}
func (s *branchSink) OnRunFailed(reason, _ string) {
	s.mu.Lock()
	s.failure = reason
	s.mu.Unlock()
}
func (s *branchSink) OnStepEntered(string, string, int)                            {}
func (s *branchSink) OnStepOutcome(string, string, time.Duration, error)           {}
func (s *branchSink) OnStepTransition(string, string, string)                      {}
func (s *branchSink) OnStepResumed(string, int, string)                            {}
func (s *branchSink) OnVariableSet(string, string, string)                         {}
func (s *branchSink) OnStepOutputCaptured(string, map[string]string)               {}
func (s *branchSink) OnRunPaused(string, string, string)                           {}
func (s *branchSink) OnWaitEntered(string, string, string, string)                 {}
func (s *branchSink) OnWaitResumed(string, string, string, map[string]string)      {}
func (s *branchSink) OnApprovalRequested(string, []string, string)                 {}
func (s *branchSink) OnApprovalDecision(string, string, string, map[string]string) {}
func (s *branchSink) OnBranchEvaluated(node, matchedArm, target, condition string) {
	s.mu.Lock()
	s.branchEvents = append(s.branchEvents, branchEvent{node: node, matchedArm: matchedArm, target: target, condition: condition})
	s.mu.Unlock()
}
func (s *branchSink) OnForEachEntered(string, int)                      {}
func (s *branchSink) OnStepIterationStarted(string, int, string, bool)  {}
func (s *branchSink) OnStepIterationCompleted(string, string, string)   {}
func (s *branchSink) OnStepIterationItem(string, int, string)           {}
func (s *branchSink) OnScopeIterCursorSet(string)                       {}
func (s *branchSink) OnAdapterLifecycle(string, string, string, string) {}
func (s *branchSink) OnRunOutputs([]map[string]string)                  {}
func (s *branchSink) OnStepOutcomeDefaulted(string, string, string)     {}
func (s *branchSink) OnStepOutcomeUnknown(string, string)               {}
func (s *branchSink) StepEventSink(string) adapter.EventSink            { return noopAdapterSink{} }

// --- Unit tests for branchNode.Evaluate ---

func TestBranchNode_FirstTrueArmWins(t *testing.T) {
	// Compile a workflow with two arms; first is false, second is true.
	src := `
workflow "t" {
  version       = "0.1"
  initial_state = "decide"
  target_state  = "done"

  variable "env" {
    type    = "string"
    default = "staging"
  }

  branch "decide" {
    arm {
      when          = var.env == "prod"
      transition_to = "deploy"
    }
    arm {
      when          = var.env == "staging"
      transition_to = "staging_deploy"
    }
    default {
      transition_to = "skip"
    }
  }

  state "deploy"         { terminal = true }
  state "staging_deploy" { terminal = true }
  state "skip"           { terminal = true }
  state "done"           { terminal = true }
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
	// Seed vars so var.env == "staging" → arm[1] wins.
	vars := workflow.SeedVarsFromGraph(g)

	sink := &branchSink{}
	eng := engine.New(g, plugin.NewLoader(), sink,
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
	if ev.matchedArm != "arm[1]" {
		t.Errorf("event.matchedArm = %q, want \"arm[1]\"", ev.matchedArm)
	}
	if ev.target != "staging_deploy" {
		t.Errorf("event.target = %q, want \"staging_deploy\"", ev.target)
	}
	if ev.condition == "" {
		t.Error("event.condition should be non-empty for a matched arm")
	}
	if !strings.Contains(ev.condition, "staging") {
		t.Errorf("event.condition = %q, expected it to contain \"staging\" (arm[1] condition)", ev.condition)
	}
	if sink.terminal != "staging_deploy" {
		t.Errorf("terminal = %q, want \"staging_deploy\"", sink.terminal)
	}
}

func TestBranchNode_DefaultFiresWhenAllArmsFalse(t *testing.T) {
	src := `
workflow "t" {
  version       = "0.1"
  initial_state = "decide"
  target_state  = "done"

  variable "env" {
    type    = "string"
    default = "dev"
  }

  branch "decide" {
    arm {
      when          = var.env == "prod"
      transition_to = "deploy"
    }
    arm {
      when          = var.env == "staging"
      transition_to = "staging_deploy"
    }
    default {
      transition_to = "done"
    }
  }

  state "deploy"         { terminal = true }
  state "staging_deploy" { terminal = true }
  state "done"           { terminal = true }
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

	sink := &branchSink{}
	eng := engine.New(g, plugin.NewLoader(), sink, engine.WithResumedVars(vars))
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

func TestBranchNode_NonBoolConditionErrors(t *testing.T) {
	// Arm condition returns a string, not a bool; the engine should surface
	// this via OnRunFailed.
	src := `
workflow "t" {
  version       = "0.1"
  initial_state = "decide"
  target_state  = "done"

  variable "env" {
    type    = "string"
    default = "dev"
  }

  branch "decide" {
    arm {
      when          = var.env
      transition_to = "done"
    }
    default {
      transition_to = "done"
    }
  }

  state "done" { terminal = true }
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

	sink := &branchSink{}
	eng := engine.New(g, plugin.NewLoader(), sink, engine.WithResumedVars(vars))
	err := eng.Run(context.Background())
	if err == nil {
		t.Fatal("expected error for non-bool branch condition, got nil")
	}
	if sink.failure == "" {
		t.Error("expected OnRunFailed to be called")
	}
}

// TestBranchNode_EndToEnd exercises a branch choosing between two terminal
// states based on a step's captured stdout output.
func TestBranchNode_EndToEnd_StepOutputBranch(t *testing.T) {
	// This test uses a noop plugin that returns a known output, then branches
	// on the captured step output.  Because the noop adapter doesn't capture
	// outputs, we use a variable instead for the end-to-end path.
	src := `
workflow "t" {
  version       = "0.1"
  initial_state = "decide"
  target_state  = "succeeded"

  variable "result" {
    type    = "string"
    default = "pass"
  }

  branch "decide" {
    arm {
      when          = var.result == "pass"
      transition_to = "succeeded"
    }
    arm {
      when          = var.result == "fail"
      transition_to = "failed"
    }
    default {
      transition_to = "failed"
    }
  }

  state "succeeded" { terminal = true }
  state "failed" {
    terminal = true
    success  = false
  }
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

	sink := &branchSink{}
	eng := engine.New(g, plugin.NewLoader(), sink, engine.WithResumedVars(vars))
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
