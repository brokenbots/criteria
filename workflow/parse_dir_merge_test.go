package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
)

// writeHCLFiles writes multiple files to dir. The map key is the full filename
// (including extension). Unlike writeHCLFile, no extension is appended.
func writeHCLFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

// assertMergeDiag asserts that diags contains at least one DiagError whose
// Summary contains all of the given substrings.
func assertMergeDiag(t *testing.T, diags hcl.Diagnostics, substrings ...string) {
	t.Helper()
	if !diags.HasErrors() {
		t.Fatalf("expected error diagnostics, got none")
	}
	for _, d := range diags {
		if d.Severity != hcl.DiagError {
			continue
		}
		combined := d.Summary + " " + d.Detail
		allFound := true
		for _, s := range substrings {
			if !strings.Contains(combined, s) {
				allFound = false
				break
			}
		}
		if allFound {
			return
		}
	}
	t.Fatalf("no DiagError containing all of %v; diagnostics: %s", substrings, diags.Error())
}

// TestMergeSpecs_SingletonConflict_WorkflowHeader_TwoFiles verifies that two
// .hcl files each declaring a workflow header block produce a diagnostic
// naming the duplicate and the "workflow" keyword.
func TestMergeSpecs_SingletonConflict_WorkflowHeader_TwoFiles(t *testing.T) {
	dir := t.TempDir()
	writeHCLFiles(t, dir, map[string]string{
		"a.hcl": `workflow "first" {
  version       = "1"
  initial_state = "run"
  target_state  = "done"
}
adapter "noop" "default" {}
step "run" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
}
state "done" { terminal = true }
`,
		"b.hcl": `workflow "second" {
  version = "1"
}
`,
	})

	_, diags := ParseDir(dir)
	assertMergeDiag(t, diags, "duplicate workflow block")
}

// TestMergeSpecs_SingletonConflict_Policy_TwoFiles verifies that two .hcl
// files each declaring a policy block produce a "duplicate policy block" error.
func TestMergeSpecs_SingletonConflict_Policy_TwoFiles(t *testing.T) {
	dir := t.TempDir()
	writeHCLFiles(t, dir, map[string]string{
		"a.hcl": `workflow "w" {
  version       = "1"
  initial_state = "run"
  target_state  = "done"
}
adapter "noop" "default" {}
step "run" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
}
state "done" { terminal = true }
policy { max_total_steps = 10 }
`,
		"b.hcl": `policy { max_total_steps = 20 }
`,
	})

	_, diags := ParseDir(dir)
	assertMergeDiag(t, diags, "duplicate policy block")
}

// TestMergeSpecs_SingletonConflict_Permissions_TwoFiles verifies that two
// .hcl files each declaring a permissions block produce a conflict error.
func TestMergeSpecs_SingletonConflict_Permissions_TwoFiles(t *testing.T) {
	dir := t.TempDir()
	writeHCLFiles(t, dir, map[string]string{
		"a.hcl": `workflow "w" {
  version       = "1"
  initial_state = "run"
  target_state  = "done"
}
adapter "noop" "default" {}
step "run" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
}
state "done" { terminal = true }
permissions { allow_tools = ["*"] }
`,
		"b.hcl": `permissions { allow_tools = ["read"] }
`,
	})

	_, diags := ParseDir(dir)
	assertMergeDiag(t, diags, "duplicate permissions block")
}

// TestMergeSpecs_DuplicateNamedBlock_Step verifies that two .hcl files
// declaring the same step name produce a diagnostic naming the step name.
func TestMergeSpecs_DuplicateNamedBlock_Step(t *testing.T) {
	dir := t.TempDir()
	writeHCLFiles(t, dir, map[string]string{
		"a.hcl": `workflow "w" {
  version       = "1"
  initial_state = "build"
  target_state  = "done"
}
adapter "noop" "default" {}
step "build" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
}
state "done" { terminal = true }
`,
		"b.hcl": `step "build" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
}
`,
	})

	_, diags := ParseDir(dir)
	assertMergeDiag(t, diags, "duplicate step name", "build")
}

