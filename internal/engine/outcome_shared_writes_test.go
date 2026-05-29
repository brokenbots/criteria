package engine

// outcome_write blocks_test.go — integration tests for write blocks in outcome blocks.
// Tests verify that data block values are correctly written and read after
// step execution, and that type enforcement is applied at runtime.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// sharedWritesAdapter returns a fixed outcome and outputs map for write blocks tests.
type sharedWritesAdapter struct {
	outcome string
	outputs map[string]string
}

func (p *sharedWritesAdapter) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: "sw", Version: "test"}, nil
}
func (p *sharedWritesAdapter) OpenSession(context.Context, string, map[string]string) error {
	return nil
}
func (p *sharedWritesAdapter) Execute(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	return adapter.Result{Outcome: p.outcome, Outputs: p.outputs}, nil
}
func (p *sharedWritesAdapter) Permit(context.Context, string, string, bool, string) error {
	return nil
}
func (p *sharedWritesAdapter) CloseSession(context.Context, string) error { return nil }
func (p *sharedWritesAdapter) Kill()                                      {}

// adapterFunc is a adapterhost.Handle backed by a function, for flexible test control.
type adapterFunc struct {
	name string
	fn   func(context.Context, string, *workflow.StepNode, adapter.EventSink) (adapter.Result, error)
}

func (p *adapterFunc) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: p.name, Version: "test"}, nil
}
func (p *adapterFunc) OpenSession(context.Context, string, map[string]string) error { return nil }
func (p *adapterFunc) Execute(ctx context.Context, sessionID string, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	return p.fn(ctx, sessionID, step, sink)
}
func (p *adapterFunc) Permit(context.Context, string, string, bool, string) error { return nil }
func (p *adapterFunc) CloseSession(context.Context, string) error                 { return nil }
func (p *adapterFunc) Kill()                                                      {}

// TestSharedWrites_AppliedAfterStep verifies that a workflow with write blocks
// compiles, runs, and completes successfully.
func TestSharedWrites_AppliedAfterStep(t *testing.T) {
	const src = `
workflow {
  name = "t"
  version       = "0.1"
  initial_state = "inc"
  target_state  = "done"
}

data "internal" "counter" {

  type = number
  value = 0
}

adapter "sw" "default" {}

step "inc" {
  target = adapter.sw.default
  outcome "success" {
    next = step.done
      write {
    target = data.internal.counter.value
    value  = output.count_val
  }
  }
}

state "done" {
  terminal = true
  success  = true
}
`
	spec, diags := workflow.Parse("t.hcl", []byte(src))
	require.False(t, diags.HasErrors(), "parse: %s", diags.Error())
	g, diags := workflow.Compile(spec, nil)
	require.False(t, diags.HasErrors(), "compile: %s", diags.Error())

	sink := &fakeSink{}
	plug := &sharedWritesAdapter{outcome: "success", outputs: map[string]string{"count_val": "7"}}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"sw": plug}}

	eng := NewTestEngine(g, loader, sink)
	require.NoError(t, eng.Run(context.Background()))
	assert.Equal(t, "done", sink.terminal)
}

