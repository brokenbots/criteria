package workflow

import (
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
)

// TestTaintPass_SecretVariableInSecretInput verifies that a step referencing a
// secret variable in secret_input is marked Tainted.
func TestTaintPass_SecretVariableInSecretInput(t *testing.T) {
	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}

variable "api_key" {
  type   = string
  secret = true
  default = "key"
}

adapter "noop" "default" {}

step "run" {
  target = adapter.noop.default
  secret_input {
    key = var.api_key
  }
  outcome "success" { next = "done" }
}
state "done" { terminal = true }
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	step := g.Steps["run"]
	if step == nil {
		t.Fatal("step 'run' not found")
	}
	if !step.Tainted {
		t.Error("expected step 'run' to be Tainted")
	}
}

// TestTaintPass_SecretVariableInRegularInput verifies that a step referencing a
// secret variable in its regular input block fails to compile (D65).
func TestTaintPass_SecretVariableInRegularInput(t *testing.T) {
	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}

variable "api_key" {
  type   = string
  secret = true
  default = "key"
}

adapter "shell" "default" {}

step "run" {
  target = adapter.shell.default
  input {
    command = var.api_key
  }
  outcome "success" { next = "done" }
}
state "done" { terminal = true }
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, testSchemas)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for tainted value in non-secret channel")
	}
	found := false
	for _, d := range diags {
		if d.Severity == hcl.DiagError && strings.Contains(d.Summary, "tainted value") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected taint error diagnostic, got: %s", diags.Error())
	}
}

// TestTaintPass_SharedVariableSecretInInput verifies that a step referencing a
// secret shared_variable in input fails to compile (D65).
func TestTaintPass_SharedVariableSecretInInput(t *testing.T) {
	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}

shared_variable "token" {
  type   = string
  secret = true
}

adapter "shell" "default" {}

step "run" {
  target = adapter.shell.default
  input {
    command = shared.token
  }
  outcome "success" { next = "done" }
}
state "done" { terminal = true }
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, testSchemas)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for tainted value in non-secret channel")
	}
	found := false
	for _, d := range diags {
		if d.Severity == hcl.DiagError && strings.Contains(d.Summary, "tainted value") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected taint error diagnostic, got: %s", diags.Error())
	}
}

// TestTaintPass_PropagationThroughOutcomes verifies that taint propagates from
// a tainted step to its successor via outcome edges. The first step uses
// secret_input so compilation succeeds and propagation can be verified.
func TestTaintPass_PropagationThroughOutcomes(t *testing.T) {
	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "first"
  target_state  = "done"
}

variable "api_key" {
  type   = string
  secret = true
  default = "key"
}

adapter "shell" "default" {}
adapter "noop" "default" {}

step "first" {
  target = adapter.shell.default
  secret_input {
    command = var.api_key
  }
  outcome "success" { next = "second" }
}

step "second" {
  target = adapter.noop.default
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
		t.Fatalf("compile: %s", diags.Error())
	}
	if !g.Steps["first"].Tainted {
		t.Error("expected step 'first' to be Tainted")
	}
	if !g.Steps["second"].Tainted {
		t.Error("expected step 'second' to be Tainted (propagated from first)")
	}
}

// TestTaintPass_NonSecretVariableNotTainted verifies that a step referencing a
// non-secret variable is NOT marked Tainted.
func TestTaintPass_NonSecretVariableNotTainted(t *testing.T) {
	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}

variable "greeting" {
  type    = string
  default = "hello"
}

adapter "shell" "default" {}

step "run" {
  target = adapter.shell.default
  input {
    command = var.greeting
  }
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
		t.Fatalf("compile: %s", diags.Error())
	}
	step := g.Steps["run"]
	if step == nil {
		t.Fatal("step 'run' not found")
	}
	if step.Tainted {
		t.Error("expected step 'run' NOT to be Tainted")
	}
}

// TestTaintPass_BranchNotTaintedWhenPredecessorClean verifies that a step with
// two predecessors is only tainted if at least one predecessor is tainted.
func TestTaintPass_BranchNotTaintedWhenPredecessorClean(t *testing.T) {
	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "first"
  target_state  = "done"
}

adapter "noop" "default" {}

step "first" {
  target = adapter.noop.default
  outcome "ok"   { next = "second" }
  outcome "skip" { next = "third" }
}

step "second" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
}

step "third" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
}

state "done" { terminal = true }
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	if g.Steps["first"].Tainted {
		t.Error("expected step 'first' NOT to be Tainted")
	}
	if g.Steps["second"].Tainted {
		t.Error("expected step 'second' NOT to be Tainted")
	}
	if g.Steps["third"].Tainted {
		t.Error("expected step 'third' NOT to be Tainted")
	}
}

// TestTaintPass_AdapterSecretsTaintsStep verifies that a step whose target
// adapter has a secrets block is marked Tainted.
func TestTaintPass_AdapterSecretsTaintsStep(t *testing.T) {
	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}

adapter "shell" "default" {
  secrets {
    api_key = "key"
  }
}

