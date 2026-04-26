package workflow

import (
	"testing"

	"github.com/zclconf/go-cty/cty"
)

// interpolWorkflow declares a variable and a step that references it.
const interpolWorkflow = `
workflow "interpolate" {
  version       = "0.1"
  initial_state = "clone"
  target_state  = "__done__"

  variable "repo" {
    type    = "string"
    default = "overlord"
  }
  step "clone" {
    adapter = "shell"
    input {
      command = "echo ${var.repo}"
    }
    outcome "success" { transition_to = "__done__" }
    outcome "failure" { transition_to = "__done__" }
  }
  state "__done__" { terminal = true }
}
`

// stepOutputWorkflow uses a step output in a subsequent step's input.
const stepOutputWorkflow = `
workflow "step_outputs" {
  version       = "0.1"
  initial_state = "build"
  target_state  = "__done__"

  step "build" {
    adapter = "shell"
    input {
      command = "echo building"
    }
    outcome "success" { transition_to = "publish" }
  }
  step "publish" {
    adapter = "shell"
    input {
      command = "echo ${steps.build.stdout}"
    }
    outcome "success" { transition_to = "__done__" }
  }
  state "__done__" { terminal = true }
}
`

func TestInputInterpolation_VarReference(t *testing.T) {
	spec, diags := Parse("test.hcl", []byte(interpolWorkflow))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags)
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags)
	}

	cloneStep, ok := g.Steps["clone"]
	if !ok {
		t.Fatal("missing 'clone' step")
	}
	if len(cloneStep.InputExprs) == 0 {
		t.Fatal("InputExprs not populated for 'clone' step")
	}

	// Build vars from graph defaults.
	vars := SeedVarsFromGraph(g)
	resolved, err := ResolveInputExprs(cloneStep.InputExprs, vars)
	if err != nil {
		t.Fatalf("ResolveInputExprs: %v", err)
	}
	cmd := resolved["command"]
	if cmd != "echo overlord" {
		t.Errorf("resolved command = %q, want 'echo overlord'", cmd)
	}
}

func TestInputInterpolation_StepOutputReference(t *testing.T) {
	spec, diags := Parse("test.hcl", []byte(stepOutputWorkflow))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags)
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags)
	}

	publishStep, ok := g.Steps["publish"]
	if !ok {
		t.Fatal("missing 'publish' step")
	}
	if len(publishStep.InputExprs) == 0 {
		t.Fatal("InputExprs not populated for 'publish' step")
	}

	// Seed vars then inject fake build output.
	vars := SeedVarsFromGraph(g)
	vars = WithStepOutputs(vars, "build", map[string]string{"stdout": "artifact.tar.gz\n"})

	resolved, err := ResolveInputExprs(publishStep.InputExprs, vars)
	if err != nil {
		t.Fatalf("ResolveInputExprs: %v", err)
	}
	cmd := resolved["command"]
	if cmd != "echo artifact.tar.gz\n" {
		t.Errorf("resolved command = %q, want 'echo artifact.tar.gz\\n'", cmd)
	}
}

func TestInputInterpolation_ResolveInputExprs_Empty(t *testing.T) {
	result, err := ResolveInputExprs(nil, map[string]cty.Value{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result for empty exprs, got %v", result)
	}
}
