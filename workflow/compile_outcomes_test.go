package workflow

// compile_outcomes_test.go — W15 tests for the outcome block schema (next,
// output expression, return sentinel, default_outcome, and legacy rejection).

import (
	"strings"
	"testing"
)

// minimalWorkflowWithStep wraps a step body in a minimal compilable workflow.
func minimalWorkflowWithStep(stepBody string) string {
	return `
workflow "t" {
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
  adapter "noop" "default" {}
  step "work" {
    target = adapter.noop.default
    ` + stepBody + `
  }
  state "done" {
    terminal = true
    success  = true
  }
  state "failed" {
    terminal = true
    success  = false
  }
}
`
}

// TestCompileOutcome_NextIsStep verifies that next = "<step_name>" compiles
// and stores the target step name in CompiledOutcome.Next.
func TestCompileOutcome_NextIsStep(t *testing.T) {
	src := `
workflow "t" {
  version       = "0.1"
  initial_state = "a"
  target_state  = "done"
  adapter "noop" "default" {}
  step "a" {
    target = adapter.noop.default
    outcome "success" { next = "b" }
  }
  step "b" {
    target = adapter.noop.default
    outcome "success" { next = "done" }
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
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	co, ok := g.Steps["a"].Outcomes["success"]
	if !ok {
		t.Fatal("outcome 'success' not found on step 'a'")
	}
	if co.Next != "b" {
		t.Errorf("Next: got %q want %q", co.Next, "b")
	}
	if co.OutputExpr != nil {
		t.Error("OutputExpr should be nil when not declared")
	}
}

// TestCompileOutcome_NextIsState verifies that next = "<terminal_state>" compiles
// and routes to the terminal state node.
func TestCompileOutcome_NextIsState(t *testing.T) {
	src := minimalWorkflowWithStep(`
    outcome "success" { next = "done" }
    outcome "failure" { next = "failed" }
`)
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	if co := g.Steps["work"].Outcomes["success"]; co.Next != "done" {
		t.Errorf("success.Next: got %q want %q", co.Next, "done")
	}
	if co := g.Steps["work"].Outcomes["failure"]; co.Next != "failed" {
		t.Errorf("failure.Next: got %q want %q", co.Next, "failed")
	}
}

// TestCompileOutcome_NextIsReturn verifies that next = "return" stores the
// ReturnSentinel constant (not treated as an unknown node name).
func TestCompileOutcome_NextIsReturn(t *testing.T) {
	src := `
workflow "t" {
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
  adapter "noop" "default" {}
  step "work" {
    target = adapter.noop.default
    outcome "success" { next = "return" }
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
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	co, ok := g.Steps["work"].Outcomes["success"]
	if !ok {
		t.Fatal("outcome 'success' not found")
	}
	if co.Next != ReturnSentinel {
		t.Errorf("Next: got %q want %q", co.Next, ReturnSentinel)
	}
}

// TestCompileOutcome_OutputExprFolds verifies that a literal output expression
// compiles without error and is stored in CompiledOutcome.OutputExpr.
func TestCompileOutcome_OutputExprFolds(t *testing.T) {
	src := minimalWorkflowWithStep(`
    outcome "success" {
      next   = "done"
      output = { status = "ok" }
    }
`)
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	co, ok := g.Steps["work"].Outcomes["success"]
	if !ok {
		t.Fatal("outcome 'success' not found")
	}
	if co.OutputExpr == nil {
		t.Fatal("OutputExpr should not be nil when output is declared")
	}
}

// TestCompileOutcome_OutputExprRuntimeRef verifies that an output expression
// referencing a runtime variable (steps.*) is accepted at compile time — it is
// not evaluated until the step runs.
func TestCompileOutcome_OutputExprRuntimeRef(t *testing.T) {
	src := `
workflow "t" {
  version       = "0.1"
  initial_state = "a"
  target_state  = "done"
  adapter "noop" "default" {}
  step "a" {
    target = adapter.noop.default
    outcome "success" {
      next   = "b"
      output = { result = steps.a.exit_code }
    }
  }
  step "b" {
    target = adapter.noop.default
    outcome "success" { next = "done" }
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
	if diags.HasErrors() {
		t.Fatalf("compile should not reject runtime refs: %s", diags.Error())
	}
}

// TestCompileOutcome_LegacyTransitionTo_HardError verifies that the removed
// transition_to attribute on outcome blocks produces a parse error.
func TestCompileOutcome_LegacyTransitionTo_HardError(t *testing.T) {
	src := minimalWorkflowWithStep(`
    outcome "success" { transition_to = "done" }
`)
	_, diags := Parse("t.hcl", []byte(src))
	if !diags.HasErrors() {
		t.Fatal("expected parse error for transition_to, got none")
	}
	if !strings.Contains(diags.Error(), "transition_to") {
		t.Errorf("error should mention transition_to, got: %s", diags.Error())
	}
}

// TestCompileStep_DefaultOutcomeMissing verifies that default_outcome referring
// to an undeclared outcome name is a compile error.
func TestCompileStep_DefaultOutcomeMissing(t *testing.T) {
	src := minimalWorkflowWithStep(`
    outcome "success" { next = "done" }
    default_outcome = "nonexistent"
`)
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for undeclared default_outcome, got none")
	}
	if !strings.Contains(diags.Error(), "default_outcome") {
		t.Errorf("error should mention default_outcome, got: %s", diags.Error())
	}
}

// TestCompileOutcome_OutputExprNotObject verifies that output = "string" (not
// an object literal) is rejected at compile time.
func TestCompileOutcome_OutputExprNotObject(t *testing.T) {
	src := minimalWorkflowWithStep(`
    outcome "success" {
      next   = "done"
      output = "not-an-object"
    }
`)
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for non-object output expression, got none")
	}
	if !strings.Contains(diags.Error(), "object") {
		t.Errorf("error should mention 'object', got: %s", diags.Error())
	}
}

// TestCompileOutcome_OutputExprBadRef verifies that output = { bad = nope.missing }
// (a reference to an undefined namespace) fails at compile time.
func TestCompileOutcome_OutputExprBadRef(t *testing.T) {
	src := minimalWorkflowWithStep(`
    outcome "success" {
      next   = "done"
      output = { bad = nope.missing }
    }
`)
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatalf("expected compile error for unknown reference in output, got none")
	}
}

// TestCompileOutcome_OutputExprSubworkflowRef verifies that an outcome output
// expression referencing the "subworkflow" namespace is accepted at compile
// time — it is only resolved at runtime when the subworkflow step has executed.
func TestCompileOutcome_OutputExprSubworkflowRef(t *testing.T) {
	src := `
workflow "t" {
  version       = "0.1"
  initial_state = "a"
  target_state  = "done"
  adapter "noop" "default" {}
  step "a" {
    target = adapter.noop.default
    outcome "success" {
      next   = "done"
      output = { result = subworkflow.answer }
    }
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
	if diags.HasErrors() {
		t.Fatalf("compile should not reject subworkflow.* refs in outcome.output: %s", diags.Error())
	}
}

func TestCompileStep_NameReturn_HardError(t *testing.T) {
	src := `
workflow "t" {
  version       = "0.1"
  initial_state = "return"
  target_state  = "done"
  adapter "noop" "default" {}
  step "return" {
    target = adapter.noop.default
    outcome "success" { next = "done" }
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
		t.Fatal("expected compile error for step named 'return', got none")
	}
	if !strings.Contains(diags.Error(), "return") {
		t.Errorf("error should mention 'return', got: %s", diags.Error())
	}
}

// TestCompileReservedName_ReturnForNonStepNodes verifies that "return" is
// rejected as a name for states, waits, approvals, and branches in addition
// to steps — since any such node named "return" would silently cause
// resolveTransitions to treat next = "return" as a ReturnSentinel instead of
// a real node reference.
func TestCompileReservedName_ReturnForNonStepNodes(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			name: "state",
			src: `
workflow "t" {
  version       = "0.1"
  initial_state = "step1"
  target_state  = "return"
  adapter "noop" "default" {}
  step "step1" {
    target = adapter.noop.default
    outcome "success" { next = "return" }
  }
  state "return" {
    terminal = true
    success  = true
  }
}`,
		},
		{
			name: "branch",
			src: `
workflow "t" {
  version       = "0.1"
  initial_state = "step1"
  target_state  = "done"
  adapter "noop" "default" {}
  step "step1" {
    target = adapter.noop.default
    outcome "success" { next = "done" }
  }
  branch "return" {
    arm {
      when         = true
      transition_to = "done"
    }
    default { transition_to = "done" }
  }
  state "done" {
    terminal = true
    success  = true
  }
}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, diags := Parse("t.hcl", []byte(tc.src))
			if diags.HasErrors() {
				t.Fatalf("parse: %s", diags.Error())
			}
			_, diags = Compile(spec, nil)
			if !diags.HasErrors() {
				t.Fatalf("expected compile error for %s named 'return', got none", tc.name)
			}
			if !strings.Contains(diags.Error(), "return") {
				t.Errorf("error should mention 'return', got: %s", diags.Error())
			}
		})
	}
}
