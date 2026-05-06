package engine

// outcome_shared_writes_test.go — integration tests for shared_writes in outcome blocks.
// Tests verify that shared_variable values are correctly written and read after
// step execution, and that type enforcement is applied at runtime.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/plugin"
	"github.com/brokenbots/criteria/workflow"
)

// sharedWritesPlugin returns a fixed outcome and outputs map for shared_writes tests.
type sharedWritesPlugin struct {
	outcome string
	outputs map[string]string
}

func (p *sharedWritesPlugin) Info(context.Context) (plugin.Info, error) {
	return plugin.Info{Name: "sw", Version: "test"}, nil
}
func (p *sharedWritesPlugin) OpenSession(context.Context, string, map[string]string) error {
	return nil
}
func (p *sharedWritesPlugin) Execute(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	return adapter.Result{Outcome: p.outcome, Outputs: p.outputs}, nil
}
func (p *sharedWritesPlugin) Permit(context.Context, string, string, bool, string) error {
	return nil
}
func (p *sharedWritesPlugin) CloseSession(context.Context, string) error { return nil }
func (p *sharedWritesPlugin) Kill()                                      {}

// pluginFunc is a plugin.Plugin backed by a function, for flexible test control.
type pluginFunc struct {
	name string
	fn   func(context.Context, string, *workflow.StepNode, adapter.EventSink) (adapter.Result, error)
}

func (p *pluginFunc) Info(context.Context) (plugin.Info, error) {
	return plugin.Info{Name: p.name, Version: "test"}, nil
}
func (p *pluginFunc) OpenSession(context.Context, string, map[string]string) error { return nil }
func (p *pluginFunc) Execute(ctx context.Context, sessionID string, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	return p.fn(ctx, sessionID, step, sink)
}
func (p *pluginFunc) Permit(context.Context, string, string, bool, string) error { return nil }
func (p *pluginFunc) CloseSession(context.Context, string) error                 { return nil }
func (p *pluginFunc) Kill()                                                      {}

