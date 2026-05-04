package workflow_test

// compile_validation_test.go — tests for validateFoldableAttrs behaviour at call sites.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brokenbots/criteria/workflow"
)

// TestValidateFoldableAttrs_AgentConfigFile verifies that a file(var.path) call
// in an agent.config block is validated at compile time when var.path has a
// known (fold-time) default value that points to a non-existent file.
func TestValidateFoldableAttrs_AgentConfigFile(t *testing.T) {
	dir := t.TempDir()
	// "missing.txt" does NOT exist in dir.
	hclContent := `workflow "test" {
  version       = "0.1"
  initial_state = "done"
  target_state  = "done"

  state "done" {
    terminal = true
    success  = true
  }

  variable "prompt_file" {
    type    = "string"
    default = "missing.txt"
  }

  adapter "noop" "a" {
    config {
      prompt = file(var.prompt_file)
    }
  }
}
`
	path := filepath.Join(dir, "test.hcl")
	if err := os.WriteFile(path, []byte(hclContent), 0o644); err != nil {
		t.Fatal(err)
	}
	spec, diags := workflow.Parse(path, []byte(hclContent))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, compileDiags := workflow.CompileWithOpts(spec, nil, workflow.CompileOpts{WorkflowDir: dir})
	if !compileDiags.HasErrors() {
		t.Fatal("expected compile error for file(var.prompt_file) with missing file; got none")
	}
	// The file-not-found error should reference the missing path.
	found := false
	for _, d := range compileDiags {
		if strings.Contains(d.Summary, "missing.txt") || strings.Contains(d.Detail, "missing.txt") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a diagnostic referencing the missing file path, got: %s", compileDiags.Error())
	}
	// Must NOT contain a spurious "Variables not allowed" diagnostic —
	// the old bug where var.* was rejected before fold evaluation.
	for _, d := range compileDiags {
		if strings.Contains(d.Summary, "Variables not allowed") || strings.Contains(d.Detail, "Variables not allowed") {
			t.Errorf("unexpected 'Variables not allowed' diagnostic; var.* should fold at compile: %s", d.Summary)
		}
	}
}
