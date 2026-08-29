package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

type criteriaVersionSink struct {
	fakeSink
	stepOutcomes []string
	stepErrs     []error
}

func (s *criteriaVersionSink) OnStepOutcome(step, outcome string, _ time.Duration, err error) {
	s.stepOutcomes = append(s.stepOutcomes, step+":"+outcome)
	s.stepErrs = append(s.stepErrs, err)
}

// TestEngineCriteriaVersion_RuntimeRejectsIncompatibleRoot verifies that an
// engine running an incompatible version fails before any step executes, even
// when the workflow was compiled against a compatible version.
func TestEngineCriteriaVersion_RuntimeRejectsIncompatibleRoot(t *testing.T) {
	t.Setenv("CRITERIA_OVERRIDE_VERSION", "0.5.9")
	g := compile(t, `
workflow {
  name             = "root"
  version          = "1"
  criteria_version = ">=0.5.9"
  initial_state    = "run"
  target_state     = "done"
}

step "run" {
  target = adapter.fake
  outcome "success" { next = state.done }
}

state "done" {
  terminal = true
  success  = true
}
`)

	sink := &fakeSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake": &fakeAdapter{name: "fake", outcome: "success"},
	}}

	// Switch to an incompatible engine version before execution.
	t.Setenv("CRITERIA_OVERRIDE_VERSION", "0.5.8")
	err := NewTestEngine(g, loader, sink).Run(context.Background())
	if err == nil {
		t.Fatal("expected criteria-version error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, `workflow "root" requires Criteria >=0.5.9`) {
		t.Errorf("missing workflow requirement: %v", msg)
	}
	if !strings.Contains(msg, "running engine is v0.5.8") {
		t.Errorf("missing running engine version: %v", msg)
	}
	if len(sink.stepsRun) != 0 {
		t.Errorf("expected no steps to run, got %v", sink.stepsRun)
	}
}

// TestEngineCriteriaVersion_RuntimeRejectsIncompatibleSubworkflow verifies that
// the engine checks the callee's criteria_version before initializing its
// adapters, and reports the parent->child chain.
func TestEngineCriteriaVersion_RuntimeRejectsIncompatibleSubworkflow(t *testing.T) {
	t.Setenv("CRITERIA_OVERRIDE_VERSION", "0.5.9")

	child := &workflow.FSMGraph{
		Name:            "child",
		InitialState:    "run",
		TargetState:     "done",
		CriteriaVersion: ">=0.5.9",
		WorkflowDir:     t.TempDir(),
		Policy:          workflow.DefaultPolicy,
		Steps: map[string]*workflow.StepNode{
			"run": {
				Name:       "run",
				TargetKind: workflow.StepTargetAdapter,
				AdapterRef: "fake.default",
				Outcomes:   map[string]*workflow.CompiledOutcome{"success": {Next: "done"}},
			},
		},
		States: map[string]*workflow.StateNode{
			"done": {Name: "done", Terminal: true, Success: true},
		},
		Adapters: map[string]*workflow.AdapterNode{
			"fake.default": {Type: "fake", Name: "default"},
		},
	}

	swNode := &workflow.SubworkflowNode{
		Name:       "child",
		SourcePath: child.WorkflowDir,
		Body:       child,
		BodyEntry:  "run",
	}

	parent := &workflow.FSMGraph{
		Name:         "parent",
		InitialState: "call",
		TargetState:  "done",
		WorkflowDir:  t.TempDir(),
		Policy:       workflow.DefaultPolicy,
		Steps: map[string]*workflow.StepNode{
			"call": {
				Name:           "call",
				TargetKind:     workflow.StepTargetSubworkflow,
				SubworkflowRef: "child",
				Outcomes:       map[string]*workflow.CompiledOutcome{"success": {Next: "done"}},
			},
		},
		States: map[string]*workflow.StateNode{
			"done": {Name: "done", Terminal: true, Success: true},
		},
		Subworkflows: map[string]*workflow.SubworkflowNode{"child": swNode},
	}

	sink := &criteriaVersionSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake": &fakeAdapter{name: "fake", outcome: "success"},
	}}

	// Run under an engine version that satisfies the parent compile-time check
	// but not the child runtime check.
	t.Setenv("CRITERIA_OVERRIDE_VERSION", "0.5.8")
	err := NewTestEngine(parent, loader, sink).Run(context.Background())
	// The incompatibility is surfaced as a step-level failure on "call" before
	// any child adapter is initialized.
	if err == nil {
		t.Fatal("expected run to fail after subworkflow step failure, got nil")
	}

	var found bool
	var criteriaErr string
	for _, e := range sink.stepErrs {
		if e == nil {
			continue
		}
		msg := e.Error()
		if strings.Contains(msg, `subworkflow "child" requires Criteria >=0.5.9`) {
			found = true
			criteriaErr = msg
			break
		}
	}
	if !found {
		t.Fatalf("expected subworkflow criteria-version error in step errors, got stepOutcomes=%v err=%v", sink.stepOutcomes, err)
	}
	if !strings.Contains(criteriaErr, "running engine is v0.5.8") {
		t.Errorf("missing running engine version: %v", criteriaErr)
	}
	if !strings.Contains(criteriaErr, "required by parent -> child") {
		t.Errorf("missing source chain: %v", criteriaErr)
	}
	if len(sink.stepsRun) != 1 || sink.stepsRun[0] != "call" {
		t.Errorf("expected only parent step to be entered, got %v", sink.stepsRun)
	}
}

// TestEngineCriteriaVersion_ResumeRechecksConstraint verifies that resuming a
// persisted run re-evaluates the original workflow constraint against the
// engine that resumes it.
func TestEngineCriteriaVersion_ResumeRechecksConstraint(t *testing.T) {
	t.Setenv("CRITERIA_OVERRIDE_VERSION", "0.5.8")
	g := compile(t, `
workflow {
  name             = "resume_check"
  version          = "1"
  criteria_version = ">=0.5.8"
  initial_state    = "loop"
  target_state     = "done"

  policy { max_total_steps = 1 }
}

step "loop" {
  target = adapter.fake
  max_visits = 5
  outcome "again" { next = step.loop }
  outcome "done"  { next = state.done }
}

state "done" {
  terminal = true
  success  = true
}
`)

	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake": &fakeAdapter{name: "fake", outcome: "again"},
	}}

	sink1 := &fakeSink{}
	eng1 := NewTestEngine(g, loader, sink1)
	err := eng1.Run(context.Background())
	if err == nil {
		t.Fatal("expected max_total_steps error from first run")
	}
	if !strings.Contains(err.Error(), "max_total_steps") {
		t.Fatalf("unexpected first-run error: %v", err)
	}
	visits := eng1.VisitCounts()

	// Resume under an incompatible engine version.
	t.Setenv("CRITERIA_OVERRIDE_VERSION", "0.5.7")
	sink2 := &fakeSink{}
	eng2 := New(g, loader, sink2, WithResumedVisits(visits))
	err2 := eng2.RunFrom(context.Background(), "loop", 1)
	if err2 == nil {
		t.Fatal("expected criteria-version error on resume, got nil")
	}
	msg := err2.Error()
	if !strings.Contains(msg, `workflow "resume_check" requires Criteria >=0.5.8`) {
		t.Errorf("missing workflow requirement: %v", msg)
	}
	if !strings.Contains(msg, "running engine is v0.5.7") {
		t.Errorf("missing running engine version: %v", msg)
	}
	if len(sink2.stepsRun) != 0 {
		t.Errorf("expected no resumed steps to run, got %v", sink2.stepsRun)
	}
}
