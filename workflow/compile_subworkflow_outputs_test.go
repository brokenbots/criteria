package workflow

import (
	"strings"
	"testing"
)

// calleeWithStatusOutput is a subworkflow body that declares a single "status"
// output, used to verify cross-step references against a subworkflow's declared
// outputs.
const calleeWithStatusOutput = `
workflow {
  name          = "inner"
  version       = "1"
  initial_state = "done"
  target_state  = "done"
}
output "status" {
  type  = string
  value = "ok"
}
state "done" {
  terminal = true
  success  = true
}
`

// parentReferencingSubworkflowOutput builds a parent whose second step's output
// projection references steps.call.<field>, where "call" is a subworkflow step.
func parentReferencingSubworkflowOutput(field string) string {
	return `
workflow {
  name          = "parent"
  version       = "1"
  initial_state = "call"
  target_state  = "done"
}

adapter "noop" "default" {}

subworkflow "inner_task" { source = "./inner" }

step "call" {
  target = subworkflow.inner_task
  outcome "success" { next = step.use }
}

step "use" {
  target = adapter.noop.default
  outcome "success" {
    next   = step.done
    output = { x = steps.call.` + field + ` }
  }
}

state "done" {
  terminal = true
  success  = true
}
`
}

func TestCompile_SubworkflowOutput_KnownField(t *testing.T) {
	tmp := t.TempDir()
	writeSubworkflowDir(t, tmp, "inner", calleeWithStatusOutput)
	_, diags := compileParentSpec(t, parentReferencingSubworkflowOutput("status"), tmp)
	if diags.HasErrors() {
		t.Fatalf("reference to a declared subworkflow output should compile, got: %s", diags.Error())
	}
}

func TestCompile_SubworkflowOutput_UnknownField(t *testing.T) {
	tmp := t.TempDir()
	writeSubworkflowDir(t, tmp, "inner", calleeWithStatusOutput)
	_, diags := compileParentSpec(t, parentReferencingSubworkflowOutput("ghost"), tmp)
	if !diags.HasErrors() {
		t.Fatal("reference to an undeclared subworkflow output should fail to compile, got no error")
	}
	if !strings.Contains(diags.Error(), "ghost") {
		t.Fatalf("expected error mentioning the unknown field %q, got: %s", "ghost", diags.Error())
	}
}
