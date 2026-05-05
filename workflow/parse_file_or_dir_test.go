package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseFileOrDir_FilePath_DelegatesToParentDir verifies that when a file
// path is given and the parent directory is a proper module (single workflow
// header across all .hcl files), ParseFileOrDir merges all sibling files.
func TestParseFileOrDir_FilePath_DelegatesToParentDir(t *testing.T) {
	dir := t.TempDir()

	// workflow.hcl — the file we'll reference by path
	writeHCLFile(t, dir, "workflow", `workflow "multi" {
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}
`)
	// steps.hcl — a sibling file that must be merged in
	writeHCLFile(t, dir, "steps", `adapter "noop" "default" {}

step "run" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
}

state "done" { terminal = true }
`)

	// Pass the file path, not the directory.
	filePath := filepath.Join(dir, "workflow.hcl")
	spec, diags := ParseFileOrDir(filePath)
	if diags.HasErrors() {
		t.Fatalf("ParseFileOrDir(file): %s", diags.Error())
	}
	if spec == nil || spec.Header == nil {
		t.Fatal("expected non-nil spec with Header")
	}
	if spec.Header.Name != "multi" {
		t.Errorf("Header.Name = %q, want %q", spec.Header.Name, "multi")
	}
	// The step from steps.hcl must be merged in.
	if len(spec.Steps) != 1 {
		t.Errorf("expected 1 step from merged dir, got %d", len(spec.Steps))
	}
}

// TestParseFileOrDir_FilePath_SingleFileDir verifies that a file alone in its
// directory works correctly when referenced by file path.
func TestParseFileOrDir_FilePath_SingleFileDir(t *testing.T) {
	dir := t.TempDir()

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

// TestParseFileOrDir_DirPath verifies that a directory path delegates to ParseDir,
// merging all .hcl files in the directory.
func TestParseFileOrDir_DirPath(t *testing.T) {
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

// TestParseFileOrDir_NonHCLFile_Error verifies that a regular file without a
// .hcl suffix is rejected with a descriptive error rather than silently
// succeeding by parsing the parent directory.
func TestParseFileOrDir_NonHCLFile_Error(t *testing.T) {
	dir := t.TempDir()

	// Create a valid workflow directory alongside a non-.hcl file.
	writeHCLFile(t, dir, "workflow", `workflow "test" {
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}
`)
	notesPath := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(notesPath, []byte("some notes"), 0o644); err != nil {
		t.Fatalf("write notes.txt: %v", err)
	}

	_, diags := ParseFileOrDir(notesPath)
	if !diags.HasErrors() {
		t.Fatal("expected error for non-.hcl file path")
	}
	found := false
	for _, d := range diags {
		if strings.Contains(d.Detail, ".hcl") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected diagnostic detail mentioning .hcl requirement, got: %s", diags.Error())
	}
}

// TestParseFileOrDir_FilePath_FallsBackToSingleFileWhenParentHasMultipleHeaders
// verifies that when the parent directory contains multiple independent workflow
// files (each with their own singleton blocks — workflow, policy, etc.),
// ParseFileOrDir falls back to parsing only the named file — rather than
// failing with "duplicate workflow block" from the directory merge attempt.
func TestParseFileOrDir_FilePath_FallsBackToSingleFileWhenParentHasMultipleHeaders(t *testing.T) {
	dir := t.TempDir()

	// Two complete, independent workflows in the same directory.
	if err := os.WriteFile(filepath.Join(dir, "wf_a.hcl"), []byte(singleFileContent), 0o644); err != nil {
		t.Fatalf("write wf_a.hcl: %v", err)
	}
	bContent := `workflow "other" {
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}

adapter "noop" "default" {}

step "run" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
}

state "done" { terminal = true }
`
	if err := os.WriteFile(filepath.Join(dir, "wf_b.hcl"), []byte(bContent), 0o644); err != nil {
		t.Fatalf("write wf_b.hcl: %v", err)
	}

	// Passing wf_a.hcl should parse it standalone (not fail due to sibling header).
	spec, diags := ParseFileOrDir(filepath.Join(dir, "wf_a.hcl"))
	if diags.HasErrors() {
		t.Fatalf("expected fallback to single-file parse, got error: %s", diags.Error())
	}
	if spec == nil || spec.Header == nil {
		t.Fatal("expected non-nil spec with Header")
	}
	if spec.Header.Name != "test" {
		t.Errorf("Header.Name = %q, want %q", spec.Header.Name, "test")
	}
}
