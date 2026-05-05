package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeMultiFileWorkflow creates a temporary directory with a split workflow module:
//   - workflow.hcl:  header block only
//   - content.hcl:  adapter, step, state declarations
//
// Returns the directory path and the path to workflow.hcl.
func writeMultiFileWorkflow(t *testing.T) (dir, filePath string) {
	t.Helper()
	dir = t.TempDir()

	header := strings.TrimSpace(`
workflow "dir_mode" {
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}
`) + "\n"

	content := strings.TrimSpace(`
adapter "noop" "demo" {
  config {
    bootstrap = "true"
  }
}

step "run" {
  target = adapter.noop.demo
  input {
    prompt = "hello"
  }
  outcome "success" { next = "done" }
  outcome "failure" { next = "failed" }
}

state "done" {
  terminal = true
  success  = true
}
state "failed" {
  terminal = true
  success  = false
}
`) + "\n"

	if err := os.WriteFile(filepath.Join(dir, "workflow.hcl"), []byte(header), 0o600); err != nil {
		t.Fatalf("write workflow.hcl: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "content.hcl"), []byte(content), 0o600); err != nil {
		t.Fatalf("write content.hcl: %v", err)
	}
	filePath = filepath.Join(dir, "workflow.hcl")
	return dir, filePath
}

// TestCompileDir_DirectoryPath verifies that compileWorkflowOutput accepts a
// directory path and correctly merges all .hcl files in it.
func TestCompileDir_DirectoryPath(t *testing.T) {
	dir, _ := writeMultiFileWorkflow(t)

	out, err := compileWorkflowOutput(context.Background(), dir, "json", nil)
	if err != nil {
		t.Fatalf("compileWorkflowOutput(dir): %v", err)
	}
	if !strings.Contains(string(out), `"name": "dir_mode"`) {
		// fixed
		t.Errorf("compiled output does not contain workflow name; got: %s", string(out))
	}
}

// TestCompileDir_FilePathDelegatesToParentDir verifies that passing a .hcl file
// path causes compileWorkflowOutput to merge all sibling files in the parent dir —
// not just the named file. Without this delegation step "run" would be missing
// (it lives in content.hcl) and compilation would fail.
func TestCompileDir_FilePathDelegatesToParentDir(t *testing.T) {
	_, filePath := writeMultiFileWorkflow(t)

	out, err := compileWorkflowOutput(context.Background(), filePath, "json", nil)
	if err != nil {
		// This specific error means steps weren't merged — the blocker is still present.
		t.Fatalf("compileWorkflowOutput(file): %v", err)
	}
	if !strings.Contains(string(out), `"name": "dir_mode"`) {
		// fixed
		t.Errorf("compiled output does not contain workflow name; got: %s", string(out))
	}
}

// TestValidateDir_DirectoryPath verifies that the validate command accepts a
// directory path and merges all .hcl files in it.
func TestValidateDir_DirectoryPath(t *testing.T) {
	t.Setenv("CRITERIA_WORKFLOW_ALLOWED_PATHS", "")
	dir, _ := writeMultiFileWorkflow(t)

	cmd := NewValidateCmd()
	cmd.SetArgs([]string{dir})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("validate dir: %v", err)
	}
}

// TestValidateDir_FilePathDelegatesToParentDir verifies that validate with a
// .hcl file path loads sibling files so that references across files resolve.
// Without the file→parent-dir delegation, the step declared in content.hcl
// would be invisible when workflow.hcl is passed directly, causing the
// initial_state reference to fail with "does not refer to a declared step".
func TestValidateDir_FilePathDelegatesToParentDir(t *testing.T) {
	t.Setenv("CRITERIA_WORKFLOW_ALLOWED_PATHS", "")
	_, filePath := writeMultiFileWorkflow(t)

	cmd := NewValidateCmd()
	cmd.SetArgs([]string{filePath})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("validate file (must delegate to parent dir): %v", err)
	}
}

// TestApplyLocal_DirectoryPath verifies that runApply accepts a directory path,
// correctly resolves the workflow dir, and runs the noop adapter successfully.
func TestApplyLocal_DirectoryPath(t *testing.T) {
	pluginBin := buildNoopPluginBinary(t)
	pluginDir := t.TempDir()
	pluginPath := filepath.Join(pluginDir, "criteria-adapter-noop")
	b, err := os.ReadFile(pluginBin)
	if err != nil {
		t.Fatalf("read plugin binary: %v", err)
	}
	if err := os.WriteFile(pluginPath, b, 0o755); err != nil {
		t.Fatalf("write plugin binary: %v", err)
	}

	t.Setenv("CRITERIA_PLUGINS", pluginDir)
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())

	dir, _ := writeMultiFileWorkflow(t)

	eventsFile := filepath.Join(t.TempDir(), "events.ndjson")
	if err := runApply(context.Background(), applyOptions{
		workflowPath: dir,
		eventsPath:   eventsFile,
	}); err != nil {
		t.Fatalf("runApply(dir): %v", err)
	}

	types, err := readPayloadTypes(eventsFile)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if countPayloadType(types, "RunCompleted") != 1 {
		t.Fatalf("expected RunCompleted event, got payload types: %v", types)
	}
}

