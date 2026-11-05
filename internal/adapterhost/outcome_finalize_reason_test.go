package adapterhost

// KB-362 regression tests for the finalize-reason alignment: the agent-facing
// submit_outcome tool documents reason as a top-level invoke field, while the
// compiled outcome contract validates the finalized payload, so a typed
// contract whose schema requires an in-payload reason must accept the
// documented submit shape via the capture-side fold (materializeFinalizeReason)
// and must keep refusing every shape the fold genuinely cannot accept.

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/workflow"
)

const (
	kb362FindingsReason = "KB-362 repro findings: the deliverable is green-complete"
	kb362RequiredIssue  = `payload_schema: property "reason": required property is missing`
)

func kb362ReasonType() *cty.Type {
	t := cty.Object(map[string]cty.Type{"reason": cty.String})
	return &t
}

func kb362CTypeToJSONSchema(t *testing.T, schemaType cty.Type) []byte {
	t.Helper()
	b, err := workflow.CTypeToJSONSchema(schemaType, nil)
	require.NoError(t, err)
	return b
}

func kb362ReasonStep(t *testing.T, extra func(*workflow.CompiledOutcome)) *workflow.StepNode {
	t.Helper()
	outcome := workflow.CompiledOutcome{Name: "approved", Next: "done", Schema: kb362ReasonType(), SchemaJSON: kb362CTypeToJSONSchema(t, *kb362ReasonType())}
	if extra != nil {
		extra(&outcome)
	}
	return &workflow.StepNode{
		Name: "validate_developer_response",
		Outcomes: map[string]*workflow.CompiledOutcome{
			"approved": &outcome,
		},
	}
}

func kb362Submit(t *testing.T, outcome, reason string) *v2.ExecuteEvent {
	t.Helper()
	args := map[string]any{"outcome": outcome}
	if reason != "" {
		args["reason"] = reason
	}
	return kb237SubmitEvent(t, args)
}

func runKB362Submit(t *testing.T, step *workflow.StepNode, client Client) (adapter.Result, error) {
	t.Helper()
	return ExecuteViaClient(context.Background(), client, "copilot", "s1", true, step, &adapterEventCollector{}, nil)
}

// kb362FoldedReason reads the reason the fold materialized into the verdict
// outputs ("" when absent).
func kb362FoldedReason(outputs map[string]cty.Value) string {
	if outputs == nil {
		return ""
	}
	if v := outputs["reason"]; v.IsKnown() && !v.IsNull() && v.Type() == cty.String {
		return v.AsString()
	}
	return ""
}

// TestKB362_SubmitReasonFoldsIntoPayloadOnDeliveredResult pins the wedge
// shape end to end at the capture funnel: the exact doc-shape submission
// ({outcome: "approved", reason: <top-level text>}) followed by a delivered
// ExecuteResult whose payload is the empty object records outcome "approved"
// with the reason materialized into the payload outputs.
func TestKB362_SubmitReasonFoldsIntoPayloadOnDeliveredResult(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []*v2.ExecuteEvent
	}{
		{
			name: "empty_payload",
			events: []*v2.ExecuteEvent{
				kb362Submit(t, "approved", kb362FindingsReason),
				kb45ResultEvent("approved", "PR is green-complete", []byte(`{}`)),
			},
		},
		{
			name: "absent_payload_member",
			events: []*v2.ExecuteEvent{
				kb362Submit(t, "approved", kb362FindingsReason),
				kb45ResultEvent("approved", "PR is green-complete", nil),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := runKB362Submit(t, kb362ReasonStep(t, nil), &kb45ReplayClient{events: tc.events})
			require.NoError(t, err)
			require.Equal(t, "approved", result.Outcome)
			require.Equal(t, "PR is green-complete", result.Comment, "the delivered comment is preserved")
			require.Equal(t, kb362FindingsReason, kb362FoldedReason(result.Outputs),
				"the documented top-level finalize reason must satisfy the payload contract")
		})
	}
}

