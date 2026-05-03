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
        adapter = "fake"
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
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
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
        adapter = "fake_producer"
        outcome "success" { transition_to = "_continue" }
      }
    }

    outcome "all_succeeded" { transition_to = "consume" }
    outcome "any_failed"    { transition_to = "done" }
  }

  step "consume" {
    adapter = "fake_consumer"
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
	if err := New(g, loader, sink).Run(context.Background()); err != nil {
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
