package workflow_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"

	"github.com/brokenbots/criteria/workflow"
)

// compileWorkflowInDir writes hcl content to a temp dir and compiles it with
// CompileWithOpts so the compile-time file() validation path is exercised.
func compileWorkflowInDir(t *testing.T, dir, hclContent string) hcl.Diagnostics {
	t.Helper()
	path := filepath.Join(dir, "test.hcl")
	if err := os.WriteFile(path, []byte(hclContent), 0o644); err != nil {
		t.Fatalf("write test.hcl: %v", err)
	}
	spec, diags := workflow.Parse(path, []byte(hclContent))
	if diags.HasErrors() {
		return diags
	}
	_, compileDiags := workflow.CompileWithOpts(spec, nil, workflow.CompileOpts{WorkflowDir: dir})
	return compileDiags
}

// minimalWorkflowHCL is a minimal valid workflow that uses file() with the
// given path in the input block of a step.
func minimalWorkflowWithFile(filePath string) string {
	return `workflow "test" {
  version       = "0.1"
  initial_state = "step1"
  target_state  = "done"

  state "done" {
    terminal = true
    success  = true
  }

  agent "a" { adapter = "noop" }

  step "step1" {
    agent = "a"
    input {
      prompt = file("` + filePath + `")
    }
    outcome "success" { transition_to = "done" }
  }
}
`
}

// Test 14: compile-time validation rejects a missing file in a constant literal file() call.
func TestCompileFileFunctionValidation_MissingFile(t *testing.T) {
	dir := t.TempDir()
	hclContent := minimalWorkflowWithFile("missing.txt")
	diags := compileWorkflowInDir(t, dir, hclContent)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for missing file; got none")
	}
	errStr := diags.Error()
	if !strings.Contains(errStr, "no such file") && !strings.Contains(errStr, "missing.txt") {
		t.Errorf("compile error %q should mention the missing file", errStr)
	}
	if diags[0].Subject == nil {
		t.Error("compile diagnostic must carry a source range (Subject must not be nil)")
	}
}

// Test 15: compile-time validation passes for a file() call with an existing file.
func TestCompileFileFunctionValidation_ExistingFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hclContent := minimalWorkflowWithFile("prompt.txt")
	diags := compileWorkflowInDir(t, dir, hclContent)
	if diags.HasErrors() {
		t.Fatalf("unexpected compile error: %s", diags.Error())
	}
}

// Test 16: compile-time validation skips file() calls with variable references.
func TestCompileFileFunctionValidation_SkipsVariableArgs(t *testing.T) {
	dir := t.TempDir()
	// The file "some.txt" does NOT exist, but since the arg contains
	// a variable reference, compile-time validation must skip it.
	hclContent := `workflow "test" {
  version       = "0.1"
  initial_state = "step1"
  target_state  = "done"

  state "done" {
    terminal = true
    success  = true
  }

  variable "path" {
    type    = "string"
    default = "some.txt"
  }

  agent "a" { adapter = "noop" }

  step "step1" {
    agent = "a"
    input {
      prompt = file(var.path)
    }
    outcome "success" { transition_to = "done" }
  }
}
`
	diags := compileWorkflowInDir(t, dir, hclContent)
	if diags.HasErrors() {
		t.Fatalf("compile-time validation should not reject variable-arg file() calls: %s", diags.Error())
	}
}

// Test 17 (R9): compile-time validation rejects an absolute path with the same error as runtime.
func TestCompileFileFunctionValidation_AbsolutePath(t *testing.T) {
	dir := t.TempDir()
	hclContent := minimalWorkflowWithFile("/etc/passwd")
	diags := compileWorkflowInDir(t, dir, hclContent)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for absolute path; got none")
	}
	if !strings.Contains(diags.Error(), "absolute paths are not supported") {
		t.Errorf("compile error %q should mention 'absolute paths are not supported', not 'no such file'", diags.Error())
	}
}
