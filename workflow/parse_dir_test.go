package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeHCLFile writes content to path/name.hcl and fails the test on error.
func writeHCLFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name+".hcl"), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s.hcl: %v", name, err)
	}
}

const singleFileContent = `workflow "test" {
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

// TestParseDir_SingleFile verifies that a directory with one complete .hcl file
// produces a valid Spec with the expected header and content.
func TestParseDir_SingleFile(t *testing.T) {
	dir := t.TempDir()
	writeHCLFile(t, dir, "main", singleFileContent)

	spec, diags := ParseDir(dir)
	if diags.HasErrors() {
		t.Fatalf("ParseDir: %s", diags.Error())
	}
	if spec == nil {
		t.Fatal("expected non-nil spec")
	}
	if spec.Header == nil {
		t.Fatal("expected non-nil Header")
	}
	if spec.Header.Name != "test" {
		t.Errorf("Header.Name = %q, want %q", spec.Header.Name, "test")
	}
	if spec.Header.InitialState != "run" {
		t.Errorf("Header.InitialState = %q, want %q", spec.Header.InitialState, "run")
	}
	if len(spec.Steps) != 1 {
		t.Errorf("expected 1 step, got %d", len(spec.Steps))
	}
	if len(spec.States) != 1 {
		t.Errorf("expected 1 state, got %d", len(spec.States))
	}
	if len(spec.Adapters) != 1 {
		t.Errorf("expected 1 adapter, got %d", len(spec.Adapters))
	}
}

// TestParseDir_MultipleFiles verifies that multiple .hcl files in a directory
// are merged into a single Spec: slice fields concatenated, singleton Header
// preserved from the one file that declares it.
func TestParseDir_MultipleFiles(t *testing.T) {
	dir := t.TempDir()

	writeHCLFile(t, dir, "workflow", `workflow "multi" {
  version       = "0.1"
  initial_state = "step_a"
  target_state  = "done"
}
`)
	writeHCLFile(t, dir, "adapters", `adapter "noop" "default" {}
`)
	writeHCLFile(t, dir, "steps", `step "step_a" {
  target = adapter.noop.default
  outcome "success" { next = "step_b" }
}

step "step_b" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
}
`)
	writeHCLFile(t, dir, "states", `state "done" { terminal = true }