// TestKB362_FinalizedPayloadReasonWinsOverSubmitArgs pins the capture-source
// priority for the fold: the outcome.finalized payload's reason slot is the
// durable copy the adapter vouches for, so it wins over the submit invoke
// argument when both are captured for the validated outcome.
func TestKB362_FinalizedPayloadReasonWinsOverSubmitArgs(t *testing.T) {
	events := []*v2.ExecuteEvent{
		kb362Submit(t, "approved", "reason from submit args"),
		kb237FinalizedEvent(t, map[string]any{"outcome": "approved", "reason": "reason from finalized payload"}),
		kb45ResultEvent("approved", "green", []byte(`{}`)),
	}
	result, err := runKB362Submit(t, kb362ReasonStep(t, nil), &kb45ReplayClient{events: events})
	require.NoError(t, err)
	require.Equal(t, "approved", result.Outcome)
	require.Equal(t, "reason from finalized payload", kb362FoldedReason(result.Outputs))
}

// TestKB362_SubmitReasonFoldsIntoRescueVerdict pins the rescue lane: the cut
// turn reconstructs the verdict from the finalized capture and the submit
// args when the upstream finalized payload dropped the reason (redaction), so
// the reason still satisfies the payload contract.
func TestKB362_SubmitReasonFoldsIntoRescueVerdict(t *testing.T) {
	client := &kb45ReplayClient{
		events: []*v2.ExecuteEvent{
			kb362Submit(t, "approved", kb362FindingsReason),
			kb237FinalizedEvent(t, map[string]any{"outcome": "approved"}),
		},
		execErr: context.Canceled,
	}
	result, err := runKB362Submit(t, kb362ReasonStep(t, nil), client)
	require.NoError(t, err)
	require.Equal(t, "approved", result.Outcome)
	require.Equal(t, kb362FindingsReason, kb362FoldedReason(result.Outputs))
}

// TestKB362_NoFinalizeReasonCaptureKeepsPinnedRejection keeps fold-away
// behavior fail-visible: an empty-object payload with no captured finalize
// reason still rejects with the pinned required-property issue, exactly as
// before the alignment (including the engine-side repair vocabulary).
func TestKB362_NoFinalizeReasonCaptureKeepsPinnedRejection(t *testing.T) {
	client := &kb45ReplayClient{
		events: []*v2.ExecuteEvent{kb45ResultEvent("approved", "green", []byte(`{}`))},
	}
	result, err := runKB362Submit(t, kb362ReasonStep(t, nil), client)
	require.Equal(t, adapter.Result{}, result)
	var invErr *OutcomeInvalidError
	require.True(t, errors.As(err, &invErr))
	require.Equal(t, "approved", invErr.Outcome)
	require.Contains(t, invErr.Issues, kb362RequiredIssue)
}

// TestKB362_ReasonCaptureDoesNotFoldAcrossOutcomes pins the outcome-match
// guard: a reason captured for the "approved" submit must not validate a
// different outcome's payload; the delivered "rejected" verdict still rejects
// for its own missing reason.
func TestKB362_ReasonCaptureDoesNotFoldAcrossOutcomes(t *testing.T) {
	t.Run("delivered_outcome_mismatch", func(t *testing.T) {
		step := &workflow.StepNode{
			Name: "validate_developer_response",
			Outcomes: map[string]*workflow.CompiledOutcome{
				"approved": {Name: "approved", Next: "done", Schema: kb362ReasonType(), SchemaJSON: kb362CTypeToJSONSchema(t, *kb362ReasonType())},
				"rejected": {Name: "rejected", Next: "failed", Schema: kb362ReasonType(), SchemaJSON: kb362CTypeToJSONSchema(t, *kb362ReasonType())},
			},
		}
		client := &kb45ReplayClient{
			events: []*v2.ExecuteEvent{
				kb362Submit(t, "approved", kb362FindingsReason),
				kb45ResultEvent("rejected", "changes requested", []byte(`{}`)),
			},
		}
		result, err := runKB362Submit(t, step, client)
		require.Equal(t, adapter.Result{}, result)
		var invErr *OutcomeInvalidError
		require.True(t, errors.As(err, &invErr))
		require.Equal(t, "rejected", invErr.Outcome)
		require.Contains(t, invErr.Issues, kb362RequiredIssue)
	})
}

