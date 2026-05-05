package workflow

import (
	"testing"

	"github.com/hashicorp/hcl/v2"
)

func requireExactErrorSummary(t *testing.T, diags hcl.Diagnostics, want string) {
	t.Helper()
	if !diags.HasErrors() {
		t.Fatal("expected compile error")
	}

	got := make([]string, 0)
	for _, d := range diags {
		if d.Severity == hcl.DiagError {
			got = append(got, d.Summary)
		}
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 error diagnostic, got %d: %#v", len(got), got)
	}
	if got[0] != want {
		t.Fatalf("expected diagnostic %q, got %q", want, got[0])
	}
}

func TestParseAndCompileAdapterLifecycleWorkflow(t *testing.T) {
	src := `
workflow "session_flow" {
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}

adapter "copilot" "exec" {
  on_crash = "respawn"
  config {
    mode = "executor"
  }
}

adapter "copilot" "review" {
  config { }
}

step "run" {
  target = adapter.copilot.exec
  outcome "approved" { next = "done" }
}

state "done" { terminal = true }
`

	spec, diags := Parse("adapters.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}

	if len(g.Adapters) != 2 {
		t.Fatalf("expected 2 adapters, got %d", len(g.Adapters))
	}
	if g.Adapters["copilot.exec"].Type != "copilot" || g.Adapters["copilot.exec"].Name != "exec" {
		t.Fatalf("unexpected adapter for copilot.exec: %+v", g.Adapters["copilot.exec"])
	}
	if g.Adapters["copilot.review"].OnCrash != "fail" {
		t.Fatalf("expected default fail on_crash for copilot.review, got %q", g.Adapters["copilot.review"].OnCrash)
	}

	run := g.Steps["run"]
	if run.AdapterRef != "copilot.exec" {
		t.Fatalf("run step did not preserve adapter: %+v", run)
	}
	if run.OnCrash != "respawn" {
		t.Fatalf("run step expected inherited on_crash=respawn, got %q", run.OnCrash)
	}
}

// TestStep_LegacyLifecycleAttr_HardError tests that lifecycle attributes on steps produce a hard parse error.
func TestStep_LegacyLifecycleAttr_HardError(t *testing.T) {
	src := `
workflow "test" {
  version       = "0.1"
  initial_state = "step_one"
  target_state  = "done"
}

adapter "noop" "default" {
  config { }
}

step "step_one" {
  target = adapter.noop.default
  lifecycle = "open"
  outcome "success" { next = "done" }
}

state "done" { terminal = true }
`

	_, diags := Parse("test.hcl", []byte(src))
	requireExactErrorSummary(t, diags, `removed attribute "lifecycle" on steps`)
}

func TestCompileAdapterValidationErrors(t *testing.T) {
	tests := []struct {
		name        string
		src         string
		wantSummary string
	}{
		{
			name: "undeclared adapter",
			src: `
workflow "x" {
  version = "0.1"
  initial_state = "a"
  target_state = "done"
}
step "a" {
  target = adapter.ghost.default
  outcome "ok" { next = "done" }
}
state "done" { terminal = true }
`,
			wantSummary: `step "a": referenced adapter "ghost.default" is not declared`,
		},
		{
			name: "duplicate adapter",
			src: `
workflow "x" {
  version = "0.1"
  initial_state = "a"
  target_state = "done"
}
adapter "copilot" "worker" {
  config { }
}
adapter "copilot" "worker" {
  config { }
}
step "a" {
  target = adapter.copilot.worker
  outcome "ok" { next = "done" }
}
state "done" { terminal = true }
`,
			wantSummary: `duplicate adapter "copilot.worker"`,
		},
		{
			name: "invalid on_crash enum",
			src: `
workflow "x" {
  version = "0.1"
  initial_state = "a"
  target_state = "done"
}
adapter "shell" "default" {
  on_crash = "explode"
  config { }
}
step "a" {
  target = adapter.shell.default
  outcome "ok" { next = "done" }
}
state "done" { terminal = true }
`,
			wantSummary: `adapter "shell.default": invalid on_crash "explode"`,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			spec, diags := Parse("case.hcl", []byte(tc.src))
			if diags.HasErrors() {
				t.Fatalf("parse: %s", diags.Error())
			}
			_, diags = Compile(spec, nil)
			requireExactErrorSummary(t, diags, tc.wantSummary)
		})
	}
}
