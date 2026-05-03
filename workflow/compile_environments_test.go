package workflow

// compile_environments_test.go — unit tests for environment block compilation.

import (
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
)

// environmentWorkflow wraps environment and step blocks into a minimal compilable workflow HCL.
func environmentWorkflow(envBlocks, extraHeaderAttrs string) string {
	header := `workflow "test" {
  version       = "0.1"
  initial_state = "done"
  target_state  = "done"
` + extraHeaderAttrs

	body := `
  state "done" {
    terminal = true
    success  = true
  }
` + envBlocks + `
}
`
	return header + body
}

func TestCompileEnvironments_Single(t *testing.T) {
	// Single environment block should compile without error.
	src := environmentWorkflow(`
  environment "shell" "default" {
    variables = {
      CI = "true"
      LOG_LEVEL = "debug"
    }
  }
`, "")
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}

	env, ok := g.Environments["shell.default"]
	if !ok {
		t.Fatal("environment shell.default not found in graph")
	}

	if env.Type != "shell" {
		t.Errorf("expected type 'shell', got %q", env.Type)
	}
	if env.Name != "default" {
		t.Errorf("expected name 'default', got %q", env.Name)
	}

	if env.Variables["CI"] != "true" {
		t.Errorf("expected CI=true, got %q", env.Variables["CI"])
	}
	if env.Variables["LOG_LEVEL"] != "debug" {
		t.Errorf("expected LOG_LEVEL=debug, got %q", env.Variables["LOG_LEVEL"])
	}

	// Verify that single environment becomes the default.
	if g.DefaultEnvironment != "shell.default" {
		t.Errorf("expected default environment 'shell.default', got %q", g.DefaultEnvironment)
	}
}

func TestCompileEnvironments_DuplicateTypeAndName(t *testing.T) {
	// Duplicate <type>.<name> should error.
	src := environmentWorkflow(`
  environment "shell" "default" {
    variables = { X = "1" }
  }
  environment "shell" "default" {
    variables = { Y = "2" }
  }
`, "")
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	diags = nil // Reset diags to capture compile result
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected error for duplicate environment")
	}
}

func TestCompileEnvironments_UnknownType(t *testing.T) {
	// Unknown environment type should error.
	src := environmentWorkflow(`
  environment "docker" "default" {
  }
`, "")
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	diags = nil // Reset diags to capture compile result
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected error for unknown environment type")
	}
	if !strings.Contains(diags.Error(), "not registered") {
		t.Errorf("expected 'not registered' in error, got: %v", diags.Error())
	}
}

func TestCompileEnvironments_InvalidName(t *testing.T) {
	// Names starting with digits should error.
	src := environmentWorkflow(`
  environment "shell" "123invalid" {
  }
`, "")
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	diags = nil // Reset diags to capture compile result
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected error for invalid name starting with digit")
	}
}

func TestCompileEnvironments_ValidNameFormats(t *testing.T) {
	// Valid names: letters, underscores, hyphens.
	validNames := []string{"dev", "prod_1", "my-env"}
	for _, name := range validNames {
		src := environmentWorkflow(`
  environment "shell" "`+name+`" {
    variables = { X = "1" }
  }
`, "")
		spec, diags := Parse("test.hcl", []byte(src))
		if diags.HasErrors() {
			t.Fatalf("parse(%s): %s", name, diags.Error())
		}
		g, diags := Compile(spec, nil)
		if diags.HasErrors() {
			t.Fatalf("compile(%s): %s", name, diags.Error())
		}
		key := "shell." + name
		if _, ok := g.Environments[key]; !ok {
			t.Errorf("environment %s not found", key)
		}
	}
}

func TestCompileEnvironments_VariablesFold(t *testing.T) {
	// Variables expression should fold at compile time.
	src := environmentWorkflow(`
  environment "shell" "test" {
    variables = {
      X = "42"
      Y = "true"
      Z = 99
    }
  }
`, "")
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}

	env := g.Environments["shell.test"]
	if env.Variables["X"] != "42" {
		t.Errorf("expected X=42, got %q", env.Variables["X"])
	}
	if env.Variables["Y"] != "true" {
		t.Errorf("expected Y=true, got %q", env.Variables["Y"])
	}
	if env.Variables["Z"] != "99" {
		t.Errorf("expected Z=99, got %q", env.Variables["Z"])
	}
}

func TestCompileEnvironments_VariablesRuntimeRef(t *testing.T) {
	// Variables with runtime references should error.
	src := environmentWorkflow(`
  environment "shell" "test" {
    variables = {
      X = each.value
    }
  }
`, "")
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	diags = nil // Reset diags to capture compile result
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected error for runtime reference in variables")
	}
}

