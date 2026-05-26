package workflow

import (
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/ext/typeexpr"
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
const minimalWorkflowHCL = `workflow {
  name = "test"
  version       = "1"
  initial_state = "start"
  target_state  = "done"
}
adapter "noop" "default" {}

step "start" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
}
state "done" { terminal = true }
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

// ------------------------------------------------------------------
// rejectLegacyWorkflowLabel — workflow "name" { ... }
// ------------------------------------------------------------------

func TestLegacyReject_WorkflowLabel(t *testing.T) {
	src := `workflow "test" {
  version       = "1"
  initial_state = "run"
  target_state  = "done"
}
`
	_, diags := Parse("test.hcl", []byte(src))
	assertDiagnosticContains(t, diags, "removed labelled workflow block")
	for _, d := range diags {
		if d.Severity == hcl.DiagError && strings.Contains(d.Summary, "removed labelled workflow block") {
			if !strings.Contains(d.Detail, `name = "..."`) {
				t.Errorf("expected detail to mention 'name = \"...\"' replacement; got: %s", d.Detail)
			}
			return
		}
	}
}

func TestLegacyReject_WorkflowLabel_AcceptsNewForm(t *testing.T) {
	src := minimalWorkflowHCL
	_, diags := Parse("test.hcl", []byte(src))
	for _, d := range diags {
		if d.Severity == hcl.DiagError && strings.Contains(d.Summary, "removed labelled workflow block") {
			t.Fatalf("new-form workflow should not trigger legacy label rejection: %s", d.Summary)
		}
	}
}

// ------------------------------------------------------------------
// rejectLegacyPolicyBlock — top-level policy { ... }
// ------------------------------------------------------------------

func TestLegacyReject_PolicyBlock_TopLevel(t *testing.T) {
	src := minimalWorkflowHCL + `
policy { max_total_steps = 100 }
`
	_, diags := Parse("test.hcl", []byte(src))
	assertDiagnosticContains(t, diags, "removed top-level policy block")
	for _, d := range diags {
		if d.Severity == hcl.DiagError && strings.Contains(d.Summary, "removed top-level policy block") {
			if !strings.Contains(d.Detail, "nested inside the workflow") {
				t.Errorf("expected detail to mention nested workflow; got: %s", d.Detail)
			}
			return
		}
	}
}

func TestLegacyReject_PolicyBlock_NestedAccepted(t *testing.T) {
	src := `workflow {
  name          = "test"
  version       = "1"
  initial_state = "run"
  target_state  = "done"
  policy { max_total_steps = 100 }
}
adapter "noop" "default" {}
`
	_, diags := Parse("test.hcl", []byte(src))
	for _, d := range diags {
		if d.Severity == hcl.DiagError && strings.Contains(d.Summary, "removed top-level policy block") {
			t.Fatalf("nested policy should not trigger top-level rejection: %s", d.Summary)
		}
	}
}

// ------------------------------------------------------------------
// rejectLegacyTypeString — type = "string" (quoted)
// ------------------------------------------------------------------

func TestLegacyReject_TypeString_Quoted(t *testing.T) {
	src := minimalWorkflowHCL + `
variable "count" {
  type = "number"
}
`
	_, diags := Parse("test.hcl", []byte(src))
	assertDiagnosticContains(t, diags, "removed quoted-string type on variable block")
	for _, d := range diags {
		if d.Severity == hcl.DiagError && strings.Contains(d.Summary, "removed quoted-string type") {
			if !strings.Contains(d.Detail, "type = string") {
				t.Errorf("expected detail to mention unquoted type expression; got: %s", d.Detail)
			}
			return
		}
	}
}

func TestLegacyReject_TypeString_QuotedSharedVar(t *testing.T) {
	src := minimalWorkflowHCL + `
shared_variable "name" {
  type = "string"
}
`
	_, diags := Parse("test.hcl", []byte(src))
	assertDiagnosticContains(t, diags, "removed quoted-string type on shared_variable block")
}

func TestLegacyReject_TypeString_QuotedOutput(t *testing.T) {
	src := minimalWorkflowHCL + `
output "result" {
  type = "string"
}
`
	_, diags := Parse("test.hcl", []byte(src))
	assertDiagnosticContains(t, diags, "removed quoted-string type on output block")
}

func TestLegacyReject_TypeString_BareAccepted(t *testing.T) {
	src := minimalWorkflowHCL + `
variable "count" {
  type = number
}
`
	_, diags := Parse("test.hcl", []byte(src))
	for _, d := range diags {
		if d.Severity == hcl.DiagError && strings.Contains(d.Summary, "removed quoted-string type") {
			t.Fatalf("bare type expression should not trigger legacy rejection: %s", d.Summary)
		}
	}
}

// ------------------------------------------------------------------
// rejectLegacyDefaultOutcome — default_outcome = "..."
// ------------------------------------------------------------------

func TestLegacyReject_DefaultOutcomeAttr(t *testing.T) {
	src := minimalWorkflowHCL + `
step "run" {
  target         = adapter.noop.default
  default_outcome = "success"
  outcome "success" { next = "done" }
}
state "done" { terminal = true }
`
	_, diags := Parse("test.hcl", []byte(src))
	assertDiagnosticContains(t, diags, `removed attribute "default_outcome" on steps`)
	for _, d := range diags {
		if d.Severity == hcl.DiagError && strings.Contains(d.Summary, "default_outcome") {
			if !strings.Contains(d.Detail, `outcome "default"`) {
				t.Errorf("expected detail to mention outcome \"default\" replacement; got: %s", d.Detail)
			}
			return
		}
	}
}

func TestLegacyReject_DefaultOutcomeBlock_AcceptsNewForm(t *testing.T) {
	src := minimalWorkflowHCL + `
step "run" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
  outcome "default" { next = "done" }
}
state "done" { terminal = true }
`
	_, diags := Parse("test.hcl", []byte(src))
	for _, d := range diags {
		if d.Severity == hcl.DiagError && strings.Contains(d.Summary, "default_outcome") {
			t.Fatalf("outcome \"default\" block should not trigger legacy rejection: %s", d.Summary)
		}
	}
}

// ------------------------------------------------------------------
// rejectLegacyEnvironmentString — environment = "..." (quoted)
// ------------------------------------------------------------------

func TestLegacyReject_EnvironmentString_QuotedOnWorkflow(t *testing.T) {
	src := `workflow {
  name          = "test"
  version       = "1"
  initial_state = "run"
  target_state  = "done"
  environment   = "shell.ci"
}
`
	_, diags := Parse("test.hcl", []byte(src))
	assertDiagnosticContains(t, diags, "removed quoted-string environment on workflow block")
	for _, d := range diags {
		if d.Severity == hcl.DiagError && strings.Contains(d.Summary, "removed quoted-string environment") {
			if !strings.Contains(d.Detail, "bare traversal") {
				t.Errorf("expected detail to mention bare traversal; got: %s", d.Detail)
			}
			return
		}
	}
}

func TestLegacyReject_EnvironmentString_QuotedOnStep(t *testing.T) {
	src := minimalWorkflowHCL + `
