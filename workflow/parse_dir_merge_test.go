package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
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

// findMergeDiag returns the first DiagError whose Summary contains summarySubstr.
// Fails the test if no such diagnostic is found.
func findMergeDiag(t *testing.T, diags hcl.Diagnostics, summarySubstr string) *hcl.Diagnostic {
	t.Helper()
	for i, d := range diags {
		if d.Severity == hcl.DiagError && strings.Contains(d.Summary, summarySubstr) {
			return diags[i]
		}
	}
	t.Fatalf("no DiagError with summary containing %q; diagnostics: %s", summarySubstr, diags.Error())
	return nil
}

// TestMergeSpecs_SingletonConflict_WorkflowHeader_TwoFiles verifies that two
// .hcl files each declaring a workflow header block produce a diagnostic
// naming both source files. The Detail must reference the original file (a.hcl)
// via the "previously declared at" range, and the Subject must point to the
// duplicate file (b.hcl).
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

	d := findMergeDiag(t, diags, "duplicate workflow block")
	// Detail must name a.hcl as the originally-declared file.
	if !strings.Contains(d.Detail, "a.hcl") {
		t.Errorf("expected Detail to name a.hcl (original declaration); got: %s", d.Detail)
	}
	// Subject must point into b.hcl (the duplicate).
	if d.Subject == nil || !strings.Contains(d.Subject.Filename, "b.hcl") {
		t.Errorf("expected Subject.Filename to contain b.hcl (duplicate); got: %v", d.Subject)
	}
}

// TestMergeSpecs_SingletonConflict_Policy_TwoFiles verifies that two .hcl
// files each declaring a policy block produce a "duplicate policy block" error
// with Detail naming a.hcl as the original and Subject pointing to b.hcl.
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

	d := findMergeDiag(t, diags, "duplicate policy block")
	if !strings.Contains(d.Detail, "a.hcl") {
		t.Errorf("expected Detail to name a.hcl (original declaration); got: %s", d.Detail)
	}
	if d.Subject == nil || !strings.Contains(d.Subject.Filename, "b.hcl") {
		t.Errorf("expected Subject.Filename to contain b.hcl (duplicate); got: %v", d.Subject)
	}
}

// TestMergeSpecs_SingletonConflict_Permissions_TwoFiles verifies that two
// .hcl files each declaring a permissions block produce a conflict error with
// Detail naming a.hcl as the original and Subject pointing to b.hcl.
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

	d := findMergeDiag(t, diags, "duplicate permissions block")
	if !strings.Contains(d.Detail, "a.hcl") {
		t.Errorf("expected Detail to name a.hcl (original declaration); got: %s", d.Detail)
	}
	if d.Subject == nil || !strings.Contains(d.Subject.Filename, "b.hcl") {
		t.Errorf("expected Subject.Filename to contain b.hcl (duplicate); got: %v", d.Subject)
	}
}

// TestMergeSpecs_DuplicateNamedBlock_Step verifies that two .hcl files
// declaring the same step name produce a diagnostic naming the step name and
// attributing the original declaration to a.hcl via the Detail field.
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

	d := findMergeDiag(t, diags, "duplicate step name")
	if !strings.Contains(d.Detail, "a.hcl") {
		t.Errorf("expected Detail to name a.hcl (original declaration); got: %s", d.Detail)
	}
	if d.Subject == nil || !strings.Contains(d.Subject.Filename, "b.hcl") {
		t.Errorf("expected Subject.Filename to contain b.hcl (duplicate); got: %v", d.Subject)
	}
}

