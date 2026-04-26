package workflow

import (
	"testing"

	"github.com/zclconf/go-cty/cty"
)

const varWorkflow = `
workflow "test" {
  version       = "0.1"
  initial_state = "start"
  target_state  = "__done__"

  variable "greeting" {
    type        = "string"
    default     = "hello"
    description = "A greeting"
  }
  variable "count" {
    type    = "number"
    default = 3
  }
  variable "no_default" {
    type = "string"
  }
  step "start" {
    adapter = "noop"
    outcome "success" { transition_to = "__done__" }
  }
  state "__done__" { terminal = true }
}
`

const varWorkflowNoVars = `
workflow "novars" {
  version       = "0.1"
  initial_state = "start"
  target_state  = "__done__"

  step "start" {
    adapter = "noop"
    outcome "success" { transition_to = "__done__" }
  }
  state "__done__" { terminal = true }
}
`

func TestVariableCompile_Defaults(t *testing.T) {
	spec, diags := Parse("test.hcl", []byte(varWorkflow))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags)
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags)
	}

	if len(g.Variables) != 3 {
		t.Fatalf("expected 3 variables, got %d", len(g.Variables))
	}

	greeting, ok := g.Variables["greeting"]
	if !ok {
		t.Fatal("missing variable 'greeting'")
	}
	if greeting.Type != cty.String {
		t.Errorf("greeting type = %v, want string", greeting.Type)
	}
	if greeting.Default == cty.NilVal || greeting.Default.AsString() != "hello" {
		t.Errorf("greeting default = %v, want 'hello'", greeting.Default)
	}
	if greeting.Description != "A greeting" {
		t.Errorf("greeting description = %q", greeting.Description)
	}

	count, ok := g.Variables["count"]
	if !ok {
		t.Fatal("missing variable 'count'")
	}
	if count.Type != cty.Number {
		t.Errorf("count type = %v, want number", count.Type)
	}
	if count.Default == cty.NilVal {
		t.Error("count.Default should not be NilVal")
	}

	nd, ok := g.Variables["no_default"]
	if !ok {
		t.Fatal("missing variable 'no_default'")
	}
	if nd.Default != cty.NilVal {
		t.Errorf("no_default.Default = %v, want NilVal", nd.Default)
	}
}

func TestVariableCompile_NoVariables(t *testing.T) {
	spec, diags := Parse("test.hcl", []byte(varWorkflowNoVars))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags)
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags)
	}
	if len(g.Variables) != 0 {
		t.Errorf("expected 0 variables, got %d", len(g.Variables))
	}
}

func TestVariableCompile_DuplicateName(t *testing.T) {
	src := `
workflow "test" {
  version       = "0.1"
  initial_state = "s"
  target_state  = "__done__"

  variable "x" {
    type    = "string"
    default = "a"
  }
  variable "x" {
    type    = "string"
    default = "b"
  }
  step "s" {
    adapter = "noop"
    outcome "success" { transition_to = "__done__" }
  }
  state "__done__" { terminal = true }
}
`
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags)
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Error("expected compile error for duplicate variable name")
	}
}

func TestVariableCompile_InvalidType(t *testing.T) {
	src := `
workflow "test" {
  version       = "0.1"
  initial_state = "s"
  target_state  = "__done__"

  variable "x" {
    type    = "badtype"
    default = "a"
  }
  step "s" {
    adapter = "noop"
    outcome "success" { transition_to = "__done__" }
  }
  state "__done__" { terminal = true }
}
`
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags)
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Error("expected compile error for invalid variable type")
	}
}

func TestVariableCompile_DefaultTypeMismatch(t *testing.T) {
	// Declare a string variable but provide a number default — must be rejected
	// under the strict "default must match declared type" rule.
	src := `
workflow "test" {
  version       = "0.1"
  initial_state = "s"
  target_state  = "__done__"

  variable "x" {
    type    = "string"
    default = 42
  }
  step "s" {
    adapter = "noop"
    outcome "success" { transition_to = "__done__" }
  }
  state "__done__" { terminal = true }
}
`
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags)
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Errorf("expected compile error for number default on string variable, got none")
	}
}

func TestVariableCompile_DefaultBoolMismatch(t *testing.T) {
	// Declare a number variable but provide a bool default — must be rejected.
	src := `
workflow "test" {
  version       = "0.1"
  initial_state = "s"
  target_state  = "__done__"

  variable "flag" {
    type    = "number"
    default = true
  }
  step "s" {
    adapter = "noop"
    outcome "success" { transition_to = "__done__" }
  }
  state "__done__" { terminal = true }
}
`
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags)
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Errorf("expected compile error for bool default on number variable, got none")
	}
}
