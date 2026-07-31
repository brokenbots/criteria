package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
)

// TestAdapterConfigFoldsVarRef verifies that a var.* reference in adapter.config
// resolves at compile time without a "Variables not allowed" error.
func TestAdapterConfigFoldsVarRef(t *testing.T) {
	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}
variable "prompt_val" {
  type = string
  default = "You are a helpful assistant."
}
adapter "copilot" "bot" {
  config {
    system_prompt = var.prompt_val
  }
}
step "work" {
  target = adapter.copilot.bot
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
		t.Fatalf("expected var.* in adapter.config to compile without error, got: %s", diags.Error())
	}
	got := g.Adapters["copilot.bot"].Config["system_prompt"]
	if got != "You are a helpful assistant." {
		t.Errorf("system_prompt = %q, want 'You are a helpful assistant.'", got)
	}
}

// TestAdapterConfigFoldsLocalRef verifies that a local.* reference in adapter.config
// resolves at compile time without a "Variables not allowed" error.
func TestAdapterConfigFoldsLocalRef(t *testing.T) {
	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}
local "greeting" {
  value = "You are a helpful assistant."
}
adapter "copilot" "bot" {
  config {
    system_prompt = local.greeting
  }
}
step "work" {
  target = adapter.copilot.bot
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
		t.Fatalf("expected local.* in adapter.config to compile without error, got: %s", diags.Error())
	}
	got := g.Adapters["copilot.bot"].Config["system_prompt"]
	if got != "You are a helpful assistant." {
		t.Errorf("system_prompt = %q, want 'You are a helpful assistant.'", got)
	}
}

// TestAdapterConfigFileVarPath_SuccessNoSpuriousError verifies that
// file(var.prompt_file) in adapter.config with an *existing* file compiles
// without any error — specifically without spurious "Variables not allowed" diagnostic.
func TestAdapterConfigFileVarPath_SuccessNoSpuriousError(t *testing.T) {
	dir := t.TempDir()
	promptPath := "prompt.txt"
	if err := os.WriteFile(filepath.Join(dir, promptPath), []byte("Be helpful.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}
variable "prompt_file" {
  type = string
  default = "prompt.txt"
}
adapter "copilot" "bot" {
  config {
    system_prompt = file(var.prompt_file)
  }
}
step "work" {
  target = adapter.copilot.bot
  outcome "success" { next = step.done }
}
state "done" { terminal = true }
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := CompileWithOpts(spec, testSchemas, CompileOpts{WorkflowDir: dir})
	if diags.HasErrors() {
		t.Fatalf("expected file(var.*) in adapter.config to compile without error, got: %s", diags.Error())
	}
	got := g.Adapters["copilot.bot"].Config["system_prompt"]
	if got != "Be helpful.\n" {
		t.Errorf("system_prompt = %q, want 'Be helpful.\\n'", got)
	}
}

// TestAdapterEnvironmentTraversalResolves verifies that an adapter declaration
// with environment = shell.ci (bare traversal) compiles to AdapterNode.Environment == "shell.ci".
func TestAdapterEnvironmentTraversalResolves(t *testing.T) {
	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}

environment "shell" "ci" {
  variables = { CI = "true" }
}

adapter "copilot" "bot" {
  environment = shell.ci
}

step "work" {
  target = adapter.copilot.bot
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
		t.Fatalf("expected adapter environment traversal to compile without error, got: %s", diags.Error())
	}
	ad := g.Adapters["copilot.bot"]
	if ad == nil {
		t.Fatal("adapter 'copilot.bot' not found in graph")
	}
	if ad.Environment != "shell.ci" {
		t.Errorf("adapter Environment = %q, want 'shell.ci'", ad.Environment)
	}
}

// TestAdapterSecretsBlock_Compiles verifies that a secrets block inside an
// adapter declaration is extracted into AdapterNode.Secrets.
func TestAdapterSecretsBlock_Compiles(t *testing.T) {
	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}

adapter "exec" "bot" {
  secrets {
    api_key = "key123"
  }
}

step "work" {
  target = adapter.exec.bot
  outcome "success" { next = state.done }
}
state "done" { terminal = true }
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, testSchemas)
	if diags.HasErrors() {
		t.Fatalf("expected adapter secrets to compile without error, got: %s", diags.Error())
	}
	ad := g.Adapters["exec.bot"]
	if ad == nil {
		t.Fatal("adapter 'shell.bot' not found")
	}
	if len(ad.Secrets) != 1 || ad.Secrets["api_key"] == nil {
		t.Errorf("expected Secrets to contain api_key expression, got %v", ad.Secrets)
	}
}