`)

	spec, diags := ParseDir(dir)
	if diags.HasErrors() {
		t.Fatalf("ParseDir: %s", diags.Error())
	}
	if spec.Header == nil || spec.Header.Name != "multi" {
		t.Errorf("Header.Name = %q, want %q", spec.Header.Name, "multi")
	}
	if len(spec.Steps) != 2 {
		t.Errorf("expected 2 steps after merge, got %d", len(spec.Steps))
	}
	if len(spec.States) != 1 {
		t.Errorf("expected 1 state after merge, got %d", len(spec.States))
	}
	if len(spec.Adapters) != 1 {
		t.Errorf("expected 1 adapter after merge, got %d", len(spec.Adapters))
	}
	// SourceBytes should be non-empty and contain content from multiple files.
	if len(spec.SourceBytes) == 0 {
		t.Error("expected non-empty SourceBytes after multi-file merge")
	}
}

// TestParseDir_NoHCLFiles_Error verifies that a directory with no .hcl files
// produces a clear diagnostic error.
func TestParseDir_NoHCLFiles_Error(t *testing.T) {
	dir := t.TempDir()
	// Write a non-.hcl file to ensure the directory is not empty.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# hello\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}

	_, diags := ParseDir(dir)
	if !diags.HasErrors() {
		t.Fatal("expected error for directory with no .hcl files")
	}
	if !strings.Contains(diags.Error(), "no .hcl files") {
		t.Errorf("expected 'no .hcl files' in error, got: %s", diags.Error())
	}
}

// TestParseDir_DirNotExist_Error verifies that passing a non-existent directory
// produces a diagnostic error.
func TestParseDir_DirNotExist_Error(t *testing.T) {
	_, diags := ParseDir("/nonexistent-criteria-test-dir-xyz")
	if !diags.HasErrors() {
		t.Fatal("expected error for non-existent directory")
	}
	if !strings.Contains(diags.Error(), "cannot read workflow directory") {
		t.Errorf("expected 'cannot read workflow directory' in error, got: %s", diags.Error())
	}
}

// TestParseDir_DuplicateStepAcrossFiles_Error verifies that the same step name
// declared in two different files produces a diagnostic with the step name and
// carries file:line source locations in Subject/Detail.
func TestParseDir_DuplicateStepAcrossFiles_Error(t *testing.T) {
	dir := t.TempDir()

	writeHCLFile(t, dir, "main", `workflow "dup" {
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
`)
	// steps2.hcl declares the same step "run" → merge collision.
	writeHCLFile(t, dir, "steps2", `step "run" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
}
`)

	_, diags := ParseDir(dir)
	if !diags.HasErrors() {
		t.Fatal("expected error for duplicate step name across files")
	}
	if !strings.Contains(diags.Error(), `duplicate step name "run"`) {
		t.Errorf("expected 'duplicate step name' in error, got: %s", diags.Error())
	}

	// The diagnostic must carry a Subject with a file:line location pointing to
	// the second declaration (steps2.hcl), and the Detail must mention the first
	// file (main.hcl) as "previously declared at ...".
	var found bool
	for _, d := range diags {
		if !strings.Contains(d.Summary, `duplicate step name "run"`) {
			continue
		}
		found = true
		if d.Subject == nil {
			t.Errorf("diagnostic Subject is nil; expected a file:line location for the second declaration")
		} else if !strings.Contains(d.Subject.Filename, "steps2.hcl") {
			t.Errorf("Subject.Filename = %q; expected it to contain 'steps2.hcl'", d.Subject.Filename)
		}
		if !strings.Contains(d.Detail, "previously declared at") {
			t.Errorf("Detail = %q; expected 'previously declared at' with the first-file location", d.Detail)
		}
		if !strings.Contains(d.Detail, "main.hcl") {
			t.Errorf("Detail = %q; expected 'main.hcl' to appear as the first-declaration location", d.Detail)
		}
	}
	if !found {
		t.Error("no diagnostic matched 'duplicate step name \"run\"'")
	}
}

// TestParseDir_DuplicateWorkflowBlock_Error verifies that two files each
// declaring a workflow header block produce a "duplicate workflow block" error.
func TestParseDir_DuplicateWorkflowBlock_Error(t *testing.T) {
	dir := t.TempDir()

	writeHCLFile(t, dir, "a", `workflow "a" {
  version       = "0.1"
  initial_state = "done"
  target_state  = "done"
}
state "done" { terminal = true }
`)
	writeHCLFile(t, dir, "b", `workflow "b" {
  version       = "0.1"
  initial_state = "done"
  target_state  = "done"
}
`)

	_, diags := ParseDir(dir)
	if !diags.HasErrors() {
		t.Fatal("expected error for duplicate workflow blocks")
	}
	if !strings.Contains(diags.Error(), "duplicate workflow block") {
		t.Errorf("expected 'duplicate workflow block' in error, got: %s", diags.Error())
	}
}

// TestParseDir_NoWorkflowBlock_Error verifies that a directory whose files
// contain no workflow header block produces a "no workflow block" error.
func TestParseDir_NoWorkflowBlock_Error(t *testing.T) {
	dir := t.TempDir()

	// Only content files — no workflow header.
	writeHCLFile(t, dir, "steps", `adapter "noop" "default" {}

step "run" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
}
state "done" { terminal = true }
`)

	_, diags := ParseDir(dir)
	if !diags.HasErrors() {
		t.Fatal("expected error when no workflow block is present")
	}
	if !strings.Contains(diags.Error(), "no workflow block") {
		t.Errorf("expected 'no workflow block' in error, got: %s", diags.Error())
	}
}

// TestParseDir_DiagnosticsHaveCorrectFilenameSubjects verifies that parse errors
// in individual files within a directory carry the correct filename in their subject
// range (not just a generic subject).
func TestParseDir_DiagnosticsHaveCorrectFilenameSubjects(t *testing.T) {
	dir := t.TempDir()

	// bad.hcl: intentionally malformed HCL.
	if err := os.WriteFile(filepath.Join(dir, "bad.hcl"), []byte("this is not valid HCL {\n"), 0o644); err != nil {
		t.Fatalf("write bad.hcl: %v", err)
	}

	_, diags := ParseDir(dir)
	if !diags.HasErrors() {
		t.Fatal("expected error for malformed HCL")
	}
	// At least one diagnostic should reference the bad.hcl file.
	found := false
	for _, d := range diags {
		if d.Subject != nil && strings.Contains(d.Subject.Filename, "bad.hcl") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected at least one diagnostic with Subject.Filename containing 'bad.hcl'; got: %s", diags.Error())
	}
}
