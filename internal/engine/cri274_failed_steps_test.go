package engine

// cri274_failed_steps_test.go — CRI-274: run completion semantics.
//
// A run that reached a success terminal state after a required step failed
// (execution error, adapter-reported failure outcome, or a subworkflow
// failure terminal) was reported with success=true, masking partial
// failures of the delivery contract (bookkeeping, teardown) as successes.
// The orchestrator therefore recorded runCompleted{success:true} for runs
// whose finalization steps all failed (observed across the CRI-271/272/273
// crash series, e.g. teardown_worktree → comment_handler_done →
// set_done_state all failing while the run completed success=true).
//
// These tests pin the fix: the terminal success bit must be the declared
// terminal success AND the absence of any failed step. Failure-arm routing
// alone (a switch choosing a branch over stale data, CRI-37) must NOT taint
// the run — only an actual failed step execution does.

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// scriptedAdapter returns pre-programmed results in execution order: each
// Execute consumes the next entry; a nil error plus outcome "success" is the
// default for exhausted scripts.
type scriptedAdapter struct {
	name     string
	results  []adapter.Result
	errs     []error
	executed int
}

func (p *scriptedAdapter) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: p.name, Version: "test"}, nil
}

func (p *scriptedAdapter) OpenSession(context.Context, string, map[string]string, map[string]string) error {
	return nil
}

func (p *scriptedAdapter) Execute(context.Context, string, *workflow.StepNode, adapter.EventSink) (adapter.Result, error) {
	i := p.executed
	p.executed++
	if i < len(p.errs) && p.errs[i] != nil {
		return adapter.Result{}, p.errs[i]
	}
	if i < len(p.results) {
		return p.results[i], nil
	}
	return adapter.Result{Outcome: "success"}, nil
}

func (p *scriptedAdapter) Permit(context.Context, string, string, bool, string) error { return nil }
func (p *scriptedAdapter) CloseSession(context.Context, string) error                 { return nil }
func (p *scriptedAdapter) Kill()                                                      {}
func (p *scriptedAdapter) Pause(context.Context, string) error                        { return nil }
func (p *scriptedAdapter) Resume(context.Context, string) error                       { return nil }
func (p *scriptedAdapter) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}
func (p *scriptedAdapter) Snapshot(context.Context, string) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{}, nil
}
func (p *scriptedAdapter) Restore(context.Context, string, []byte, uint32) error { return nil }

// TestEngineFailedStepMarksRunFailed reproduces the CRI-274 evidence cascade:
// the work step succeeds, then a required bookkeeping step (the teardown
// analog) errors with a declared failure outcome whose arm routes to a
// SUCCESS terminal. Before the fix the run completed success=true; the
// failed step must taint the run's completion instead.
func TestEngineFailedStepMarksRunFailed(t *testing.T) {
	g := compile(t, `
workflow {
  name = "t"
  version = "0.1"
  initial_state = "work"
  target_state  = "done"
}
step "work" {
  target = adapter.fake
  outcome "success" { next = step.teardown }
}
step "teardown" {
  target = adapter.fake
  outcome "success" { next = state.done }
  outcome "failure" { next = state.done }
}
state "done" {
  terminal = true
  success  = true
}`)
	sink := &fakeSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake": &scriptedAdapter{
			name: "fake",
			errs: []error{nil, fmt.Errorf("gRPC client transport closed (adapter or shim closed the connection)")},
		},
	}}
	if err := NewTestEngine(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "done" {
		t.Fatalf("terminal state: %s, want done", sink.terminal)
	}
	if sink.terminalOK {
		t.Errorf("RunCompleted.success = true after a failed teardown step, want false (CRI-274)")
	}
}

// TestEngineAdapterReportedFailureOutcomeMarksRunFailed covers the no-error
// path: the adapter reports its failure outcome cleanly and the failure arm
// routes to a success terminal. The run must still complete as failed.
func TestEngineAdapterReportedFailureOutcomeMarksRunFailed(t *testing.T) {
	g := compile(t, `
workflow {
  name = "t"
  version = "0.1"
  initial_state = "work"
  target_state  = "done"
}
step "work" {
  target = adapter.fake
  outcome "success" { next = state.done }
  outcome "failure" { next = state.done }
}
state "done" {
  terminal = true
  success  = true
}`)
	sink := &fakeSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake": &scriptedAdapter{name: "fake", results: []adapter.Result{{Outcome: "failure"}}},
	}}
	if err := NewTestEngine(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "done" {
		t.Fatalf("terminal state: %s, want done", sink.terminal)
	}
	if sink.terminalOK {
		t.Errorf("RunCompleted.success = true after an adapter-reported failure outcome, want false (CRI-274)")
	}
}

