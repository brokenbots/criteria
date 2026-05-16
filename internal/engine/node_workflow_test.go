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

	"github.com/brokenbots/criteria/internal/adapterhost"
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
	t.Skip("test uses removed inline workflow body feature (W13); pending W14 subworkflow invocation support")
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "process"
  target_state  = "done"
}

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
      target = adapter.fake
      input {
        label = var.prefix
      }
      outcome "success" { next = "_continue" }
    }
  }

  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}`)

	var captured []map[string]string
	sink := &iterSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake": &captureInputAdapter{outcome: "success", capture: &captured},
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
	t.Skip("test uses removed inline workflow body feature (W13); pending W14 subworkflow invocation support")
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "produce"
  target_state  = "done"
}

step "produce" {
  type     = "workflow"
  for_each = ["x"]

  workflow {
    step "inner" {
      target = adapter.fake_producer
      outcome "success" { next = "_continue" }
    }
  }

  outcome "all_succeeded" { next = "check" }
  outcome "any_failed"    { next = "done" }
}

step "check" {
  target = adapter.fake_consumer
  input {
    received = steps.inner.result
  }
  outcome "success" { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}`)

	sink := &iterSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake_producer": &captureOutputAdapter{
			outcomes: []string{"success"},
			outputs:  []map[string]string{{"result": "body-only-output"}},
		},
		"fake_consumer": &captureInputAdapter{outcome: "success", capture: &[]map[string]string{}},
	}}

	err := New(g, loader, sink).Run(context.Background())
	if err == nil {
		t.Fatal("expected error: body step outputs must not be visible in outer steps.* scope (no-leakage regression)")
	}
}

// TestRunWorkflowBody_OutputUsesChildStepsScope verifies that output{} block
// expressions in a type="workflow" step are evaluated against the body's final
// variable scope (childFinalVars), so that references to `steps.<body_step>.*`
// resolve correctly. This test uses a adapter that returns a known output value
// from the body step, then checks that the outer step accumulates it via the
// output{} block.
func TestRunWorkflowBody_OutputUsesChildStepsScope(t *testing.T) {
	t.Skip("test uses removed inline workflow body feature (W13); pending W14 subworkflow invocation support")
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "produce"
  target_state  = "done"
}

step "produce" {
  type     = "workflow"
  for_each = ["x"]

  workflow {
    output "tag" {
      value = steps.inner.result
    }
    step "inner" {
      target = adapter.fake_producer
      outcome "success" { next = "_continue" }
    }
  }

  outcome "all_succeeded" { next = "consume" }
  outcome "any_failed"    { next = "done" }
}

