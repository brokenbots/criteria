package workflow

import (
	"strings"
	"testing"
)

// selfRefWorkflow builds a single-step workflow whose own outcome contains the
// given write value expression, so tests can probe self-reference handling.
func selfRefWorkflow(writeValue string) string {
	return `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "first"
  target_state  = "done"
}

data "internal" "slot" {
  type = string
}

adapter "exec" "default" {}

step "first" {
  target = adapter.exec.default
  input { command = "echo hi" }
  outcome "success" {
    next = step.done
    write {
      target = data.internal.slot.value
      value  = ` + writeValue + `
    }
  }
}

state "done" { terminal = true }
`
}

func TestCompile_RejectsSelfStepRefInWrite(t *testing.T) {
	_, diags := compileWithSchemas(t, selfRefWorkflow("steps.first.stdout"), nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for a write reading steps.<self>, got none")
	}
	if !strings.Contains(diags.Error(), "own output via steps.first") {
		t.Fatalf("expected self-reference error, got: %s", diags.Error())
	}
}

func TestCompile_AllowsOutputNamespaceInWrite(t *testing.T) {
	// output.<field> is the correct, always-in-scope replacement and must compile.
	_, diags := compileWithSchemas(t, selfRefWorkflow("output.stdout"), nil)
	if diags.HasErrors() {
		t.Fatalf("output.<field> in a write should compile, got: %s", diags.Error())
	}
}

func TestCompile_AllowsCrossStepRefInWrite(t *testing.T) {
	src := `
workflow {
  name          = "t"
  version       = "0.1"
  initial_state = "first"
  target_state  = "done"
}

data "internal" "slot" {
  type = string
}

adapter "exec" "default" {}

step "first" {
  target = adapter.exec.default
  input { command = "echo hi" }
  outcome "success" { next = step.second }
}

step "second" {
  target = adapter.exec.default
  input { command = "echo hi" }
  outcome "success" {
    next = step.done
    write {
      target = data.internal.slot.value
      value  = steps.first.stdout
    }
  }
}

state "done" { terminal = true }
`
	if _, diags := compileWithSchemas(t, src, nil); diags.HasErrors() {
		t.Fatalf("cross-step ref in a write should compile, got: %s", diags.Error())
	}
}
