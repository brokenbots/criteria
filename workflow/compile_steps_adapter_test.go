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
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "open"
  target_state  = "done"
}

adapter "copilot" "default" {}
step "open" {
  target = adapter.copilot.default
  outcome "success" { next = step.run }
}
step "run" {
  target = adapter.copilot.default
  input {
    prompt        = "hello"
    system_prompt = "You are a bot."
  }
  outcome "success" { next = step.close }
}
step "close" {
  target = adapter.copilot.default
  outcome "success" { next = step.done }
}
state "done" { terminal = true }
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
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}

adapter "exec" "default" {}
step "run" {
  target = adapter.exec.default
  input {
    command       = "echo hi"
    system_prompt = "not-valid-for-shell"
  }
  outcome "success" { next = step.done }
  outcome "failure" { next = step.done }
}
state "done" { terminal = true }
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
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "open"
  target_state  = "done"
}

adapter "copilot" "default" {}
step "open" {
  target = adapter.copilot.default
  outcome "success" { next = step.run }
}
step "run" {
  target = adapter.copilot.default
  input {
    prompt           = "hello"
    reasoning_effort = "high"
  }
  outcome "success" { next = step.close }
}
step "close" {
  target = adapter.copilot.default
  outcome "success" { next = step.done }
}
state "done" { terminal = true }
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
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "open"
  target_state  = "done"
}

adapter "copilot" "default" {}
step "open" {
  target = adapter.copilot.default
  outcome "success" { next = step.run }
}
step "run" {
  target = adapter.copilot.default
  allow_tools = ["read_file", "write_file"]
  input {
    prompt = "hello"
  }
  outcome "success" { next = step.close }
}
step "close" {
  target = adapter.copilot.default
  outcome "success" { next = step.done }
}
state "done" { terminal = true }
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
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "open"
  target_state  = "done"
}

adapter "copilot" "default" {}
step "open" {
  target = adapter.copilot.default
  outcome "success" { next = step.run }
}
step "run" {
  target = adapter.copilot.default
  allow_tools = ["read", "write"]
  input {
    prompt = "hello"
  }
  outcome "success" { next = step.close }
}
step "close" {
  target = adapter.copilot.default
  outcome "success" { next = step.done }
}
state "done" { terminal = true }
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

// TestStepSecretInput_Compiles verifies that a step with a secret_input block
// compiles correctly and the expressions are stored in the node.
func TestStepSecretInput_Compiles(t *testing.T) {
	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}

variable "api_key" {
  type    = string
  secret  = true
}

adapter "exec" "default" {}

step "run" {
  target = adapter.exec.default
  input {
    command = "echo hi"
  }
  secret_input {
    api_key = var.api_key
  }
  outcome "success" { next = step.done }
}
state "done" { terminal = true }
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	// Use nil schemas to skip input validation so secret_input fields are accepted.
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("expected secret_input to compile without error, got: %s", diags.Error())
	}
	step := g.Steps["run"]
	if step == nil {
		t.Fatal("step 'run' not found")
	}
	if len(step.SecretInputs) != 1 || step.SecretInputs["api_key"] != "" {
		t.Errorf("expected SecretInputs to contain api_key, got %v", step.SecretInputs)
	}
	if len(step.SecretInputExprs) != 1 || step.SecretInputExprs["api_key"] == nil {
		t.Errorf("expected SecretInputExprs to contain api_key expression, got %v", step.SecretInputExprs)
	}
}

// TestAllowToolsInvalidGlobWarning verifies that an allow_tools entry with an
// unclosed character class emits a compile-time warning pointing to the pattern
// syntax documentation.
func TestAllowToolsInvalidGlobWarning(t *testing.T) {
	schemas := map[string]AdapterInfo{
		"claude-agent": {
			InputSchema:       map[string]ConfigField{"prompt": {Required: true, Type: ConfigFieldString}},
			Permissions:       []string{"Read", "Bash"},
			PermissionAliases: map[string]string{},
		},
	}
	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}

adapter "claude-agent" "default" {}
step "run" {
  target = adapter.claude-agent.default
  allow_tools = ["Bash[unclosed"]
  input { prompt = "hello" }
  outcome "success" { next = step.done }
}
state "done" { terminal = true }
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, schemas)
	if diags.HasErrors() {
		t.Fatalf("invalid glob must be a warning, not an error: %s", diags.Error())
	}
	found := false
	for _, d := range diags {
		if d.Severity == hcl.DiagWarning && strings.Contains(d.Summary, "not a valid glob pattern") {
			found = true
			if !strings.Contains(d.Detail, "pattern-matching") {
				t.Errorf("expected pattern syntax doc reference in detail, got: %s", d.Detail)
			}
		}
	}
	if !found {
		t.Errorf("expected invalid-glob warning, got: %s", diags.Error())
	}
}

