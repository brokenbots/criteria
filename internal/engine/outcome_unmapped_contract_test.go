package engine

// The outcome-completeness verifier is absolute: every adapter step must
// handle adapter failure/error lanes (a failure-class outcome arm, a default
// mapping, or a declared fallback). A step designed to leave an unknown
// adapter verdict wholly unmapped — the retired never-finalize probe shape,
// whose observation WAS the loud unmapped-name failure — is a compile-verified
// impossibility. These tests pin the surviving semantics: on a contract-mode
// step an unknown verdict routes through the StepOutcomeInvalid repair loop
// (loud but routed: exhaustion fails the run with the wrapped contract
// issues, never the unmapped-name branch), a session that never finalizes
// synthesizes the step's DECLARED fallback by name so the probe observation
// survives as a typed routed outcome, and the verifier rejects the unhandled
// shape even for contract-mode (schema-bearing) steps.

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	criteriav2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/workflowtest"
)

// probeSink extends contractSink with the unmapped-name callback: the
// unmapped-outcome branch must stay unreachable for contract-mode steps, so
// these tests assert it never fires while the routed repair-loop events do.
type probeSink struct {
	contractSink
	unknown []string
}

func (s *probeSink) OnStepOutcomeUnknown(step, outcome string) {
	s.mu.Lock()
	s.unknown = append(s.unknown, step+":"+outcome)
	s.mu.Unlock()
}

// probeRun compiles the builder source and runs it against a scripted adapter
// that answers every attempt with verdict.
func probeRun(t *testing.T, b *workflowtest.WorkflowBuilder, verdict func() (adapter.Result, error)) (*probeSink, error) {
	t.Helper()
	g := compile(t, b.Build())
	sink := &probeSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"noop": &adapterFunc{fn: func(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink, _ *criteriav2.ExecutionRejection) (adapter.Result, error) {
			return verdict()
		}},
	}}
	return sink, NewTestEngine(g, loader, sink).Run(context.Background())
}

// loudProbeWorkflow builds the compile-compliant probe shape: a success arm
// plus a declared failure fallback, NO default — an unknown verdict must then
// exhaust the repair loop and fail the run through the exhaustion path.
func loudProbeWorkflow() *workflowtest.WorkflowBuilder {
	return workflowtest.NewStandard().
		WithInitial("ship").
		WithPolicy(workflowtest.PolicyDef{MaxStepRetries: 1}).
		WithStep("ship",
			workflowtest.Outcome("success", "state.done"),
			workflowtest.FallbackOutcome("failure", "state.failed"),
		)
}

// neverFinalizeProbeWorkflow builds the recomposed probe: the never-finalize
// stimulus is preserved through a DECLARED fallback arm whose outcome name is
// the probe's own marker, so the observation stays typed and routed instead
// of collapsing into an unmapped-name failure.
func neverFinalizeProbeWorkflow() *workflowtest.WorkflowBuilder {
	falseVal := false
	return workflowtest.NewStandard().
		WithInitial("ship").
		WithPolicy(workflowtest.PolicyDef{MaxStepRetries: 2}).
		WithStep("ship",
			workflowtest.Outcome("success", "state.done"),
			workflowtest.FallbackOutcome("finalize_exhausted", "state.probe_failed"),
		).
		WithState("probe_failed", true, &falseVal)
}

// TestRun_UnknownOutcomeOnContractStepNeverReachesUnmappedBranch pins the
// loud-but-routed observation for a contract-mode step without a default: a
// verdict outside allowed_outcomes feeds the repair loop (one
// StepOutcomeInvalid per attempt) and exhaustion fails the run with the
// wrapped contract issues. The unmapped-name branch of applyOutcome never
// fires — host validation restricts verdict names to the declared set before
// mapping, so that branch is unreachable for contract-mode steps.
func TestRun_UnknownOutcomeOnContractStepNeverReachesUnmappedBranch(t *testing.T) {
	sink, err := probeRun(t, loudProbeWorkflow(), func() (adapter.Result, error) {
		return adapter.Result{Outcome: "ghost-name"}, nil
	})
	require.Error(t, err, "exhaustion without a default must fail the run")
	require.Contains(t, err.Error(), `step "ship" failed after 2 attempts`)
	require.Contains(t, err.Error(), `outcome_not_allowed: outcome "ghost-name" is not in allowed_outcomes`)
	require.NotContains(t, err.Error(), "produced unmapped outcome")

	require.Empty(t, sink.unknown, "the unmapped-name branch must never fire for contract-mode steps")
	require.Len(t, sink.invalid, 2, "one StepOutcomeInvalid per attempt (budget 1 + initial)")
	for i, inv := range sink.invalid {
		require.Equal(t, "ship", inv.step)
		require.Equal(t, "ghost-name", inv.outcome)
		require.Equal(t, i+1, inv.attempt)
		require.Contains(t, inv.issues, `outcome_not_allowed: outcome "ghost-name" is not in allowed_outcomes`)
	}
	for _, o := range sink.outcome {
		require.NotEqual(t, "ship", o.step, "a rejected-then-exhausted step emits no outcome event")
	}
	require.Empty(t, sink.defaulted, "no default is declared, so nothing is default-mapped")
}