// TestAdapterEnvironmentCompatibility_Match verifies that an adapter with
// CompatibleEnvironments compiles when the environment type matches.
func TestAdapterEnvironmentCompatibility_Match(t *testing.T) {
	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}

environment "shell" "default" {
  variables = { CI = "true" }
}

adapter "exec" "bot" {}

step "work" {
  target = adapter.exec.bot
  outcome "success" { next = state.done }
}
state "done" { terminal = true }
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	schemas := map[string]AdapterInfo{
		"exec": {
			CompatibleEnvironments: []string{"shell"},
		},
	}
	_, diags = Compile(spec, schemas)
	if diags.HasErrors() {
		t.Fatalf("expected compatibility match to compile without error, got: %s", diags.Error())
	}
}

// TestAdapterEnvironmentCompatibility_Mismatch verifies that an adapter with
// CompatibleEnvironments emits an error when the environment type does not match.
func TestAdapterEnvironmentCompatibility_Mismatch(t *testing.T) {
	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}

environment "shell" "default" {}

adapter "exec" "bot" {
  environment = shell.default
}

step "work" {
  target = adapter.exec.bot
  outcome "success" { next = state.done }
}
state "done" { terminal = true }
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	schemas := map[string]AdapterInfo{
		"exec": {
			CompatibleEnvironments: []string{"container"},
		},
	}
	_, diags = Compile(spec, schemas)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for incompatible environment")
	}
	if !strings.Contains(diags.Error(), "compatible_environments") {
		t.Errorf("expected error about compatible_environments, got: %s", diags.Error())
	}
}

// TestAdapterEnvironmentCompatibility_Wildcard verifies that an adapter with
// CompatibleEnvironments containing "*" compiles with any environment type.
func TestAdapterEnvironmentCompatibility_Wildcard(t *testing.T) {
	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}

environment "shell" "default" {}
adapter "exec" "main" {}

step "run" {
  target = adapter.exec.main
  outcome "success" { next = state.done }
}
state "done" { terminal = true }
`
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	schemas := map[string]AdapterInfo{
		"exec": {
			CompatibleEnvironments: []string{"*"},
		},
	}
	_, diags = Compile(spec, schemas)
	if diags.HasErrors() {
		t.Fatalf("expected wildcard compatibility to compile without error, got: %s", diags.Error())
	}
}

// TestAdapterConfig_UnresolvedStringVariable_NoPanic verifies that an adapter
// config attribute of type string which references a variable with no default
// does not panic. Instead the compiler returns a located diagnostic naming the
// attribute and explaining that the value cannot be determined.
func TestAdapterConfig_UnresolvedStringVariable_NoPanic(t *testing.T) {
	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}

variable "missing_default" {
  type = string
}

adapter "copilot" "bot" {
  config {
    system_prompt = var.missing_default
  }
}

step "work" {
  target = adapter.copilot.bot
  outcome "success" { next = step.done }
}

state "done" { terminal = true }
`

	spec, parseDiags := Parse("t.hcl", []byte(src))
	if parseDiags.HasErrors() {
		t.Fatalf("parse: %s", parseDiags.Error())
	}
	_, compileDiags := Compile(spec, testSchemas)
	if !compileDiags.HasErrors() {
		t.Fatal("expected compile error for unresolved adapter config value, got none")
	}

	found := false
	for _, d := range compileDiags {
		if d.Severity == hcl.DiagError && strings.Contains(d.Summary, `field "system_prompt" cannot be determined`) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected diagnostic about unresolved system_prompt, got: %s", compileDiags.Error())
	}
}