// TestSharedWrites_AppliedAfterStep verifies that a workflow with shared_writes
// compiles, runs, and completes successfully.
func TestSharedWrites_AppliedAfterStep(t *testing.T) {
	const src = `
workflow "t" {
  version       = "0.1"
  initial_state = "inc"
  target_state  = "done"
}

shared_variable "counter" {
  type  = "number"
  value = 0
}

adapter "sw" "default" {}

step "inc" {
  target = adapter.sw.default
  outcome "success" {
    next         = "done"
    shared_writes = { counter = "count_val" }
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
	plug := &sharedWritesPlugin{outcome: "success", outputs: map[string]string{"count_val": "7"}}
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{"sw": plug}}

	eng := NewTestEngine(g, loader, sink)
	require.NoError(t, eng.Run(context.Background()))
	assert.Equal(t, "done", sink.terminal)
}

// TestSharedWrites_StoreReflectsWrittenValue runs a two-step workflow: step 1
// writes shared.msg via shared_writes, step 2 reads shared.msg via an output
// expression and emits it. We verify step 2's output carries the value written
// by step 1.
func TestSharedWrites_StoreReflectsWrittenValue(t *testing.T) {
	const src = `
workflow "t" {
  version       = "0.1"
  initial_state = "set_val"
  target_state  = "done"
}

shared_variable "msg" {
  type  = "string"
  value = "initial"
}

adapter "sw" "default" {}

step "set_val" {
  target = adapter.sw.default
  outcome "success" {
    next         = "read_val"
    shared_writes = { msg = "the_msg" }
  }
}

step "read_val" {
  target = adapter.sw.default
  outcome "success" {
    next   = "done"
    output = { result = shared.msg }
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

	plug := &pluginFunc{
		name: "sw",
		fn: func(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
			callNum++
			if callNum == 1 {
				// set_val: return the_msg output
				return adapter.Result{Outcome: "success", Outputs: map[string]string{"the_msg": "hello-from-shared"}}, nil
			}
			// read_val: return no outputs (output block reads shared.msg)
			return adapter.Result{Outcome: "success"}, nil
		},
	}
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{"sw": plug}}

	eng := NewTestEngine(g, loader, capturedSink)
	require.NoError(t, eng.Run(context.Background()))

	assert.Equal(t, "done", capturedSink.terminal)
	// read_val's output should contain shared.msg = "hello-from-shared".
	// String values in output projection are JSON-encoded (with quotes).
	readValOutputs := capturedSink.captured["read_val"]
	require.NotNil(t, readValOutputs, "read_val outputs not captured")
	assert.Equal(t, `"hello-from-shared"`, readValOutputs["result"])
}

// TestSharedWrites_OutputKeyMissing verifies that a missing output key in
// shared_writes causes the engine to fail with a clear error.
func TestSharedWrites_OutputKeyMissing(t *testing.T) {
	const src = `
workflow "t" {
  version       = "0.1"
  initial_state = "bad"
  target_state  = "done"
}

shared_variable "v" {
  type = "string"
}

adapter "sw" "default" {}

step "bad" {
  target = adapter.sw.default
  outcome "success" {
    next         = "done"
    shared_writes = { v = "nonexistent_key" }
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
	// Plugin returns "success" but WITHOUT "nonexistent_key" in outputs
	plug := &sharedWritesPlugin{outcome: "success", outputs: map[string]string{"other_key": "val"}}
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{"sw": plug}}

	eng := NewTestEngine(g, loader, sink)
	err := eng.Run(context.Background())
	require.Error(t, err, "expected error when output key is missing")
	assert.Contains(t, err.Error(), "nonexistent_key")
}

// TestSharedWrites_TypeMismatchAtRuntime verifies that writing an incompatible
// type value to a shared_variable fails the run with a clear type error.
// shared_writes maps "counter" (type=number) to output key "val". The plugin
// returns val="not-a-number" (a string). Since rawOutputs produce cty.String
// values, Store.Set will reject the type mismatch.
func TestSharedWrites_TypeMismatchAtRuntime(t *testing.T) {
	const src = `
workflow "t" {
  version       = "0.1"
  initial_state = "bad"
  target_state  = "done"
}

shared_variable "counter" {
  type = "number"
}

adapter "sw" "default" {}

step "bad" {
  target = adapter.sw.default
  outcome "success" {
    next         = "done"
    shared_writes = { counter = "val" }
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
	plug := &sharedWritesPlugin{outcome: "success", outputs: map[string]string{"val": "not-a-number"}}
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{"sw": plug}}

	eng := NewTestEngine(g, loader, sink)
	err := eng.Run(context.Background())
	require.Error(t, err, "expected type error on shared_writes with wrong type")
	assert.Contains(t, err.Error(), "type")
}

// TestSharedWrites_InitialValueVisibleInExpr verifies that the shared_variable
// initial value is readable in HCL expressions via shared.* at the first step.
func TestSharedWrites_InitialValueVisibleInExpr(t *testing.T) {
	const src = `
workflow "t" {
  version       = "0.1"
  initial_state = "read_initial"
  target_state  = "done"
}

shared_variable "greeting" {
  type  = "string"
  value = "hello"
}

adapter "sw" "default" {}

step "read_initial" {
  target = adapter.sw.default
  outcome "success" {
    next   = "done"
    output = { val = shared.greeting }
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
	plug := &sharedWritesPlugin{outcome: "success", outputs: map[string]string{}}
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{"sw": plug}}

	eng := NewTestEngine(g, loader, capturedSink)
	require.NoError(t, eng.Run(context.Background()))

	outputs := capturedSink.captured["read_initial"]
	require.NotNil(t, outputs)
	assert.Equal(t, `"hello"`, outputs["val"])
}
