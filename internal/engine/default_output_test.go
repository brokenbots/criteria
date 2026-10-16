package engine

import (
	"context"
	"strings"
	"sync/atomic"
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
		for _, d := range diags {
			t.Logf("compile diag (%v): %s", d.Severity, d.Summary)
		}
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

// whileDriverOutputField is the countdown driver output the
// whileSequenceAdapter emits alongside its adapter-default outputs; while-test
// compile-time schemas merge it into the adapter's default declaration.
var whileDriverOutputField = map[string]workflow.ConfigField{
	"remaining": {Type: workflow.ConfigFieldNumber, CtyType: cty.Number},
}

// copilotWhileOutputSchema is copilot's default output type plus the while
// driver output used by the while-executor tests.
var copilotWhileOutputSchema = map[string]workflow.ConfigField{
	"reason":    copilotDefaultOutputSchema["reason"],
	"remaining": whileDriverOutputField["remaining"],
}

// shellWhileOutputSchema is shell's default output type plus the while driver
// output used by the while-executor tests.
var shellWhileOutputSchema = map[string]workflow.ConfigField{
	"exit_code": shellDefaultOutputSchema["exit_code"],
	"stdout":    shellDefaultOutputSchema["stdout"],
	"stderr":    shellDefaultOutputSchema["stderr"],
	"remaining": whileDriverOutputField["remaining"],
}

// commentAdapter is a legacy-lane handle: it finalizes with a comment (the
// legacy reason param) and optionally its own outputs. Capabilities carry
// well-known names like "parallel_safe" for parallel-executor tests.
type commentAdapter struct {
	name         string
	outcome      string
	outputs      map[string]cty.Value
	comment      string
	capabilities []string
}

func (p *commentAdapter) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: p.name, Version: "test", Capabilities: p.capabilities}, nil
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

// TestDefaultOutputs_UntypedCopilotEmptyCommentYieldsEmptyReason
// (acceptance b, empty-comment half): an untyped copilot-step outcome carries
// the declared default field even when the comment is empty — output.reason
// reads "" (the declared type is satisfied in full) instead of failing the
// run with `output key "reason" not found`.
func TestDefaultOutputs_UntypedCopilotEmptyCommentYieldsEmptyReason(t *testing.T) {
	src := strings.ReplaceAll(writeBackWorkflow("string"), "OUTPUTKEY", "reason")
	g := compileWithSchemas(t, src, map[string]workflow.AdapterInfo{
		"target": {OutputSchema: copilotDefaultOutputSchema},
	})

	runHandle := &commentAdapter{
		name:    "target",
		outcome: "success",
		comment: "",
	}
	sink, err := runDefaultOutput(t, g, map[string]adapterhost.Handle{
		"target":   runHandle,
		"verifier": &fakeAdapter{name: "verifier", outcome: "success"},
	})
	require.NoError(t, err)
	assert.Equal(t, "", sink.captured["verify"]["result"])
	// The declared shape is materialized for the capture too: an empty
	// comment still renders {"reason": ""}, never {}.
	assert.Equal(t, map[string]string{"reason": ""}, sink.captured["run"])
}

