package workflow

import (
	"os"
	"path/filepath"
	"testing"
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
  outcome "success" { next = "done" }
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
  outcome "success" { next = "done" }
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
  outcome "success" { next = "done" }
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
  outcome "success" { next = "done" }
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
