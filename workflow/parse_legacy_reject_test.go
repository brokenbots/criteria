package workflow

import (
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
)

// assertDiagnosticContains asserts that diags contains at least one DiagError
// whose Summary contains the given substring. Tests in this file call Parse
// directly to exercise the rejection branches in parse_legacy_reject.go.
func assertDiagnosticContains(t *testing.T, diags hcl.Diagnostics, summarySubstr string) {
	t.Helper()
	if !diags.HasErrors() {
		t.Fatalf("expected error diagnostics containing %q, got none", summarySubstr)
	}
	for _, d := range diags {
		if d.Severity == hcl.DiagError && strings.Contains(d.Summary, summarySubstr) {
			return
		}
	}
	// Collect all error summaries for a useful failure message.
	var summaries []string
	for _, d := range diags {
		if d.Severity == hcl.DiagError {
			summaries = append(summaries, d.Summary)
		}
	}
	t.Fatalf("no DiagError containing %q; got summaries: %v", summarySubstr, summaries)
}

// minimalWorkflowHCL is a minimal, syntactically valid workflow preamble used
// as a prefix in tests that need a parse-able file body.
const minimalWorkflowHCL = `workflow "test" {
  version       = "1"
  initial_state = "run"
  target_state  = "done"
}
adapter "noop" "default" {}
`

// ------------------------------------------------------------------
// rejectLegacyBlocks — top-level removed block types
// ------------------------------------------------------------------

// TestLegacyReject_TopLevelAgentBlock verifies that a top-level "agent" block
// (renamed to "adapter" in v0.3.0) is rejected with a DiagError naming "agent".
//
// Note: rejectLegacyBlocks uses PartialContent with LabelNames: nil, so it
// only matches zero-label blocks. A labeled form like `agent "myagent" {}` is
// NOT caught by the legacy check and instead receives a generic "Unsupported
// block type" from gohcl. The zero-label form is the canonical test case.
func TestLegacyReject_TopLevelAgentBlock(t *testing.T) {
	src := minimalWorkflowHCL + `
agent {
  model = "gpt-4"
}
`
	_, diags := Parse("test.hcl", []byte(src))
	assertDiagnosticContains(t, diags, `removed block "agent"`)
	// Detail should point to the v0.3.0 replacement.
	for _, d := range diags {
		if d.Severity == hcl.DiagError && strings.Contains(d.Summary, "agent") {
			if !strings.Contains(d.Detail, "adapter") {
				t.Errorf("expected detail to mention 'adapter' replacement; got: %s", d.Detail)
			}
			return
		}
	}
}

// TestLegacyReject_TopLevelBranchBlock verifies that a top-level "branch" block
// (renamed to "switch" in v0.3.0) is rejected with a DiagError naming "branch"
// and with Detail pointing to the "switch" replacement.
// See TestLegacyReject_TopLevelAgentBlock for the no-label constraint note.
func TestLegacyReject_TopLevelBranchBlock(t *testing.T) {
	src := minimalWorkflowHCL + `
branch {}
`
	_, diags := Parse("test.hcl", []byte(src))
	assertDiagnosticContains(t, diags, `removed block "branch"`)
	for _, d := range diags {
		if d.Severity == hcl.DiagError && strings.Contains(d.Summary, "branch") {
			if !strings.Contains(d.Detail, "switch") {
				t.Errorf("expected detail to mention 'switch' replacement; got: %s", d.Detail)
			}
			return
		}
	}
}

// ------------------------------------------------------------------
// rejectLegacyStepAgentAttr — "agent" attribute on step blocks
// ------------------------------------------------------------------

// TestLegacyReject_StepAgentAttr verifies that the removed "agent" attribute on
// a top-level step block is rejected with a clear error naming the attribute
// and with Detail pointing to the "target" replacement.
func TestLegacyReject_StepAgentAttr(t *testing.T) {
	src := minimalWorkflowHCL + `
step "run" {
  agent = "gpt-4"
  outcome "success" { next = "done" }
}
state "done" { terminal = true }
`
	_, diags := Parse("test.hcl", []byte(src))
	assertDiagnosticContains(t, diags, `removed attribute "agent" on steps`)
	for _, d := range diags {
		if d.Severity == hcl.DiagError && strings.Contains(d.Summary, `"agent" on steps`) {
			if !strings.Contains(d.Detail, "target") {
				t.Errorf("expected detail to mention 'target' replacement; got: %s", d.Detail)
			}
			return
		}
	}
}

