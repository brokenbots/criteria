package engine

// KB-362 wire-path regression: the validator-style submission shape the
// agent-facing submit_outcome tool documents — {outcome, reason(top-level),
// payload:{}} — must record outcome "approved" at the engine level when the
// step's contract requires an in-payload reason. The host-side fold runs at
// the capture funnel, so the full
// SessionManager.execute → resolvedOutcome → attempt-loop route sees the
// documented shape as a valid finalized outcome instead of wedging the run
// in a payload_invalid repair loop.

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"

	criteriav2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
)

func kb362WireSubmitEvent(t *testing.T, outcome, reason string) *criteriav2.ExecuteEvent {
	t.Helper()
	st, err := structpb.NewStruct(map[string]any{"outcome": outcome, "reason": reason})
	require.NoError(t, err)
	return &criteriav2.ExecuteEvent{
		Event: &criteriav2.ExecuteEvent_Tool{
			Tool: &criteriav2.ToolInvocation{ToolName: "submit_outcome", Args: st},
		},
	}
}

func kb362WireWorkflow() string {
	return `
workflow {
  name          = "kb362wire"
  version         = "0.1"
  initial_state = "validate_developer_response"
  target_state  = "done"
  policy {
    max_step_retries = 1
  }
}

type "finding" {
  schema = object({ reason = string })
}

adapter "fake" "default" {}

step "validate_developer_response" {
  target = adapter.fake.default
  outcome "approved" {
    schema = type.finding
    next   = state.done
  }
  outcome "failure" {
    fallback = true
    next     = state.failed
  }
  outcome "default" {
    next = state.defaulted
  }
}

state "done" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}
state "defaulted" {
  terminal = true
  success  = false
}
`
}

// TestRun_ValidatorSubmitShapeRecordsApproved pins the wedge fix end to end:
// the outcome-submit invoke carries the reason top-level and the delivered
// ExecuteResult payload is the empty object, and the run records outcome
// "approved" with the reason materialized into the captured outputs — no
// payload_invalid repair loop.
func TestRun_ValidatorSubmitShapeRecordsApproved(t *testing.T) {
	const reason = "KB-362 repro: the deliverable is green-complete; reason rides top-level"
	client := &kb45WireClient{
		events: func(int) []*criteriav2.ExecuteEvent {
			return []*criteriav2.ExecuteEvent{
				kb362WireSubmitEvent(t, "approved", reason),
				kb45ResultEvent("approved", "PR is green-complete", []byte(`{}`)),
			}
		},
		execErr: func(int) error { return nil },
	}

	sink, err := kb45WireRun(t, kb362WireWorkflow(), client)
	require.NoError(t, err)
	require.Equal(t, "done", sink.terminal, "the approved outcome drives the run to its terminal state")
	require.Empty(t, sink.invalid, "the documented submit shape must not enter the repair loop")
	require.Empty(t, sink.defaulted, "no default mapping may fire over the documented submit shape")

	var outcomes []string
	for _, o := range sink.outcome {
		if o.step == "validate_developer_response" {
			outcomes = append(outcomes, o.outcome)
			require.NoError(t, o.err)
			require.Equal(t, "PR is green-complete", o.comment, "the delivered comment is preserved")
		}
	}
	require.Equal(t, []string{"approved"}, outcomes)

	captured := sink.captured["validate_developer_response"]
	require.Contains(t, captured["reason"], "the deliverable is green-complete",
		"the folded reason must surface in the step's captured outputs")
}

// TestRun_ValidatorEmptyPayloadWithoutReasonRejects pins the fail-visible
// companion: with no finalize captures at all, the wrapped empty payload still
// lands in the repair loop with the pinned required-property issue and the
// exhausted budget maps to the default outcome — the fold never invents a
// reason out of nowhere.
func TestRun_ValidatorEmptyPayloadWithoutReasonRejects(t *testing.T) {
	client := &kb45WireClient{
		events: func(int) []*criteriav2.ExecuteEvent {
			return []*criteriav2.ExecuteEvent{
				kb45ResultEvent("approved", "green", []byte(`{}`)),
			}
		},
		execErr: func(int) error { return nil },
	}

	sink, err := kb45WireRun(t, kb362WireWorkflow(), client)
	require.NoError(t, err)
	require.Equal(t, "defaulted", sink.terminal, "exhausted repairs map to the default outcome")

	require.Len(t, sink.invalid, 2, "one payload_invalid per rejected attempt (budget 1 + initial)")
	for i, inv := range sink.invalid {
		require.Equal(t, "validate_developer_response", inv.step)
		require.Equal(t, "approved", inv.outcome)
		require.Equal(t, i+1, inv.attempt)
		require.Contains(t, inv.issues, `payload_schema: property "reason": required property is missing`)
	}
}
