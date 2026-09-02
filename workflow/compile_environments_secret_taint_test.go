package workflow

// compile_environments_secret_taint_test.go — regression tests for CRI-88
// exit criterion EC4: secret values cannot be assigned to normal environment
// variables or environment config.

import (
	"strings"
	"testing"
)

func secretVarEnvWorkflow(envBody string) string {
	return `
workflow {
  name = "w"
  version       = "0.1"
  initial_state = "done"
  target_state  = "done"
}

variable "api_key" {
  type    = string
  secret  = true
  default = "supersecret-default"
}

` + envBody + `

state "done" {
  terminal = true
  success  = true
}
`
}

func secretDataEnvWorkflow(envBody string) string {
	return `
workflow {
  name = "w"
  version       = "0.1"
  initial_state = "done"
  target_state  = "done"
}

variable "api_key_src" {
  type    = string
  default = "supersecret-data"
}

data "internal" "api_key" {
  type    = string
  secret  = true
  value   = var.api_key_src
}

` + envBody + `

state "done" {
  terminal = true
  success  = true
}
`
}

func TestCompileEnvironments_SecretVarInVariablesRejected(t *testing.T) {
	src := secretVarEnvWorkflow(`
environment "shell" "default" {
  variables = { LEAK = var.api_key }
}`)
	diags := compileString(t, src)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for secret variable in environment.variables")
	}
	if !strings.Contains(diags.Error(), "tainted value var.api_key") {
		t.Fatalf("expected taint error mentioning var.api_key, got: %v", diags.Error())
	}
}

func TestCompileEnvironments_SecretVarInConfigRejected(t *testing.T) {
	src := secretVarEnvWorkflow(`
environment "shell" "default" {
  config = { LEAK = var.api_key }
}`)
	diags := compileString(t, src)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for secret variable in environment.config")
	}
	if !strings.Contains(diags.Error(), "tainted value var.api_key") {
		t.Fatalf("expected taint error mentioning var.api_key, got: %v", diags.Error())
	}
}

func TestCompileEnvironments_SecretDataInVariablesRejected(t *testing.T) {
	src := secretDataEnvWorkflow(`
environment "shell" "default" {
  variables = { LEAK = data.internal.api_key.value }
}`)
	diags := compileString(t, src)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for secret data block in environment.variables")
	}
	errText := diags.Error()
	if !strings.Contains(errText, "tainted value data.internal.api_key") {
		t.Fatalf("expected taint error mentioning data.internal.api_key, got: %v", errText)
	}
}

func TestCompileEnvironments_SecretDataInConfigRejected(t *testing.T) {
	src := secretDataEnvWorkflow(`
environment "shell" "default" {
  config = { LEAK = data.internal.api_key.value }
}`)
	diags := compileString(t, src)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for secret data block in environment.config")
	}
	errText := diags.Error()
	if !strings.Contains(errText, "tainted value data.internal.api_key") {
		t.Fatalf("expected taint error mentioning data.internal.api_key, got: %v", errText)
	}
}

func TestCompileEnvironments_NonSecretVarInVariablesAccepted(t *testing.T) {
	src := `
workflow {
  name = "w"
  version       = "0.1"
  initial_state = "done"
  target_state  = "done"
}

variable "api_key" {
  type    = string
  default = "not-a-secret"
}

environment "shell" "default" {
  variables = { OK = var.api_key }
}

state "done" {
  terminal = true
  success  = true
}
`
	if diags := compileString(t, src); diags.HasErrors() {
		t.Fatalf("expected non-secret variable in environment.variables to compile, got: %v", diags.Error())
	}
}

func TestCompileEnvironments_SecretVarLocalDerivedInVariablesRejected(t *testing.T) {
	// A value derived from a secret variable (via a local) must also be
	// rejected from environment.variables.
	src := `
workflow {
  name = "w"
  version       = "0.1"
  initial_state = "done"
  target_state  = "done"
}

variable "api_key" {
  type    = string
  secret  = true
  default = "supersecret-default"
}

local "derived" {
  value = "${var.api_key}-suffix"
}

environment "shell" "default" {
  variables = { LEAK = local.derived }
}

state "done" {
  terminal = true
  success  = true
}
`
	diags := compileString(t, src)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for secret-derived local in environment.variables")
	}
}

func TestCompileEnvironments_SecretDataLocalDerivedInVariablesRejected(t *testing.T) {
	// A local derived from a secret data block must also be rejected from
	// environment.variables.
	src := secretDataEnvWorkflow(`
local "derived" {
  value = "${data.internal.api_key.value}-suffix"
}

environment "shell" "default" {
  variables = { LEAK = local.derived }
}`)
	diags := compileString(t, src)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for secret-data-derived local in environment.variables")
	}
}