// TestRun_NeverFinalizeSynthesizesTheDeclaredFallbackByName pins the
// recomposed probe observation: an adapter that produces no verdict at all
// finalizes with the step's declared fallback arm BY NAME, synthesized by the
// host with no comment, no payload, and no repair loop. The observation stays
// loud AND routed: the run ends on a declared failure terminal reached
// through the standard mapping path, not through an engine error or a
// swallowed silence.
func TestRun_NeverFinalizeSynthesizesTheDeclaredFallbackByName(t *testing.T) {
	sink, err := probeRun(t, neverFinalizeProbeWorkflow(), func() (adapter.Result, error) {
		return adapter.Result{}, nil
	})
	require.NoError(t, err, "the synthesis routes cleanly; loudness is the failure terminal, not an engine error")
	require.Equal(t, "probe_failed", sink.terminal)
	require.False(t, sink.terminalOK, "the probe observation must land on a failure terminal, not a swallowed success")
	require.Contains(t, sink.transitions, "ship->probe_failed")

	var observed []recordedOutcome
	for _, o := range sink.outcome {
		if o.step == "ship" {
			observed = append(observed, o)
		}
	}
	require.Len(t, observed, 1)
	require.Equal(t, "finalize_exhausted", observed[0].outcome)
	require.NoError(t, observed[0].err)
	require.Empty(t, observed[0].comment, "the synthesized fallback carries no comment")

	require.Empty(t, sink.invalid, "fallback synthesis is not a contract rejection and never enters the repair loop")
	require.Empty(t, sink.unknown)
	require.Empty(t, sink.defaulted)
	if outs, ok := sink.captured["ship"]; ok {
		require.Empty(t, outs, "the synthesized fallback carries no payload")
	}
}

// probeDiagText compiles builder output non-fatally and renders every
// diagnostic (summary + detail) so completeness rejections can be asserted.
func probeDiagText(t *testing.T, src string) string {
	t.Helper()
	spec, parseDiags := workflow.Parse("probe.hcl", []byte(src))
	require.False(t, parseDiags.HasErrors(), "parse: %s", parseDiags.Error())
	_, compileDiags := workflow.Compile(spec, nil)
	var sb strings.Builder
	for _, d := range compileDiags {
		sb.WriteString(d.Summary)
		if d.Detail != "" {
			sb.WriteString(": ")
			sb.WriteString(d.Detail)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// TestCompile_UnmappedProbeShapeIsACompletenessRejection pins the compile
// half of the impossibility: a contract-mode (schema-bearing) step whose
// design leaves adapter failure/error lanes unhandled is rejected outright —
// the verifier is absolute and has no contract-mode or adapter-kind opt-out —
// while the recomposed probe shape (schema success arm declared alongside a
// non-failure-class fallback arm) compiles clean.
func TestCompile_UnmappedProbeShapeIsACompletenessRejection(t *testing.T) {
	t.Run("schema-bearing success arm without failure lanes is rejected", func(t *testing.T) {
		b := workflowtest.NewStandard().
			WithInitial("probe").
			WithStep("probe", workflowtest.StepRaw(`outcome "success" {
  schema = object({ reason = string })
  next   = state.done
}`))
		msgs := probeDiagText(t, b.Build())
		require.Contains(t, msgs, `step "probe": outcome completeness requires handling adapter failure/error paths`)
	})
	t.Run("declared fallback synthesis shape compiles", func(t *testing.T) {
		g := compile(t, neverFinalizeProbeWorkflow().Build())
		require.NotNil(t, g)
	})
}
