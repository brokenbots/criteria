package adapterhost

// KB-237: the finalize comment must reach the engine's require_comment
// evaluator. The copilot adapter validates the comment in-turn (it would
// reject a comment-less submit against the outcome's require_comment
// contract via the pinned EvaluateOutcomeContracts), but the comment the
// model supplied only rides:
//
//   - the submit_outcome tool invocation arguments (adapterEventFinalizeTool,
//     guard KB-42: {"outcome", "comment", "reason", ...}), and
//   - the contract-mode terminal ExecuteResult the adapter delivers when the
//     copilot turn goes idle (ExecuteResult.Comment, via its
//     outcome.finalized → contractResultEvent handoff).
//
// The KB-56 turn cut cancels the stream the instant outcome.finalized lands,
// so the comment-bearing terminal result often never makes it across, and
// the outcome.finalized event payload carries only {outcome, reason}. When
// the KB-53 rescue then reconstructs the verdict, the comment used to be
// lost (missing_comment burned the attempt loop, then max_visits parked the
// run in Review). These tests pin the reconstruction contract:
//
//  1. the outcome.finalized "comment" echo (the durable adapter contract
//     slot) is surfaced as ExecuteResult.Comment;
//  2. the accepted submit_outcome invocation arguments graft the comment
//     when no echo exists — but only when the invocation's outcome matches
//     the finalized verdict (unaccepted/mismatched invocations are not
//     require_comment evidence);
//  3. the comment never leaks into the step outputs, and
//  4. with no comment source the verdict stays fail-visible — the comment is
//     never aliased from the reason (reason carries the step's findings;
//     comment is the require_comment satisfaction signal), and a delivered
//     result always wins over the graft.

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"

	"github.com/brokenbots/criteria/workflow"
)

const kb237Comment = "No-op resume: workstream complete, branch converges PR-ready"

const kb237MissingCommentIssue = `missing_comment: outcome "ready_for_review" requires a comment (require_comment)`

// kb237RequireCommentStep is the contract-mode develop step whose
// ready_for_review arm requires a comment (the pair_programming_loop shape).
func kb237RequireCommentStep() *workflow.StepNode {
	return &workflow.StepNode{
		Name: "develop",
		Outcomes: map[string]*workflow.CompiledOutcome{
			"ready_for_review": {Name: "ready_for_review", Next: "create_pr", RequireComment: true},
			"checkpoint":       {Name: "checkpoint", Next: "push_checkpoint"},
		},
	}
}

func kb237SubmitEvent(t *testing.T, args map[string]any) *v2.ExecuteEvent {
	t.Helper()
	st, err := structpb.NewStruct(args)
	require.NoError(t, err)
	return &v2.ExecuteEvent{
		Event: &v2.ExecuteEvent_Tool{
			Tool: &v2.ToolInvocation{ToolName: adapterEventFinalizeTool, Args: st},
		},
	}
}

func kb237FinalizedEvent(t *testing.T, payload map[string]any) *v2.ExecuteEvent {
	t.Helper()
	st, err := structpb.NewStruct(payload)
	require.NoError(t, err)
	return &v2.ExecuteEvent{
		Event: &v2.ExecuteEvent_Adapter{
			Adapter: &v2.AdapterEvent{EventKind: adapterEventFinalizedOutcome, Payload: st},
		},
	}
}

// kb237IncidentEvents wires the incident shape: the accepted submit_outcome
// tool invocation (all three declared parameters, seq1278) followed by the
// adapter's outcome.finalized observation (seq1279 — by contract it carries
// only {outcome, reason}) and a stream that ends before the comment-bearing
// terminal ExecuteResult can be delivered (the KB-56 cut). The execErr is
// set by the caller.
func kb237IncidentEvents(t *testing.T, finalized map[string]any) []*v2.ExecuteEvent {
	t.Helper()
	return []*v2.ExecuteEvent{
		kb237SubmitEvent(t, map[string]any{
			"outcome": "ready_for_review",
			"comment": kb237Comment,
			"reason":  "KB-237 repro: findings ride reason",
		}),
		kb237FinalizedEvent(t, finalized),
	}
}

