package workflow_test

import (
	"testing"

	"github.com/brokenbots/criteria/workflow"
)

func TestCompile_WaitDurationOnly(t *testing.T) {
	src := []byte(`
workflow "w" {
  version       = "0.1"
  initial_state = "pause"
  target_state  = "done"

  wait "pause" {
    duration = "2s"
    outcome "elapsed" { transition_to = "done" }
  }

  state "done" {
    terminal = true
    success  = true
  }
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
workflow "w" {
  version       = "0.1"
  initial_state = "gating"
  target_state  = "done"

  wait "gating" {
    signal = "approve"
    outcome "approved" { transition_to = "done" }
    outcome "rejected" { transition_to = "done" }
  }

  state "done" {
    terminal = true
    success  = true
  }
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
workflow "w" {
  version       = "0.1"
  initial_state = "pause"
  target_state  = "done"

  wait "pause" {
    duration = "1s"
    signal   = "go"
    outcome "elapsed" { transition_to = "done" }
  }

  state "done" {
    terminal = true
    success  = true
  }
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
workflow "w" {
  version       = "0.1"
  initial_state = "pause"
  target_state  = "done"

  wait "pause" {
    signal = "go"
  }

  state "done" {
    terminal = true
    success  = true
  }
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
workflow "w" {
  version       = "0.1"
  initial_state = "check"
  target_state  = "done"

  approval "check" {
    approvers = ["alice"]
    reason    = "LGTM?"
    outcome "approved"  { transition_to = "done" }
    outcome "rejected"  { transition_to = "done" }
  }

  state "done" {
    terminal = true
    success  = true
  }
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

func TestCompile_ApprovalMissingRejected_Error(t *testing.T) {
	src := []byte(`
workflow "w" {
  version       = "0.1"
  initial_state = "check"
  target_state  = "done"

  approval "check" {
    approvers = ["alice"]
    reason    = "LGTM?"
    outcome "approved" { transition_to = "done" }
  }

  state "done" {
    terminal = true
    success  = true
  }
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
