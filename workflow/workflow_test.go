package workflow

import (
	"strings"
	"testing"
)

const validHCL = `
workflow "build_and_test" {
  adapter "shell" "default" {}
  version       = "0.1"
  initial_state = "build"
  target_state  = "verified"

  step "build" {
    adapter = adapter.shell.default
    input {
      command = "echo build"
    }
    timeout = "30s"

    outcome "success" { transition_to = "test" }
    outcome "failure" { transition_to = "failed" }
  }

  step "test" {
    adapter = adapter.shell.default
    input {
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
  adapter "shell" "default" {}
  version = "0.1"
  initial_state = "a"
  target_state  = "done"
  step "a" {
    adapter = adapter.shell.default
    outcome "success" { transition_to = "missing" }
  }
  state "done" { terminal = true }
}
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
  adapter "shell" "default" {}
  version = "0.1"
  initial_state = "a"
  target_state  = "halfway"
  step "a" {
    adapter = adapter.shell.default
    outcome "success" { transition_to = "halfway" }
  }
  state "halfway" {}
}
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
  adapter "shell" "default" {}
  version = "0.1"
  initial_state = "a"
  target_state  = "done"
  step "a" {
    adapter = adapter.shell.default
    outcome "success" { transition_to = "done" }
  }
  step "orphan" {
    adapter = adapter.shell.default
    outcome "success" { transition_to = "done" }
  }
  state "done" { terminal = true }
}
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
  adapter "shell" "default" {}
  version = "0.1"
  initial_state = "a"
  target_state  = "done"
  step "a" {
    adapter = adapter.shell.default
  }
  state "done" { terminal = true }
}
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
  adapter "copilot" "default" {}
  version       = "0.1"
  initial_state = "open"
  target_state  = "done"

  step "open" {
    adapter = adapter.copilot.default
    lifecycle   = "open"
    allow_tools = ["read_file"]
    outcome "success" { transition_to = "done" }
  }
  step "close" {
    adapter = adapter.copilot.default
    lifecycle = "close"
    outcome "success" { transition_to = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for allow_tools on lifecycle step")
	}
	if !strings.Contains(diags.Error(), "allow_tools") {
		t.Errorf("expected error mentioning allow_tools, got: %s", diags.Error())
	}
}

func TestCompileAllowToolsWithoutAgentIsError(t *testing.T) {
	// TestCompileAllowToolsWithoutAgentIsError verifies that using allow_tools on a
	// bare adapter reference (not declared) produces an error about the bare type.
	// (The allow_tools validation is now at runtime, not compile time.)
	src := `
workflow "x" {
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"

  step "run" {
    adapter     = adapter.shell.default
    allow_tools = ["shell:git status"]
    outcome "success" { transition_to = "done" }
  }
  state "done" { terminal = true }
}
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
  adapter "copilot" "default" {}
  version       = "0.1"
  initial_state = "open"
  target_state  = "done"

  step "open" {
    adapter = adapter.copilot.default
    lifecycle = "open"
    outcome "success" { transition_to = "run" }
  }
  step "run" {
    adapter = adapter.copilot.default
    allow_tools = ["read_file"]
    outcome "success" { transition_to = "close" }
  }
  step "close" {
    adapter = adapter.copilot.default
    lifecycle = "close"
    outcome "success" { transition_to = "done" }
  }
  state "done" { terminal = true }

  permissions {
    allow_tools = ["shell:echo *"]
  }
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
	// Lifecycle steps must not get AllowTools (even from workflow-level)
	for _, name := range []string{"open", "close"} {
		step := g.Steps[name]
		if step == nil {
			continue
		}
		if len(step.AllowTools) != 0 {
			t.Errorf("lifecycle step %q should have empty AllowTools, got %v", name, step.AllowTools)
		}
	}
}
