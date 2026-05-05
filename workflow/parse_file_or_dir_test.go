package workflow

import (
	"os"
	"path/filepath"
	"testing"
)

// TestParseFileOrDir_FilePathParsesFile verifies that a path pointing to a
// single .hcl file is parsed directly (not expanded to the parent directory)
// and requires a workflow header block.
func TestParseFileOrDir_FilePathParsesFile(t *testing.T) {
	dir := t.TempDir()

	// Write a complete single-file workflow.
	filePath := filepath.Join(dir, "main.hcl")
	if err := os.WriteFile(filePath, []byte(singleFileContent), 0o644); err != nil {
		t.Fatalf("write main.hcl: %v", err)
	}

	spec, diags := ParseFileOrDir(filePath)
	if diags.HasErrors() {
		t.Fatalf("ParseFileOrDir(file): %s", diags.Error())
	}
	if spec == nil || spec.Header == nil {
		t.Fatal("expected non-nil spec with Header")
	}
	if spec.Header.Name != "test" {
		t.Errorf("Header.Name = %q, want %q", spec.Header.Name, "test")
	}
	if len(spec.Steps) != 1 {
		t.Errorf("expected 1 step, got %d", len(spec.Steps))
	}
}

// TestParseFileOrDir_FilePathNoWorkflowBlock_Error verifies that passing a
// file path to a file with no workflow header block returns an error — a file
// path cannot act as a content-only file.
func TestParseFileOrDir_FilePathNoWorkflowBlock_Error(t *testing.T) {
	dir := t.TempDir()

	contentOnly := filepath.Join(dir, "content.hcl")
	if err := os.WriteFile(contentOnly, []byte(`step "run" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
}
`), 0o644); err != nil {
		t.Fatalf("write content.hcl: %v", err)
	}

	_, diags := ParseFileOrDir(contentOnly)
	if !diags.HasErrors() {
		t.Fatal("expected error when file has no workflow block")
	}
}

// TestParseFileOrDir_DirPathDelegatesToParseDir verifies that a directory path
// delegates to ParseDir, merging all .hcl files in the directory.
func TestParseFileOrDir_DirPathDelegatesToParseDir(t *testing.T) {
	dir := t.TempDir()

	writeHCLFile(t, dir, "workflow", `workflow "dir_test" {
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}
`)
	writeHCLFile(t, dir, "content", `adapter "noop" "default" {}

step "run" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
}

state "done" { terminal = true }
`)

	spec, diags := ParseFileOrDir(dir)
	if diags.HasErrors() {
		t.Fatalf("ParseFileOrDir(dir): %s", diags.Error())
	}
	if spec == nil || spec.Header == nil {
		t.Fatal("expected non-nil spec with Header")
	}
	if spec.Header.Name != "dir_test" {
		t.Errorf("Header.Name = %q, want %q", spec.Header.Name, "dir_test")
	}
	if len(spec.Steps) != 1 {
		t.Errorf("expected 1 step from merged dir, got %d", len(spec.Steps))
	}
}

// TestParseFileOrDir_NonexistentPath_Error verifies that a path that does not
// exist returns a descriptive error.
func TestParseFileOrDir_NonexistentPath_Error(t *testing.T) {
	_, diags := ParseFileOrDir("/nonexistent-criteria-parsedir-xyz/file.hcl")
	if !diags.HasErrors() {
		t.Fatal("expected error for non-existent path")
	}
}