// TestDefaultOutputs_UntypedFallbackArmMaterializesEmptyReason (acceptance b,
// synthesized-fallback half): the host's KB-45 fallback lane synthesizes a
// verdict with no payload and an empty comment; an untyped fallback arm on a
// reason-declaring adapter still exposes output.reason = "" and the
// write-through read succeeds.
func TestDefaultOutputs_UntypedFallbackArmMaterializesEmptyReason(t *testing.T) {
	src := `
workflow {
  name = "copilot_fallback_reason"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}

data "internal" "captured" {
  type = string
}

step "run" {
  target = adapter.target
  outcome "failed" {
    fallback = true
    next = step.report
    write {
      target = data.internal.captured.value
      value  = output.reason
    }
  }
  outcome "success" { next = step.done }
}

step "report" {
  target = adapter.verifier
  outcome "success" {
    next = step.done
    output = { result = data.internal.captured.value }
  }
}

state "done" { terminal = true }
`
	g := compileWithSchemas(t, src, map[string]workflow.AdapterInfo{
		"target": {OutputSchema: copilotDefaultOutputSchema},
	})
	// An empty Result never finalizes: the host synthesizes the fallback
	// verdict (no payload, empty comment).
	sink, err := runDefaultOutput(t, g, map[string]adapterhost.Handle{
		"target":   &commentAdapter{name: "target", outcome: "", comment: ""},
		"verifier": &fakeAdapter{name: "verifier", outcome: "success"},
	})
	require.NoError(t, err)
	assert.Equal(t, "", sink.captured["report"]["result"])
	assert.Equal(t, map[string]string{"reason": ""}, sink.captured["run"])
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

// whileSequenceAdapter emits a sequenced (remaining, reason) pair per call —
// the countdown shape that drives a while loop — with the legacy comment
// carried by reason. Sequence entries repeat their last element once the
// sequence is exhausted, so an overshooting iteration still finalizes.
type whileSequenceAdapter struct {
	name       string
	outcome    string
	remaining  []int64
	reasons    []string
	stdouts    []string
	emitStdout bool
	calls      int32
}

func (p *whileSequenceAdapter) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: p.name, Version: "test"}, nil
}
func (p *whileSequenceAdapter) OpenSession(context.Context, string, map[string]string, map[string]string) error {
	return nil
}
func (p *whileSequenceAdapter) Execute(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink, _ *v2.ExecutionRejection) (adapter.Result, error) {
	i := int(atomic.AddInt32(&p.calls, 1)) - 1
	if i >= len(p.remaining) {
		i = len(p.remaining) - 1
	}
	outputs := map[string]cty.Value{"remaining": cty.NumberIntVal(p.remaining[i])}
	if p.emitStdout {
		outputs["exit_code"] = cty.NumberIntVal(0)
		outputs["stdout"] = cty.StringVal(p.stdouts[i])
		outputs["stderr"] = cty.StringVal("")
	}
	var comment string
	if i < len(p.reasons) {
		comment = p.reasons[i]
	}
	return adapter.Result{Outcome: p.outcome, Outputs: outputs, Comment: comment}, nil
}
func (p *whileSequenceAdapter) Permit(context.Context, string, string, bool, string) error {
	return nil
}
func (p *whileSequenceAdapter) CloseSession(context.Context, string) error { return nil }
func (p *whileSequenceAdapter) Kill()                                      {}
func (p *whileSequenceAdapter) Pause(context.Context, string) error        { return nil }
func (p *whileSequenceAdapter) Resume(context.Context, string) error       { return nil }
func (p *whileSequenceAdapter) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}
func (p *whileSequenceAdapter) Snapshot(context.Context, string) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{}, nil
}
func (p *whileSequenceAdapter) Restore(context.Context, string, []byte, uint32) error { return nil }

// whileReasonWorkflowHCL shapes a self-contained while workflow over the given
// adapter type, untyped outcome, and per-iteration output read: the while step
// counts down data.internal.remaining (sequenced adapter outputs), writes one
// adapter-default output read into data.internal.captured each iteration (last
// iteration wins), and a verify step projects the captured value back so the
// sink captures it.
func whileReasonWorkflowHCL(adapterType, outputKey string) string {
	return `
workflow {
  name = "while_reason"
  version       = "0.1"
  initial_state = "loop"
  target_state  = "done"
}

data "internal" "remaining" {
  type = number
  value = 2
}

data "internal" "captured" {
  type = string
}

step "loop" {
  target = adapter.` + adapterType + `
  while  = data.internal.remaining.value > 0
  outcome "success" {
    next = continue
    write {
      target = data.internal.remaining.value
      value  = output.remaining
    }
    write {
      target = data.internal.captured.value
      value  = output.` + outputKey + `
    }
  }
  outcome "all_succeeded" { next = step.verify }
  outcome "any_failed"    { next = state.failed }
}

step "verify" {
  target = adapter.verifier
  outcome "success" {
    next = step.done
    output = { result = data.internal.captured.value }
  }
}

state "done"   { terminal = true }
state "failed" { terminal = true }
`
}

// TestDefaultOutputs_WhileUntypedCopilotOutcomeWritesReason (KB-264 review
// remediation): while iterations execute through runStepFromAttempt, so an
// untyped outcome reads the adapter-default output.reason on every iteration
// exactly as a plain step does — output.<key> is executor-shape-agnostic.
func TestDefaultOutputs_WhileUntypedCopilotOutcomeWritesReason(t *testing.T) {
	g := compileWithSchemas(t, whileReasonWorkflowHCL("copilot", "reason"), map[string]workflow.AdapterInfo{
		"copilot": {OutputSchema: copilotWhileOutputSchema},
	})
	runHandle := &whileSequenceAdapter{
		name:      "copilot",
		outcome:   "success",
		remaining: []int64{2, 1, 0},
		reasons:   []string{"first", "second", "third"},
	}
	sink, err := runDefaultOutput(t, g, map[string]adapterhost.Handle{
		"copilot":  runHandle,
		"verifier": &fakeAdapter{name: "verifier", outcome: "success"},
	})
	require.NoError(t, err)
	// Each iteration's untyped outcome wrote output.reason; the last
	// iteration's write lands in the data store.
	assert.Equal(t, "third", sink.captured["verify"]["result"])
	// The per-iteration capture sees the enriched outputs too: the legacy
	// comment is materialized as reason, not just carried in the event stream.
	assert.Equal(t, map[string]string{"remaining": "0", "reason": "third"}, sink.captured["loop"])
}