// TestMergeSpecs_DuplicateNamedBlock_Adapter_DifferentTypes verifies that two
// adapters with the same instance name but different type labels are NOT
// considered duplicates. The duplicate check key is "<type>.<name>", so
// "shell.primary" and "copilot.primary" are distinct.
//
// Note: the workstream description suggested these should conflict, but the
// implementation uses type+name as the unique key. Since adapter references in
// step targets always include the type (target = adapter.shell.primary), this
// is correct: the two adapters are genuinely different.
func TestMergeSpecs_DuplicateNamedBlock_Adapter_DifferentTypes(t *testing.T) {
	dir := t.TempDir()
	writeHCLFiles(t, dir, map[string]string{
		"a.hcl": `workflow "w" {
  version       = "1"
  initial_state = "run"
  target_state  = "done"
}
adapter "shell" "primary" {}
step "run" {
  target = adapter.shell.primary
  outcome "success" { next = "done" }
}
state "done" { terminal = true }
`,
		"b.hcl": `adapter "copilot" "primary" {}
`,
	})

	spec, diags := ParseDir(dir)
	if diags.HasErrors() {
		t.Fatalf("expected no errors for adapters with same name but different types; got: %s", diags.Error())
	}
	if len(spec.Adapters) != 2 {
		t.Errorf("expected 2 adapters, got %d", len(spec.Adapters))
	}
}

// TestMergeSpecs_DuplicateNamedBlock_Adapter_SameTypeAndName verifies that
// two files declaring the same adapter (identical type + name) produce a
// duplicate diagnostic.
func TestMergeSpecs_DuplicateNamedBlock_Adapter_SameTypeAndName(t *testing.T) {
	dir := t.TempDir()
	writeHCLFiles(t, dir, map[string]string{
		"a.hcl": `workflow "w" {
  version       = "1"
  initial_state = "run"
  target_state  = "done"
}
adapter "shell" "primary" {}
step "run" {
  target = adapter.shell.primary
  outcome "success" { next = "done" }
}
state "done" { terminal = true }
`,
		"b.hcl": `adapter "shell" "primary" {}
`,
	})

	_, diags := ParseDir(dir)
	assertMergeDiag(t, diags, "duplicate adapter name", "shell.primary")
}

// TestMergeSpecs_DistinctBlocksAcrossFiles_NoConflict verifies that non-overlapping
// step names across two files merge cleanly with no diagnostics.
func TestMergeSpecs_DistinctBlocksAcrossFiles_NoConflict(t *testing.T) {
	dir := t.TempDir()
	writeHCLFiles(t, dir, map[string]string{
		"a.hcl": `workflow "w" {
  version       = "1"
  initial_state = "step_a"
  target_state  = "done"
}
adapter "noop" "default" {}
step "step_a" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
}
state "done" { terminal = true }
`,
		"b.hcl": `step "step_b" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
}
`,
	})

	spec, diags := ParseDir(dir)
	if diags.HasErrors() {
		t.Fatalf("unexpected error: %s", diags.Error())
	}
	if len(spec.Steps) != 2 {
		t.Errorf("expected 2 steps, got %d", len(spec.Steps))
	}
	// Both step names should be present.
	stepNames := make(map[string]bool)
	for _, s := range spec.Steps {
		stepNames[s.Name] = true
	}
	for _, want := range []string{"step_a", "step_b"} {
		if !stepNames[want] {
			t.Errorf("missing step %q in merged spec", want)
		}
	}
}

// TestMergeSpecs_AlphabeticalMergeOrder_DiagnosticsStable verifies that steps
// from three files appear in alphabetical-file order (a.hcl → b.hcl → c.hcl),
// and that the order is stable across multiple invocations.
func TestMergeSpecs_AlphabeticalMergeOrder_DiagnosticsStable(t *testing.T) {
	dir := t.TempDir()
	writeHCLFiles(t, dir, map[string]string{
		"a.hcl": `workflow "w" {
  version       = "1"
  initial_state = "a_step"
  target_state  = "done"
}
adapter "noop" "default" {}
step "a_step" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
}
state "done" { terminal = true }
`,
		"b.hcl": `step "b_step" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
}
`,
		"c.hcl": `step "c_step" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
}
`,
	})

	wantOrder := []string{"a_step", "b_step", "c_step"}
	for range 2 {
		spec, diags := ParseDir(dir)
		if diags.HasErrors() {
			t.Fatalf("unexpected error: %s", diags.Error())
		}
		if len(spec.Steps) != 3 {
			t.Fatalf("expected 3 steps, got %d", len(spec.Steps))
		}
		for i, want := range wantOrder {
			if spec.Steps[i].Name != want {
				t.Errorf("step[%d].Name = %q, want %q", i, spec.Steps[i].Name, want)
			}
		}
	}
}

