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

// TestParseDir_PolicyNestedInWorkflowBlock verifies that:
//   - A policy block nested inside the workflow { } block is parsed and compiled
//     correctly (the policy applies to the whole workflow).
//   - A standalone top-level policy { } block (old syntax) produces a parse error
//     because Spec no longer declares that HCL block type.
func TestParseDir_PolicyNestedInWorkflowBlock(t *testing.T) {
	t.Run("valid_nested", func(t *testing.T) {
		dir := t.TempDir()
		writeHCLFile(t, dir, "workflow", `workflow "pol" {
  version       = "0.1"
  initial_state = "done"
  target_state  = "done"

  policy {
    max_total_steps = 10
  }
}
state "done" { terminal = true }
`)
		spec, diags := ParseDir(dir)
		if diags.HasErrors() {
			t.Fatalf("unexpected error: %s", diags.Error())
		}
		if spec == nil || spec.Header == nil {
			t.Fatal("expected non-nil spec with header")
		}
		if spec.Header.Policy == nil {
			t.Fatal("expected non-nil policy in workflow header")
		}
		if spec.Header.Policy.MaxTotalSteps != 10 {
			t.Errorf("Policy.MaxTotalSteps = %d, want 10", spec.Header.Policy.MaxTotalSteps)
		}
	})

	t.Run("top_level_policy_rejected", func(t *testing.T) {
		dir := t.TempDir()
		writeHCLFile(t, dir, "workflow", `workflow "pol" {
  version       = "0.1"
  initial_state = "done"
  target_state  = "done"
}
state "done" { terminal = true }
`)
		// Standalone policy block at top level — no longer valid syntax.
		writeHCLFile(t, dir, "policy", `policy {
  max_total_steps = 10
}
`)
		_, diags := ParseDir(dir)
		if !diags.HasErrors() {
			t.Fatal("expected error for top-level standalone policy block; got none")
		}
	})
}

// TestParseDir_PermissionsMergeAndDuplicateBlock_Error verifies that:
//   - A single permissions block is merged successfully (covers the
//     first-seen permissionsRange setter path in mergeSpecs).
//   - A second permissions block produces a "duplicate permissions block" error
//     that includes the previous-declaration location.
func TestParseDir_PermissionsMergeAndDuplicateBlock_Error(t *testing.T) {
	dir := t.TempDir()

	writeHCLFile(t, dir, "workflow", `workflow "perm" {
  version       = "0.1"
  initial_state = "done"
  target_state  = "done"
}
state "done" { terminal = true }
`)
	writeHCLFile(t, dir, "perms_a", `permissions {
  allow_tools = ["read_file"]
}
`)
	writeHCLFile(t, dir, "perms_b", `permissions {
  allow_tools = ["write_file"]
}
`)

	_, diags := ParseDir(dir)
	if !diags.HasErrors() {
		t.Fatal("expected error for duplicate permissions blocks")
	}
	if !strings.Contains(diags.Error(), "duplicate permissions block") {
		t.Errorf("expected 'duplicate permissions block' in error, got: %s", diags.Error())
	}
	var found bool
	for _, d := range diags {
		if strings.Contains(d.Summary, "duplicate permissions block") {
			found = true
			if !strings.Contains(d.Detail, "previously declared at") {
				t.Errorf("Detail = %q; expected 'previously declared at' with first-file location", d.Detail)
			}
		}
	}
	if !found {
		t.Error("no diagnostic with summary 'duplicate permissions block' found")
	}
}

// TestParseDir_UnreadableFile_Error verifies that an os.ReadFile failure
// (e.g. a file that becomes unreadable between directory scan and read) is
// surfaced as a "cannot read workflow file" diagnostic.
func TestParseDir_UnreadableFile_Error(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("cannot test file-permission errors as root")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "unreadable.hcl")
	if err := os.WriteFile(path, []byte(`workflow "x" {}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	_, diags := ParseDir(dir)
	if !diags.HasErrors() {
		t.Fatal("expected error for unreadable file")
	}
	if !strings.Contains(diags.Error(), "cannot read workflow file") {
		t.Errorf("expected 'cannot read workflow file' in error, got: %s", diags.Error())
	}
}

// TestMergeSpecs_EmptyEntries verifies that mergeSpecs returns nil, nil for an
// empty entry slice (the zero-entries early-return path).
func TestMergeSpecs_EmptyEntries(t *testing.T) {
	spec, diags := mergeSpecs("/some/dir", nil)
	if spec != nil {
		t.Errorf("expected nil spec for empty entries, got: %+v", spec)
	}
	if diags != nil {
		t.Errorf("expected nil diags for empty entries, got: %s", diags.Error())
	}

	// Also test with an explicit empty slice.
	spec2, diags2 := mergeSpecs("/some/dir", []fileEntry{})
	if spec2 != nil {
		t.Errorf("expected nil spec for empty slice, got: %+v", spec2)
	}
	if diags2 != nil {
		t.Errorf("expected nil diags for empty slice, got: %s", diags2.Error())
	}
}

// TestCollectFileBlockRanges_ParseError verifies that collectFileBlockRanges
// returns nil when the source bytes cannot be parsed as HCL (the
// diags.HasErrors() early-return path).
func TestCollectFileBlockRanges_ParseError(t *testing.T) {
	got := collectFileBlockRanges([]byte("this { is not valid HCL at all !!!"), "bad.hcl")
	if got != nil {
		t.Errorf("expected nil for invalid HCL, got: %v", got)
	}
}

// TestJoinBytes_EmptyParts verifies that joinBytes returns nil for an empty
// parts slice (the defensive early-return path).
func TestJoinBytes_EmptyParts(t *testing.T) {
	got := joinBytes(nil, '\n')
	if got != nil {
		t.Errorf("expected nil for empty parts, got: %v", got)
	}
	got2 := joinBytes([][]byte{}, '\n')
	if got2 != nil {
		t.Errorf("expected nil for empty slice parts, got: %v", got2)
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