// runKB237Attempt drives one attempt through ExecuteViaClient and returns
// the resolved verdict (outcome + comment).
func runKB237Attempt(t *testing.T, client Client, step *workflow.StepNode) (string, string, error) {
	t.Helper()
	res, execErr := ExecuteViaClient(context.Background(), client, "copilot", "s1", true, step, &adapterEventCollector{}, nil)
	return res.Outcome, res.Comment, execErr
}

// TestKB237_SubmitOutcomeGraftReachesEvaluator pins the incident fix (run
// 92bd1876-6770-4647-85ae-e64f0fdf3055): the submit_outcome invocation with
// {outcome, comment, reason} followed by the comment-less outcome.finalized
// observation reaches the require_comment evaluator WITH the comment — no
// missing_comment, verdict accepted.
func TestKB237_SubmitOutcomeGraftReachesEvaluator(t *testing.T) {
	client := &kb45ReplayClient{
		events:  kb237IncidentEvents(t, map[string]any{"outcome": "ready_for_review", "reason": "KB-237 COMPLETE"}),
		execErr: io.EOF,
	}

	outcome, comment, err := runKB237Attempt(t, client, kb237RequireCommentStep())
	require.NoError(t, err, "the accepted submit's comment must satisfy require_comment")
	require.Equal(t, "ready_for_review", outcome)
	require.Equal(t, kb237Comment, comment, "the graft must ride ExecuteResult.Comment")
}

// TestKB237_FinalizedEchoCommentWins pins the durable echo slot: when the
// adapter echoes the comment on the outcome.finalized payload (the durable
// contract the in-repo host reads), the echo is the comment source and the
// grafted submit copy is not consulted.
func TestKB237_FinalizedEchoCommentWins(t *testing.T) {
	client := &kb45ReplayClient{
		events: []*v2.ExecuteEvent{
			kb237SubmitEvent(t, map[string]any{"outcome": "ready_for_review", "comment": kb237Comment}),
			kb237FinalizedEvent(t, map[string]any{
				"outcome": "ready_for_review",
				"reason":  "echo lane",
				"comment": "echo-shipping",
			}),
		},
		execErr: io.EOF,
	}

	outcome, comment, err := runKB237Attempt(t, client, kb237RequireCommentStep())
	require.NoError(t, err)
	require.Equal(t, "ready_for_review", outcome)
	require.Equal(t, "echo-shipping", comment)
}

// TestKB237_EchoCommentNeverLeaksIntoOutputs pins that the comment is
// verdict metadata, not a step output — the payload projection excludes it.
func TestKB237_EchoCommentNeverLeaksIntoOutputs(t *testing.T) {
	step := &workflow.StepNode{
		Name: "develop",
		Outcomes: map[string]*workflow.CompiledOutcome{
			"ready_for_review": {Name: "ready_for_review", Next: "create_pr", RequireComment: true},
		},
	}
	client := &kb45ReplayClient{
		events: []*v2.ExecuteEvent{
			kb237FinalizedEvent(t, map[string]any{
				"outcome":  "ready_for_review",
				"reason":   "echo lane",
				"comment":  "echo-shipping",
				"attempts": float64(1),
			}),
		},
		execErr: io.EOF,
	}

	res, execErr := ExecuteViaClient(context.Background(), client, "copilot", "s1", true, step, &adapterEventCollector{}, nil)
	require.NoError(t, execErr)
	require.Equal(t, "echo-shipping", res.Comment)
	require.Contains(t, res.Outputs, "attempts")
	require.NotContains(t, res.Outputs, "comment", "the comment echo must not project as a step output")
}

// TestKB237_GraftRequiresOutcomeMatch pins the guard: a recorded submit for
// a different outcome is not evidence for the finalized verdict — its
// comment must not satisfy require_comment, so the verdict stays
// fail-visible.
func TestKB237_GraftRequiresOutcomeMatch(t *testing.T) {
	client := &kb45ReplayClient{
		events: []*v2.ExecuteEvent{
			kb237SubmitEvent(t, map[string]any{"outcome": "checkpoint", "comment": kb237Comment}),
			kb237FinalizedEvent(t, map[string]any{"outcome": "ready_for_review", "reason": "swapped outcome"}),
		},
		execErr: io.EOF,
	}

	outcome, _, err := runKB237Attempt(t, client, kb237RequireCommentStep())
	require.Empty(t, outcome, "an invalid reconstructed verdict must not resolve")
	var invErr *OutcomeInvalidError
	require.ErrorAs(t, err, &invErr)
	require.Contains(t, invErr.Issues, kb237MissingCommentIssue)
	require.Equal(t, "ready_for_review", invErr.Outcome)
}