// TestKB362_ExistingPayloadReasonStaysAuthoritative pins that the fold never
// overwrites a payload member the model already submitted: a wrong-typed
// in-payload reason still rejects with the pinned type issue (not the
// required-missing issue).
func TestKB362_ExistingPayloadReasonStaysAuthoritative(t *testing.T) {
	client := &kb45ReplayClient{
		events: []*v2.ExecuteEvent{
			kb362Submit(t, "approved", kb362FindingsReason),
			kb45ResultEvent("approved", "green", []byte(`{"reason": 42}`)),
		},
	}
	result, err := runKB362Submit(t, kb362ReasonStep(t, nil), client)
	require.Equal(t, adapter.Result{}, result)
	var invErr *OutcomeInvalidError
	require.True(t, errors.As(err, &invErr))
	require.Contains(t, invErr.Issues, `payload_schema: property "reason": expected "string", got "number"`)
	require.NotContains(t, invErr.Issues, kb362RequiredIssue)
}

// TestKB362_FoldOnlyFiresForReasonRequiringSchemas pins the schema gate from
// both sides: a contract requiring a different properties set is not satisfied
// by the captured reason, and an optional-reason schema accepts an absent
// payload with no materialization.
func TestKB362_FoldOnlyFiresForReasonRequiringSchemas(t *testing.T) {
	t.Run("other_required_property", func(t *testing.T) {
		summaryType := cty.Object(map[string]cty.Type{"summary": cty.String})
		step := &workflow.StepNode{
			Name: "audit_shape",
			Outcomes: map[string]*workflow.CompiledOutcome{
				"approved": {Name: "approved", Next: "done", Schema: &summaryType, SchemaJSON: kb362CTypeToJSONSchema(t, summaryType)},
			},
		}
		client := &kb45ReplayClient{
			events: []*v2.ExecuteEvent{
				kb362Submit(t, "approved", kb362FindingsReason),
				kb45ResultEvent("approved", "green", []byte(`{}`)),
			},
		}
		result, err := runKB362Submit(t, step, client)
		require.Equal(t, adapter.Result{}, result)
		var invErr *OutcomeInvalidError
		require.True(t, errors.As(err, &invErr))
		require.Contains(t, invErr.Issues, `payload_schema: property "summary": required property is missing`)
	})

	t.Run("optional_reason_not_materialized", func(t *testing.T) {
		optionalReason := cty.ObjectWithOptionalAttrs(map[string]cty.Type{"reason": cty.String}, []string{"reason"})
		step := &workflow.StepNode{
			Name: "optional_shape",
			Outcomes: map[string]*workflow.CompiledOutcome{
				"approved": {Name: "approved", Next: "done", Schema: &optionalReason, SchemaJSON: kb362CTypeToJSONSchema(t, optionalReason)},
			},
		}
		client := &kb45ReplayClient{
			events: []*v2.ExecuteEvent{
				kb362Submit(t, "approved", kb362FindingsReason),
				kb45ResultEvent("approved", "green", []byte(`{}`)),
			},
		}
		result, err := runKB362Submit(t, step, client)
		require.NoError(t, err)
		require.Equal(t, "approved", result.Outcome)
		require.Empty(t, result.Outputs, "an optional-reason schema accepts the empty payload; no fold is needed")
	})

	t.Run("untyped_outcome_no_fold_no_issue", func(t *testing.T) {
		step := &workflow.StepNode{
			Name: "untyped_shape",
			Outcomes: map[string]*workflow.CompiledOutcome{
				"approved": {Name: "approved", Next: "done"},
			},
		}
		client := &kb45ReplayClient{
			events: []*v2.ExecuteEvent{
				kb362Submit(t, "approved", kb362FindingsReason),
				kb45ResultEvent("approved", "green", []byte(`{}`)),
			},
		}
		result, err := runKB362Submit(t, step, client)
		require.NoError(t, err)
		require.Equal(t, adapter.Result{Outcome: "approved", Comment: "green"}, result)
	})
}

