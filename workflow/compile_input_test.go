package workflow

import (
	"strings"
	"testing"
)

// shellSchema is a test schema that mirrors the real shell adapter.
var shellSchema = AdapterInfo{
	InputSchema: map[string]ConfigField{
		"command": {Required: true, Type: ConfigFieldString},
	},
}

var listSchema = AdapterInfo{
	InputSchema: map[string]ConfigField{
		"items": {Required: true, Type: ConfigFieldListString},
	},
	ConfigSchema: map[string]ConfigField{
		"items": {Required: true, Type: ConfigFieldListString},
	},
}

// noopSchema is a test schema for the no-op adapter used in workflow-step tests.
var noopSchema = AdapterInfo{
	InputSchema:  map[string]ConfigField{},
	ConfigSchema: map[string]ConfigField{},
}

// copilotSchema mirrors the real copilot adapter schema.
var copilotSchema = AdapterInfo{
	ConfigSchema: map[string]ConfigField{
		"max_turns":         {Type: ConfigFieldNumber},
		"system_prompt":     {Type: ConfigFieldString},
		"model":             {Type: ConfigFieldString},
		"reasoning_effort":  {Type: ConfigFieldString},
		"working_directory": {Type: ConfigFieldString},
	},
	InputSchema: map[string]ConfigField{
		"prompt":           {Required: true, Type: ConfigFieldString},
		"max_turns":        {Type: ConfigFieldNumber},
		"reasoning_effort": {Type: ConfigFieldString},
	},
}

var testSchemas = map[string]AdapterInfo{
	"shell":   shellSchema,
	"copilot": copilotSchema,
	"listy":   listSchema,
	"noop":    noopSchema,
}

