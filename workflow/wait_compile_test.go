package workflow_test

import (
	"strings"
	"testing"

	"github.com/brokenbots/criteria/workflow"
)

func TestCompile_WaitDurationOnly(t *testing.T) {
	src := []byte(`
workflow {
  name = "w"
  version       = "0.1"
  initial_state = "pause"
  target_state  = "done"
}

wait "pause" {
  duration = "2s"
  outcome "elapsed" { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}
`)
	spec, diags := workflow.Parse("test.hcl", src)
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	w, ok := g.Waits["pause"]
	if !ok {
		t.Fatal("wait node 'pause' missing from compiled graph")
	}
	if w.Duration == 0 {
		t.Error("expected non-zero duration")
	}
	if w.Signal != "" {
		t.Error("signal should be empty for duration-only wait")
	}
	if len(w.Outcomes) == 0 {
		t.Error("expected at least one outcome")
	}
}

func TestCompile_WaitSignalOnly(t *testing.T) {
	src := []byte(`
workflow {
  name = "w"
  version       = "0.1"
  initial_state = "gating"
  target_state  = "done"
}

wait "gating" {
  signal = "approve"
  outcome "approved" { next = "done" }
  outcome "rejected" { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}
`)
	spec, diags := workflow.Parse("test.hcl", src)
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	w, ok := g.Waits["gating"]
	if !ok {
		t.Fatal("wait node 'gating' missing from compiled graph")
	}
	if w.Signal != "approve" {
		t.Errorf("signal = %q, want 'approve'", w.Signal)
	}
}

func TestCompile_WaitBothDurationAndSignal_Error(t *testing.T) {
	src := []byte(`
workflow {
  name = "w"
  version       = "0.1"
  initial_state = "pause"
  target_state  = "done"
}

wait "pause" {
  duration = "1s"
  signal   = "go"
  outcome "elapsed" { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}
`)
	spec, diags := workflow.Parse("test.hcl", src)
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = workflow.Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for wait with both duration and signal")
	}
}

func TestCompile_WaitNoOutcomes_Error(t *testing.T) {
	src := []byte(`
workflow {
  name = "w"
  version       = "0.1"
  initial_state = "pause"
  target_state  = "done"
}

wait "pause" {
  signal = "go"
}

state "done" {
  terminal = true
  success  = true
}
`)
	spec, diags := workflow.Parse("test.hcl", src)
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = workflow.Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for wait with no outcomes")
	}
}

func TestCompile_ApprovalRequiresApprovedAndRejected(t *testing.T) {
	src := []byte(`
workflow {
  name = "w"
  version       = "0.1"
  initial_state = "check"
  target_state  = "done"
}

approval "check" {
  approvers = ["alice"]
  reason    = "LGTM?"
  outcome "approved"  { next = "done" }
  outcome "rejected"  { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}
`)
	spec, diags := workflow.Parse("test.hcl", src)
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	a, ok := g.Approvals["check"]
	if !ok {
		t.Fatal("approval node 'check' missing from compiled graph")
	}
	if _, ok := a.Outcomes["approved"]; !ok {
		t.Error("approval missing 'approved' outcome")
	}
	if _, ok := a.Outcomes["rejected"]; !ok {
		t.Error("approval missing 'rejected' outcome")
	}
}

func TestCompile_UnreachableWaitErrors(t *testing.T) {
	src := []byte(`
workflow {
  name = "w"
  version       = "0.1"
  initial_state = "start"
  target_state  = "done"
}

adapter "noop" "default" {}

step "start" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
}

wait "orphan" {
  signal = "go"
  outcome "received" { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}
`)
	spec, diags := workflow.Parse("test.hcl", src)
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = workflow.Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for unreachable wait, got none")
	}
	found := false
	for _, d := range diags {
		if strings.Contains(d.Summary, "unreachable") && strings.Contains(d.Summary, "orphan") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected unreachability error for 'orphan' wait; got: %v", diags)
	}
}

func TestCompile_UnreachableApprovalErrors(t *testing.T) {
	src := []byte(`
workflow {
  name = "w"
  version       = "0.1"
  initial_state = "start"
  target_state  = "done"
}

adapter "noop" "default" {}

step "start" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
}

approval "orphan" {
  approvers = ["alice"]
  reason    = "LGTM?"
  outcome "approved" { next = "done" }
  outcome "rejected" { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}
`)
	spec, diags := workflow.Parse("test.hcl", src)
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = workflow.Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for unreachable approval, got none")
	}
	found := false
	for _, d := range diags {
		if strings.Contains(d.Summary, "unreachable") && strings.Contains(d.Summary, "orphan") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected unreachability error for 'orphan' approval; got: %v", diags)
	}
}

func TestCompile_WaitDuplicateOutcome_Error(t *testing.T) {
	src := []byte(`
workflow {
  name = "w"
  version       = "0.1"
  initial_state = "pause"
  target_state  = "done"
}

wait "pause" {
  signal  = "go"
  outcome "received" { next = "done" }
  outcome "received" { next = "done" }
}

state "done" { terminal = true }
`)
	spec, diags := workflow.Parse("test.hcl", src)
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = workflow.Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for duplicate wait outcome, got none")
	}
	if !strings.Contains(diags.Error(), "duplicate outcome") {
		t.Errorf("expected 'duplicate outcome' in error; got: %v", diags)
	}
}

func TestCompile_ApprovalDuplicateOutcome_Error(t *testing.T) {
	src := []byte(`
workflow {
  name = "w"
  version       = "0.1"
  initial_state = "gate"
  target_state  = "done"
}

approval "gate" {
  approvers = ["alice"]
  reason    = "LGTM?"
  outcome "approved" { next = "done" }
  outcome "approved" { next = "done" }
  outcome "rejected" { next = "done" }
}

state "done" { terminal = true }
`)
	spec, diags := workflow.Parse("test.hcl", src)
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = workflow.Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for duplicate approval outcome, got none")
	}
	if !strings.Contains(diags.Error(), "duplicate outcome") {
		t.Errorf("expected 'duplicate outcome' in error; got: %v", diags)
	}
}

func TestCompile_ApprovalMissingRejected_Error(t *testing.T) {
	src := []byte(`
workflow {
  name = "w"
  version       = "0.1"
  initial_state = "check"
  target_state  = "done"
}

approval "check" {
  approvers = ["alice"]
  reason    = "LGTM?"
  outcome "approved" { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}
`)
	spec, diags := workflow.Parse("test.hcl", src)
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = workflow.Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for approval missing 'rejected' outcome")
	}
}
