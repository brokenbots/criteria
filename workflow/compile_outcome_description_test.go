package workflow

// compile_outcome_description_test.go — KB-259 tests for the outcome block's
// description attribute: it compiles onto CompiledOutcome (step outcomes and
// the reserved "default" outcome), is runtime-inert (an otherwise identical
// workflow compiles to the same compiled contract surface and routing), and
// is rejected on wait/approval outcomes where no compiled surface exists to
// carry it.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
)

// parseCompile is the shared harness: parse src and compile it with the noop
// adapter surface, returning the graph and both diagnostics sets.
func parseCompile(t *testing.T, src string) (*FSMGraph, hcl.Diagnostics) {
	t.Helper()
	spec, diags := Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	return Compile(spec, nil)
}

// TestOutcomeDescription_CarriedOnCompiledGraph: a step's outcome blocks with
// description compile onto CompiledOutcome.Description, including the
// reserved "default" outcome; routing targets are unaffected.
func TestOutcomeDescription_CarriedOnCompiledGraph(t *testing.T) {
	src := `
workflow {
  name = "t"
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}
adapter "noop" "default" {}
step "work" {
  target = adapter.noop.default
  outcome "success" {
    next        = step.done
    description = "the step finished cleanly; run bookkeeping may proceed"
  }
  outcome "failure" {
    next        = step.done
    description = "the step errored; route to the failure state"
  }
  outcome "default" {
    next        = return
    description = "the adapter returned an unrecognized verdict"
  }
}
state "done" {
  terminal = true
  success  = true
}
`
	g, diags := parseCompile(t, src)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	want := map[string]string{
		"success": "the step finished cleanly; run bookkeeping may proceed",
		"failure": "the step errored; route to the failure state",
	}
	for name, desc := range want {
		co, ok := g.Steps["work"].Outcomes[name]
		if !ok {
			t.Fatalf("outcome %q not compiled", name)
		}
		if co.Description != desc {
			t.Errorf("outcome %q description = %q, want %q", name, co.Description, desc)
		}
	}
	if g.Steps["work"].Outcomes["success"].Next != "done" {
		t.Errorf("description changed routing: success.Next = %q", g.Steps["work"].Outcomes["success"].Next)
	}
	if def := g.Steps["work"].DefaultOutcome; def == nil {
		t.Fatal("default outcome not compiled")
	} else if def.Description != "the adapter returned an unrecognized verdict" {
		t.Errorf("default outcome description = %q", def.Description)
	}
}

// TestOutcomeDescription_RuntimeInert: two workflows that differ only in
// outcome description attributes compile to identical contract surfaces and
// routing. Descriptions are model-facing metadata only.
func TestOutcomeDescription_RuntimeInert(t *testing.T) {
	withDesc := typedWorkflow(`
  outcome "success" {
    next    = step.done
    schema  = object({ severity = optional(string, "low"), attempts = number })
    require_comment = true
    fallback        = true
    output  = { severity = "low", attempts = 2 }
    description = "finished cleanly"
  }
`)
	withoutDesc := typedWorkflow(`
  outcome "success" {
    next    = step.done
    schema  = object({ severity = optional(string, "low"), attempts = number })
    require_comment = true
    fallback        = true
    output  = { severity = "low", attempts = 2 }
  }
`)
	gWith, diags := compileOutcomeSchemaSrc(t, withDesc)
	if diags.HasErrors() {
		t.Fatalf("compile with description: %s", diags.Error())
	}
	gWithout, diags := compileOutcomeSchemaSrc(t, withoutDesc)
	if diags.HasErrors() {
		t.Fatalf("compile without description: %s", diags.Error())
	}
	coWith := gWith.Steps["work"].Outcomes["success"]
	coWithout := gWithout.Steps["work"].Outcomes["success"]
	if coWith == nil || coWithout == nil {
		t.Fatal("outcome not compiled")
	}
	if coWith.Description == "" {
		t.Fatal("description did not compile through")
	}
	if coWithout.Description != "" {
		t.Fatal("control workflow unexpectedly carries a description")
	}
	// Contract surface and routing must be byte-for-byte identical.
	if coWith.Name != coWithout.Name || coWith.Next != coWithout.Next ||
		coWith.RequireComment != coWithout.RequireComment || coWith.Fallback != coWithout.Fallback {
		t.Errorf("declared outcome fields differ: %+v vs %+v", coWithout, coWith)
	}
	if (coWith.Schema == nil) != (coWithout.Schema == nil) ||
		coWith.Schema.FriendlyName() != coWithout.Schema.FriendlyName() {
		t.Errorf("Schema differs with description present: %v vs %v", coWithout.Schema, coWith.Schema)
	}
	if !bytes.Equal(coWith.SchemaJSON, coWithout.SchemaJSON) {
		t.Errorf("SchemaJSON differs with description present: %s vs %s", coWithout.SchemaJSON, coWith.SchemaJSON)
	}
	if (coWith.SchemaDefaults == nil) != (coWithout.SchemaDefaults == nil) {
		t.Errorf("SchemaDefaults nil-ness differs with description present")
	}
	if (coWith.OutputExpr == nil) != (coWithout.OutputExpr == nil) {
		t.Errorf("OutputExpr nil-ness differs with description present")
	}
	if len(coWith.Writes) != len(coWithout.Writes) {
		t.Errorf("Writes count differs with description present")
	}
}

// TestWaitOutcome_DescriptionRejected: wait outcomes have no compiled
// outcome surface — a declared description must fail loudly rather than be
// silently swallowed (KB-259 acceptance: nothing new silently swallowed).
func TestWaitOutcome_DescriptionRejected(t *testing.T) {
	src := `
workflow {
  name = "t"
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}
adapter "noop" "default" {}
step "work" {
  target = adapter.noop.default
  outcome "success" { next = wait.pause }
}
wait "pause" {
  duration = "1s"
  outcome "expired" {
    next        = step.done
    description = "the timeout elapsed"
  }
}
state "done" {
  terminal = true
  success  = true
}
`
	_, diags := parseCompile(t, src)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for description on wait outcome")
	}
	if !strings.Contains(diagsText(diags), `description is not supported on wait outcomes`) {
		t.Errorf("diagnostics = %s", diagsText(diags))
	}
}

// TestApprovalOutcome_DescriptionRejected: same as the wait case — approval
// outcomes are engine-mandated and carry no compiled contract surface.
func TestApprovalOutcome_DescriptionRejected(t *testing.T) {
	src := `
workflow {
  name = "t"
  version       = "0.1"
  initial_state = "work"
  target_state  = "done"
}
adapter "noop" "default" {}
step "work" {
  target = adapter.noop.default
  outcome "success" { next = approval.gate }
}
approval "gate" {
  approvers = ["alice"]
  reason    = "gate"
  outcome "approved" {
    next        = step.done
    description = "the reviewer accepted"
  }
  outcome "rejected" { next = step.done }
}
state "done" {
  terminal = true
  success  = true
}
`
	_, diags := parseCompile(t, src)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for description on approval outcome")
	}
	if !strings.Contains(diagsText(diags), `description is not supported on approval outcomes`) {
		t.Errorf("diagnostics = %s", diagsText(diags))
	}
}