// TestDefaultOutputs_WhileUntypedShellOutcomeWritesStdout pins the shell
// default on the while executor shape: the natural exit_code/stdout/stderr set
// stays the untyped outcome's outputs and the failure-arm-style read
// output.stdout lands the command text (no reason materialization — the shell
// default type declares none).
func TestDefaultOutputs_WhileUntypedShellOutcomeWritesStdout(t *testing.T) {
	g := compileWithSchemas(t, whileReasonWorkflowHCL("shell", "stdout"), map[string]workflow.AdapterInfo{
		"shell": {OutputSchema: shellWhileOutputSchema},
	})
	runHandle := &whileSequenceAdapter{
		name:       "shell",
		outcome:    "success",
		remaining:  []int64{2, 1, 0},
		stdouts:    []string{"out-2", "out-1", "out-0"},
		emitStdout: true,
	}
	sink, err := runDefaultOutput(t, g, map[string]adapterhost.Handle{
		"shell":    runHandle,
		"verifier": &fakeAdapter{name: "verifier", outcome: "success"},
	})
	require.NoError(t, err)
	assert.Equal(t, "out-0", sink.captured["verify"]["result"])
	assert.Equal(t, map[string]string{
		"remaining": "0",
		"exit_code": "0",
		"stdout":    "out-0",
		"stderr":    "",
	}, sink.captured["loop"])
}

// TestDefaultOutputs_ParallelUntypedCopilotOutcomeWritesReason (KB-264 review
// remediation): parallel iterations execute through runParallelAdapterIteration
// → runStepFromAttempt, so an untyped outcome reads the adapter-default
// output.reason per iteration exactly as a plain step does.
func TestDefaultOutputs_ParallelUntypedCopilotOutcomeWritesReason(t *testing.T) {
	src := `
workflow {
  name = "parallel_reason"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}

data "internal" "captured" {
  type = string
}

step "run" {
  target   = adapter.copilot
  parallel = ["a", "b"]
  outcome "success" {
    next = continue
    write {
      target = data.internal.captured.value
      value  = output.reason
    }
  }
  outcome "all_succeeded" { next = step.verify }
  outcome "any_failed"    { next = state.failed }
}

step "verify" {
  target = adapter.verifier
  outcome "success" {
    next = step.done
    output = { result = data.internal.captured.value }
  }
}

state "done"   { terminal = true }
state "failed" { terminal = true }
`
	g := compileWithSchemas(t, src, map[string]workflow.AdapterInfo{
		"copilot": {
			OutputSchema: copilotDefaultOutputSchema,
			Capabilities: []string{"parallel_safe"},
		},
	})
	// Both iterations write the same reason, so last-writer-wins stays
	// deterministic; the parallel_safe capability clears the compile-time and
	// runtime parallel gates. The per-iteration write is the enrichment
	// observable on the parallel path: runParallelAdapterIteration consumes the
	// runStepFromAttempt result whose outputs the write block reads.
	runHandle := &commentAdapter{
		name:         "copilot",
		outcome:      "success",
		comment:      "parallel reason",
		capabilities: []string{"parallel_safe"},
	}
	sink, err := runDefaultOutput(t, g, map[string]adapterhost.Handle{
		"copilot":  runHandle,
		"verifier": &fakeAdapter{name: "verifier", outcome: "success"},
	})
	require.NoError(t, err)
	assert.Equal(t, "parallel reason", sink.captured["verify"]["result"])
}

// TestDefaultOutputs_UntypedReasonDeclaredNonStringNotMaterialized pins the
// type guard: a default output type that declares reason as a non-string type
// (here number) is not satisfied by the comment, so no cty.StringVal is forced
// into the untyped payload and the output.<key> read of the undeclared-output
// key fails exactly like any other missing output.
func TestDefaultOutputs_UntypedReasonDeclaredNonStringNotMaterialized(t *testing.T) {
	src := strings.ReplaceAll(writeBackWorkflow("string"), "OUTPUTKEY", "reason")
	g := compileWithSchemas(t, src, map[string]workflow.AdapterInfo{
		"target": {OutputSchema: map[string]workflow.ConfigField{
			"reason": {Type: workflow.ConfigFieldNumber, CtyType: cty.Number},
		}},
	})
	runHandle := &commentAdapter{
		name:    "target",
		outcome: "success",
		comment: "a number-declared reason never materializes",
	}
	_, err := runDefaultOutput(t, g, map[string]adapterhost.Handle{
		"target":   runHandle,
		"verifier": &fakeAdapter{name: "verifier", outcome: "success"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `output key "reason" not found`)
}