// TestAllowToolsWrongScopingFormWarning verifies that an allow_tools entry using
// the Claude Code "Tool(...)" argument-scoping form emits a compile-time warning.
func TestAllowToolsWrongScopingFormWarning(t *testing.T) {
	schemas := map[string]AdapterInfo{
		"claude-agent": {
			InputSchema:       map[string]ConfigField{"prompt": {Required: true, Type: ConfigFieldString}},
			Permissions:       []string{"Read", "Bash"},
			PermissionAliases: map[string]string{},
		},
	}
	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}

adapter "claude-agent" "default" {}
step "run" {
  target = adapter.claude-agent.default
  allow_tools = ["Bash(git log:*)"]
  input { prompt = "hello" }
  outcome "success" { next = step.done }
}
state "done" { terminal = true }
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, schemas)
	if diags.HasErrors() {
		t.Fatalf("wrong scoping form must be a warning, not an error: %s", diags.Error())
	}
	found := false
	for _, d := range diags {
		if d.Severity == hcl.DiagWarning && strings.Contains(d.Summary, "argument-scoping form") {
			found = true
			if !strings.Contains(d.Detail, "colon form") || !strings.Contains(d.Detail, "pattern-matching") {
				t.Errorf("expected colon-form hint and pattern syntax doc, got: %s", d.Detail)
			}
		}
	}
	if !found {
		t.Errorf("expected wrong-scoping-form warning, got: %s", diags.Error())
	}
}

// TestAllowToolsUnknownToolWarning verifies that an allow_tools entry naming a
// tool not in the adapter's declared permissions vocabulary emits a warning.
func TestAllowToolsUnknownToolWarning(t *testing.T) {
	schemas := map[string]AdapterInfo{
		"claude-agent": {
			InputSchema:       map[string]ConfigField{"prompt": {Required: true, Type: ConfigFieldString}},
			Permissions:       []string{"Read"},
			PermissionAliases: map[string]string{},
		},
	}
	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}

adapter "claude-agent" "default" {}
step "run" {
  target = adapter.claude-agent.default
  allow_tools = ["Bash:git *"]
  input { prompt = "hello" }
  outcome "success" { next = step.done }
}
state "done" { terminal = true }
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, schemas)
	if diags.HasErrors() {
		t.Fatalf("unknown tool must be a warning, not an error: %s", diags.Error())
	}
	found := false
	for _, d := range diags {
		if d.Severity == hcl.DiagWarning && strings.Contains(d.Summary, "not declared in the adapter's permissions vocabulary") {
			found = true
			if !strings.Contains(d.Summary, "Bash") {
				t.Errorf("expected summary to mention tool name Bash, got: %s", d.Summary)
			}
			if !strings.Contains(d.Detail, "Read") {
				t.Errorf("expected detail to list declared permissions, got: %s", d.Detail)
			}
		}
	}
	if !found {
		t.Errorf("expected unknown-tool warning, got: %s", diags.Error())
	}
}

// TestAllowToolsAliasWarningGeneralized verifies that alias warnings are emitted
// for any adapter that declares PermissionAliases, not only the hard-coded copilot
// adapter.
func TestAllowToolsAliasWarningGeneralized(t *testing.T) {
	schemas := map[string]AdapterInfo{
		"my-agent": {
			InputSchema:       map[string]ConfigField{"prompt": {Required: true, Type: ConfigFieldString}},
			Permissions:       []string{"fetch", "edit"},
			PermissionAliases: map[string]string{"get_file": "fetch", "patch_file": "edit"},
		},
	}
	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}

adapter "my-agent" "default" {}
step "run" {
  target = adapter.my-agent.default
  allow_tools = ["get_file"]
  input { prompt = "hello" }
  outcome "success" { next = step.done }
}
state "done" { terminal = true }
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, schemas)
	if diags.HasErrors() {
		t.Fatalf("alias must be accepted without error: %s", diags.Error())
	}
	found := false
	for _, d := range diags {
		if d.Severity == hcl.DiagWarning && strings.Contains(d.Summary, "get_file") && strings.Contains(d.Summary, "fetch") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected alias warning for non-copilot adapter, got: %s", diags.Error())
	}
}

// TestAllowToolsNoVocabularyNoUnknownWarning verifies that when an adapter does
// not declare a permissions vocabulary, allow_tools entries are not flagged as
// unknown tools.
func TestAllowToolsNoVocabularyNoUnknownWarning(t *testing.T) {
	schemas := map[string]AdapterInfo{
		"permissive": {
			InputSchema: map[string]ConfigField{"prompt": {Required: true, Type: ConfigFieldString}},
			// Permissions left empty.
		},
	}
	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}

adapter "permissive" "default" {}
step "run" {
  target = adapter.permissive.default
  allow_tools = ["AnythingGoes"]
  input { prompt = "hello" }
  outcome "success" { next = step.done }
}
state "done" { terminal = true }
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, schemas)
	if diags.HasErrors() {
		t.Fatalf("permissive adapter must not error: %s", diags.Error())
	}
	for _, d := range diags {
		if d.Severity == hcl.DiagWarning && strings.Contains(d.Summary, "permissions vocabulary") {
			t.Errorf("unexpected vocabulary warning for adapter with empty permissions: %s", d.Summary)
		}
	}
}