// TestEngineSubworkflowFailureTerminalMarksRunFailed covers the nested case:
// the callee reaches its failure terminal, the parent routes the subworkflow
// step's failure outcome to a success terminal. "Some subworkflow finished"
// is not success — the run must complete as failed.
func TestEngineSubworkflowFailureTerminalMarksRunFailed(t *testing.T) {
	root := t.TempDir()
	childDir := filepath.Join(root, "child")
	writeFile(t, filepath.Join(root, "parent.hcl"), `
workflow {
  name = "cri274-parent"
  version = "0.1"
  initial_state = "run"
  target_state  = "done"
}

subworkflow "child" {
  source = "./child"
}

step "run" {
  target = subworkflow.child
  outcome "success" { next = state.done }
  outcome "failure" { next = state.done }
}

state "done" {
  terminal = true
  success  = true
}`)
	writeFile(t, filepath.Join(childDir, "child.hcl"), `
workflow {
  name = "cri274-child"
  version = "0.1"
  initial_state = "work"
  target_state  = "done"
}

adapter "fake" "default" {}

step "work" {
  target = adapter.fake.default
  outcome "success" { next = step.teardown }
  outcome "failure" { next = state.fail }
}
step "teardown" {
  target = adapter.fake.default
  outcome "success" { next = state.done }
  outcome "failure" { next = state.fail }
}
state "done" {
  terminal = true
  success  = true
}
state "fail" {
  terminal = true
  success  = false
}`)
	g := compileWorkflowDir(t, root)

	sink := &fakeSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake": &scriptedAdapter{
			name: "fake",
			errs: []error{nil, fmt.Errorf("session closed mid-run")},
		},
	}}
	eng := NewTestEngine(g, loader, sink)
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "done" {
		t.Fatalf("terminal state: %s, want done", sink.terminal)
	}
	if sink.terminalOK {
		t.Errorf("RunCompleted.success = true after a subworkflow ended in its failure terminal, want false (CRI-274)")
	}
}

// TestEngineFailedIterationMarksRunFailed pins the for_each/count path: a
// step whose iteration fails (any_failed aggregate) taints the run even when
// the aggregate arm routes to a success terminal.
func TestEngineFailedIterationMarksRunFailed(t *testing.T) {
	g := compile(t, `
workflow {
  name = "t"
  version = "0.1"
  initial_state = "work"
  target_state  = "done"
}
step "work" {
  target = adapter.fake
  count = 2
  outcome "all_succeeded" { next = state.done }
  outcome "any_failed" { next = state.done }
}
state "done" {
  terminal = true
  success  = true
}`)
	sink := &fakeSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake": &scriptedAdapter{
			name: "fake",
			results: []adapter.Result{
				{Outcome: "success"},
				{Outcome: "failure"},
			},
		},
	}}
	if err := NewTestEngine(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "done" {
		t.Fatalf("terminal state: %s, want done", sink.terminal)
	}
	if sink.terminalOK {
		t.Errorf("RunCompleted.success = true after a failed iteration, want false (CRI-274)")
	}
}

// TestEngineCleanRunStaysSuccess guards against over-counting: declaring a
// failure outcome (or routing a switch through a failure-named state) without
// any step actually failing must keep the run successful — the CRI-37
// failure-branch routing contract.
func TestEngineCleanRunStaysSuccess(t *testing.T) {
	g := compile(t, `
workflow {
  name = "t"
  version = "0.1"
  initial_state = "work"
  target_state  = "done"
}
step "work" {
  target = adapter.fake
  outcome "success" { next = step.teardown }
  outcome "failure" { next = state.fail }
}
step "teardown" {
  target = adapter.fake
  outcome "success" { next = state.done }
  outcome "failure" { next = state.fail }
}
state "done" {
  terminal = true
  success  = true
}
state "fail" {
  terminal = true
  success  = false
}`)
	sink := &fakeSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake": &scriptedAdapter{name: "fake", results: []adapter.Result{{Outcome: "success"}, {Outcome: "success"}}},
	}}
	if err := NewTestEngine(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Fatalf("terminal: %s ok=%v, want done/true", sink.terminal, sink.terminalOK)
	}
}