// TestKB237_LastSubmitWins pins last-wins: the last submit_outcome
// invocation for the finalized outcome carries the accepted comment (the
// adapter hard-rejects resubmission after acceptance, but rejected submits
// may trail an accepted one).
func TestKB237_LastSubmitWins(t *testing.T) {
	client := &kb45ReplayClient{
		events: []*v2.ExecuteEvent{
			kb237SubmitEvent(t, map[string]any{"outcome": "ready_for_review", "comment": "first draft"}),
			kb237SubmitEvent(t, map[string]any{"outcome": "ready_for_review", "comment": kb237Comment}),
			kb237FinalizedEvent(t, map[string]any{"outcome": "ready_for_review", "reason": "accepted"}),
		},
		execErr: io.EOF,
	}

	_, comment, err := runKB237Attempt(t, client, kb237RequireCommentStep())
	require.NoError(t, err)
	require.Equal(t, kb237Comment, comment)
}

// TestKB237_NoCommentSourcesStaysFailVisible pins the fail-visible lane:
// with no comment anywhere (no echo, no submit copy), the reconstructed
// verdict still hits the pinned missing_comment issue — the comment is
// never aliased from reason and never fabricated.
func TestKB237_NoCommentSourcesStaysFailVisible(t *testing.T) {
	client := &kb45ReplayClient{
		events: []*v2.ExecuteEvent{
			kb237SubmitEvent(t, map[string]any{"outcome": "ready_for_review", "reason": "findings only"}),
			kb237FinalizedEvent(t, map[string]any{"outcome": "ready_for_review", "reason": "KB-237 findings ride reason"}),
		},
		execErr: io.EOF,
	}

	outcome, _, err := runKB237Attempt(t, client, kb237RequireCommentStep())
	require.Empty(t, outcome)
	var invErr *OutcomeInvalidError
	require.ErrorAs(t, err, &invErr)
	require.Contains(t, invErr.Issues, kb237MissingCommentIssue)
}

// TestKB237_DeliveredResultWinsOverGraft pins the precedence: a delivered
// ExecuteResult always beats the reconstruction, including an honest empty
// comment — the graft never launders a delivered comment-less verdict.
func TestKB237_DeliveredResultWinsOverGraft(t *testing.T) {
	client := &kb45ReplayClient{
		events: []*v2.ExecuteEvent{
			kb237SubmitEvent(t, map[string]any{"outcome": "ready_for_review", "comment": kb237Comment}),
			kb237FinalizedEvent(t, map[string]any{"outcome": "ready_for_review", "reason": "delivered after submit"}),
			kb45ResultEvent("ready_for_review", "", nil),
		},
		execErr: io.EOF,
	}

	outcome, _, err := runKB237Attempt(t, client, kb237RequireCommentStep())
	require.Empty(t, outcome)
	var invErr *OutcomeInvalidError
	require.ErrorAs(t, err, &invErr)
	require.Contains(t, invErr.Issues, kb237MissingCommentIssue)
}

// TestKB237_ContinueStreamingVerdictCarriesComment pins the KB-56
// continue-past-submit shape (stream kept alive, execErr nil): the rebuild
// carries the comment the same way.
func TestKB237_ContinueStreamingVerdictCarriesComment(t *testing.T) {
	client := &kb45ReplayClient{
		events: kb237IncidentEvents(t, map[string]any{"outcome": "ready_for_review", "reason": "wrap-up streaming continues"}),
	}

	outcome, comment, err := runKB237Attempt(t, client, kb237RequireCommentStep())
	require.NoError(t, err)
	require.Equal(t, "ready_for_review", outcome)
	require.Equal(t, kb237Comment, comment)
}
