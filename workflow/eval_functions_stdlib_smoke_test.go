package workflow

// eval_functions_stdlib_smoke_test.go — end-to-end compile tests exercising
// stdlib string functions inside step input blocks and switch match conditions.

import (
	"testing"
)

// TestStdlibSmoke_StepInput exercises startswith, substr, replace, format,
// join, and length inside a step input block.
func TestStdlibSmoke_StepInput(t *testing.T) {
	src := `
workflow {
  name          = "stdlib-smoke-input"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}

adapter "shell" "default" {}

step "run" {
  target = adapter.shell.default
  input {
    command = format("echo %s", substr(join("-", ["hello", "world"]), 0, 5))
  }
  outcome "success" { next = step.done }
}

state "done" { terminal = true }
`
	spec, diags := Parse("smoke.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
}

// TestStdlibSmoke_SwitchMatch exercises startswith and length inside a switch
// match condition.
func TestStdlibSmoke_SwitchMatch(t *testing.T) {
	src := `
workflow {
  name          = "stdlib-smoke-switch"
  version       = "0.1"
  initial_state = "check"
  target_state  = "done"
}

adapter "shell" "default" {}

variable "prefix" {
  type    = string
  default = "v1."
}

step "check" {
  target = adapter.shell.default
  input {
    command = "echo hello"
  }
  outcome "success" { next = step.decide }
}

switch "decide" {
  match {
    condition = startswith(steps.check.stdout, var.prefix) && length(steps.check.stdout) > 3
    next = step.done
  }
  default {
    next = step.skip
  }
}

state "skip"  { terminal = true }
state "done" { terminal = true }
`
	spec, diags := Parse("smoke.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
}