step "consume" {
  target = adapter.fake_consumer
  input {
    received = steps.produce[0].tag
  }
  outcome "success" { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}`)

	// Body step "inner" returns output "result" = "child-output".
	var consumeCapture []map[string]string
	sink := &iterSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake_producer": &captureOutputAdapter{
			outcomes: []string{"success"},
			outputs:  []map[string]string{{"result": "child-output"}},
		},
		"fake_consumer": &captureInputAdapter{outcome: "success", capture: &consumeCapture},
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

// TestRunWorkflowBody_ScalarInputFails is a regression for the runtime
// object-shape contract on body input. When a for_each step uses
// `input = each.value` and each.value evaluates to a string (not an object),
// the run must fail with a clear error — not silently ignore the malformed
// input. This covers the runtime path that the compile-time FoldExpr check
// cannot reach for runtime-only namespaces like each.*.
func TestRunWorkflowBody_ScalarInputFails(t *testing.T) {
	t.Skip("test uses removed inline workflow body feature (W13); pending W14 subworkflow invocation support")
	// Compile succeeds: each.value is a runtime-only namespace, so FoldExpr
	// returns foldable=false and the object-shape check is deferred to runtime.
	g := compile(t, `
workflow "t" {
  version       = "0.1"
  initial_state = "process"
  target_state  = "done"
}

step "process" {
  type     = "workflow"
  for_each = ["a"]

  input = each.value

  workflow {
    step "body" {
      target = adapter.fake
      outcome "success" { next = "_continue" }
    }
  }

  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}`)

	sink := &iterSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake": &fakeAdapter{name: "fake", outcome: "success"},
	}}

	err := New(g, loader, sink).Run(context.Background())
	if err == nil {
		t.Fatal("expected error: non-object body input (each.value = string) must be rejected at runtime")
	}
	if !strings.Contains(err.Error(), "object") {
		t.Errorf("error should mention 'object', got: %v", err)
	}
}

// TestRunWorkflowBody_BodyAdapterIsolated verifies that adapters provisioned in a step's
// workflow body are cleaned up properly when the body completes (verifying isolation).
func TestRunWorkflowBody_BodyAdapterIsolated(t *testing.T) {
	t.Skip("test uses removed inline workflow body feature (W13); pending W14 subworkflow invocation support")
	// A simple iteration with a workflow body that uses an adapter
	parentG := compile(t, `
workflow "parent" {
  version       = "0.1"
  initial_state = "process"
  target_state  = "done"
}

step "process" {
  type     = "workflow"
  for_each = ["item"]

  workflow {
    step "body_step" {
      target = adapter.noop
      outcome "success" { next = "_continue" }
    }
  }

  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}`)

	bodyTracker := &lifecycleTrackingAdapter{
		fakeAdapter: fakeAdapter{name: "noop", outcome: "success"},
	}

	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"noop": bodyTracker,
	}}

	sink := &lifecycleTrackingSink{}
	eng := New(parentG, loader, sink)

	err := eng.Run(context.Background())
	if err != nil {
		t.Errorf("Run failed: %v", err)
	}

	// Verify that the adapter in the body was provisioned and torn down
	bodyTracker.mu.Lock()
	opens := bodyTracker.opensCount
	closes := bodyTracker.closesCount
	bodyTracker.mu.Unlock()

	if opens != 1 {
		t.Errorf("body adapter: expected 1 open, got %d", opens)
	}
	if closes != 1 {
		t.Errorf("body adapter: expected 1 close, got %d", closes)
	}
}

// TestRunWorkflowBody_BodyAndParentAdaptersIsolated verifies that adapters provisioned in a
// parent workflow stay open through body execution while body-scoped adapters are isolated
// and closed when the body completes.
func TestRunWorkflowBody_BodyAndParentAdaptersIsolated(t *testing.T) {
	t.Skip("test uses removed inline workflow body feature (W13); pending W14 subworkflow invocation support")
	// Parent uses adapter noop_a in multiple steps
	// Body uses adapter noop_b
	// Verify a opens/closes once for parent, b opens/closes once for body
	parentG := compile(t, `
workflow "parent" {
  version       = "0.1"
  initial_state = "pre"
  target_state  = "done"
}

step "pre" {
  target = adapter.noop_a
  outcome "success" { next = "body" }
}

step "body" {
  type     = "workflow"
  for_each = ["x"]

  workflow {
    step "inner" {
      target = adapter.noop_b
      outcome "success" { next = "_continue" }
    }
  }

  outcome "all_succeeded" { next = "post" }
  outcome "any_failed"    { next = "done" }
}

step "post" {
  target = adapter.noop_a
  outcome "success" { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}`)

	// We'll use two separate plugins to track each adapter's lifecycle independently
	adapterA := &lifecycleTrackingAdapter{
		fakeAdapter: fakeAdapter{name: "noop_a", outcome: "success"},
	}
	adapterB := &lifecycleTrackingAdapter{
		fakeAdapter: fakeAdapter{name: "noop_b", outcome: "success"},
	}

	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"noop_a": adapterA,
		"noop_b": adapterB,
	}}

	sink := &lifecycleTrackingSink{}
	eng := New(parentG, loader, sink)

	err := eng.Run(context.Background())
	if err != nil {
		t.Errorf("Run failed: %v", err)
	}

	// Verify adapter A (parent): should open once at parent scope start, close once at end
	adapterA.mu.Lock()
	aOpens := adapterA.opensCount
	aCloses := adapterA.closesCount
	adapterA.mu.Unlock()

	if aOpens != 1 {
		t.Errorf("adapter a (parent): expected 1 open, got %d", aOpens)
	}
	if aCloses != 1 {
		t.Errorf("adapter a (parent): expected 1 close, got %d", aCloses)
	}

	// Verify adapter B (body): should open once when body starts, close once when body ends
	adapterB.mu.Lock()
	bOpens := adapterB.opensCount
	bCloses := adapterB.closesCount
	adapterB.mu.Unlock()

	if bOpens != 1 {
		t.Errorf("adapter b (body): expected 1 open, got %d", bOpens)
	}
	if bCloses != 1 {
		t.Errorf("adapter b (body): expected 1 close, got %d", bCloses)
	}

	// Verify event order: a opens, b opens (in body), b closes (body ends), a closes (parent ends)
	events := sink.adapterLifecycleEvents
	if len(events) < 4 {
		t.Errorf("expected at least 4 lifecycle events, got %d: %v", len(events), events)
	} else {
		// Check that the essential events occurred (may have execution events in between)
		hasAOpened := containsEvent(events, "noop_a.default:opened")
		hasBOpened := containsEvent(events, "noop_b.default:opened")
		hasBClosed := containsEvent(events, "noop_b.default:closed")
		hasAClosed := containsEvent(events, "noop_a.default:closed")

		if !hasAOpened || !hasBOpened || !hasBClosed || !hasAClosed {
			t.Errorf("missing essential lifecycle events: %v (opened_a=%v, opened_b=%v, closed_b=%v, closed_a=%v)",
				events, hasAOpened, hasBOpened, hasBClosed, hasAClosed)
		}
	}
}

// TestRunWorkflowBody_BodyDoesNotInheritParentAdapter verifies the scope-isolation
// invariant: a body step that references an adapter declared only in the parent
// scope must produce a compile error. This guarantees parent adapters are NOT
// implicitly visible inside subworkflow bodies.
//
// Bypasses the compile() helper because injectDefaultAdapters auto-injects bare
// adapter references; we need the dotted reference to remain unresolved against
// the body's own (empty) Adapters map.
func TestRunWorkflowBody_BodyDoesNotInheritParentAdapter(t *testing.T) {
	t.Skip("test uses removed inline workflow body feature (W13); pending W14 subworkflow invocation support")
	src := `
workflow "parent" {
  version       = "0.1"
  initial_state = "outer"
  target_state  = "done"
}

adapter "noop" "parent_only" {
  config {}
}

step "outer" {
  type     = "workflow"
  for_each = ["x"]

  workflow {
    step "inner" {
      target = adapter.noop.parent_only
      outcome "success" { next = "_continue" }
    }
  }

  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}`
	spec, diags := workflow.Parse("body_inherit_test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %v", diags)
	}
	_, diags = workflow.Compile(spec, nil)
	if !diags.HasErrors() {
		t.Fatal("expected compile error: body step references parent-only adapter, got no errors")
	}
	wantSubstr := `referenced adapter "noop.parent_only" is not declared`
	found := false
	for _, d := range diags {
		if strings.Contains(d.Summary, wantSubstr) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected diagnostic containing %q, got diagnostics: %v", wantSubstr, diags)
	}
}

// Helper to check if an event string is in the events slice
func containsEvent(events []string, substr string) bool {
	for _, evt := range events {
		if evt == substr {
			return true
		}
	}
	return false
}
