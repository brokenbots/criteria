package workflow_test

import (
	"strings"
	"testing"

	"github.com/brokenbots/criteria/workflow"
)

const verificationWorkflowTmpl = `
workflow {
  name          = "w"
  version       = "0.1"
  initial_state = "done"
  target_state  = "done"
  %s
}

state "done" { terminal = true }
`

// TestCompile_VerificationAttribute_Valid confirms each accepted value parses,
// compiles, and is surfaced on the graph.
func TestCompile_VerificationAttribute_Valid(t *testing.T) {
	for _, mode := range []string{"off", "warn", "strict"} {
		t.Run(mode, func(t *testing.T) {
			src := strings.Replace(verificationWorkflowTmpl, "%s", `verification = "`+mode+`"`, 1)
			spec, diags := workflow.Parse("test.hcl", []byte(src))
			if diags.HasErrors() {
				t.Fatalf("parse: %v", diags)
			}
			g, diags := workflow.Compile(spec, nil)
			if diags.HasErrors() {
				t.Fatalf("compile: %v", diags)
			}
			if g.Verification != mode {
				t.Errorf("graph.Verification = %q, want %q", g.Verification, mode)
			}
		})
	}
}

// TestCompile_VerificationAttribute_Omitted leaves the field empty so the CLI
// transition default applies downstream.
func TestCompile_VerificationAttribute_Omitted(t *testing.T) {
	src := strings.Replace(verificationWorkflowTmpl, "%s", "", 1)
	spec, diags := workflow.Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %v", diags)
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %v", diags)
	}
	if g.Verification != "" {
		t.Errorf("graph.Verification = %q, want empty", g.Verification)
	}
}

// TestCompile_VerificationAttribute_Invalid rejects an unknown value at compile.
func TestCompile_VerificationAttribute_Invalid(t *testing.T) {
	src := strings.Replace(verificationWorkflowTmpl, "%s", `verification = "loose"`, 1)
	spec, diags := workflow.Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %v", diags)
	}
	_, diags = workflow.Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error for invalid verification value")
	}
	if !strings.Contains(diags.Error(), "verification must be one of") {
		t.Errorf("error %q does not mention the allowed values", diags.Error())
	}
}
