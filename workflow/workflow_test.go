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

  policy {
    max_total_steps  = 10
    max_step_retries = 2
  }
}

adapter "shell" "default" {}

step "build" {
  target = adapter.shell.default
  input {
    command = "echo build"
  }
  timeout = "30s"

  outcome "success" { next = "test" }
  outcome "failure" { next = "failed" }
}

step "test" {
  target = adapter.shell.default
  input {
    command = "echo test"
  }

  outcome "success" { next = "verified" }
  outcome "failure" { next = "failed" }
}

state "verified" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}
`

func TestParseAndCompileValid(t *testing.T) {
	spec, diags := Parse("test.hcl", []byte(validHCL))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
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
}

adapter "shell" "default" {}
step "a" {
  target = adapter.shell.default
  outcome "success" { next = "missing" }
}
state "done" { terminal = true }
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, nil)
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
}

adapter "shell" "default" {}
step "a" {
  target = adapter.shell.default
  outcome "success" { next = "halfway" }
}
state "halfway" {}
`
	spec, _ := Parse("t.hcl", []byte(src))
	_, diags := Compile(spec, nil)
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
}

adapter "shell" "default" {}
step "a" {
  target = adapter.shell.default
  outcome "success" { next = "done" }
}
step "orphan" {
  target = adapter.shell.default
  outcome "success" { next = "done" }
}
state "done" { terminal = true }
`
	spec, _ := Parse("t.hcl", []byte(src))
	_, diags := Compile(spec, nil)
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
}

adapter "shell" "default" {}
step "a" {
  target = adapter.shell.default
}
state "done" { terminal = true }
`
	spec, _ := Parse("t.hcl", []byte(src))
	_, diags := Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected error for missing outcomes")
	}
}

func TestCompileAllowToolsOnLifecycleStepIsError(t *testing.T) {
	src := `
workflow "x" {
  version       = "0.1"
  initial_state = "open"
  target_state  = "done"
}

adapter "copilot" "default" {}

step "open" {
  target = adapter.copilot.default
  lifecycle   = "open"
  allow_tools = ["read_file"]
  outcome "success" { next = "done" }
}
step "close" {
  target = adapter.copilot.default
  outcome "success" { next = "done" }
}
state "done" { terminal = true }
`
	_, diags := Parse("t.hcl", []byte(src))
	if !diags.HasErrors() {
		t.Fatal("expected parse error for lifecycle attribute")
	}
	if !strings.Contains(diags.Error(), `removed attribute "lifecycle"`) {
		t.Errorf("expected error about lifecycle attribute, got: %s", diags.Error())
	}
}

func TestCompileAllowToolsWithoutAgentIsError(t *testing.T) {
	// TestCompileAllowToolsWithoutAgentIsError verifies that using allow_tools on a
	// step referencing an undeclared adapter produces an error about the undeclared adapter.
	src := `
workflow "x" {
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}

step "run" {
  target      = adapter.shell.default
  allow_tools = ["shell:git status"]
  outcome "success" { next = "done" }
}
state "done" { terminal = true }
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for undefined adapter")
	}
	if !strings.Contains(diags.Error(), `adapter "shell.default" is not declared`) {
		t.Fatalf("expected adapter not declared error, got: %s", diags.Error())
	}
}

func TestCompileAllowToolsUnionedWithWorkflowLevel(t *testing.T) {
	src := `
workflow "x" {
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}

adapter "copilot" "default" {}

step "run" {
  target = adapter.copilot.default
  allow_tools = ["read_file"]
  outcome "success" { next = "done" }
}
state "done" { terminal = true }

permissions {
  allow_tools = ["shell:echo *"]
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	run := g.Steps["run"]
	if run == nil {
		t.Fatal("step 'run' not found")
	}
	// Expect step-level + workflow-level merged
	found := map[string]bool{}
	for _, p := range run.AllowTools {
		found[p] = true
	}
	if !found["read_file"] {
		t.Errorf("AllowTools missing step-level 'read_file': %v", run.AllowTools)
	}
	if !found["shell:echo *"] {
		t.Errorf("AllowTools missing workflow-level 'shell:echo *': %v", run.AllowTools)
	}
}