// TestSharedWrites_StoreReflectsWrittenValue runs a two-step workflow: step 1
// writes data.internal.msg.value via write blocks, step 2 reads data.internal.msg.value via an output
// expression and emits it. We verify step 2's output carries the value written
// by step 1.
func TestSharedWrites_StoreReflectsWrittenValue(t *testing.T) {
	const src = `
workflow {
  name = "t"
  version       = "0.1"
  initial_state = "set_val"
  target_state  = "done"
}

data "internal" "msg" {

  type = string
  value = "initial"
}

adapter "sw" "default" {}

step "set_val" {
  target = adapter.sw.default
  outcome "success" {
    next = step.read_val
      write {
    target = data.internal.msg.value
    value  = output.the_msg
  }
  }
}

step "read_val" {
  target = adapter.sw.default
  outcome "success" {
    next = step.done
    output = { result = data.internal.msg.value }
  }
}

state "done" {
  terminal = true
  success  = true
}
`
	spec, diags := workflow.Parse("t.hcl", []byte(src))
	require.False(t, diags.HasErrors(), "parse: %s", diags.Error())
	g, diags := workflow.Compile(spec, nil)
	require.False(t, diags.HasErrors(), "compile: %s", diags.Error())

	callNum := 0
	capturedSink := &outputCaptureSink{}

	plug := &adapterFunc{
		name: "sw",
		fn: func(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
			callNum++
			if callNum == 1 {
				// set_val: return the_msg output
				return adapter.Result{Outcome: "success", Outputs: map[string]string{"the_msg": "hello-from-shared"}}, nil
			}
			// read_val: return no outputs (output block reads data.internal.msg.value)
			return adapter.Result{Outcome: "success"}, nil
		},
	}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"sw": plug}}

	eng := NewTestEngine(g, loader, capturedSink)
	require.NoError(t, eng.Run(context.Background()))

	assert.Equal(t, "done", capturedSink.terminal)
	// read_val's output should contain data.internal.msg.value = "hello-from-shared".
	// String values in output projection are JSON-encoded (with quotes).
	readValOutputs := capturedSink.captured["read_val"]
	require.NotNil(t, readValOutputs, "read_val outputs not captured")
	assert.Equal(t, `"hello-from-shared"`, readValOutputs["result"])
}

// TestSharedWrites_OutputKeyMissing verifies that a missing output key in
// write blocks causes the engine to fail with a clear error.
func TestSharedWrites_OutputKeyMissing(t *testing.T) {
	const src = `
workflow {
  name = "t"
  version       = "0.1"
  initial_state = "bad"
  target_state  = "done"
}

data "internal" "v" {

  type = string
}

adapter "sw" "default" {}

step "bad" {
  target = adapter.sw.default
  outcome "success" {
    next = step.done
      write {
    target = data.internal.v.value
    value  = output.nonexistent_key
  }
  }
}

state "done" {
  terminal = true
  success  = true
}
`
	spec, diags := workflow.Parse("t.hcl", []byte(src))
	require.False(t, diags.HasErrors(), "parse: %s", diags.Error())
	g, diags := workflow.Compile(spec, nil)
	require.False(t, diags.HasErrors(), "compile: %s", diags.Error())

	sink := &fakeSink{}
	// Adapter returns "success" but WITHOUT "nonexistent_key" in outputs
	plug := &sharedWritesAdapter{outcome: "success", outputs: map[string]string{"other_key": "val"}}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"sw": plug}}

	eng := NewTestEngine(g, loader, sink)
	err := eng.Run(context.Background())
	require.Error(t, err, "expected error when output key is missing")
	assert.Contains(t, err.Error(), "nonexistent_key")
}

// TestSharedWrites_TypeMismatchAtRuntime verifies that writing an incompatible
// type value to a data block fails the run with a clear type error.
// write blocks maps "counter" (type=number) to output key "val". The adapter
// returns val="not-a-number" (a string). Since rawOutputs produce cty.String
// values, Store.Set will reject the type mismatch.
func TestSharedWrites_TypeMismatchAtRuntime(t *testing.T) {
	const src = `
workflow {
  name = "t"
  version       = "0.1"
  initial_state = "bad"
  target_state  = "done"
}

data "internal" "counter" {

  type = number
}

adapter "sw" "default" {}

step "bad" {
  target = adapter.sw.default
  outcome "success" {
    next = step.done
      write {
    target = data.internal.counter.value
    value  = output.val
  }
  }
}

state "done" {
  terminal = true
  success  = true
}
`
	spec, diags := workflow.Parse("t.hcl", []byte(src))
	require.False(t, diags.HasErrors(), "parse: %s", diags.Error())
	g, diags := workflow.Compile(spec, nil)
	require.False(t, diags.HasErrors(), "compile: %s", diags.Error())

	sink := &fakeSink{}
	plug := &sharedWritesAdapter{outcome: "success", outputs: map[string]string{"val": "not-a-number"}}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"sw": plug}}

	eng := NewTestEngine(g, loader, sink)
	err := eng.Run(context.Background())
	require.Error(t, err, "expected type error on write blocks with wrong type")
	assert.Contains(t, err.Error(), "type")
}