// TestApplyLocal_FilePathDelegatesToParentDir verifies that runApply with a
// file path merges all sibling files. This specifically catches the regression
// where a file path would parse only the named file and miss steps in siblings.
func TestApplyLocal_FilePathDelegatesToParentDir(t *testing.T) {
	pluginBin := buildNoopPluginBinary(t)
	pluginDir := t.TempDir()
	pluginPath := filepath.Join(pluginDir, "criteria-adapter-noop")
	b, err := os.ReadFile(pluginBin)
	if err != nil {
		t.Fatalf("read plugin binary: %v", err)
	}
	if err := os.WriteFile(pluginPath, b, 0o755); err != nil {
		t.Fatalf("write plugin binary: %v", err)
	}

	t.Setenv("CRITERIA_PLUGINS", pluginDir)
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())

	_, filePath := writeMultiFileWorkflow(t)

	eventsFile := filepath.Join(t.TempDir(), "events.ndjson")
	if err := runApply(context.Background(), applyOptions{
		workflowPath: filePath,
		eventsPath:   eventsFile,
	}); err != nil {
		t.Fatalf("runApply(file path, delegates to parent dir): %v", err)
	}

	types, err := readPayloadTypes(eventsFile)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if countPayloadType(types, "RunCompleted") != 1 {
		t.Fatalf("expected RunCompleted event, got payload types: %v", types)
	}
}

// writeFileFunctionWorkflow creates a temporary directory with:
//   - payload.txt:   a small text file referenced by file("./payload.txt")
//   - workflow.hcl:  header + adapter + state declarations
//   - content.hcl:   a step whose input.command uses file("./payload.txt")
//
// The shell adapter is used because its `command` field is validated at compile
// time via validateFoldableAttrs, so a wrong WorkflowDir causes a hard compile
// error rather than a deferred runtime failure. This makes the test
// regression-sensitive: if workflowDirFromPath returns the wrong directory,
// file() cannot resolve the path and compileWorkflowOutput returns an error.
//
// Returns (dir, path-to-workflow.hcl).
func writeFileFunctionWorkflow(t *testing.T) (dir, filePath string) {
	t.Helper()
	dir = t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "payload.txt"), []byte("echo hello-from-payload"), 0o644); err != nil {
		t.Fatalf("write payload.txt: %v", err)
	}

	header := strings.TrimSpace(`
workflow "file_func_dir_mode" {
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}

adapter "shell" "default" {
  config {}
}

state "done" {
  terminal = true
  success  = true
}
state "failed" {
  terminal = true
  success  = false
}
`) + "\n"

	content := strings.TrimSpace(`
step "run" {
  target = adapter.shell.default
  input {
    command = file("./payload.txt")
  }
  outcome "success" { next = "done" }
  outcome "failure" { next = "failed" }
}
`) + "\n"

	if err := os.WriteFile(filepath.Join(dir, "workflow.hcl"), []byte(header), 0o600); err != nil {
		t.Fatalf("write workflow.hcl: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "content.hcl"), []byte(content), 0o600); err != nil {
		t.Fatalf("write content.hcl: %v", err)
	}
	filePath = filepath.Join(dir, "workflow.hcl")
	return dir, filePath
}

// TestCompileDir_FileFunction_DirectoryPath verifies that compileWorkflowOutput
// with a directory path correctly sets WorkflowDir so that file("./payload.txt")
// in a step input resolves against the module directory. A wrong or empty
// WorkflowDir would cause validateFoldableAttrs to surface a hard compile error.
func TestCompileDir_FileFunction_DirectoryPath(t *testing.T) {
	dir, _ := writeFileFunctionWorkflow(t)

	out, err := compileWorkflowOutput(context.Background(), dir, "json", nil)
	if err != nil {
		t.Fatalf("compileWorkflowOutput(dir) with file(): %v", err)
	}
	if !strings.Contains(string(out), `"name": "file_func_dir_mode"`) {
		t.Errorf("compiled output does not contain workflow name; got: %s", string(out))
	}
}

// TestCompileDir_FileFunction_FilePath verifies that compileWorkflowOutput with
// a .hcl file path resolves WorkflowDir to the parent directory (not the file
// itself), so file("./payload.txt") still resolves correctly. This is a
// regression test for workflowDirFromPath: if it returned filepath.Dir("") or
// the wrong directory, the file() call would fail with a hard compile error.
func TestCompileDir_FileFunction_FilePath(t *testing.T) {
	_, filePath := writeFileFunctionWorkflow(t)

	out, err := compileWorkflowOutput(context.Background(), filePath, "json", nil)
	if err != nil {
		t.Fatalf("compileWorkflowOutput(file path) with file(): %v", err)
	}
	if !strings.Contains(string(out), `"name": "file_func_dir_mode"`) {
		t.Errorf("compiled output does not contain workflow name; got: %s", string(out))
	}
}
