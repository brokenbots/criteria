package workflow

import (
	"strconv"
	"strings"
	"testing"
)

// loopWorkflow is a minimal workflow with a back-edge on step "execute":
// execute -> again -> execute (via the "again" outcome and the step's own
// back-edge). Used by multiple back-edge warning tests.
func loopWorkflowSrc(maxVisits, maxTotalSteps, warnThreshold int) string {
	policyBlock := ""
	if maxTotalSteps > 0 || warnThreshold >= 0 {
		parts := ""
		if maxTotalSteps > 0 {
			parts += "    max_total_steps = " + strconv.Itoa(maxTotalSteps) + "\n"
		}
		if warnThreshold >= 0 {
			parts += "    max_visits_warn_threshold = " + strconv.Itoa(warnThreshold) + "\n"
		}
		policyBlock = "  policy {\n" + parts + "  }\n"
	}
	maxVisitsAttr := ""
	if maxVisits != 0 {
		maxVisitsAttr = "    max_visits = " + strconv.Itoa(maxVisits) + "\n"
	}
	return `
workflow "loop" {
  adapter "fake" "default" {}
  version       = "0.1"
  initial_state = "execute"
  target_state  = "done"
  step "execute" {
    adapter = adapter.fake.default
` + maxVisitsAttr + `    outcome "again" { transition_to = "execute" }
    outcome "success" { transition_to = "done" }
  }
  state "done" { terminal = true }
` + policyBlock + `}
`
}

// TestCompile_MaxVisits_Decodes verifies that max_visits = 5 on a step
// decodes into StepNode.MaxVisits correctly.
func TestCompile_MaxVisits_Decodes(t *testing.T) {
	src := loopWorkflowSrc(5, 0, -1)
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	step, ok := g.Steps["execute"]
	if !ok {
		t.Fatal("step 'execute' not found")
	}
	if step.MaxVisits != 5 {
		t.Errorf("MaxVisits = %d, want 5", step.MaxVisits)
	}
}

// TestCompile_MaxVisits_Zero verifies that omitting max_visits results in 0.
func TestCompile_MaxVisits_Zero(t *testing.T) {
	src := loopWorkflowSrc(0, 0, -1)
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	step := g.Steps["execute"]
	if step.MaxVisits != 0 {
		t.Errorf("MaxVisits = %d, want 0 (unlimited)", step.MaxVisits)
	}
}

// TestCompile_MaxVisits_Negative verifies that max_visits = -1 fails
// compilation with a diagnostic naming the step.
func TestCompile_MaxVisits_Negative(t *testing.T) {
	src := loopWorkflowSrc(-1, 0, -1)
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for negative max_visits")
	}
	msg := diags.Error()
	if !strings.Contains(msg, "max_visits must be >= 0") {
		t.Errorf("unexpected error message: %s", msg)
	}
	if !strings.Contains(msg, `"execute"`) {
		t.Errorf("expected step name in error: %s", msg)
	}
}

// TestCompile_BackEdgeWarning verifies that a step with a self-loop,
// max_total_steps = 500 (> default threshold 200), and no max_visits emits
// the documented warning.
func TestCompile_BackEdgeWarning(t *testing.T) {
	src := loopWorkflowSrc(0, 500, -1)
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("unexpected compile error: %s", diags.Error())
	}
	var warned bool
	for _, d := range diags {
		if strings.Contains(d.Summary, "appears in a loop") &&
			strings.Contains(d.Summary, "max_visits") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("expected back-edge warning; got diags: %v", diags)
	}
}

// TestCompile_BackEdgeWarning_Suppressed verifies that a step with a self-loop
// and max_visits explicitly set does NOT emit the back-edge warning.
func TestCompile_BackEdgeWarning_Suppressed(t *testing.T) {
	src := loopWorkflowSrc(10, 500, -1)
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("unexpected compile error: %s", diags.Error())
	}
	for _, d := range diags {
		if strings.Contains(d.Summary, "appears in a loop") {
			t.Errorf("unexpected back-edge warning when max_visits is set: %s", d.Summary)
		}
	}
}