// TestSharedWrites_NonScalarViaTypedProjection tests the end-to-end non-scalar
// shared_write path: a step uses an output = {} projection to assemble a
// list(string) from the adapter's raw outputs (accessed via step.output.*),
// and write blocks maps the projected key to the data block. A second
// step reads back the shared variable and emits it as a JSON-encoded output,
// proving the write was committed with the correct type.
//
// This exercises:
//  1. step.output.<key> namespace availability in evalOutcomeOutputProjection
//  2. Tuple→list type coercion in SetBatch (HCL [a, b] produces a tuple)
//  3. The full typed projection path (projectedCty) used by resolveDataWriteValue
func TestSharedWrites_NonScalarViaTypedProjection(t *testing.T) {
	const src = `
workflow {
  name = "t"
  version       = "0.1"
  initial_state = "collect"
  target_state  = "done"
}

data "internal" "items" {

  type = list(string)
}

adapter "sw" "default" {}

step "collect" {
  target = adapter.sw.default
  outcome "success" {
    next = step.read_back
    output        = { tag_list = [step.output.tag1, step.output.tag2] }
      write {
    target = data.internal.items.value
    value  = output.tag_list
  }
  }
}

step "read_back" {
  target = adapter.sw.default
  outcome "success" {
    next = step.done
    output = { first = data.internal.items.value[0], second = data.internal.items.value[1] }
  }
}

state "done" {
  terminal = true
  success  = true
}
`
	spec, diags := workflow.Parse("t.hcl", []byte(src))
	require.False(t, diags.HasErrors(), "parse: %s", diags.Error())
	g, diags := workflow.Compile(spec, nil)
	require.False(t, diags.HasErrors(), "compile: %s", diags.Error())

	callNum := 0
	capturedSink := &outputCaptureSink{}

	plug := &adapterFunc{
		name: "sw",
		fn: func(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
			callNum++
			if callNum == 1 {
				// collect: return tag1 and tag2 raw outputs
				return adapter.Result{Outcome: "success", Outputs: map[string]string{"tag1": "foo", "tag2": "bar"}}, nil
			}
			// read_back: no adapter outputs needed; projection reads data.internal.items.value
			return adapter.Result{Outcome: "success"}, nil
		},
	}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"sw": plug}}

	eng := NewTestEngine(g, loader, capturedSink)
	require.NoError(t, eng.Run(context.Background()))
	assert.Equal(t, "done", capturedSink.terminal)

	// read_back's projected outputs should carry the list elements.
	readBackOutputs := capturedSink.captured["read_back"]
	require.NotNil(t, readBackOutputs, "read_back outputs not captured")
	assert.Equal(t, `"foo"`, readBackOutputs["first"])
	assert.Equal(t, `"bar"`, readBackOutputs["second"])
}

// initial value is readable in HCL expressions via data.internal.*.value at the first step.
func TestSharedWrites_InitialValueVisibleInExpr(t *testing.T) {
	const src = `
workflow {
  name = "t"
  version       = "0.1"
  initial_state = "read_initial"
  target_state  = "done"
}

data "internal" "greeting" {

  type = string
  value = "hello"
}

adapter "sw" "default" {}

step "read_initial" {
  target = adapter.sw.default
  outcome "success" {
    next = step.done
    output = { val = data.internal.greeting.value }
  }
}

state "done" {
  terminal = true
  success  = true
}
`
	spec, diags := workflow.Parse("t.hcl", []byte(src))
	require.False(t, diags.HasErrors(), "parse: %s", diags.Error())
	g, diags := workflow.Compile(spec, nil)
	require.False(t, diags.HasErrors(), "compile: %s", diags.Error())

	capturedSink := &outputCaptureSink{}
	plug := &sharedWritesAdapter{outcome: "success", outputs: map[string]string{}}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"sw": plug}}

	eng := NewTestEngine(g, loader, capturedSink)
	require.NoError(t, eng.Run(context.Background()))

	outputs := capturedSink.captured["read_initial"]
	require.NotNil(t, outputs)
	assert.Equal(t, `"hello"`, outputs["val"])
}

