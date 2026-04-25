package workflow

import (
	"os"
	"path/filepath"
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

func TestParseAndCompileAgentLifecycleWorkflow(t *testing.T) {
	src := `
workflow "session_flow" {
  version       = "0.1"
  initial_state = "open_exec"
  target_state  = "done"

  agent "exec" {
    adapter  = "copilot"
    on_crash = "respawn"
  }

  agent "review" {
    adapter = "copilot"
  }

  step "open_exec" {
    agent     = "exec"
    lifecycle = "open"
    config = {
      mode = "executor"
    }
    outcome "success" { transition_to = "run" }
  }

  step "run" {
    agent = "exec"
    outcome "approved" { transition_to = "close_exec" }
  }

  step "close_exec" {
    agent     = "exec"
    lifecycle = "close"
    outcome "success" { transition_to = "done" }
  }

  state "done" { terminal = true }
}
`

	spec, diags := Parse("agents.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}

	if len(g.Agents) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(g.Agents))
	}
	if g.Agents["exec"].Adapter != "copilot" {
		t.Fatalf("unexpected adapter for exec: %q", g.Agents["exec"].Adapter)
	}
	if g.Agents["review"].OnCrash != "fail" {
		t.Fatalf("expected default fail on_crash for review, got %q", g.Agents["review"].OnCrash)
	}

	open := g.Steps["open_exec"]
	if open.Agent != "exec" || open.Lifecycle != "open" {
		t.Fatalf("open step did not preserve agent/lifecycle: %+v", open)
	}
	if open.OnCrash != "respawn" {
		t.Fatalf("open step expected inherited on_crash=respawn, got %q", open.OnCrash)
	}

	run := g.Steps["run"]
	if run.OnCrash != "respawn" {
		t.Fatalf("run step expected inherited on_crash=respawn, got %q", run.OnCrash)
	}

	close := g.Steps["close_exec"]
	if close.Lifecycle != "close" {
		t.Fatalf("close step lifecycle mismatch: %q", close.Lifecycle)
	}
}