// TestCompile_BackEdgeWarning_BelowThreshold verifies that the warning is NOT
// emitted when max_total_steps is at or below the default threshold (200).
func TestCompile_BackEdgeWarning_BelowThreshold(t *testing.T) {
	src := loopWorkflowSrc(0, 200, -1)
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("unexpected compile error: %s", diags.Error())
	}
	for _, d := range diags {
		if strings.Contains(d.Summary, "appears in a loop") {
			t.Errorf("unexpected back-edge warning at threshold boundary: %s", d.Summary)
		}
	}
}

// TestCompile_BackEdgeWarning_CustomThreshold verifies that max_visits_warn_threshold
// overrides the default threshold of 200.
func TestCompile_BackEdgeWarning_CustomThreshold(t *testing.T) {
	// max_total_steps = 50, threshold = 10: 50 > 10, so warning should fire.
	src := loopWorkflowSrc(0, 50, 10)
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("unexpected compile error: %s", diags.Error())
	}
	var warned bool
	for _, d := range diags {
		if strings.Contains(d.Summary, "appears in a loop") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("expected back-edge warning with custom threshold; got diags: %v", diags)
	}
}

// TestCompile_BackEdgeWarning_ThroughBranch verifies that a loop mediated by
// a branch node (step → branch → step) is detected as a back-edge, so the
// warning fires even when there is no direct step-to-step edge.
func TestCompile_BackEdgeWarning_ThroughBranch(t *testing.T) {
	src := `
workflow "t" {
  adapter "fake" "default" {}
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
  step "work" {
    adapter = adapter.fake.default
    outcome "check" { transition_to = "decide" }
    outcome "done"  { transition_to = "done" }
  }
  branch "decide" {
    arm {
      when          = true
      transition_to = "work"
    }
    default {
      transition_to = "done"
    }
  }
  state "done" { terminal = true }
  policy { max_total_steps = 500 }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("unexpected compile error: %s", diags.Error())
	}
	var warned bool
	for _, d := range diags {
		if strings.Contains(d.Summary, "appears in a loop") && strings.Contains(d.Summary, "max_visits") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("expected back-edge warning for step->branch->step loop; got diags: %v", diags)
	}
}

// TestCompile_NegativeMaxVisitsWarnThreshold_Rejected verifies that a negative
// max_visits_warn_threshold is rejected at compile time with a clear diagnostic.
func TestCompile_NegativeMaxVisitsWarnThreshold_Rejected(t *testing.T) {
	src := `
workflow "loop" {
  version       = "0.1"
  initial_state = "execute"
  target_state  = "done"
  policy {
    max_visits_warn_threshold = -1
  }
  step "execute" {
    adapter = adapter.fake.default
    outcome "success" { transition_to = "done" }
  }
  state "done" {
    terminal = true
    success  = true
  }
}
`
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for negative max_visits_warn_threshold; got none")
	}
	var found bool
	for _, d := range diags {
		if strings.Contains(d.Summary, "max_visits_warn_threshold must be >= 0") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected diagnostic containing \"max_visits_warn_threshold must be >= 0\"; got: %s", diags.Error())
	}
}

// TestCompile_BackEdgeWarning_DisabledByZeroThreshold verifies that threshold=0 disables warnings.
func TestCompile_BackEdgeWarning_DisabledByZeroThreshold(t *testing.T) {
	// max_total_steps = 1000, threshold = 0 (disabled): no warning expected.
	src := loopWorkflowSrc(0, 1000, 0)
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("unexpected compile error: %s", diags.Error())
	}
	for _, d := range diags {
		if strings.Contains(d.Summary, "appears in a loop") {
			t.Errorf("unexpected back-edge warning when threshold = 0: %s", d.Summary)
		}
	}
}