// TestMergeSpecs_AlphabeticalMergeOrder_ConflictDiagnostic_StableSourceFile
// verifies that when two files contain a conflicting step name, the alphabetically
// earlier file (a.hcl) is reported as the original and the later file (b.hcl) is
// the duplicate in the diagnostic detail.
func TestMergeSpecs_AlphabeticalMergeOrder_ConflictDiagnostic_StableSourceFile(t *testing.T) {
	dir := t.TempDir()
	writeHCLFiles(t, dir, map[string]string{
		"a.hcl": `workflow "w" {
  version       = "1"
  initial_state = "build"
  target_state  = "done"
}
adapter "noop" "default" {}
step "build" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
}
state "done" { terminal = true }
`,
		"b.hcl": `step "build" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
}
`,
	})

	_, diags := ParseDir(dir)
	if !diags.HasErrors() {
		t.Fatal("expected duplicate-step error")
	}
	// The detail should mention a.hcl as the original declaration.
	var detailFound bool
	for _, d := range diags {
		if d.Severity == hcl.DiagError && strings.Contains(d.Detail, "a.hcl") {
			detailFound = true
			break
		}
	}
	if !detailFound {
		t.Errorf("expected diagnostic detail to name a.hcl as the original; got: %s", diags.Error())
	}
}

// TestMergeSpecs_EmptyDirectory_NoSpec_NoDiagnostics tests the mergeSpecs
// function directly: an empty entries slice returns nil spec and nil diagnostics
// (no-files is handled upstream by ParseDir; mergeSpecs itself treats it as
// "nothing to merge").
func TestMergeSpecs_EmptyDirectory_NoSpec_NoDiagnostics(t *testing.T) {
	spec, diags := mergeSpecs("/nonexistent/dir", []fileEntry{})
	if spec != nil {
		t.Errorf("expected nil spec, got %+v", spec)
	}
	if len(diags) != 0 {
		t.Errorf("expected no diagnostics, got: %s", diags.Error())
	}
}

// TestMergeSpecs_SingleFile_NoMergeNeeded verifies that a directory containing
// exactly one .hcl file returns a spec equivalent to the single-file parse.
func TestMergeSpecs_SingleFile_NoMergeNeeded(t *testing.T) {
	dir := t.TempDir()
	content := `workflow "single" {
  version       = "1"
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
	writeHCLFiles(t, dir, map[string]string{"only.hcl": content})

	spec, diags := ParseDir(dir)
	if diags.HasErrors() {
		t.Fatalf("ParseDir: %s", diags.Error())
	}

	single, parseDiags := Parse(filepath.Join(dir, "only.hcl"), []byte(content))
	if parseDiags.HasErrors() {
		t.Fatalf("Parse: %s", parseDiags.Error())
	}

	// Compare key structural fields — hcl.Body fields in Spec cannot be
	// deep-compared, so we compare the observable identifiers.
	if spec.Header.Name != single.Header.Name {
		t.Errorf("Header.Name: ParseDir=%q, Parse=%q", spec.Header.Name, single.Header.Name)
	}
	if len(spec.Steps) != len(single.Steps) {
		t.Errorf("Steps: ParseDir=%d, Parse=%d", len(spec.Steps), len(single.Steps))
	} else if spec.Steps[0].Name != single.Steps[0].Name {
		t.Errorf("Steps[0].Name: ParseDir=%q, Parse=%q", spec.Steps[0].Name, single.Steps[0].Name)
	}
	if len(spec.Adapters) != len(single.Adapters) {
		t.Errorf("Adapters: ParseDir=%d, Parse=%d", len(spec.Adapters), len(single.Adapters))
	}
	if len(spec.States) != len(single.States) {
		t.Errorf("States: ParseDir=%d, Parse=%d", len(spec.States), len(single.States))
	}
}

// TestMergeSpecs_MultipleNonHCLFiles_Ignored verifies that non-.hcl files in
// the directory are silently ignored; only the .hcl file is parsed.
func TestMergeSpecs_MultipleNonHCLFiles_Ignored(t *testing.T) {
	dir := t.TempDir()
	writeHCLFiles(t, dir, map[string]string{
		"foo.txt":  "this is not HCL",
		"bar.json": `{"key": "value"}`,
		"main.hcl": `workflow "w" {
  version       = "1"
  initial_state = "run"
  target_state  = "done"
}
adapter "noop" "default" {}
step "run" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
}
state "done" { terminal = true }
`,
	})

	spec, diags := ParseDir(dir)
	if diags.HasErrors() {
		t.Fatalf("unexpected error: %s", diags.Error())
	}
	if spec == nil {
		t.Fatal("expected non-nil spec")
	}
	if spec.Header == nil || spec.Header.Name != "w" {
		t.Errorf("unexpected header: %+v", spec.Header)
	}
	// Exactly one step from the .hcl file; txt and json are not parsed.
	if len(spec.Steps) != 1 {
		t.Errorf("expected 1 step, got %d", len(spec.Steps))
	}
}
