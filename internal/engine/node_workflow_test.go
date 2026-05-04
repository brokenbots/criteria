package engine

// node_workflow_test.go — unit and integration tests for seedChildVars and
// runWorkflowBody behavior introduced by the schema-unification workstream
// (phase3/08-schema-unification). The tests cover:
//
//   - seedChildVars threads each.* from parent scope into child scope (unit).
//   - seedChildVars returns an error when a required body variable has no
//     parentInput binding (unit).
//   - A body variable declared in the body and passed via input={} is resolved
//     correctly inside body step inputs (integration).
//   - Output block expressions are evaluated against the child's final scope
//     (body steps.*), not the outer workflow scope (integration).

import (
	"context"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/internal/plugin"
	"github.com/brokenbots/criteria/workflow"
)

// --- unit tests for seedChildVars ---

// TestSeedChildVars_EachThreaded verifies that each.value and each._idx from
// the parent scope are copied into the child scope by seedChildVars, so that
// body steps can reference each.* without explicit input declarations.
func TestSeedChildVars_EachThreaded(t *testing.T) {
	body := &workflow.FSMGraph{
		Variables: map[string]*workflow.VariableNode{},
	}

	parentVars := map[string]cty.Value{
		"var":   cty.EmptyObjectVal,
		"steps": cty.EmptyObjectVal,
		"each": cty.ObjectVal(map[string]cty.Value{
			"value": cty.StringVal("item-x"),
			"_idx":  cty.NumberIntVal(2),
		}),
	}

	child, err := seedChildVars(body, cty.NilVal, parentVars)
	if err != nil {
		t.Fatalf("seedChildVars: %v", err)
	}

	each, ok := child["each"]
	if !ok {
		t.Fatal("each not present in child vars")
	}
	if got := each.GetAttr("value").AsString(); got != "item-x" {
		t.Errorf("each.value: want %q, got %q", "item-x", got)
	}
	idx, _ := each.GetAttr("_idx").AsBigFloat().Int64()
	if idx != 2 {
		t.Errorf("each._idx: want 2, got %d", idx)
	}
}

// TestSeedChildVars_MissingRequiredVar verifies that seedChildVars returns an
// error when the body declares a required variable (no default) that is absent
// from the parentInput object. This is the runtime safety net complementing the
// compile-time check in compileWorkflowStep.
func TestSeedChildVars_MissingRequiredVar(t *testing.T) {
	body := &workflow.FSMGraph{
		Variables: map[string]*workflow.VariableNode{
			"topic": {Name: "topic", Type: cty.String, Default: cty.NilVal},
		},
	}

	parentVars := map[string]cty.Value{
		"var":   cty.EmptyObjectVal,
		"steps": cty.EmptyObjectVal,
	}

	_, err := seedChildVars(body, cty.NilVal, parentVars)
	if err == nil {
		t.Fatal("expected error for missing required variable, got nil")
	}
}

// --- integration tests (full compile + engine run) ---

// TestRunWorkflowBody_BodyInputBindsVar verifies that a body variable declared
// in the workflow { } block and bound via the parent step's input={} attribute
// is resolved correctly when body steps evaluate their input expressions.
//
// The outer variable "prefix" (default "hello") is passed into the body as
// input = { prefix = var.prefix }. The body step uses `var.prefix` in its
// input, which should resolve to "hello".
func TestRunWorkflowBody_BodyInputBindsVar(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "process"
  target_state  = "done"

  variable "prefix" {
    type    = "string"
    default = "hello"
  }

  step "process" {
    type     = "workflow"
    for_each = ["a"]

    input = { prefix = var.prefix }

    workflow {
      variable "prefix" {
        type = "string"
      }
      step "body" {
        adapter = adapter.fake
        input {
          label = var.prefix
        }
        outcome "success" { transition_to = "_continue" }
      }
    }

    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "done" }
  }

  state "done" {
    terminal = true
    success  = true
  }
}`)

	var captured []map[string]string
	sink := &iterSink{}
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{
		"fake": &captureInputPlugin{outcome: "success", capture: &captured},
	}}
	if err := New(g, loader, sink, WithAutoBootstrapAdapters()).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal: %s ok=%v", sink.terminal, sink.terminalOK)
	}

	// Find the body step invocation (it should have the "label" key).
	var bodyInput map[string]string
	for _, inp := range captured {
		if _, ok := inp["label"]; ok {
			bodyInput = inp
			break
		}
	}
	if bodyInput == nil {
		t.Fatal("body step never captured or missing 'label' key")
	}
	if got := bodyInput["label"]; got != "hello" {
		t.Errorf("body step var.prefix: want %q, got %q", "hello", got)
	}
}

// TestRunWorkflowBody_NoOuterStepLeakage is a regression test verifying that
// body step outputs are never visible in the outer workflow's steps.* scope.
//
// The body step "inner" runs and produces output, but only the body's child
// scope is aware of it. The outer "check" step references steps.inner.result
// in its input — if body outputs leaked to the outer scope (as in the old
// childSt.Vars = st.Vars aliasing bug), this run would succeed. With proper
// scope isolation, the expression is unresolvable in the outer scope and the
// run must fail with an error.
func TestRunWorkflowBody_NoOuterStepLeakage(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "produce"
  target_state  = "done"

  step "produce" {
    type     = "workflow"
    for_each = ["x"]

    workflow {
      step "inner" {
        adapter = adapter.fake_producer
        outcome "success" { transition_to = "_continue" }
      }
    }

    outcome "all_succeeded" { transition_to = "check" }
    outcome "any_failed"    { transition_to = "done" }
  }

  step "check" {
    adapter = adapter.fake_consumer
    input {
      received = steps.inner.result
    }
    outcome "success" { transition_to = "done" }
  }

  state "done" {
    terminal = true
    success  = true
  }
}`)

	sink := &iterSink{}
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{
		"fake_producer": &captureOutputPlugin{
			outcomes: []string{"success"},
			outputs:  []map[string]string{{"result": "body-only-output"}},
		},
		"fake_consumer": &captureInputPlugin{outcome: "success", capture: &[]map[string]string{}},
	}}

	err := New(g, loader, sink, WithAutoBootstrapAdapters()).Run(context.Background())
	if err == nil {
		t.Fatal("expected error: body step outputs must not be visible in outer steps.* scope (no-leakage regression)")
	}
}

