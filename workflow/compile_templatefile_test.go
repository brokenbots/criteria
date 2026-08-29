package workflow_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Test that templatefile() with a non-defaulted variable validates successfully
// when the template file exists and is well-formed. The rendered value is left
// unknown until runtime values are supplied.
func TestCompileTemplatefileValidation_UnknownVar(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "command.tmpl"), []byte("hello {{ .name }}"), 0o644); err != nil {
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

variable "name" {
  type = string
}

adapter "noop" "a" {
  config {}
}

step "step1" {
  target = adapter.noop.a
  input {
    prompt = templatefile("./command.tmpl", { name = var.name })
  }
  outcome "success" { next = step.done }
}
`
	diags := compileWorkflowInDir(t, dir, hclContent)
	if diags.HasErrors() {
		t.Fatalf("unexpected compile error for templatefile() with unknown var: %s", diags.Error())
	}
}

// Test that templatefile() with a non-defaulted variable still reports a
// missing template file at validation time.
func TestCompileTemplatefileValidation_UnknownVar_MissingFile(t *testing.T) {
	dir := t.TempDir()
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

variable "name" {
  type = string
}

adapter "noop" "a" {
  config {}
}

step "step1" {
  target = adapter.noop.a
  input {
    prompt = templatefile("./missing.tmpl", { name = var.name })
  }
  outcome "success" { next = step.done }
}
`
	diags := compileWorkflowInDir(t, dir, hclContent)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for missing template file; got none")
	}
	if !strings.Contains(diags.Error(), "no such file") {
		t.Errorf("compile error %q should mention 'no such file'", diags.Error())
	}
}

// Test that templatefile() with a non-defaulted variable still reports a
// template parse error at validation time.
func TestCompileTemplatefileValidation_UnknownVar_ParseError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.tmpl"), []byte("{{ .unclosed"), 0o644); err != nil {
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

variable "name" {
  type = string
}

adapter "noop" "a" {
  config {}
}

step "step1" {
  target = adapter.noop.a
  input {
    prompt = templatefile("./bad.tmpl", { name = var.name })
  }
  outcome "success" { next = step.done }
}
`
	diags := compileWorkflowInDir(t, dir, hclContent)
	if !diags.HasErrors() {
		t.Fatal("expected compile parse error; got none")
	}
	if !strings.Contains(diags.Error(), "parse") {
		t.Errorf("compile error %q should mention 'parse'", diags.Error())
	}
}

// Test that templatefile() validates successfully when the variable has a
// known default value.
func TestCompileTemplatefileValidation_KnownVar(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "command.tmpl"), []byte("hello {{ .name }}"), 0o644); err != nil {
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

variable "name" {
  type    = string
  default = "world"
}

adapter "noop" "a" {
  config {}
}

step "step1" {
  target = adapter.noop.a
  input {
    prompt = templatefile("./command.tmpl", { name = var.name })
  }
  outcome "success" { next = step.done }
}
`
	diags := compileWorkflowInDir(t, dir, hclContent)
	if diags.HasErrors() {
		t.Fatalf("unexpected compile error for templatefile() with known var: %s", diags.Error())
	}
}
