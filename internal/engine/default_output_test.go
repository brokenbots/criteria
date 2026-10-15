package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// KB-264 acceptance tests: an untyped outcome takes the adapter's default
// output type. The tests model adapters by their declared handshake
// output_schema — the shell default (exit_code/stdout/stderr) and the
// copilot default (reason) — and exercise output.<key> reads on untyped
// outcome blocks.

// compileWithSchemas is compile() with adapter handshake schemas injected,
// the same surface the wire path has when adapters declare output_schema.
func compileWithSchemas(t *testing.T, src string, schemas map[string]workflow.AdapterInfo) *workflow.FSMGraph {
	t.Helper()
	src = injectDefaultAdapters(src)
	spec, diags := workflow.Parse("t.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	g, diags := workflow.Compile(spec, schemas)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	return g
}

// runDefaultOutput runs a compiled graph with the given handles.
func runDefaultOutput(t *testing.T, g *workflow.FSMGraph, handlers map[string]adapterhost.Handle) (*outputCaptureSink, error) {
	t.Helper()
	sink := &outputCaptureSink{}
	loader := &fakeLoader{adapters: handlers}
	err := New(g, loader, sink).Run(context.Background())
	return sink, err
}

// writeBackWorkflow is the shared shape of the write-through tests: an
// adapter step with an untyped outcome writes one output.<key> read into a
// data value, and a verify step projects the data value back so the sink
// captures it.
func writeBackWorkflow(dataType string) string {
	return `
workflow {
  name = "default_outputs"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}

data "internal" "captured" {
  type = ` + dataType + `
}

step "run" {
  target = adapter.target
  outcome "success" {
    next = step.verify
    write {
      target = data.internal.captured.value
      value  = output.OUTPUTKEY
    }
  }
}

step "verify" {
  target = adapter.verifier
  outcome "success" {
    next = step.done
    output = { result = data.internal.captured.value }
  }
}

state "done" { terminal = true }
`
}

// shellDefaultOutputSchema is the shell adapter's declared default output
// type: exit_code/stdout/stderr are the natural result shape of a command.
var shellDefaultOutputSchema = map[string]workflow.ConfigField{
	"exit_code": {Type: workflow.ConfigFieldNumber, CtyType: cty.Number},
	"stdout":    {Type: workflow.ConfigFieldString, CtyType: cty.String},
	"stderr":    {Type: workflow.ConfigFieldString, CtyType: cty.String},
}

// copilotDefaultOutputSchema is the copilot adapter's declared default output
// type (KB-264): the legacy reason param maps into object({reason: string}).
var copilotDefaultOutputSchema = map[string]workflow.ConfigField{
	"reason": {Type: workflow.ConfigFieldString, CtyType: cty.String},
}

// commentAdapter is a legacy-lane handle: it finalizes with a comment (the
// legacy reason param) and optionally its own outputs.
type commentAdapter struct {
	name    string
	outcome string
	outputs map[string]cty.Value
	comment string
}

func (p *commentAdapter) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: p.name, Version: "test"}, nil
}
func (p *commentAdapter) OpenSession(context.Context, string, map[string]string, map[string]string) error {
	return nil
}
func (p *commentAdapter) Execute(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink, rejection *v2.ExecutionRejection) (adapter.Result, error) {
	return adapter.Result{Outcome: p.outcome, Outputs: p.outputs, Comment: p.comment}, nil
}
func (p *commentAdapter) Permit(context.Context, string, string, bool, string) error {
	return nil
}
func (p *commentAdapter) CloseSession(context.Context, string) error { return nil }
func (p *commentAdapter) Kill()                                      {}
func (p *commentAdapter) Pause(context.Context, string) error        { return nil }
func (p *commentAdapter) Resume(context.Context, string) error       { return nil }
func (p *commentAdapter) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}
func (p *commentAdapter) Snapshot(context.Context, string) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{}, nil
}
func (p *commentAdapter) Restore(context.Context, string, []byte, uint32) error { return nil }

// TestDefaultOutputs_UntypedShellOutcomeWritesStdout (acceptance a): an
// untyped shell-step outcome exposes exit_code/stdout/stderr as outputs to
// the outcome projection; write {value = output.stdout} lands the command's
// text.
func TestDefaultOutputs_UntypedShellOutcomeWritesStdout(t *testing.T) {
	src := strings.ReplaceAll(writeBackWorkflow("string"), "OUTPUTKEY", "stdout")
	g := compileWithSchemas(t, src, map[string]workflow.AdapterInfo{
		"target": {OutputSchema: shellDefaultOutputSchema},
	})

	runHandle := &fakeTypedOutputAdapter{
		name: "target",
		outputs: map[string]cty.Value{
			"exit_code": cty.NumberIntVal(0),
			"stdout":    cty.StringVal("hello build"),
			"stderr":    cty.StringVal(""),
		},
	}
	sink, err := runDefaultOutput(t, g, map[string]adapterhost.Handle{
		"target":   runHandle,
		"verifier": &fakeAdapter{name: "verifier", outcome: "success"},
	})
	require.NoError(t, err)
	assert.Equal(t, "hello build", sink.captured["verify"]["result"])
}

