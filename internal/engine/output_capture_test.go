package engine

import (
	"context"
	"testing"

	"github.com/brokenbots/overlord/overseer/internal/adapter"
	"github.com/brokenbots/overlord/overseer/internal/plugin"
	"github.com/brokenbots/overlord/workflow"
)

// outputCaptureSink extends fakeSink to capture OnStepOutputCaptured calls
// in order.
type outputCaptureSink struct {
	fakeSink
	captured map[string]map[string]string // step -> outputs
	order    []string                     // steps in the order OnStepOutputCaptured was called
}

func (s *outputCaptureSink) OnStepOutputCaptured(step string, outputs map[string]string) {
	if s.captured == nil {
		s.captured = make(map[string]map[string]string)
	}
	cp := make(map[string]string, len(outputs))
	for k, v := range outputs {
		cp[k] = v
	}
	s.captured[step] = cp
	s.order = append(s.order, step)
}

// fakeOutputPlugin is a plugin.Plugin that returns Outputs.
type fakeOutputPlugin struct {
	name    string
	outcome string
	outputs map[string]string
}

func (p *fakeOutputPlugin) Info(context.Context) (plugin.Info, error) {
	return plugin.Info{Name: p.name, Version: "test"}, nil
}
func (p *fakeOutputPlugin) OpenSession(context.Context, string, map[string]string) error { return nil }
func (p *fakeOutputPlugin) Execute(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	return adapter.Result{Outcome: p.outcome, Outputs: p.outputs}, nil
}
func (p *fakeOutputPlugin) Permit(context.Context, string, string, bool, string) error { return nil }
func (p *fakeOutputPlugin) CloseSession(context.Context, string) error                 { return nil }
func (p *fakeOutputPlugin) Kill()                                                      {}

const outputWorkflow = `
workflow "outputs" {
  version       = "0.1"
  initial_state = "produce"
  target_state  = "__done__"

  step "produce" {
    adapter = "fake_out"
    outcome "success" { transition_to = "__done__" }
  }
  state "__done__" { terminal = true }
}
`

func TestOutputCapture_StepOutputsCapturedInVars(t *testing.T) {
	spec, diags := workflow.Parse("test.hcl", []byte(outputWorkflow))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags)
	}
	g, diags := workflow.Compile(spec, map[string]workflow.AdapterInfo{
		"fake_out": {
			InputSchema:  map[string]workflow.ConfigField{},
			OutputSchema: map[string]workflow.ConfigField{"result": {Type: workflow.ConfigFieldString}},
		},
	})
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags)
	}

	plug := &fakeOutputPlugin{
		name:    "fake_out",
		outcome: "success",
		outputs: map[string]string{"result": "hello_output"},
	}

	sink := &outputCaptureSink{}
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{
		"fake_out": plug,
	}}

	eng := New(g, loader, sink)
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// OnStepOutputCaptured must have been called for "produce".
	captured, ok := sink.captured["produce"]
	if !ok {
		t.Fatal("OnStepOutputCaptured not called for 'produce'")
	}
	if captured["result"] != "hello_output" {
		t.Errorf("captured result = %q, want 'hello_output'", captured["result"])
	}
}

const interpolOutputWorkflow = `
workflow "interp_outputs" {
  version       = "0.1"
  initial_state = "build"
  target_state  = "__done__"

  step "build" {
    adapter = "fake_out"
    outcome "success" { transition_to = "deploy" }
  }
  step "deploy" {
    adapter = "fake_consumer"
    input {
      artifact = "${steps.build.result}"
    }
    outcome "success" { transition_to = "__done__" }
  }
  state "__done__" { terminal = true }
}
`

// fakeConsumerPlugin records what Input it received.
type fakeConsumerPlugin struct {
	name          string
	receivedInput map[string]string
}

func (p *fakeConsumerPlugin) Info(context.Context) (plugin.Info, error) {
	return plugin.Info{Name: p.name, Version: "test"}, nil
}
func (p *fakeConsumerPlugin) OpenSession(context.Context, string, map[string]string) error {
	return nil
}
func (p *fakeConsumerPlugin) Execute(_ context.Context, _ string, step *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	p.receivedInput = step.Input
	return adapter.Result{Outcome: "success"}, nil
}
func (p *fakeConsumerPlugin) Permit(context.Context, string, string, bool, string) error { return nil }
func (p *fakeConsumerPlugin) CloseSession(context.Context, string) error                 { return nil }
func (p *fakeConsumerPlugin) Kill()                                                      {}

func TestOutputCapture_ExpressionInterpolation(t *testing.T) {
	adapterSchemas := map[string]workflow.AdapterInfo{
		"fake_out": {
			InputSchema:  map[string]workflow.ConfigField{},
			OutputSchema: map[string]workflow.ConfigField{"result": {Type: workflow.ConfigFieldString}},
		},
		"fake_consumer": {
			InputSchema: map[string]workflow.ConfigField{"artifact": {Type: workflow.ConfigFieldString}},
		},
	}
	spec, diags := workflow.Parse("test.hcl", []byte(interpolOutputWorkflow))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags)
	}
	g, diags := workflow.Compile(spec, adapterSchemas)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags)
	}

	producer := &fakeOutputPlugin{
		name:    "fake_out",
		outcome: "success",
		outputs: map[string]string{"result": "myapp-1.0.tar.gz"},
	}
	consumer := &fakeConsumerPlugin{name: "fake_consumer"}

	sink := &outputCaptureSink{}
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{
		"fake_out":      producer,
		"fake_consumer": consumer,
	}}

	eng := New(g, loader, sink)
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The consumer's step.Input["artifact"] should have been resolved from the
	// build step's output.
	if consumer.receivedInput["artifact"] != "myapp-1.0.tar.gz" {
		t.Errorf("deploy.artifact = %q, want 'myapp-1.0.tar.gz'", consumer.receivedInput["artifact"])
	}
}

const sequenceWorkflow = `
workflow "sequence" {
  version       = "0.1"
  initial_state = "first"
  target_state  = "__done__"

  step "first" {
    adapter = "fake_out"
    outcome "success" { transition_to = "second" }
  }
  step "second" {
    adapter = "fake_out"
    outcome "success" { transition_to = "__done__" }
  }
  state "__done__" { terminal = true }
}
`

// TestOutputCapture_EmissionOrder asserts that OnStepOutputCaptured is called
// for steps in execution order (first before second).
func TestOutputCapture_EmissionOrder(t *testing.T) {
	spec, diags := workflow.Parse("test.hcl", []byte(sequenceWorkflow))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags)
	}
	g, diags := workflow.Compile(spec, map[string]workflow.AdapterInfo{
		"fake_out": {
			OutputSchema: map[string]workflow.ConfigField{"k": {Type: workflow.ConfigFieldString}},
		},
	})
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags)
	}

	plug := &fakeOutputPlugin{name: "fake_out", outcome: "success", outputs: map[string]string{"k": "v"}}
	sink := &outputCaptureSink{}
	loader := &fakeLoader{plugins: map[string]plugin.Plugin{"fake_out": plug}}

	eng := New(g, loader, sink)
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(sink.order) != 2 {
		t.Fatalf("expected 2 OnStepOutputCaptured calls, got %d", len(sink.order))
	}
	if sink.order[0] != "first" {
		t.Errorf("first emission = %q, want 'first'", sink.order[0])
	}
	if sink.order[1] != "second" {
		t.Errorf("second emission = %q, want 'second'", sink.order[1])
	}
}
