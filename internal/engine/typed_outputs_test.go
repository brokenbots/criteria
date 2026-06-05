package engine

import (
	"context"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/workflow"
)

// fakeTypedOutputAdapter returns a structured (nested object) step output so we
// can assert downstream nested attribute access works without jsondecode().
type fakeTypedOutputAdapter struct {
	name    string
	outputs map[string]cty.Value
}

func (p *fakeTypedOutputAdapter) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: p.name, Version: "test"}, nil
}
func (p *fakeTypedOutputAdapter) OpenSession(context.Context, string, map[string]string, map[string]string) error {
	return nil
}
func (p *fakeTypedOutputAdapter) Execute(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	return adapter.Result{Outcome: "success", Outputs: p.outputs}, nil
}
func (p *fakeTypedOutputAdapter) Permit(context.Context, string, string, bool, string) error {
	return nil
}
func (p *fakeTypedOutputAdapter) CloseSession(context.Context, string) error { return nil }
func (p *fakeTypedOutputAdapter) Kill()                                      {}
func (p *fakeTypedOutputAdapter) Pause(context.Context, string) error        { return nil }
func (p *fakeTypedOutputAdapter) Resume(context.Context, string) error       { return nil }
func (p *fakeTypedOutputAdapter) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}
func (p *fakeTypedOutputAdapter) Snapshot(context.Context, string) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{}, nil
}
func (p *fakeTypedOutputAdapter) Restore(context.Context, string, []byte, uint32) error { return nil }

const typedOutputWorkflow = `
workflow {
  name = "typed_outputs"
  version       = "0.1"
  initial_state = "build"
  target_state  = "__done__"
}

adapter "fake_out" "default" {}
adapter "fake_consumer" "default" {}

step "build" {
  target = adapter.fake_out.default
  outcome "success" { next = step.consume }
}
step "consume" {
  target = adapter.fake_consumer.default
  input {
    name  = "${steps.build.meta.name}"
    id    = "${steps.build.meta.id}"
    count = "${steps.build.count}"
  }
  outcome "success" { next = step.__done__ }
}
state "__done__" { terminal = true }
`

// TestTypedOutputs_NestedAccessWithoutJSONDecode is the headline WS-B test: an
// adapter returns a structured object output, and a downstream step reaches into
// it with native attribute access (steps.build.meta.name / .id) — no jsondecode()
// anywhere. If outputs were still flattened to strings, steps.build.meta.name
// would fail at evaluation because meta would be a string, not an object.
func TestTypedOutputs_NestedAccessWithoutJSONDecode(t *testing.T) {
	spec, diags := workflow.Parse("test.hcl", []byte(typedOutputWorkflow))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags)
	}
	g, diags := workflow.Compile(spec, map[string]workflow.AdapterInfo{
		"fake_out.default": {
			InputSchema: map[string]workflow.ConfigField{},
			OutputSchema: map[string]workflow.ConfigField{
				"meta":  {Type: workflow.ConfigFieldString, CtyType: cty.DynamicPseudoType},
				"count": {Type: workflow.ConfigFieldNumber, CtyType: cty.Number},
			},
		},
		"fake_consumer.default": {
			InputSchema: map[string]workflow.ConfigField{
				"name":  {Type: workflow.ConfigFieldString},
				"id":    {Type: workflow.ConfigFieldString},
				"count": {Type: workflow.ConfigFieldString},
			},
		},
	})
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags)
	}

	producer := &fakeTypedOutputAdapter{
		name: "fake_out",
		outputs: map[string]cty.Value{
			"meta": cty.ObjectVal(map[string]cty.Value{
				"name": cty.StringVal("widget"),
				"id":   cty.NumberIntVal(7),
			}),
			"count": cty.NumberIntVal(42),
		},
	}
	consumer := &fakeConsumerAdapter{name: "fake_consumer"}

	sink := &outputCaptureSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake_out":      producer,
		"fake_consumer": consumer,
	}}

	if err := New(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The consumer's resolved input proves nested typed access worked end to end.
	if got := consumer.receivedInput["name"]; got != "widget" {
		t.Errorf("steps.build.meta.name = %q, want 'widget'", got)
	}
	if got := consumer.receivedInput["id"]; got != "7" {
		t.Errorf("steps.build.meta.id = %q, want '7'", got)
	}
	if got := consumer.receivedInput["count"]; got != "42" {
		t.Errorf("steps.build.count = %q, want '42'", got)
	}
}