func TestCompileEnvironments_ConfigFold(t *testing.T) {
	// Config expression should fold at compile time.
	src := environmentWorkflow(`
  environment "shell" "test" {
    config = {
      foo = "bar"
      num = 42
      nested = {
        key = "value"
      }
    }
  }
`, "")
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}

	env := g.Environments["shell.test"]
	if env.Config == nil {
		t.Fatal("config is nil")
	}

	if env.Config["foo"].AsString() != "bar" {
		t.Errorf("expected foo=bar, got %v", env.Config["foo"])
	}
}

func TestCompileEnvironments_MultipleNoDefault(t *testing.T) {
	// Multiple environments without explicit default should not error at compile,
	// but DefaultEnvironment should be empty.
	src := environmentWorkflow(`
  environment "shell" "dev" {
  }
  environment "shell" "prod" {
  }
`, "")
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}

	// Multiple environments with no explicit default should not set a default.
	if g.DefaultEnvironment != "" {
		t.Errorf("expected empty default environment, got %q", g.DefaultEnvironment)
	}
}

func TestCompileEnvironments_ExplicitDefault(t *testing.T) {
	// Explicit default environment should be respected.
	src := environmentWorkflow(`
  environment "shell" "dev" {
  }
  environment "shell" "prod" {
  }
`, `  environment = "shell.prod"
`)
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}

	if g.DefaultEnvironment != "shell.prod" {
		t.Errorf("expected default environment 'shell.prod', got %q", g.DefaultEnvironment)
	}
}

func TestCompileEnvironments_NonexistentDefault(t *testing.T) {
	// Explicit default referencing a non-existent environment should error.
	src := environmentWorkflow(`
  environment "shell" "real" {
  }
`, `  environment = "shell.nonexistent"
`)
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	diags = nil // Reset diags to capture compile result
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected error for nonexistent default environment")
	}
}

func TestCompileEnvironments_WithVariablesAndConfig(t *testing.T) {
	// Environment with both variables and config.
	src := environmentWorkflow(`
  environment "shell" "complete" {
    variables = {
      ENV_NAME = "prod"
      DEBUG = "false"
    }
    config = {
      timeout_seconds = 300
      retry_limit = 3
    }
  }
`, "")
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}

	env := g.Environments["shell.complete"]
	if env.Variables["ENV_NAME"] != "prod" {
		t.Errorf("expected ENV_NAME=prod, got %q", env.Variables["ENV_NAME"])
	}
	if !env.Config["timeout_seconds"].RawEquals(cty.NumberIntVal(300)) {
		t.Errorf("expected timeout_seconds=300, got %v", env.Config["timeout_seconds"])
	}
}

func TestCompileEnvironments_Empty(t *testing.T) {
	// Workflow with no environment blocks should compile fine.
	src := environmentWorkflow("", "")
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}

	if len(g.Environments) != 0 {
		t.Errorf("expected no environments, got %d", len(g.Environments))
	}
	if g.DefaultEnvironment != "" {
		t.Errorf("expected empty default, got %q", g.DefaultEnvironment)
	}
}

func TestCompileEnvironments_ControlledSetConflictWarning(t *testing.T) {
	// Environment with variables that conflict with shell's controlled set should produce warnings.
	src := environmentWorkflow(`
  environment "shell" "prod" {
    variables = {
      PATH = "/custom/bin"
      HOME = "/tmp"
      X    = "y"
    }
  }
`, "")
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	// Should compile successfully but with warnings
	if diags.HasErrors() {
		t.Fatalf("compile had errors: %s", diags.Error())
	}

	// Should have warnings for PATH and HOME
	hasPathWarning := false
	hasHomeWarning := false
	for _, d := range diags {
		if d.Severity == hcl.DiagWarning {
			if strings.Contains(d.Summary, "PATH") {
				hasPathWarning = true
			}
			if strings.Contains(d.Summary, "HOME") {
				hasHomeWarning = true
			}
		}
	}

	if !hasPathWarning {
		t.Error("expected warning for PATH conflict")
	}
	if !hasHomeWarning {
		t.Error("expected warning for HOME conflict")
	}

	// Environment should still be stored with the conflicting variables
	if env, ok := g.Environments["shell.prod"]; !ok {
		t.Fatal("environment shell.prod not found")
	} else if env.Variables["PATH"] != "/custom/bin" || env.Variables["HOME"] != "/tmp" {
		t.Error("environment variables not stored correctly")
	}
}
