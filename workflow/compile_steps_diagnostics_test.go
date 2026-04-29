package workflow

import (
	"strings"
	"testing"
)

// Test 6.6: A copilot-backed step with system_prompt in the input block fails
// compile with the targeted diagnostic naming the agent config block as the fix.
func TestStepInputMisplacedCopilotAgentField(t *testing.T) {
	src := `
workflow "x" {
  version       = "0.1"
  initial_state = "open"
  target_state  = "done"
  agent "bot" {
    adapter = "copilot"
  }
  step "open" {
    agent     = "bot"
    lifecycle = "open"
    outcome "success" { transition_to = "run" }
  }
  step "run" {
    agent = "bot"
    input {
      prompt        = "hello"
      system_prompt = "You are a bot."
    }
    outcome "success" { transition_to = "close" }
  }
  step "close" {
    agent     = "bot"
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
	_, diags = Compile(spec, testSchemas)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for system_prompt in step input")
	}
	msg := diags.Error()
	if !strings.Contains(msg, `"system_prompt"`) {
		t.Errorf("expected field name in diagnostic, got: %s", msg)
	}
	if !strings.Contains(msg, "agent config block") {
		t.Errorf("expected 'agent config block' hint in diagnostic, got: %s", msg)
	}
	if !strings.Contains(msg, `adapter = "copilot"`) {
		t.Errorf("expected copilot adapter in diagnostic, got: %s", msg)
	}
}

// Test 6.7: A step with a different adapter (shell) and an unknown field keeps
// the generic "unknown field" diagnostic — the targeted message is only for
// adapter-known agent-level fields.
func TestStepInputUnknownFieldNonCopilotAdapterKeepsGenericDiagnostic(t *testing.T) {
	src := `
workflow "x" {
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
  step "run" {
    adapter = "shell"
    input {
      command       = "echo hi"
      system_prompt = "not-valid-for-shell"
    }
    outcome "success" { transition_to = "done" }
    outcome "failure" { transition_to = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, testSchemas)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for system_prompt in shell step input")
	}
	msg := diags.Error()
	// Should be the generic message, not the targeted copilot one.
	if strings.Contains(msg, "agent config block") {
		t.Errorf("expected generic diagnostic for shell adapter, not the targeted copilot hint; got: %s", msg)
	}
	if !strings.Contains(msg, `unknown field "system_prompt"`) {
		t.Errorf("expected generic 'unknown field' diagnostic, got: %s", msg)
	}
}

// Additional: reasoning_effort in step input for copilot is valid (it's in InputSchema).
func TestStepInputReasoningEffortAcceptedForCopilot(t *testing.T) {
	src := `
workflow "x" {
  version       = "0.1"
  initial_state = "open"
  target_state  = "done"
  agent "bot" {
    adapter = "copilot"
  }
  step "open" {
    agent     = "bot"
    lifecycle = "open"
    outcome "success" { transition_to = "run" }
  }
  step "run" {
    agent = "bot"
    input {
      prompt           = "hello"
      reasoning_effort = "high"
    }
    outcome "success" { transition_to = "close" }
  }
  step "close" {
    agent     = "bot"
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
	g, diags := Compile(spec, testSchemas)
	if diags.HasErrors() {
		t.Fatalf("expected reasoning_effort in step input to be valid: %s", diags.Error())
	}
	run := g.Steps["run"]
	if run == nil {
		t.Fatal("step 'run' not found")
	}
	if run.Input["reasoning_effort"] != "high" {
		t.Errorf("expected reasoning_effort=high in step input, got %q", run.Input["reasoning_effort"])
	}
}