// TestDefaultOutputs_UntypedCopilotOutcomeWritesReason (acceptance b): an
// untyped copilot-step outcome exposes output.reason = the legacy reason
// string, with the adapter emitting no outputs of its own.
func TestDefaultOutputs_UntypedCopilotOutcomeWritesReason(t *testing.T) {
	src := strings.ReplaceAll(writeBackWorkflow("string"), "OUTPUTKEY", "reason")
	g := compileWithSchemas(t, src, map[string]workflow.AdapterInfo{
		"target": {OutputSchema: copilotDefaultOutputSchema},
	})

	runHandle := &commentAdapter{
		name:    "target",
		outcome: "success",
		comment: "the plan is to refactor first",
	}
	sink, err := runDefaultOutput(t, g, map[string]adapterhost.Handle{
		"target":   runHandle,
		"verifier": &fakeAdapter{name: "verifier", outcome: "success"},
	})
	require.NoError(t, err)
	assert.Equal(t, "the plan is to refactor first", sink.captured["verify"]["result"])
}

// TestDefaultOutputs_UntypedCopilotOutcomeReasonReadableInProjection pins the
// additive capture shape (requirement 4): the reason lands in
// stepOutputCaptured for untyped copilot steps.
func TestDefaultOutputs_UntypedCopilotOutcomeReasonReadableInProjection(t *testing.T) {
	src := `
workflow {
  name = "copilot_reason"
  version       = "0.1"
  initial_state = "plan"
  target_state  = "done"
}

adapter "copilot" "default" {}

step "plan" {
  target = adapter.copilot.default
  outcome "success" { next = step.done }
}

state "done" { terminal = true }
`
	g := compileWithSchemas(t, src, map[string]workflow.AdapterInfo{
	"copilot": {OutputSchema: copilotDefaultOutputSchema},
	})
	runHandle := &commentAdapter{
		name:    "copilot",
		outcome: "success",
		comment: "plan body as a single line",
	}
	sink, err := runDefaultOutput(t, g, map[string]adapterhost.Handle{"copilot": runHandle})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"reason": "plan body as a single line"}, sink.captured["plan"])
}

// TestDefaultOutputs_UntypedShellOutcomeStaysIdentical pins requirement 4 for
// existing trees: a shell-shaped adapter keeps exactly its three natural
// outputs (the comment is never projected into a reason key).
func TestDefaultOutputs_UntypedShellOutcomeStaysIdentical(t *testing.T) {
	src := `
workflow {
  name = "shell_identical"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}

adapter "shell" "default" {}

step "run" {
  target = adapter.shell.default
  outcome "success" { next = step.done }
}

state "done" { terminal = true }
`
	g := compileWithSchemas(t, src, map[string]workflow.AdapterInfo{
		"shell": {OutputSchema: shellDefaultOutputSchema},
	})
	runHandle := &commentAdapter{
		name:    "shell",
		outcome: "success",
		outputs: map[string]cty.Value{
			"exit_code": cty.NumberIntVal(0),
			"stdout":    cty.StringVal("out"),
			"stderr":    cty.StringVal(""),
		},
		comment: "shell comments are not outputs",
	}
	sink, err := runDefaultOutput(t, g, map[string]adapterhost.Handle{"shell": runHandle})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"exit_code": "0",
		"stdout":    "out",
		"stderr":    "",
	}, sink.captured["run"])
}

// TestDefaultOutputs_TypedOutcomeKeepsVerbatimPayload pins requirement 4 for
// typed (contract) steps: a schema-declared outcome is the model-submitted
// payload; the comment is NOT projected into it even when the adapter's
// default output type declares reason.
func TestDefaultOutputs_TypedOutcomeKeepsVerbatimPayload(t *testing.T) {
	src := `
workflow {
  name = "typed_keeps_payload"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}

adapter "copilot" "default" {}

step "run" {
  target = adapter.copilot.default
  outcome "success" {
    next   = step.done
    schema = object({
      reason = optional(string, "(no reason)")
    })
  }
}

state "done" { terminal = true }
`
	g := compileWithSchemas(t, src, map[string]workflow.AdapterInfo{
	"copilot": {OutputSchema: copilotDefaultOutputSchema},
	})
	runHandle := &commentAdapter{
		name:    "copilot",
		outcome: "success",
		comment: "typed payload must not take the comment",
	}
	sink, err := runDefaultOutput(t, g, map[string]adapterhost.Handle{"copilot": runHandle})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"reason": "(no reason)"}, sink.captured["run"])
}