func TestInputRequiredFieldMissing(t *testing.T) {
	src := `
workflow "x" {
  adapter "shell" "default" {}
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
  step "run" {
    adapter = adapter.shell.default
    input {}
    outcome "success" { transition_to = "done" }
    outcome "failure" { transition_to = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, testSchemas)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for missing required input field")
	}
	if !strings.Contains(diags.Error(), `required field "command" is missing`) {
		t.Errorf("unexpected error: %s", diags.Error())
	}
	if strings.HasPrefix(diags.Error(), "<nil>:") {
		t.Errorf("expected source context in diagnostic, got: %s", diags.Error())
	}
}

func TestInputUnknownField(t *testing.T) {
	src := `
workflow "x" {
  adapter "shell" "default" {}
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
  step "run" {
    adapter = adapter.shell.default
    input {
      command = "echo hi"
      unknown_key = "bad"
    }
    outcome "success" { transition_to = "done" }
    outcome "failure" { transition_to = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, testSchemas)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for unknown input field")
	}
	if !strings.Contains(diags.Error(), `unknown field "unknown_key"`) {
		t.Errorf("unexpected error: %s", diags.Error())
	}
}

func TestInputOnLifecycleOpenIsError(t *testing.T) {
	src := `
workflow "x" {
  adapter "copilot" "default" {}
  version       = "0.1"
  initial_state = "open"
  target_state  = "done"
  step "open" {
    adapter   = adapter.copilot.default
    lifecycle = "open"
    input {
      prompt = "hello"
    }
    outcome "success" { transition_to = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, testSchemas)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for input on lifecycle open")
	}
	if !strings.Contains(diags.Error(), `lifecycle "open" must not include input`) {
		t.Errorf("unexpected error: %s", diags.Error())
	}
}

func TestInputOnLifecycleCloseIsError(t *testing.T) {
	src := `
workflow "x" {
  adapter "copilot" "default" {}
  version       = "0.1"
  initial_state = "open"
  target_state  = "done"
  step "open" {
    adapter   = adapter.copilot.default
    lifecycle = "open"
    outcome "success" { transition_to = "run" }
  }
  step "run" {
    adapter = adapter.copilot.default
    outcome "success" { transition_to = "close" }
  }
  step "close" {
    adapter   = adapter.copilot.default
    lifecycle = "close"
    input {
      prompt = "bye"
    }
    outcome "success" { transition_to = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, testSchemas)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for input on lifecycle close")
	}
	if !strings.Contains(diags.Error(), `lifecycle "close" must not include input`) {
		t.Errorf("unexpected error: %s", diags.Error())
	}
}

func TestLegacyConfigAttributeEmitsDiagnostic(t *testing.T) {
	src := `
workflow "x" {
  adapter "shell" "default" {}
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
  step "run" {
    adapter = adapter.shell.default
    config = {
      command = "echo old"
    }
    outcome "success" { transition_to = "done" }
    outcome "failure" { transition_to = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for legacy config attribute")
	}
	if !strings.Contains(diags.Error(), `"config" attribute removed`) {
		t.Errorf("expected legacy config error, got: %s", diags.Error())
	}
	if !strings.Contains(diags.Error(), `input { }`) {
		t.Errorf("expected hint to use input block, got: %s", diags.Error())
	}
	if strings.HasPrefix(diags.Error(), "<nil>:") {
		t.Errorf("expected source context in legacy diagnostic, got: %s", diags.Error())
	}
}

func TestInputPermissiveWhenNoSchema(t *testing.T) {
	// When schemas = nil, any keys should be accepted without error.
	src := `
workflow "x" {
  adapter "shell" "default" {}
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
  step "run" {
    adapter = adapter.shell.default
    input {
      command  = "echo hi"
      extra    = "ok"
    }
    outcome "success" { transition_to = "done" }
    outcome "failure" { transition_to = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("expected permissive compile to succeed: %s", diags.Error())
	}
	if g.Steps["run"].Input["command"] != "echo hi" {
		t.Errorf("unexpected input: %v", g.Steps["run"].Input)
	}
}

func TestInputDecodesNumberAndBoolToString(t *testing.T) {
	src := `
workflow "x" {
  adapter "shell" "default" {}
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
  step "run" {
    adapter = adapter.shell.default
    input {
      command = "echo hi"
    }
    outcome "success" { transition_to = "done" }
    outcome "failure" { transition_to = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	if g.Steps["run"].Input["command"] != "echo hi" {
		t.Errorf("unexpected command: %q", g.Steps["run"].Input["command"])
	}
}

func TestInputTypeMismatch_StringForNumber(t *testing.T) {
	// max_turns is declared as ConfigFieldNumber; passing a string should fail.
	src := `
workflow "x" {
  adapter "copilot" "default" {}
  version       = "0.1"
  initial_state = "open"
  target_state  = "done"
  step "open" {
    adapter = adapter.copilot.default
    lifecycle = "open"
    outcome "success" { transition_to = "run" }
  }
  step "run" {
    adapter = adapter.copilot.default
    input {
      prompt    = "do work"
      max_turns = "not-a-number"
    }
    outcome "success" { transition_to = "close" }
  }
  step "close" {
    adapter = adapter.copilot.default
    lifecycle = "close"
    outcome "success" { transition_to = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, testSchemas)
	if !diags.HasErrors() {
		t.Fatal("expected type mismatch error for string passed to number field")
	}
	if !strings.Contains(diags.Error(), `field "max_turns" must be a number`) {
		t.Errorf("unexpected error: %s", diags.Error())
	}
}

func TestInputTypeMismatch_NumberForString(t *testing.T) {
	// prompt is declared as ConfigFieldString; passing a number should fail.
	src := `
workflow "x" {
  adapter "copilot" "default" {}
  version       = "0.1"
  initial_state = "open"
  target_state  = "done"
  step "open" {
    adapter = adapter.copilot.default
    lifecycle = "open"
    outcome "success" { transition_to = "run" }
  }
  step "run" {
    adapter = adapter.copilot.default
    input {
      prompt = 42
    }
    outcome "success" { transition_to = "close" }
  }
  step "close" {
    adapter = adapter.copilot.default
    lifecycle = "close"
    outcome "success" { transition_to = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, testSchemas)
	if !diags.HasErrors() {
		t.Fatal("expected type mismatch error for number passed to string field")
	}
	if !strings.Contains(diags.Error(), `field "prompt" must be a string`) {
		t.Errorf("unexpected error: %s", diags.Error())
	}
}

func TestInputListStringAcceptsStringTupleLiteral(t *testing.T) {
	src := `
workflow "x" {
  adapter "listy" "default" {}
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
  step "run" {
    adapter = adapter.listy.default
    input {
      items = ["a", "b"]
    }
    outcome "success" { transition_to = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, testSchemas)
	if diags.HasErrors() {
		t.Fatalf("expected list_string tuple literal to pass: %s", diags.Error())
	}
}

func TestInputListStringRejectsMixedTupleLiteral(t *testing.T) {
	src := `
workflow "x" {
  adapter "listy" "default" {}
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
  step "run" {
    adapter = adapter.listy.default
    input {
      items = ["a", 1]
    }
    outcome "success" { transition_to = "done" }
  }
  state "done" { terminal = true }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, testSchemas)
	if !diags.HasErrors() {
		t.Fatal("expected list_string type mismatch for mixed tuple")
	}
	if !strings.Contains(diags.Error(), `field "items" must be a list of strings`) {
		t.Errorf("unexpected error: %s", diags.Error())
	}
}
