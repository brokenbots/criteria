package workflow_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCompileTemplatefile_ShellQuote_UnknownVar validates that a templatefile
// template using the shellquote function passes compile-time validation even
// when the substituted variable is unknown at compile time.
func TestCompileTemplatefile_ShellQuote_UnknownVar(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "command.tmpl"), []byte("printf '%s\\n' {{ .value | shellquote }}"), 0o644); err != nil {
		t.Fatal(err)
	}

	hclContent := `workflow {
  name = "test"
  version       = "0.1"
  initial_state = "step1"
  target_state  = "done"
}

state "done" {
  terminal = true
  success  = true
}

variable "value" {
  type = string
}

adapter "noop" "a" {
  config {}
}

step "step1" {
  target = adapter.noop.a
  input {
    prompt = templatefile("./command.tmpl", { value = var.value })
  }
  outcome "success" { next = step.done }
}
`
	diags := compileWorkflowInDir(t, dir, hclContent)
	if diags.HasErrors() {
		t.Fatalf("unexpected compile error: %s", diags.Error())
	}
}

// TestCompileTemplatefile_ShellQuote_UnknownFunc ensures that referencing an
// undefined template function still produces a compile-time render error.
func TestCompileTemplatefile_ShellQuote_UnknownFunc(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.tmpl"), []byte("{{ nonexistent .value }}"), 0o644); err != nil {
		t.Fatal(err)
	}

	hclContent := `workflow {
  name = "test"
  version       = "0.1"
  initial_state = "step1"
  target_state  = "done"
}

state "done" {
  terminal = true
  success  = true
}

variable "value" {
  type    = string
  default = "x"
}

adapter "noop" "a" {
  config {}
}

step "step1" {
  target = adapter.noop.a
  input {
    prompt = templatefile("./bad.tmpl", { value = var.value })
  }
  outcome "success" { next = step.done }
}
`
	diags := compileWorkflowInDir(t, dir, hclContent)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for unknown template function; got none")
	}
	if !strings.Contains(diags.Error(), "function") && !strings.Contains(diags.Error(), "nonexistent") {
		t.Errorf("compile error %q should mention unknown function", diags.Error())
	}
}