// TestSharedWrites_PerIterationOutcome proves that a for_each step's per-iteration
// outcome applies write blocks on each adapter call. Each iteration overwrites
// the shared variable with the current iteration's output value. A subsequent
// step reads the final value to confirm the last write was committed.
func TestSharedWrites_PerIterationOutcome(t *testing.T) {
	const src = `
workflow {
  name = "t"
  version       = "0.1"
  initial_state = "loop"
  target_state  = "done"
}

data "internal" "last_tag" {

  type = string
  value = ""
}

adapter "sw" "default" {}

step "loop" {
  target   = adapter.sw.default
  for_each = ["alpha", "beta", "gamma"]
  outcome "success" {
    next = continue
      write {
    target = data.internal.last_tag.value
    value  = output.tag
  }
  }
  outcome "all_succeeded" { next = step.read_back }
  outcome "any_failed"    { next = step.done }
}

step "read_back" {
  target = adapter.sw.default
  outcome "success" {
    next = step.done
    output = { result = data.internal.last_tag.value }
  }
}

state "done" {
  terminal = true
  success  = true
}
`
	spec, diags := workflow.Parse("t.hcl", []byte(src))
	require.False(t, diags.HasErrors(), "parse: %s", diags.Error())
	g, diags := workflow.Compile(spec, nil)
	require.False(t, diags.HasErrors(), "compile: %s", diags.Error())

	items := []string{"alpha", "beta", "gamma"}
	callNum := 0
	capturedSink := &outputCaptureSink{}

	plug := &adapterFunc{
		name: "sw",
		fn: func(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
			if callNum < len(items) {
				tag := items[callNum]
				callNum++
				return adapter.Result{Outcome: "success", Outputs: map[string]string{"tag": tag}}, nil
			}
			callNum++
			return adapter.Result{Outcome: "success"}, nil
		},
	}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"sw": plug}}

	eng := NewTestEngine(g, loader, capturedSink)
	require.NoError(t, eng.Run(context.Background()))
	assert.Equal(t, "done", capturedSink.terminal)

	// data.internal.last_tag.value should hold the final iteration's value ("gamma").
	readBackOutputs := capturedSink.captured["read_back"]
	require.NotNil(t, readBackOutputs, "read_back outputs not captured")
	assert.Equal(t, `"gamma"`, readBackOutputs["result"])
}

// TestSharedWrites_AggregateOutcome proves that write blocks declared on an
// aggregate outcome (all_succeeded / any_failed) in a for_each step is applied
// when finishIterationInGraph fires, not during any individual iteration.
func TestSharedWrites_AggregateOutcome(t *testing.T) {
	const src = `
workflow {
  name = "t"
  version       = "0.1"
  initial_state = "loop"
  target_state  = "done"
}

data "internal" "done_flag" {

  type = string
  value = "pending"
}

adapter "sw" "default" {}

step "loop" {
  target   = adapter.sw.default
  for_each = ["x", "y"]
  outcome "success" { next = continue }
  outcome "all_succeeded" {
    next = step.read_back
    output        = { status = "completed" }
      write {
    target = data.internal.done_flag.value
    value  = output.status
  }
  }
  outcome "any_failed" { next = step.done }
}

step "read_back" {
  target = adapter.sw.default
  outcome "success" {
    next = step.done
    output = { result = data.internal.done_flag.value }
  }
}

state "done" {
  terminal = true
  success  = true
}
`
	spec, diags := workflow.Parse("t.hcl", []byte(src))
	require.False(t, diags.HasErrors(), "parse: %s", diags.Error())
	g, diags := workflow.Compile(spec, nil)
	require.False(t, diags.HasErrors(), "compile: %s", diags.Error())

	capturedSink := &outputCaptureSink{}
	plug := &sharedWritesAdapter{outcome: "success", outputs: map[string]string{}}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"sw": plug}}

	eng := NewTestEngine(g, loader, capturedSink)
	require.NoError(t, eng.Run(context.Background()))
	assert.Equal(t, "done", capturedSink.terminal)

	// data.internal.done_flag.value should be "completed" — written by the aggregate outcome.
	readBackOutputs := capturedSink.captured["read_back"]
	require.NotNil(t, readBackOutputs, "read_back outputs not captured")
	assert.Equal(t, `"completed"`, readBackOutputs["result"])
}