// TestRunWorkflowBody_OutputUsesChildStepsScope verifies that output{} block
// expressions in a type="workflow" step are evaluated against the body's final
// variable scope (childFinalVars), so that references to `steps.<body_step>.*`
// resolve correctly. This test uses a plugin that returns a known output value
// from the body step, then checks that the outer step accumulates it via the
// output{} block.
func TestRunWorkflowBody_OutputUsesChildStepsScope(t *testing.T) {
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "produce"
  target_state  = "done"

  step "produce" {
    type     = "workflow"
    for_each = ["x"]

    workflow {
      output "tag" {
        value = steps.inner.result
      }
      step "inner" {
        adapter = adapter.fake_producer
        outcome "success" { transition_to = "_continue" }
      }
    }

    outcome "all_succeeded" { transition_to = "consume" }
    outcome "any_failed"    { transition_to = "done" }
  }

  step "consume" {
    adapter = adapter.fake_consumer
    input {
      received = steps.produce[0].tag
    }
    outcome "success" { transition_to = "done" }
  }

  state "done" {
    terminal = true
    success  = true
  }
}`)

	// Body step "inner" returns output "result" = "child-output".
	var consumeCapture []map[string]string
	sink := &iterSink{}
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{
		"fake_producer": &captureOutputPlugin{
			outcomes: []string{"success"},
			outputs:  []map[string]string{{"result": "child-output"}},
		},
		"fake_consumer": &captureInputPlugin{outcome: "success", capture: &consumeCapture},
	}}
	if err := New(g, loader, sink, WithAutoBootstrapAdapters()).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal: %s ok=%v", sink.terminal, sink.terminalOK)
	}

	// The consume step should have received the output from the body step via
	// the output{} block: steps.produce[0].tag = "child-output".
	if len(consumeCapture) == 0 {
		t.Fatal("consume step never executed")
	}
	if got := consumeCapture[0]["received"]; got != "child-output" {
		t.Errorf("output block value via child scope: want %q, got %q", "child-output", got)
	}
}

// TestRunWorkflowBody_ScalarInputFails is a regression for the runtime
// object-shape contract on body input. When a for_each step uses
// `input = each.value` and each.value evaluates to a string (not an object),
// the run must fail with a clear error — not silently ignore the malformed
// input. This covers the runtime path that the compile-time FoldExpr check
// cannot reach for runtime-only namespaces like each.*.
func TestRunWorkflowBody_ScalarInputFails(t *testing.T) {
	// Compile succeeds: each.value is a runtime-only namespace, so FoldExpr
	// returns foldable=false and the object-shape check is deferred to runtime.
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "process"
  target_state  = "done"

  step "process" {
    type     = "workflow"
    for_each = ["a"]

    input = each.value

    workflow {
      step "body" {
        adapter = adapter.fake
        outcome "success" { transition_to = "_continue" }
      }
    }

    outcome "all_succeeded" { transition_to = "done" }
    outcome "any_failed"    { transition_to = "done" }
  }

  state "done" {
    terminal = true
    success  = true
  }
}`)

	sink := &iterSink{}
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{
		"fake": &fakePlugin{name: "fake", outcome: "success"},
	}}

	err := New(g, loader, sink, WithAutoBootstrapAdapters()).Run(context.Background())
	if err == nil {
		t.Fatal("expected error: non-object body input (each.value = string) must be rejected at runtime")
	}
	if !strings.Contains(err.Error(), "object") {
		t.Errorf("error should mention 'object', got: %v", err)
	}
}
