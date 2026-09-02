package workflow

import (
	"strings"
	"testing"
)

func TestCompile_AdapterSecrets_RejectsLiteral(t *testing.T) {
	src := `
workflow {
  name = "w"
  version = "0.1"
  initial_state = "s"
  target_state = "s"
}
adapter "exec" "bot" {
  secrets { api_key = "literal-secret" }
}
state "s" { terminal = true }
`
	want := "must reference a declared secret variable or data block"
	if diags := compileString(t, src); !diags.HasErrors() || !strings.Contains(diags.Error(), want) {
		t.Fatalf("expected literal adapter secret to be rejected with %q, got %v", want, diags)
	}
}

func TestCompile_AdapterSecrets_RejectsNonSecretVar(t *testing.T) {
	src := `
workflow {
  name = "w"
  version = "0.1"
  initial_state = "s"
  target_state = "s"
}
variable "api_key" { type = string }
adapter "exec" "bot" {
  secrets { api_key = var.api_key }
}
state "s" { terminal = true }
`
	want := "is not declared as a secret variable"
	if diags := compileString(t, src); !diags.HasErrors() || !strings.Contains(diags.Error(), want) {
		t.Fatalf("expected non-secret var in adapter secret to be rejected with %q, got %v", want, diags)
	}
}

func TestCompile_AdapterSecrets_AcceptsSecretVar(t *testing.T) {
	src := `
workflow {
  name = "w"
  version = "0.1"
  initial_state = "s"
  target_state = "s"
}
variable "api_key" {
  type   = string
  secret = true
}
adapter "exec" "bot" {
  secrets { api_key = var.api_key }
}
state "s" { terminal = true }
`
	if diags := compileString(t, src); diags.HasErrors() {
		t.Fatalf("expected secret variable reference in adapter secrets to compile, got %v", diags)
	}
}

func TestCompile_AdapterSecrets_AcceptsSecretData(t *testing.T) {
	src := `
workflow {
  name = "w"
  version = "0.1"
  initial_state = "s"
  target_state = "s"
}
data "internal" "api_key" {
  type   = string
  secret = true
}
adapter "exec" "bot" {
  secrets { api_key = data.internal.api_key.value }
}
state "s" { terminal = true }
`
	if diags := compileString(t, src); diags.HasErrors() {
		t.Fatalf("expected secret data reference in adapter secrets to compile, got %v", diags)
	}
}

func TestCompile_StepSecretInput_RejectsLiteral(t *testing.T) {
	src := `
workflow {
  name = "w"
  version = "0.1"
  initial_state = "run"
  target_state = "s"
}
adapter "exec" "bot" {}
step "run" {
  target = adapter.exec.bot
  secret_input { api_key = "literal-secret" }
  outcome "ok" { next = state.s }
}
state "s" { terminal = true }
`
	want := "secret-tainted expression"
	if diags := compileString(t, src); !diags.HasErrors() || !strings.Contains(diags.Error(), want) {
		t.Fatalf("expected literal secret_input to be rejected with %q, got %v", want, diags)
	}
}

func TestCompile_StepSecretInput_RejectsNonSecretVar(t *testing.T) {
	src := `
workflow {
  name = "w"
  version = "0.1"
  initial_state = "run"
  target_state = "s"
}
variable "api_key" { type = string }
adapter "exec" "bot" {}
step "run" {
  target = adapter.exec.bot
  secret_input { api_key = var.api_key }
  outcome "ok" { next = state.s }
}
state "s" { terminal = true }
`
	want := "secret-tainted expression"
	if diags := compileString(t, src); !diags.HasErrors() || !strings.Contains(diags.Error(), want) {
		t.Fatalf("expected non-secret var in secret_input to be rejected with %q, got %v", want, diags)
	}
}

func TestCompile_StepSecretInput_AcceptsSecretVar(t *testing.T) {
	src := `
workflow {
  name = "w"
  version = "0.1"
  initial_state = "run"
  target_state = "s"
}
variable "api_key" {
  type   = string
  secret = true
}
adapter "exec" "bot" {}
step "run" {
  target = adapter.exec.bot
  secret_input { api_key = var.api_key }
  outcome "ok" { next = state.s }
}
state "s" { terminal = true }
`
	if diags := compileString(t, src); diags.HasErrors() {
		t.Fatalf("expected secret var in secret_input to compile, got %v", diags)
	}
}

func compileString(t *testing.T, src string) hclDiagnostics {
	t.Helper()
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, nil)
	return diags
}

// hclDiagnostics wraps hcl.Diagnostics so the test helper can return it from a
// function without importing hcl/v2 in every test.
type hclDiagnostics interface {
	HasErrors() bool
	Error() string
}
