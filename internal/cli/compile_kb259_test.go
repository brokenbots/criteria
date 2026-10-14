package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// KB-259: an outcome block's description attribute compiles through to the
// compiled-graph JSON (and therefore the WorkflowGraphs bodies). The
// attribute is runtime-inert: it never alters contract validation or
// routing, so the assertion is serialization-only.
func TestKB259_OutcomeDescriptionCompiledThrough(t *testing.T) {
	dir := t.TempDir()
	hcl := `
workflow {
  name          = "kb259_outcome_description"
  version       = "0.1"
  initial_state = "begin"
  target_state  = "done"
}

adapter "noop" "default" {}

step "begin" {
  target = adapter.noop.default
  outcome "success" {
    next        = state.done
    description = "the step finished cleanly; run bookkeeping may proceed"
  }
  outcome "failure" {
    next        = state.failed
    description = "the step errored; route to the failure state"
  }
}

state "done" {
  terminal = true
  success  = true
}
state "failed" {
  terminal = true
  success  = false
}
`
	path := filepath.Join(dir, "main.hcl")
	if err := os.WriteFile(path, []byte(hcl), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := compileWorkflowOutput(context.Background(), path, "", "json", nil, false, false)
	if err != nil {
		t.Fatalf("compile json: %v", err)
	}
	var parsed struct {
		Steps []struct {
			Name     string `json:"name"`
			Outcomes []struct {
				Name        string `json:"name"`
				Next        string `json:"next"`
				Description string `json:"description"`
			} `json:"outcomes"`
		} `json:"steps"`
	}
	_ = strings.TrimSpace(string(out))
	if err := json.Unmarshal([]byte(strings.Join([]string{string(out)}, "")), &parsed); err != nil {
		t.Fatalf("parse compiled json: %v\nbody: %s", err, out)
	}
	want := map[string]string{
		"success": "the step finished cleanly; run bookkeeping may proceed",
		"failure": "the step errored; route to the failure state",
	}
	got := map[string]string{}
	for _, st := range parsed.Steps {
		if st.Name != "begin" {
			continue
		}
		for _, o := range st.Outcomes {
			got[o.Name] = o.Description
		}
	}
	for name, desc := range want {
		if got[name] != desc {
			t.Fatalf("outcome %q description = %q, want %q (all: %v)", name, got[name], desc, got)
		}
	}
}