// TestKB362_UndecodableDeliveredPayloadKeepsPinnedIssue pins that the fold
// never launders an undecodable payload: verbatim bytes that are not a JSON
// object still reject with the pinned issue even with a captured reason.
func TestKB362_UndecodableDeliveredPayloadKeepsPinnedIssue(t *testing.T) {
	client := &kb45ReplayClient{
		events: []*v2.ExecuteEvent{
			kb362Submit(t, "approved", kb362FindingsReason),
			kb237FinalizedEvent(t, map[string]any{"outcome": "approved", "reason": kb362FindingsReason}),
			kb45ResultEvent("approved", "green", kb45UndecodablePayload),
		},
	}
	result, err := runKB362Submit(t, kb362ReasonStep(t, nil), client)
	require.Equal(t, adapter.Result{}, result)
	var invErr *OutcomeInvalidError
	require.True(t, errors.As(err, &invErr))
	require.Contains(t, invErr.Issues, kb45PinnedNonObjectIssue)
}

// TestKB362_FoldDoesNotSatisfyRequireComment pins the comment-contract lane
// boundary: the fold satisfies the payload schema only; a require_comment
// outcome submitted without a comment still rejects with missing_comment (the
// comment is never aliased from the reason).
func TestKB362_FoldDoesNotSatisfyRequireComment(t *testing.T) {
	step := kb362ReasonStep(t, func(c *workflow.CompiledOutcome) { c.RequireComment = true })
	client := &kb45ReplayClient{
		events: []*v2.ExecuteEvent{
			kb362Submit(t, "approved", kb362FindingsReason),
			kb45ResultEvent("approved", "", []byte(`{}`)),
		},
	}
	result, err := runKB362Submit(t, step, client)
	require.Equal(t, adapter.Result{}, result)
	var invErr *OutcomeInvalidError
	require.True(t, errors.As(err, &invErr))
	require.Contains(t, invErr.Issues, `missing_comment: outcome "approved" requires a comment (require_comment)`)
	require.NotContains(t, invErr.Issues, kb362RequiredIssue,
		"the payload fold still satisfies the schema; only the comment contract rejects")
}

// TestOutcomeSchemaRequiresReason pins the gate helper directly: declared
// outcome contracts, the default-outcome-by-name fallback, optional reason
// attributes, and schema-less outcomes.
func TestOutcomeSchemaRequiresReason(t *testing.T) {
	declared := kb362ReasonStep(t, nil)
	require.True(t, outcomeSchemaRequiresReason(declared, "approved"))
	require.False(t, outcomeSchemaRequiresReason(declared, "rejected"), "undeclared outcomes require nothing")

	requireComment := kb362ReasonStep(t, func(c *workflow.CompiledOutcome) { c.RequireComment = true })
	require.True(t, outcomeSchemaRequiresReason(requireComment, "approved"))

	optionalReason := cty.ObjectWithOptionalAttrs(map[string]cty.Type{"reason": cty.String}, []string{"reason"})
	optionalStep := &workflow.StepNode{
		Name: "optional",
		Outcomes: map[string]*workflow.CompiledOutcome{
			"approved": {Name: "approved", Next: "done", Schema: &optionalReason, SchemaJSON: kb362CTypeToJSONSchema(t, optionalReason)},
		},
	}
	require.False(t, outcomeSchemaRequiresReason(optionalStep, "approved"))

	bare := &workflow.StepNode{
		Name: "bare",
		Outcomes: map[string]*workflow.CompiledOutcome{
			"approved": {Name: "approved", Next: "done"},
		},
	}
	require.False(t, outcomeSchemaRequiresReason(bare, "approved"))

	defaulted := &workflow.StepNode{
		Name:     "defaulted",
		Outcomes: map[string]*workflow.CompiledOutcome{},
		DefaultOutcome: &workflow.CompiledOutcome{
			Name:       "approved",
			Next:       "done",
			Schema:     kb362ReasonType(),
			SchemaJSON: kb362CTypeToJSONSchema(t, *kb362ReasonType()),
		},
	}
	require.True(t, outcomeSchemaRequiresReason(defaulted, "approved"), "the default outcome resolves by name")
	require.False(t, outcomeSchemaRequiresReason(defaulted, ""), "an unnamed outcome resolves no contract")

	nilEntry := &workflow.StepNode{
		Name: "nil-entry",
		Outcomes: map[string]*workflow.CompiledOutcome{
			"approved": nil, // nil compiled entries are skipped, as on the wire builder
		},
	}
	require.False(t, outcomeSchemaRequiresReason(nilEntry, "approved"))
	require.False(t, outcomeSchemaRequiresReason(&workflow.StepNode{Name: "nil"}, "approved"), "a contract-less step requires nothing")
}
