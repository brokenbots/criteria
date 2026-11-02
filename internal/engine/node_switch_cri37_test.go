package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// compileParentWithSubworkflow writes a child subworkflow into parentDir/child
// and parses/compiles the parent HCL located in parentDir.
func compileParentWithSubworkflow(t *testing.T, parentHCL, parentDir string) *workflow.FSMGraph {
	t.Helper()
	childDir := filepath.Join(parentDir, "child")
	if err := os.Mkdir(childDir, 0o755); err != nil {
		t.Fatalf("create child dir: %v", err)
	}
	childHCL := `
workflow {
  name = "child"
  version       = "0.1"
  initial_state = "done"
  target_state  = "done"
}

output "verdict" {
  value = "reproduced"
}

state "done" {
  terminal = true
  success  = true
}
`
	if err := os.WriteFile(filepath.Join(childDir, "main.hcl"), []byte(childHCL), 0o644); err != nil {
		t.Fatalf("write child main.hcl: %v", err)
	}

	spec, diags := workflow.Parse("parent.hcl", []byte(parentHCL))
	if diags.HasErrors() {
		t.Fatalf("parse parent: %s", diags.Error())
	}
	g, diags := workflow.CompileWithOpts(spec, nil, workflow.CompileOpts{
		WorkflowDir:         parentDir,
		SubWorkflowResolver: &workflow.LocalSubWorkflowResolver{},
	})
	if diags.HasErrors() {
		t.Fatalf("compile parent: %s", diags.Error())
	}
	return g
}

// TestCRI37_SwitchReadsSubworkflowWrite is a regression test for the bug where
// a switch node evaluated its match condition against stale data after a preceding
// step wrote a subworkflow output into a data block.
//
// Shape: parent step invokes a subworkflow, the outcome writes
// subworkflow.verdict into data.internal.verdict.value, and a downstream
// switch routes to the success terminal only when the data block holds the
// freshly written value.
func TestCRI37_SwitchReadsSubworkflowWrite(t *testing.T) {
	parentHCL := `
workflow {
  name = "cri37_subworkflow"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}

data "internal" "verdict" {
  type  = string
  value = "initial"
}

subworkflow "child" {
  source = "./child"
}

step "run" {
  target = subworkflow.child

  outcome "success" {
    next = switch.route
    write {
      target = data.internal.verdict.value
      value  = subworkflow.verdict
    }
  }
  outcome "failure" {
    next = state.fail
  }
}

switch "route" {
  match {
    condition = data.internal.verdict.value == "reproduced"
    next      = state.done
  }
  default {
    next = state.fail
  }
}

state "done" {
  terminal = true
  success  = true
}
state "fail" {
  terminal = true
  success  = false
}
`
	parentDir := t.TempDir()
	g := compileParentWithSubworkflow(t, parentHCL, parentDir)

	vars := workflow.SeedVarsFromGraph(g)
	sink := &fakeSink{}
	eng := New(g, adapterhost.NewLoader(), sink, WithResumedVars(vars))
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if sink.terminal != "done" {
		t.Errorf("terminal = %q, want \"done\"; switch saw stale data and fell through", sink.terminal)
	}
	if !sink.terminalOK {
		t.Errorf("terminalOK = false, want true; switch routed to failure branch")
	}
	if sink.failure != "" {
		t.Errorf("unexpected run failure: %s", sink.failure)
	}
}

// TestCRI37_SwitchReadsAdapterStepWrite is a regression test for the same stale
// data bug with a plain adapter step instead of a subworkflow. The outcome writes
// a literal value into a data block; the downstream switch must read the
// updated value.
func TestCRI37_SwitchReadsAdapterStepWrite(t *testing.T) {
	const src = `
workflow {
  name = "cri37_adapter"
  version       = "0.1"
  initial_state = "set"
  target_state  = "done"
}

data "internal" "verdict" {
  type  = string
  value = "initial"
}

adapter "noop" "default" {}

step "set" {
  target = adapter.noop.default
  outcome "success" {
    next = switch.route
    write {
      target = data.internal.verdict.value
      value  = "reproduced"
    }
  }
  outcome "failure" {
    next = state.fail
  }
}

switch "route" {
  match {
    condition = data.internal.verdict.value == "reproduced"
    next      = state.done
  }
  default {
    next = state.fail
  }
}

state "done" {
  terminal = true
  success  = true
}
state "fail" {
  terminal = true
  success  = false
}
`
	spec, diags := workflow.Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := workflow.Compile(spec, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}

	loader := &fakeLoader{
		adapters: map[string]adapterhost.Handle{
			"noop":         &fakeAdapter{name: "noop", outcome: "success"},
			"noop.default": &fakeAdapter{name: "noop", outcome: "success"},
		},
	}
	vars := workflow.SeedVarsFromGraph(g)
	sink := &fakeSink{}
	eng := New(g, loader, sink, WithResumedVars(vars))
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if sink.terminal != "done" {
		t.Errorf("terminal = %q, want \"done\"; switch saw stale data and fell through", sink.terminal)
	}
	if !sink.terminalOK {
		t.Errorf("terminalOK = false, want true; switch routed to failure branch")
	}
	if sink.failure != "" {
		t.Errorf("unexpected run failure: %s", sink.failure)
	}
}
