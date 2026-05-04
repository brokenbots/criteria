package workflow

import (
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
)

// Test 6.6: A copilot-backed step with system_prompt in the input block fails
// compile with the targeted diagnostic naming the agent config block as the fix.
func TestStepInputMisplacedCopilotAgentField(t *testing.T) {
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
    input {
      prompt        = "hello"
      system_prompt = "You are a bot."
    }
    outcome "success" { transition_to = "close" }
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
	_, diags = Compile(spec, testSchemas)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for system_prompt in step input")
	}
	msg := diags.Error()
	if !strings.Contains(msg, `"system_prompt"`) {
		t.Errorf("expected field name in diagnostic, got: %s", msg)
	}
	if !strings.Contains(msg, "adapter config block") {
		t.Errorf("expected 'adapter config block' hint in diagnostic, got: %s", msg)
	}
	if !strings.Contains(msg, `adapter "copilot"`) {
		t.Errorf("expected copilot adapter in diagnostic, got: %s", msg)
	}
}

// Test 6.7: A step with a different adapter (shell) and an unknown field keeps
// the generic "unknown field" diagnostic — the targeted message is only for
// adapter-known agent-level fields.
func TestStepInputUnknownFieldNonCopilotAdapterKeepsGenericDiagnostic(t *testing.T) {
	src := `
workflow "x" {
  adapter "shell" "default" {}
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
  step "run" {
    adapter = adapter.shell.default
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
    input {
      prompt           = "hello"
      reasoning_effort = "high"
    }
    outcome "success" { transition_to = "close" }
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

// TestCopilotAllowToolsAliasWarning verifies that using a Copilot alias like
// "read_file" in allow_tools emits a compile-time warning pointing to the
// canonical SDK kind "read". The alias must still be accepted (no error).
func TestCopilotAllowToolsAliasWarning(t *testing.T) {
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
    allow_tools = ["read_file", "write_file"]
    input {
      prompt = "hello"
    }
    outcome "success" { transition_to = "close" }
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
	g, diags := Compile(spec, testSchemas)
	if diags.HasErrors() {
		t.Fatalf("alias in allow_tools must not cause error: %s", diags.Error())
	}
	// Both aliases should produce warnings.
	if !diags.HasErrors() {
		var warnMsgs []string
		for _, d := range diags {
			if d.Severity == hcl.DiagWarning {
				warnMsgs = append(warnMsgs, d.Summary)
			}
		}
		if len(warnMsgs) < 2 {
			t.Errorf("expected at least 2 alias warnings, got %d: %v", len(warnMsgs), warnMsgs)
		}
		foundRead := false
		foundWrite := false
		for _, msg := range warnMsgs {
			if strings.Contains(msg, "read_file") && strings.Contains(msg, "\"read\"") {
				foundRead = true
			}
			if strings.Contains(msg, "write_file") && strings.Contains(msg, "\"write\"") {
				foundWrite = true
			}
		}
		if !foundRead {
			t.Errorf("expected warning mentioning read_file → read, got: %v", warnMsgs)
		}
		if !foundWrite {
			t.Errorf("expected warning mentioning write_file → write, got: %v", warnMsgs)
		}
	}
	// AllowTools must be populated correctly regardless of warnings.
	run := g.Steps["run"]
	if run == nil {
		t.Fatal("step 'run' not found")
	}
	if len(run.AllowTools) < 2 {
		t.Errorf("expected 2 allow_tools entries, got %d", len(run.AllowTools))
	}
}

// TestCopilotAllowToolsCanonicalNoWarning verifies that using a canonical
// Copilot SDK kind like "read" does NOT trigger a warning.
func TestCopilotAllowToolsCanonicalNoWarning(t *testing.T) {
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
    allow_tools = ["read", "write"]
    input {
      prompt = "hello"
    }
    outcome "success" { transition_to = "close" }
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
	_, diags = Compile(spec, testSchemas)
	if diags.HasErrors() {
		t.Fatalf("canonical allow_tools must not cause error: %s", diags.Error())
	}
	for _, d := range diags {
		if d.Severity == hcl.DiagWarning && (strings.Contains(d.Summary, "alias") || strings.Contains(d.Summary, "canonical")) {
			t.Errorf("unexpected alias warning for canonical kind: %s", d.Summary)
		}
	}
}
