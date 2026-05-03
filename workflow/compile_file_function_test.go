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

// Test 16: file(var.path) is now validated at compile time when the var has a
// known default value. Behavior change from W07: previously variable args were
// skipped; now the fold pass resolves them and validates the file path.
func TestCompileFileFunctionValidation_VarArgFileExists(t *testing.T) {
	dir := t.TempDir()
	// Create "some.txt" so the fold-time file() validation succeeds.
	if err := os.WriteFile(filepath.Join(dir, "some.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
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
		t.Fatalf("unexpected compile error for file(var.path) with existing file: %s", diags.Error())
	}
}

// Test 16b: file(var.path) errors at compile when the resolved file does not exist.
// This is the new behavior from W07: variable-arg file() calls are now validated
// at compile time when the variable has a fold-time value.
func TestCompileFileFunctionValidation_VarArgFileMissing(t *testing.T) {
	dir := t.TempDir()
	// "some.txt" does NOT exist in dir — fold-time file() validation should catch it.
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
	if !diags.HasErrors() {
		t.Fatal("expected compile error for file(var.path) with missing file; got none")
	}
	if !strings.Contains(diags.Error(), "some.txt") {
		t.Errorf("compile error %q should reference the missing file", diags.Error())
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