step "run" {
  target      = adapter.noop.default
  environment = "shell.ci"
  outcome "success" { next = "done" }
}
state "done" { terminal = true }
`
	_, diags := Parse("test.hcl", []byte(src))
	assertDiagnosticContains(t, diags, "removed quoted-string environment on step block")
}

func TestLegacyReject_EnvironmentString_QuotedOnAdapter(t *testing.T) {
	src := minimalWorkflowHCL + `
adapter "shell" "ci" {
  environment = "shell.ci"
}
`
	_, diags := Parse("test.hcl", []byte(src))
	assertDiagnosticContains(t, diags, "removed quoted-string environment on adapter block")
}

func TestLegacyReject_EnvironmentString_QuotedOnSubworkflow(t *testing.T) {
	src := minimalWorkflowHCL + `
subworkflow "child" {
  environment = "shell.ci"
}
`
	_, diags := Parse("test.hcl", []byte(src))
	assertDiagnosticContains(t, diags, "removed quoted-string environment on subworkflow block")
}

func TestLegacyReject_EnvironmentString_BareAccepted(t *testing.T) {
	src := minimalWorkflowHCL + `
step "run" {
  target      = adapter.noop.default
  environment = shell.ci
  outcome "success" { next = "done" }
}
state "done" { terminal = true }
`
	_, diags := Parse("test.hcl", []byte(src))
	for _, d := range diags {
		if d.Severity == hcl.DiagError && strings.Contains(d.Summary, "removed quoted-string environment") {
			t.Fatalf("bare traversal environment should not trigger legacy rejection: %s", d.Summary)
		}
	}
}

// ------------------------------------------------------------------
// Positive feature tests — new forms compile correctly
// ------------------------------------------------------------------

func TestPositive_NestedPolicy(t *testing.T) {
	src := `workflow {
  name          = "test"
  version       = "1"
  initial_state = "run"
  target_state  = "done"
  policy { max_total_steps = 100 }
}
adapter "noop" "default" {}
`
	g, diags := Parse("test.hcl", []byte(src))
	for _, d := range diags {
		if d.Severity == hcl.DiagError {
			t.Fatalf("unexpected error: %s: %s", d.Summary, d.Detail)
		}
	}
	if g.Header.Policy == nil {
		t.Fatal("expected Header.Policy to be non-nil")
	}
	if g.Header.Policy.MaxTotalSteps != 100 {
		t.Fatalf("expected MaxTotalSteps=100, got %d", g.Header.Policy.MaxTotalSteps)
	}
}

func TestPositive_TypeExpressions(t *testing.T) {
	cases := []struct {
		name     string
		varName  string
		src      string
		wantType string
	}{
		{
			name:     "string",
			varName:  "s",
			src:      `variable "s" { type = string }`,
			wantType: "string",
		},
		{
			name:     "number",
			varName:  "n",
			src:      `variable "n" { type = number }`,
			wantType: "number",
		},
		{
			name:     "bool",
			varName:  "b",
			src:      `variable "b" { type = bool }`,
			wantType: "bool",
		},
		{
			name:     "list(string)",
			varName:  "l",
			src:      `variable "l" { type = list(string) }`,
			wantType: "list(string)",
		},
		{
			name:     "map(string)",
			varName:  "m",
			src:      `variable "m" { type = map(string) }`,
			wantType: "map(string)",
		},
		{
			name:     "object",
			varName:  "o",
			src:      `variable "o" { type = object({ a = string, b = number }) }`,
			wantType: "object({a=string,b=number})",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := minimalWorkflowHCL + tc.src + "\n"
			spec, diags := Parse("test.hcl", []byte(src))
			for _, d := range diags {
				if d.Severity == hcl.DiagError {
					t.Fatalf("unexpected parse error: %s: %s", d.Summary, d.Detail)
				}
			}
			g, diags := Compile(spec, nil)
			for _, d := range diags {
				if d.Severity == hcl.DiagError {
					t.Fatalf("unexpected compile error: %s: %s", d.Summary, d.Detail)
				}
			}
			if len(g.Variables) == 0 {
				t.Fatal("expected at least one compiled variable")
			}
			vn, ok := g.Variables[tc.varName]
			if !ok {
				t.Fatalf("expected compiled variable %q", tc.varName)
			}
			got := typeexpr.TypeString(vn.Type)
			if got != tc.wantType {
				t.Fatalf("expected type %q, got %q", tc.wantType, got)
			}
		})
	}
}

func TestPositive_DefaultOutcomeBlock(t *testing.T) {
	src := `workflow {
  name = "test"
  version       = "1"
  initial_state = "start"
  target_state  = "done"
}
adapter "noop" "default" {}

step "start" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
  outcome "default" { next = "done" }
}
state "done" { terminal = true }
`
	spec, diags := Parse("test.hcl", []byte(src))
	for _, d := range diags {
		if d.Severity == hcl.DiagError {
			t.Fatalf("unexpected parse error: %s: %s", d.Summary, d.Detail)
		}
	}
	g, diags := Compile(spec, nil)
	for _, d := range diags {
		if d.Severity == hcl.DiagError {
			t.Fatalf("unexpected compile error: %s: %s", d.Summary, d.Detail)
		}
	}
	step, ok := g.Steps["start"]
	if !ok {
		t.Fatal("expected compiled step 'start'")
	}
	if step.DefaultOutcome == nil {
		t.Fatal("expected DefaultOutcome to be non-nil")
	}
	if step.DefaultOutcome.Name != "default" {
		t.Fatalf("expected DefaultOutcome.Name=\"default\", got %q", step.DefaultOutcome.Name)
	}
	if step.DefaultOutcome.Next != "done" {
		t.Fatalf("expected DefaultOutcome.Next=\"done\", got %q", step.DefaultOutcome.Next)
	}
}
