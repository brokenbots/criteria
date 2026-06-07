package workflow

import (
	"testing"

	"github.com/zclconf/go-cty/cty"
)

const varWorkflow = `
workflow {
  name = "test"
  version       = "0.1"
  initial_state = "start"
  target_state  = "__done__"
}

adapter "noop" "default" {}

variable "greeting" {
  type = string
  default     = "hello"
  description = "A greeting"
}
variable "count" {
  type = number
  default = 3
}
variable "no_default" {
  type = string
}
step "start" {
  target = adapter.noop.default
  outcome "success" { next = step.__done__ }
}
state "__done__" { terminal = true }
`

const varWorkflowNoVars = `
workflow {
  name = "novars"
  version       = "0.1"
  initial_state = "start"
  target_state  = "__done__"
}

adapter "noop" "default" {}

step "start" {
  target = adapter.noop.default
  outcome "success" { next = step.__done__ }
}
state "__done__" { terminal = true }
`

func TestVariableCompile_SecretFlag(t *testing.T) {
	src := `
workflow {
  name = "test"
  version       = "0.1"
  initial_state = "start"
  target_state  = "__done__"
}

adapter "noop" "default" {}

variable "api_key" {
  type    = string
  secret  = true
  default = "shh"
}

step "start" {
  target = adapter.noop.default
  outcome "success" { next = step.__done__ }
}
state "__done__" { terminal = true }
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	v := g.Variables["api_key"]
	if v == nil {
		t.Fatal("variable 'api_key' not found")
	}
	if !v.Secret {
		t.Errorf("expected variable 'api_key' to have Secret=true")
	}
}

func TestVariableCompile_TypeDefaults(t *testing.T) {
	src := `
workflow {
  name = "test"
  version       = "0.1"
  initial_state = "start"
  target_state  = "__done__"
}

adapter "noop" "default" {}

variable "config" {
  type = object({
    greeting = optional(string, "hello")
    count    = optional(number, 42)
  })
  default = {}
}

step "start" {
  target = adapter.noop.default
  outcome "success" { next = step.__done__ }
}
state "__done__" { terminal = true }
`

	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags)
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags)
	}

	vn, ok := g.Variables["config"]
	if !ok {
		t.Fatal("variable 'config' not compiled")
	}

	// The default {} should have type defaults applied: greeting="hello", count=42.
	if vn.Default == cty.NilVal {
		t.Fatal("expected default value")
	}
	if !vn.Default.Type().IsObjectType() {
		t.Fatalf("expected object default, got %s", vn.Default.Type().FriendlyName())
	}
	if got := vn.Default.GetAttr("greeting").AsString(); got != "hello" {
		t.Errorf("greeting default: got %q, want %q", got, "hello")
	}
	if got, _ := vn.Default.GetAttr("count").AsBigFloat().Int64(); got != 42 {
		t.Errorf("count default: got %d, want %d", got, 42)
	}
}

func TestVariableCompile_TypeDefaults_MissingDefault(t *testing.T) {
	// When no variable-level default is declared, the type defaults don't
	// auto-populate a default — the variable remains required.
	src := `
workflow {
  name = "test"
  version       = "0.1"
  initial_state = "start"
  target_state  = "__done__"
}

adapter "noop" "default" {}

variable "config" {
  type = object({
    greeting = optional(string, "hello")
  })
}

step "start" {
  target = adapter.noop.default
  outcome "success" { next = step.__done__ }
}
state "__done__" { terminal = true }
`

	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags)
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags)
	}

	vn, ok := g.Variables["config"]
	if !ok {
		t.Fatal("variable 'config' not compiled")
	}
	if !vn.IsRequired() {
		t.Error("expected variable to be required (no var-level default)")
	}
}

func TestVariableCompile_TypeDefaults_SingleArgOptional(t *testing.T) {
	// optional(string) without default value should parse fine.
	src := `
workflow {
  name = "test"
  version       = "0.1"
  initial_state = "start"
  target_state  = "__done__"
}

adapter "noop" "default" {}

variable "config" {
  type = object({
    greeting = optional(string)
  })
  default = {}
}

step "start" {
  target = adapter.noop.default
  outcome "success" { next = step.__done__ }
}
state "__done__" { terminal = true }
`

	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags)
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags)
	}

	vn, ok := g.Variables["config"]
	if !ok {
		t.Fatal("variable 'config' not compiled")
	}
	if vn.Default == cty.NilVal {
		t.Fatal("expected default value")
	}
	// optional(string) without default means attribute can be omitted; it
	// does not provide a default value. The convert should fill with null.
	if !vn.Default.GetAttr("greeting").IsNull() {
		t.Errorf("greeting should be null when optional has no default")
	}
}

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
workflow {
  name = "test"
  version       = "0.1"
  initial_state = "s"
  target_state  = "__done__"
}

variable "x" {
  type = string
  default = "a"
}
variable "x" {
  type = string
  default = "b"
}
step "s" {
  target = adapter.noop.default
  outcome "success" { next = step.__done__ }
}
state "__done__" { terminal = true }
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
workflow {
  name = "test"
  version       = "0.1"
  initial_state = "s"
  target_state  = "__done__"
}

variable "x" {
  type    = badtype
  default = "a"
}
step "s" {
  target = adapter.noop.default
  outcome "success" { next = step.__done__ }
}
state "__done__" { terminal = true }
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
workflow {
  name = "test"
  version       = "0.1"
  initial_state = "s"
  target_state  = "__done__"
}

variable "x" {
  type = string
  default = 42
}
step "s" {
  target = adapter.noop.default
  outcome "success" { next = step.__done__ }
}
state "__done__" { terminal = true }
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
workflow {
  name = "test"
  version       = "0.1"
  initial_state = "s"
  target_state  = "__done__"
}

variable "flag" {
  type = number
  default = true
}
step "s" {
  target = adapter.noop.default
  outcome "success" { next = step.__done__ }
}
state "__done__" { terminal = true }
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

func TestVariableCompile_ListDefaultTupleLiteral(t *testing.T) {
	src := `
workflow {
  name = "test"
  version       = "0.1"
  initial_state = "s"
  target_state  = "__done__"
}

adapter "noop" "default" {}

variable "tags" {
  type = list(string)
  default = ["foo", "bar"]
}
step "s" {
  target = adapter.noop.default
  outcome "success" { next = step.__done__ }
}
state "__done__" { terminal = true }
`
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags)
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("expected list(string) tuple literal default to compile: %s", diags)
	}
	v, ok := g.Variables["tags"]
	if !ok {
		t.Fatal("variable 'tags' not found in compiled graph")
	}
	if !v.Default.Type().Equals(cty.List(cty.String)) {
		t.Errorf("expected default type list(string), got %s", v.Default.Type().FriendlyName())
	}
	var elems []string
	for it := v.Default.ElementIterator(); it.Next(); {
		_, elem := it.Element()
		elems = append(elems, elem.AsString())
	}
	if len(elems) != 2 || elems[0] != "foo" || elems[1] != "bar" {
		t.Errorf("unexpected default elements: %v", elems)
	}
}
