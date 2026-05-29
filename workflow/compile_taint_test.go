package workflow

import (
	"testing"
)

// TestTaintPass_SecretVariableInSecretInput verifies that a step referencing a
// secret variable in secret_input is marked Tainted.
func TestTaintPass_SecretVariableInSecretInput(t *testing.T) {
	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}

variable "api_key" {
  type   = string
  secret = true
  default = "key"
}

adapter "noop" "default" {}

step "run" {
  target = adapter.noop.default
  secret_input {
    key = var.api_key
  }
  outcome "success" { next = "done" }
}
state "done" { terminal = true }
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	step := g.Steps["run"]
	if step == nil {
		t.Fatal("step 'run' not found")
	}
	if !step.Tainted {
		t.Error("expected step 'run' to be Tainted")
	}
}

// TestTaintPass_SecretVariableInRegularInput verifies that a step referencing a
// secret variable in its regular input block is marked Tainted.
func TestTaintPass_SecretVariableInRegularInput(t *testing.T) {
	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}

variable "api_key" {
  type   = string
  secret = true
  default = "key"
}

adapter "shell" "default" {}

step "run" {
  target = adapter.shell.default
  input {
    command = var.api_key
  }
  outcome "success" { next = "done" }
}
state "done" { terminal = true }
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, testSchemas)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	step := g.Steps["run"]
	if step == nil {
		t.Fatal("step 'run' not found")
	}
	if !step.Tainted {
		t.Error("expected step 'run' to be Tainted")
	}
}

// TestTaintPass_SharedVariableSecretInInput verifies that a step referencing a
// secret shared_variable in input is marked Tainted.
func TestTaintPass_SharedVariableSecretInInput(t *testing.T) {
	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}

shared_variable "token" {
  type   = string
  secret = true
}

adapter "shell" "default" {}

step "run" {
  target = adapter.shell.default
  input {
    command = shared.token
  }
  outcome "success" { next = "done" }
}
state "done" { terminal = true }
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, testSchemas)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	step := g.Steps["run"]
	if step == nil {
		t.Fatal("step 'run' not found")
	}
	if !step.Tainted {
		t.Error("expected step 'run' to be Tainted")
	}
}

// TestTaintPass_PropagationThroughOutcomes verifies that taint propagates from
// a tainted step to its successor via outcome edges.
func TestTaintPass_PropagationThroughOutcomes(t *testing.T) {
	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "first"
  target_state  = "done"
}

variable "api_key" {
  type   = string
  secret = true
  default = "key"
}

adapter "shell" "default" {}
adapter "noop" "default" {}

step "first" {
  target = adapter.shell.default
  input {
    command = var.api_key
  }
  outcome "success" { next = "second" }
}

step "second" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
}

state "done" { terminal = true }
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, testSchemas)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	if !g.Steps["first"].Tainted {
		t.Error("expected step 'first' to be Tainted")
	}
	if !g.Steps["second"].Tainted {
		t.Error("expected step 'second' to be Tainted (propagated from first)")
	}
}

// TestTaintPass_NonSecretVariableNotTainted verifies that a step referencing a
// non-secret variable is NOT marked Tainted.
func TestTaintPass_NonSecretVariableNotTainted(t *testing.T) {
	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}

variable "greeting" {
  type    = string
  default = "hello"
}

adapter "shell" "default" {}

step "run" {
  target = adapter.shell.default
  input {
    command = var.greeting
  }
  outcome "success" { next = "done" }
}
state "done" { terminal = true }
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, testSchemas)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	step := g.Steps["run"]
	if step == nil {
		t.Fatal("step 'run' not found")
	}
	if step.Tainted {
		t.Error("expected step 'run' NOT to be Tainted")
	}
}

// TestTaintPass_BranchNotTaintedWhenPredecessorClean verifies that a step with
// two predecessors is only tainted if at least one predecessor is tainted.
func TestTaintPass_BranchNotTaintedWhenPredecessorClean(t *testing.T) {
	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "first"
  target_state  = "done"
}

adapter "noop" "default" {}

step "first" {
  target = adapter.noop.default
  outcome "ok"   { next = "second" }
  outcome "skip" { next = "third" }
}

step "second" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
}

step "third" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
}

state "done" { terminal = true }
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	if g.Steps["first"].Tainted {
		t.Error("expected step 'first' NOT to be Tainted")
	}
	if g.Steps["second"].Tainted {
		t.Error("expected step 'second' NOT to be Tainted")
	}
	if g.Steps["third"].Tainted {
		t.Error("expected step 'third' NOT to be Tainted")
	}
}
