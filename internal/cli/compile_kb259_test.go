package cli

// compile_kb259_test.go — KB-259: the outcome block's description attribute
// is carried through to both published compiled-graph surfaces:
//   - the compile-JSON document (criteria compile --format json), and
//   - the once-per-run WorkflowGraphs event (root layer body).
//
// Descriptions are model-facing metadata rendered by agent adapters in the
// allowed-outcomes menu; they never alter contract validation or routing.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const kb259OutcomeDescriptionHCL = `
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

func writeKB259Workflow(t *testing.T, src string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "main.hcl")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

type kb259OutcomeJSON struct {
	Name        string `json:"name"`
	Next        string `json:"next"`
	Description string `json:"description"`
}

// kb259OutcomeDescriptions extracts outcome name -> outcome from a
// compiled-JSON document's step "begin".
func kb259OutcomeDescriptions(t *testing.T, doc []byte) map[string]*kb259OutcomeJSON {
	t.Helper()
	var parsed struct {
		Steps []struct {
			Name     string             `json:"name"`
			Outcomes []kb259OutcomeJSON `json:"outcomes"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(doc, &parsed); err != nil {
		t.Fatalf("parse compiled json: %v\nbody: %s", err, doc)
	}
	got := map[string]*kb259OutcomeJSON{}
	for _, st := range parsed.Steps {
		if st.Name != "begin" {
			continue
		}
		for i := range st.Outcomes {
			got[st.Outcomes[i].Name] = &st.Outcomes[i]
		}
	}
	return got
}

// TestKB259_OutcomeDescriptionCompiledThrough: the compile-JSON document
// carries each outcome's declared description next to its name.
func TestKB259_OutcomeDescriptionCompiledThrough(t *testing.T) {
	path := writeKB259Workflow(t, kb259OutcomeDescriptionHCL)
	out, err := compileWorkflowOutput(context.Background(), path, "", "json", nil, false, false)
	if err != nil {
		t.Fatalf("compile json: %v", err)
	}
	got := kb259OutcomeDescriptions(t, out)
	for name, want := range map[string]string{
		"success": "the step finished cleanly; run bookkeeping may proceed",
		"failure": "the step errored; route to the failure state",
	} {
		co, ok := got[name]
		if !ok {
			t.Fatalf("outcome %q missing from compiled json: %v", name, got)
		}
		if co.Description != want {
			t.Errorf("outcome %q description = %q, want %q", name, co.Description, want)
		}
	}
}

// TestKB259_WorkflowGraphsEmitsOutcomeDescription: the WorkflowGraphs
// payload's root layer body carries the outcome descriptions, so consumers
// of the once-per-run event see the same declared semantics.
func TestKB259_WorkflowGraphsEmitsOutcomeDescription(t *testing.T) {
	path := writeKB259Workflow(t, kb259OutcomeDescriptionHCL)
	_, graph, err := parseCompileForCli(context.Background(), path, nil, false, false)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	msg, err := buildWorkflowGraphsPayload(graph)
	if err != nil {
		t.Fatalf("build payload: %v", err)
	}
	if len(msg.Subworkflows) == 0 || msg.Subworkflows[0].Name != "root" {
		t.Fatalf("payload layers malformed: %d entries", len(msg.Subworkflows))
	}
	var body struct {
		Steps []struct {
			Name     string             `json:"name"`
			Outcomes []kb259OutcomeJSON `json:"outcomes"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(msg.Subworkflows[0].Body), &body); err != nil {
		t.Fatalf("decode root layer body: %v\nbody: %s", err, msg.Subworkflows[0].Body)
	}
	got := map[string]string{}
	for _, st := range body.Steps {
		if st.Name != "begin" {
			continue
		}
		for _, o := range st.Outcomes {
			got[o.Name] = o.Description
		}
	}
	for name, want := range map[string]string{
		"success": "the step finished cleanly; run bookkeeping may proceed",
		"failure": "the step errored; route to the failure state",
	} {
		if got[name] != want {
			t.Errorf("root layer outcome %q description = %q, want %q", name, got[name], want)
		}
	}
}

// TestKB259_OutcomeDescriptionOmittedWhenUnset: workflows without declared
// descriptions compile exactly as before — the field is omitempty on both
// surfaces and carries no new keys for existing workflows. Presence is
// checked on the outcome entries themselves: a present-but-empty key is
// distinguished from an absent one via a non-nil raw value.
func TestKB259_OutcomeDescriptionOmittedWhenUnset(t *testing.T) {
	legacy := strings.ReplaceAll(kb259OutcomeDescriptionHCL, "    description = \"the step errored; route to the failure state\"\n", "")
	legacy = strings.ReplaceAll(legacy, "    description = \"the step finished cleanly; run bookkeeping may proceed\"\n", "")
	path := writeKB259Workflow(t, legacy)
	out, err := compileWorkflowOutput(context.Background(), path, "", "json", nil, false, false)
	if err != nil {
		t.Fatalf("compile json: %v", err)
	}
	got := kb259OutcomeDescriptions(t, out)
	if len(got) != 2 {
		t.Fatalf("expected two outcomes, got %v", got)
	}
	for name, co := range got {
		if co.Description != "" {
			t.Errorf("outcome %q carries an unexpected description %q", name, co.Description)
		}
	}
	// Presence check on the compile-JSON outcomes array: no entry carries a
	// description key at all (not even an empty one).
	assertNoDescriptionKey(t, out)
	_, graph, err := parseCompileForCli(context.Background(), path, nil, false, false)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	msg, err := buildWorkflowGraphsPayload(graph)
	if err != nil {
		t.Fatalf("build payload: %v", err)
	}
	body := msg.Subworkflows[0].Body
	assertNoDescriptionKey(t, []byte(body))
}

// assertNoDescriptionKey fails when any compiled-JSON outcome entry carries
// a description key (present-but-empty included). It scopes the search to
// outcome entries so that other documents containing the word "description"
// (e.g. a workflow name) don't create false positives.
func assertNoDescriptionKey(t *testing.T, doc []byte) {
	t.Helper()
	var docObj struct {
		Steps []struct {
			Outcomes []struct {
				Name        string          `json:"name"`
				Description json.RawMessage `json:"description"`
			} `json:"outcomes"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(doc, &docObj); err != nil {
		t.Fatalf("decode compiled json: %v\nbody: %s", err, doc)
	}
	for _, st := range docObj.Steps {
		for _, o := range st.Outcomes {
			if len(o.Description) > 0 {
				t.Errorf("outcome %q unexpectedly carries a description key", o.Name)
			}
		}
	}
}