func TestCompileAgentValidationErrors(t *testing.T) {
	tests := []struct {
		name        string
		src         string
		wantSummary string
	}{
		{
			name: "undeclared agent",
			src: `
workflow "x" {
  version = "0.1"
  initial_state = "a"
  target_state = "done"

  step "a" {
    agent = "ghost"
    outcome "ok" { transition_to = "done" }
  }

  state "done" { terminal = true }
}
`,
			wantSummary: `step "a": unknown agent "ghost"`,
		},
		{
			name: "duplicate agent",
			src: `
workflow "x" {
  version = "0.1"
  initial_state = "a"
  target_state = "done"

  agent "worker" { adapter = "copilot" }
  agent "worker" { adapter = "copilot" }

  step "a" {
    adapter = "shell"
    outcome "ok" { transition_to = "done" }
  }

  state "done" { terminal = true }
}
`,
			wantSummary: `duplicate agent "worker"`,
		},
		{
			name: "missing adapter and agent",
			src: `
workflow "x" {
  version = "0.1"
  initial_state = "a"
  target_state = "done"

  step "a" {
    outcome "ok" { transition_to = "done" }
  }

  state "done" { terminal = true }
}
`,
			wantSummary: `step "a": exactly one of adapter or agent must be set`,
		},
		{
			name: "both adapter and agent",
			src: `
workflow "x" {
  version = "0.1"
  initial_state = "a"
  target_state = "done"

  agent "worker" { adapter = "copilot" }

  step "a" {
    adapter = "shell"
    agent = "worker"
    outcome "ok" { transition_to = "done" }
  }

  state "done" { terminal = true }
}
`,
			wantSummary: `step "a": exactly one of adapter or agent must be set`,
		},
		{
			name: "lifecycle requires agent",
			src: `
workflow "x" {
  version = "0.1"
  initial_state = "a"
  target_state = "done"

  step "a" {
    adapter = "shell"
    lifecycle = "open"
    outcome "ok" { transition_to = "done" }
  }

  state "done" { terminal = true }
}
`,
			wantSummary: `step "a": lifecycle requires agent`,
		},
		{
			name: "invalid lifecycle enum",
			src: `
workflow "x" {
  version = "0.1"
  initial_state = "a"
  target_state = "done"

  agent "worker" { adapter = "copilot" }

  step "a" {
    agent = "worker"
    lifecycle = "wat"
    outcome "ok" { transition_to = "done" }
  }

  state "done" { terminal = true }
}
`,
			wantSummary: `step "a": invalid lifecycle "wat"`,
		},
		{
			name: "invalid on_crash enum",
			src: `
workflow "x" {
  version = "0.1"
  initial_state = "a"
  target_state = "done"

  agent "worker" { adapter = "copilot" }

  step "a" {
    agent = "worker"
    on_crash = "explode"
    outcome "ok" { transition_to = "done" }
  }

  state "done" { terminal = true }
}
`,
			wantSummary: `step "a": invalid on_crash "explode"`,
		},
		{
			name: "close with config",
			src: `
workflow "x" {
  version = "0.1"
  initial_state = "a"
  target_state = "done"

  agent "worker" { adapter = "copilot" }

  step "a" {
    agent = "worker"
    lifecycle = "close"
    config = {
      nope = "1"
    }
    outcome "ok" { transition_to = "done" }
  }

  state "done" { terminal = true }
}
`,
			wantSummary: `step "a": lifecycle "close" must not include config`,
		},
		{
			name: "invalid agent on_crash enum",
			src: `
workflow "x" {
  version = "0.1"
  initial_state = "a"
  target_state = "done"

  agent "worker" {
    adapter = "copilot"
    on_crash = "explode"
  }

  step "a" {
    agent = "worker"
    outcome "ok" { transition_to = "done" }
  }

  state "done" { terminal = true }
}
`,
			wantSummary: `agent "worker": invalid on_crash "explode"`,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			spec, diags := Parse("case.hcl", []byte(tc.src))
			if diags.HasErrors() {
				t.Fatalf("parse: %s", diags.Error())
			}
			_, diags = Compile(spec)
			requireExactErrorSummary(t, diags, tc.wantSummary)
		})
	}
}

func TestLifecycleStepsStillValidateTargetsAndReachability(t *testing.T) {
	src := `
workflow "x" {
  version = "0.1"
  initial_state = "open"
  target_state = "done"

  agent "worker" { adapter = "copilot" }

  step "open" {
    agent = "worker"
    lifecycle = "open"
    outcome "ok" { transition_to = "run" }
  }

  step "run" {
    agent = "worker"
    outcome "ok" { transition_to = "missing" }
  }

  state "done" { terminal = true }
}
`

	spec, diags := Parse("targets.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	_, diags = Compile(spec)
	requireExactErrorSummary(t, diags, `step "run" outcome "ok" -> unknown target "missing"`)
}

func TestAdapterOnlyBackwardCompatibility(t *testing.T) {
	src := `
workflow "x" {
  version = "0.1"
  initial_state = "a"
  target_state = "done"

  step "a" {
    adapter = "shell"
    outcome "ok" { transition_to = "done" }
  }

  state "done" { terminal = true }
}
`

	spec, diags := Parse("compat.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	if g.Steps["a"].Adapter != "shell" {
		t.Fatalf("expected adapter shell, got %q", g.Steps["a"].Adapter)
	}
}

func TestTwoAgentLoopFixtureCompiles(t *testing.T) {
	path := filepath.Join("testdata", "two_agent_loop.hcl")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	spec, diags := Parse(path, src)
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := Compile(spec)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	if g.InitialState != "open_executor" || g.TargetState != "done" {
		t.Fatalf("unexpected initial/target: %s/%s", g.InitialState, g.TargetState)
	}
}