// TestMergeSpecs_DuplicateNamedBlock_Adapter_DifferentTypes documents that the
// workstream specification required same-name adapters to conflict regardless of
// type label, but the current parser uses "<type>.<name>" as the adapter identity
// key — so adapter "shell" "primary" and adapter "copilot" "primary" are distinct.
//
// This test is skipped pending an architecture decision. The executor has
// escalated the contract mismatch: see [ARCH-REVIEW] in the workstream file.
func TestMergeSpecs_DuplicateNamedBlock_Adapter_DifferentTypes(t *testing.T) {
	t.Skip("ARCH-REVIEW pending: workstream requires same-name different-type adapters to conflict, " +
		"but the parser uses type+name as the adapter identity key (adapter.shell.primary ≠ " +
		"adapter.copilot.primary). Changing this would be a breaking contract change; see " +
		"[ARCH-REVIEW] in workstreams/test-02-hcl-parsing-eval-coverage.md.")
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

// TestMergeSpecs_EmptyDirectory_NoSpec_NoDiagnostics tests that ParseDir on a
// directory containing no .hcl files returns a "no .hcl files" error diagnostic.
// (mergeSpecs itself returns nil,nil for an empty entries slice, but that code
// path is unreachable via the public API — ParseDir exits early with an error
// before calling mergeSpecs when no files are present.)
func TestMergeSpecs_EmptyDirectory_NoSpec_NoDiagnostics(t *testing.T) {
	dir := t.TempDir() // empty directory: no .hcl files
	spec, diags := ParseDir(dir)
	if spec != nil {
		t.Errorf("expected nil spec for empty directory, got %+v", spec)
	}
	if !diags.HasErrors() {
		t.Fatal("expected error diagnostic for empty directory, got none")
	}
	// ParseDir must specifically report "no .hcl files" — not a generic failure.
	found := false
	for _, d := range diags {
		if d.Severity == hcl.DiagError && strings.Contains(d.Summary, "no .hcl files") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'no .hcl files' error; got: %s", diags.Error())
	}
}

// TestMergeSpecs_SingleFile_NoMergeNeeded verifies that a directory containing
// exactly one .hcl file returns a spec equivalent to the single-file parse.
// Uses cmp.Diff on a structural summary to prove spec equivalence at the
// surface level (hcl.Body fields are interface types and cannot be deep-compared).
func TestMergeSpecs_SingleFile_NoMergeNeeded(t *testing.T) {
	dir := t.TempDir()
	content := `workflow "single" {
  version       = "1"
  initial_state = "run"
  target_state  = "done"
}
adapter "noop" "default" {}
adapter "shell" "runner" {}
step "run" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
}
step "cleanup" {
  target = adapter.shell.runner
  outcome "success" { next = "done" }
}
state "done" { terminal = true }
state "failed" { terminal = true }
`
	writeHCLFiles(t, dir, map[string]string{"only.hcl": content})

	merged, diags := ParseDir(dir)
	if diags.HasErrors() {
		t.Fatalf("ParseDir: %s", diags.Error())
	}

	single, parseDiags := Parse(filepath.Join(dir, "only.hcl"), []byte(content))
	if parseDiags.HasErrors() {
		t.Fatalf("Parse: %s", parseDiags.Error())
	}

	// specSummary captures all comparable structural identifiers. hcl.Body and
	// hcl.Expression fields cannot be deep-compared so are excluded.
	type specSummary struct {
		WorkflowName string
		StepNames    []string
		StateNames   []string
		AdapterKeys  []string
	}
	summarize := func(s *Spec) specSummary {
		sum := specSummary{}
		if s.Header != nil {
			sum.WorkflowName = s.Header.Name
		}
		for _, st := range s.Steps {
			sum.StepNames = append(sum.StepNames, st.Name)
		}
		for _, st := range s.States {
			sum.StateNames = append(sum.StateNames, st.Name)
		}
		for _, a := range s.Adapters {
			sum.AdapterKeys = append(sum.AdapterKeys, a.Type+"."+a.Name)
		}
		return sum
	}
	if diff := cmp.Diff(summarize(single), summarize(merged)); diff != "" {
		t.Errorf("single-file ParseDir/Parse mismatch (-want +got):\n%s", diff)
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
