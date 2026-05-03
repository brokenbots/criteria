package workflow

// compile_locals_test.go — unit tests for compileLocals in compile_locals.go.

import (
	"strings"
	"testing"
)

// localWorkflow wraps a snippet into a minimal compilable workflow HCL.
func localWorkflow(localBlocks, extraBlocks string) string {
	return `workflow "test" {
  version       = "0.1"
  initial_state = "done"
  target_state  = "done"

  state "done" {
    terminal = true
    success  = true
  }
` + extraBlocks + localBlocks + `
}
`
}

func TestCompileLocals_Simple(t *testing.T) {
	src := localWorkflow(`
  local "greeting" {
    value = "hello"
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
	if len(g.Locals) != 1 {
		t.Fatalf("expected 1 local, got %d", len(g.Locals))
	}
	ln, ok := g.Locals["greeting"]
	if !ok {
		t.Fatal("local \"greeting\" not found in g.Locals")
	}
	if ln.Value.AsString() != "hello" {
		t.Errorf("expected value \"hello\", got %s", ln.Value.AsString())
	}
}

func TestCompileLocals_DependsOnVar(t *testing.T) {
	src := localWorkflow(`
  local "full_path" {
    value = "/base/${var.name}"
  }
`, `
  variable "name" {
    type    = "string"
    default = "world"
  }
`)
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	ln, ok := g.Locals["full_path"]
	if !ok {
		t.Fatal("local \"full_path\" not found")
	}
	if ln.Value.AsString() != "/base/world" {
		t.Errorf("expected \"/base/world\", got %q", ln.Value.AsString())
	}
}

func TestCompileLocals_DependsOnEarlierLocal(t *testing.T) {
	src := localWorkflow(`
  local "base" {
    value = "hello"
  }
  local "full" {
    value = "${local.base} world"
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
	ln, ok := g.Locals["full"]
	if !ok {
		t.Fatal("local \"full\" not found")
	}
	if ln.Value.AsString() != "hello world" {
		t.Errorf("expected \"hello world\", got %q", ln.Value.AsString())
	}
}

func TestCompileLocals_Cycle(t *testing.T) {
	// a depends on b, b depends on a — a cycle.
	src := localWorkflow(`
  local "a" {
    value = local.b
  }
  local "b" {
    value = local.a
  }
`, "")
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for local cycle; got none")
	}
	errStr := diags.Error()
	if !strings.Contains(errStr, "cycle") {
		t.Errorf("expected error to mention 'cycle', got %q", errStr)
	}
	if !strings.Contains(errStr, `"a"`) || !strings.Contains(errStr, `"b"`) {
		t.Errorf("expected error to list both cycle participants, got %q", errStr)
	}
}

func TestCompileLocals_MultipleAttrs(t *testing.T) {
	src := localWorkflow(`
  local "bad" {
    value = "ok"
    extra = "not allowed"
  }
`, "")
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for extra attribute; got none")
	}
}

func TestCompileLocals_NoValueAttr(t *testing.T) {
	src := localWorkflow(`
  local "empty" {
  }
`, "")
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for missing value; got none")
	}
	if !strings.Contains(diags.Error(), "value") {
		t.Errorf("expected error to mention 'value', got %q", diags.Error())
	}
}

func TestCompileLocals_RuntimeRef(t *testing.T) {
	// local.x = steps.foo.out is a compile error — locals must fold.
	src := `workflow "test" {
  version       = "0.1"
  initial_state = "step1"
  target_state  = "done"

  state "done" {
    terminal = true
    success  = true
  }

  agent "a" { adapter = "noop" }

  local "bad" {
    value = steps.step1.out
  }

  step "step1" {
    agent = "a"
    outcome "success" { transition_to = "done" }
  }
}
`
	spec, diags := Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for runtime ref in local; got none")
	}
	if !strings.Contains(diags.Error(), "compile-time constant") {
		t.Errorf("expected error to mention 'compile-time constant', got %q", diags.Error())
	}
}

// TestCompileLocals_FileWithNoWorkflowDir verifies that a local whose value
// calls file() produces a compile error when Compile() is used without a
// WorkflowDir (i.e. the file cannot be resolved). Locals must fully resolve to
// known values at compile time; an unknown result is not allowed.
func TestCompileLocals_FileWithNoWorkflowDir(t *testing.T) {
src := `
workflow "x" {
  version       = "0.1"
  initial_state = "open"
  target_state  = "done"
  local "prompt" {
    value = file("prompt.txt")
  }
  step "open" {
    adapter   = "noop"
    lifecycle = "open"
    outcome "success" { transition_to = "done" }
  }
  state "done" { terminal = true }
}
`
spec, diags := Parse("t.hcl", []byte(src))
if diags.HasErrors() {
t.Fatalf("parse: %s", diags.Error())
}
// Compile without WorkflowDir — file() stubs return unknown, so the local
// cannot be resolved. The compile must fail.
_, diags = Compile(spec, nil)
if !diags.HasErrors() {
t.Fatal("expected compile error for local { value = file(...) } with no WorkflowDir; got none")
}
if !strings.Contains(diags.Error(), "fully resolved") {
t.Errorf("expected error to mention 'fully resolved', got %q", diags.Error())
}
}