step "run" {
  target = adapter.shell.default
  input {
    command = "echo hi"
  }
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
		t.Fatalf("compile: %s", diags.Error())
	}
	if !g.Steps["run"].Tainted {
		t.Error("expected step 'run' to be Tainted because adapter has secrets")
	}
}

// TestTaintPass_SensitiveOutputTaintsDownstream verifies that a downstream step
// referencing a sensitive output field is marked Tainted.
func TestTaintPass_SensitiveOutputTaintsDownstream(t *testing.T) {
	schemas := map[string]AdapterInfo{
		"sensitive": {
			InputSchema: map[string]ConfigField{
				"command": {Required: true, Type: ConfigFieldString},
			},
			OutputSchema: map[string]ConfigField{
				"token": {Sensitive: true, Type: ConfigFieldString},
			},
		},
		"noop": noopSchema,
	}

	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "first"
  target_state  = "done"
}

adapter "sensitive" "default" {}
adapter "noop" "default" {}

step "first" {
  target = adapter.sensitive.default
  input {
    command = "echo hi"
  }
  outcome "success" { next = "second" }
}

step "second" {
  target = adapter.noop.default
  secret_input {
    command = steps.first.token
  }
  outcome "success" { next = "done" }
}

state "done" { terminal = true }
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, schemas)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	if g.Steps["first"].Tainted {
		t.Error("expected step 'first' NOT to be Tainted (it produces sensitive output but does not consume it)")
	}
	if !g.Steps["second"].Tainted {
		t.Error("expected step 'second' to be Tainted (references sensitive output from first)")
	}
}

// TestTaintPass_SensitiveOutputFourPartTaintsDownstream verifies that a
// downstream step referencing a sensitive output field via the 4-part
// steps.X.outputs.Y form is marked Tainted.
func TestTaintPass_SensitiveOutputFourPartTaintsDownstream(t *testing.T) {
	schemas := map[string]AdapterInfo{
		"sensitive": {
			InputSchema: map[string]ConfigField{
				"command": {Required: true, Type: ConfigFieldString},
			},
			OutputSchema: map[string]ConfigField{
				"token": {Sensitive: true, Type: ConfigFieldString},
			},
		},
		"noop": noopSchema,
	}

	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "first"
  target_state  = "done"
}

adapter "sensitive" "default" {}
adapter "noop" "default" {}

step "first" {
  target = adapter.sensitive.default
  input {
    command = "echo hi"
  }
  outcome "success" { next = "second" }
}

step "second" {
  target = adapter.noop.default
  secret_input {
    command = steps.first.outputs.token
  }
  outcome "success" { next = "done" }
}

state "done" { terminal = true }
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, schemas)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	if g.Steps["first"].Tainted {
		t.Error("expected step 'first' NOT to be Tainted (it produces sensitive output but does not consume it)")
	}
	if !g.Steps["second"].Tainted {
		t.Error("expected step 'second' to be Tainted (references sensitive output from first)")
	}
}

// TestTaintPass_SensitiveOutputFourPartInInputFails verifies that a downstream
// step referencing a sensitive output field via steps.X.outputs.Y in a
// regular (non-secret) input block fails to compile.
func TestTaintPass_SensitiveOutputFourPartInInputFails(t *testing.T) {
	schemas := map[string]AdapterInfo{
		"sensitive": {
			InputSchema: map[string]ConfigField{
				"command": {Required: true, Type: ConfigFieldString},
			},
			OutputSchema: map[string]ConfigField{
				"token": {Sensitive: true, Type: ConfigFieldString},
			},
		},
		"noop": noopSchema,
	}

	src := `
workflow {
  name = "x"
  version       = "0.1"
  initial_state = "first"
  target_state  = "done"
}

adapter "sensitive" "default" {}
adapter "noop" "default" {}

step "first" {
  target = adapter.sensitive.default
  input {
    command = "echo hi"
  }
  outcome "success" { next = "second" }
}

step "second" {
  target = adapter.noop.default
  input {
    command = steps.first.outputs.token
  }
  outcome "success" { next = "done" }
}

state "done" { terminal = true }
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, schemas)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for tainted value in non-secret channel")
	}
	found := false
	for _, d := range diags {
		if d.Severity == hcl.DiagError && strings.Contains(d.Summary, "tainted value") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected taint error diagnostic, got: %s", diags.Error())
	}
}

// TestTaintPass_AdapterConfigWithSecretVarFails verifies that an adapter config
// referencing a secret variable fails to compile (D65).
func TestTaintPass_AdapterConfigWithSecretVarFails(t *testing.T) {
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
  default = "key"
}

adapter "copilot" "default" {
  config {
    system_prompt = var.api_key
  }
}

step "run" {
  target = adapter.copilot.default
  input {
    prompt = "hi"
  }
  outcome "success" { next = "done" }
}
state "done" { terminal = true }
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, testSchemas)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for tainted value in adapter config")
	}
	found := false
	for _, d := range diags {
		if d.Severity == hcl.DiagError && strings.Contains(d.Summary, "tainted value") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected taint error diagnostic, got: %s", diags.Error())
	}
}