// TestLegacyReject_StepAgentAttr_InNestedWorkflow verifies that the "agent"
// attribute on a step nested inside an inline workflow block is also rejected,
// with Detail pointing to the "target" replacement.
// This exercises the recursive walk in rejectLegacyStepAgentAttrInBody.
func TestLegacyReject_StepAgentAttr_InNestedWorkflow(t *testing.T) {
	src := minimalWorkflowHCL + `
step "outer" {
  target = adapter.noop.default
  workflow {
    step "inner" {
      agent = "gpt-4"
      outcome "success" { next = "done" }
    }
  }
  outcome "success" { next = "done" }
}
state "done" { terminal = true }
`
	_, diags := Parse("test.hcl", []byte(src))
	assertDiagnosticContains(t, diags, `removed attribute "agent" on steps`)
	for _, d := range diags {
		if d.Severity == hcl.DiagError && strings.Contains(d.Summary, `"agent" on steps`) {
			if !strings.Contains(d.Detail, "target") {
				t.Errorf("expected detail to mention 'target' replacement; got: %s", d.Detail)
			}
			return
		}
	}
}

// ------------------------------------------------------------------
// rejectLegacyStepAdapterAttr — "adapter" attribute on step blocks
// ------------------------------------------------------------------

// TestLegacyReject_StepAdapterAttr verifies that the removed "adapter" attribute
// on a step block (replaced by "target") is rejected with Detail pointing to
// the "target" replacement.
func TestLegacyReject_StepAdapterAttr(t *testing.T) {
	src := minimalWorkflowHCL + `
step "run" {
  adapter = adapter.noop.default
  outcome "success" { next = "done" }
}
state "done" { terminal = true }
`
	_, diags := Parse("test.hcl", []byte(src))
	assertDiagnosticContains(t, diags, `removed attribute "adapter" on steps`)
	for _, d := range diags {
		if d.Severity == hcl.DiagError && strings.Contains(d.Summary, `"adapter" on steps`) {
			if !strings.Contains(d.Detail, "target") {
				t.Errorf("expected detail to mention 'target' replacement; got: %s", d.Detail)
			}
			return
		}
	}
}

// ------------------------------------------------------------------
// rejectLegacyStepLifecycleAttr — "lifecycle" attribute on step blocks
// ------------------------------------------------------------------

// TestLegacyReject_StepLifecycleAttr verifies that the removed "lifecycle"
// attribute on a step block is rejected, with Detail indicating that lifecycle
// is now automatic (managed by the engine).
func TestLegacyReject_StepLifecycleAttr(t *testing.T) {
	src := minimalWorkflowHCL + `
step "run" {
  target    = adapter.noop.default
  lifecycle = "open"
  outcome "success" { next = "done" }
}
state "done" { terminal = true }
`
	_, diags := Parse("test.hcl", []byte(src))
	assertDiagnosticContains(t, diags, `removed attribute "lifecycle" on steps`)
	for _, d := range diags {
		if d.Severity == hcl.DiagError && strings.Contains(d.Summary, `"lifecycle" on steps`) {
			if !strings.Contains(d.Detail, "automatic") {
				t.Errorf("expected detail to indicate lifecycle is 'automatic'; got: %s", d.Detail)
			}
			return
		}
	}
}

// TestLegacyReject_StepLifecycleAttr_InNestedWorkflow verifies that "lifecycle"
// on a step nested inside an inline workflow block is also caught with Detail
// indicating that lifecycle is automatic. Exercises the recursive walk in
// rejectLegacyStepLifecycleAttrInBody.
func TestLegacyReject_StepLifecycleAttr_InNestedWorkflow(t *testing.T) {
	src := minimalWorkflowHCL + `
step "outer" {
  target = adapter.noop.default
  workflow {
    step "inner" {
      lifecycle = "open"
      outcome "success" { next = "done" }
    }
  }
  outcome "success" { next = "done" }
}
state "done" { terminal = true }
`
	_, diags := Parse("test.hcl", []byte(src))
	assertDiagnosticContains(t, diags, `removed attribute "lifecycle" on steps`)
	for _, d := range diags {
		if d.Severity == hcl.DiagError && strings.Contains(d.Summary, `"lifecycle" on steps`) {
			if !strings.Contains(d.Detail, "automatic") {
				t.Errorf("expected detail to indicate lifecycle is 'automatic'; got: %s", d.Detail)
			}
			return
		}
	}
}

// ------------------------------------------------------------------
// rejectLegacyStepWorkflowBlock — inline "workflow { }" on step blocks
// ------------------------------------------------------------------

