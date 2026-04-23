package workflow

import (
	"strings"
	"testing"
)

const validHCL = `
workflow "build_and_test" {
  version       = "0.1"
  initial_state = "build"
  target_state  = "verified"

  step "build" {
    adapter = "shell"
    config = {
      command = "echo build"
    }
    timeout = "30s"

    outcome "success" { transition_to = "test" }
    outcome "failure" { transition_to = "failed" }
  }

  step "test" {
    adapter = "shell"
    config = {
      command = "echo test"
    }

    outcome "success" { transition_to = "verified" }
    outcome "failure" { transition_to = "failed" }
  }

  state "verified" { terminal = true }
  state "failed" {
    terminal = true
    success  = false
  }

  policy {
    max_total_steps  = 10
    max_step_retries = 2
  }
}
`

func TestParseAndCompileValid(t *testing.T) {
	spec, diags := Parse("test.hcl", []byte(validHCL))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	if g.Name != "build_and_test" {
		t.Errorf("name: %s", g.Name)
	}
	if g.InitialState != "build" || g.TargetState != "verified" {
		t.Errorf("initial/target: %s/%s", g.InitialState, g.TargetState)
	}
	if len(g.Steps) != 2 || len(g.States) != 2 {
		t.Errorf("counts: %d steps, %d states", len(g.Steps), len(g.States))
	}
	if !g.IsTerminal("verified") || !g.IsTerminal("failed") {
		t.Errorf("expected terminals")
	}
	if g.Policy.MaxTotalSteps != 10 || g.Policy.MaxStepRetries != 2 {
		t.Errorf("policy: %+v", g.Policy)
	}
}

func TestCompileDanglingTransition(t *testing.T) {
	src := `
workflow "x" {
  version = "0.1"
  initial_state = "a"
  target_state  = "done"
  step "a" {
    adapter = "shell"
    outcome "success" { transition_to = "missing" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec)
	if !diags.HasErrors() {
		t.Fatal("expected error for dangling transition")
	}
	if !strings.Contains(diags.Error(), `unknown target "missing"`) {
		t.Errorf("unexpected error: %s", diags.Error())
	}
}

func TestCompileNonTerminalTarget(t *testing.T) {
	src := `
workflow "x" {
  version = "0.1"
  initial_state = "a"
  target_state  = "halfway"
  step "a" {
    adapter = "shell"
    outcome "success" { transition_to = "halfway" }
  }
  state "halfway" {}
}
`
	spec, _ := Parse("t.hcl", []byte(src))
	_, diags := Compile(spec)
	if !diags.HasErrors() {
		t.Fatal("expected error for non-terminal target")
	}
}

func TestCompileUnreachableStep(t *testing.T) {
	src := `
workflow "x" {
  version = "0.1"
  initial_state = "a"
  target_state  = "done"
  step "a" {
    adapter = "shell"
    outcome "success" { transition_to = "done" }
  }
  step "orphan" {
    adapter = "shell"
    outcome "success" { transition_to = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, _ := Parse("t.hcl", []byte(src))
	_, diags := Compile(spec)
	if !diags.HasErrors() {
		t.Fatal("expected error for unreachable step")
	}
}

func TestCompileMissingOutcome(t *testing.T) {
	src := `
workflow "x" {
  version = "0.1"
  initial_state = "a"
  target_state  = "done"
  step "a" {
    adapter = "shell"
  }
  state "done" { terminal = true }
}
`
	spec, _ := Parse("t.hcl", []byte(src))
	_, diags := Compile(spec)
	if !diags.HasErrors() {
		t.Fatal("expected error for missing outcomes")
	}
}