// TestLegacyReject_StepInlineWorkflowBlock verifies that an inline
// "workflow { ... }" body block inside a step is rejected with Detail pointing
// to the "subworkflow" replacement. Exercises the diagnostic append in
// rejectLegacyStepWorkflowBlockInBody.
func TestLegacyReject_StepInlineWorkflowBlock(t *testing.T) {
	src := minimalWorkflowHCL + `
step "run" {
  target = adapter.noop.default
  workflow {
    step "child" {
      target = adapter.noop.default
      outcome "success" { next = "done" }
    }
    state "done" { terminal = true }
  }
  outcome "success" { next = "done" }
}
state "done" { terminal = true }
`
	_, diags := Parse("test.hcl", []byte(src))
	assertDiagnosticContains(t, diags, `removed block "workflow" on steps`)
	for _, d := range diags {
		if d.Severity == hcl.DiagError && strings.Contains(d.Summary, `"workflow" on steps`) {
			if !strings.Contains(d.Detail, "subworkflow") {
				t.Errorf("expected detail to mention 'subworkflow' replacement; got: %s", d.Detail)
			}
			return
		}
	}
}

// ------------------------------------------------------------------
// rejectLegacyStepWorkflowFile — "workflow_file" attribute on step blocks
// ------------------------------------------------------------------

// TestLegacyReject_StepWorkflowFileAttr verifies that the removed
// "workflow_file" attribute on a step block is rejected with Detail pointing
// to the "subworkflow" replacement. Exercises the diagnostic in
// rejectLegacyStepWorkflowFileInBody.
func TestLegacyReject_StepWorkflowFileAttr(t *testing.T) {
	src := minimalWorkflowHCL + `
step "run" {
  target        = adapter.noop.default
  workflow_file = "child.hcl"
  outcome "success" { next = "done" }
}
state "done" { terminal = true }
`
	_, diags := Parse("test.hcl", []byte(src))
	assertDiagnosticContains(t, diags, `removed attribute "workflow_file" on steps`)
	for _, d := range diags {
		if d.Severity == hcl.DiagError && strings.Contains(d.Summary, `"workflow_file" on steps`) {
			if !strings.Contains(d.Detail, "subworkflow") {
				t.Errorf("expected detail to mention 'subworkflow' replacement; got: %s", d.Detail)
			}
			return
		}
	}
}

// ------------------------------------------------------------------
// rejectLegacyStepTypeAttr — "type" attribute on step blocks
// ------------------------------------------------------------------

// TestLegacyReject_StepTypeAttr verifies that the removed "type" attribute on a
// step block is rejected with Detail pointing to "target" and the "adapter"
// migration path. Exercises the diagnostic append in rejectLegacyStepTypeAttrInBody.
func TestLegacyReject_StepTypeAttr(t *testing.T) {
	src := minimalWorkflowHCL + `
step "run" {
  type = "adapter"
  outcome "success" { next = "done" }
}
state "done" { terminal = true }
`
	_, diags := Parse("test.hcl", []byte(src))
	assertDiagnosticContains(t, diags, `removed attribute "type" on steps`)
	for _, d := range diags {
		if d.Severity == hcl.DiagError && strings.Contains(d.Summary, `"type" on steps`) {
			if !strings.Contains(d.Detail, "target") && !strings.Contains(d.Detail, "adapter") {
				t.Errorf("expected detail to mention 'target' or 'adapter' replacement; got: %s", d.Detail)
			}
			return
		}
	}
}

// ------------------------------------------------------------------
// rejectLegacyOutcomeTransitionTo — "transition_to" attribute on outcome blocks
// ------------------------------------------------------------------

// TestLegacyReject_OutcomeTransitionTo verifies that the removed "transition_to"
// attribute inside an outcome block (renamed to "next" in v0.3.0) is rejected.
func TestLegacyReject_OutcomeTransitionTo(t *testing.T) {
	src := minimalWorkflowHCL + `
step "run" {
  target = adapter.noop.default
  outcome "success" { transition_to = "done" }
}
state "done" { terminal = true }
`
	_, diags := Parse("test.hcl", []byte(src))
	assertDiagnosticContains(t, diags, `removed attribute "transition_to" on outcome blocks`)
	// Detail should mention "next" as the replacement.
	for _, d := range diags {
		if d.Severity == hcl.DiagError && strings.Contains(d.Summary, "transition_to") {
			if !strings.Contains(d.Detail, "next") {
				t.Errorf("expected detail to mention 'next' replacement; got: %s", d.Detail)
			}
			return
		}
	}
}
