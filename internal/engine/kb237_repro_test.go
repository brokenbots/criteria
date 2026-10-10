package engine

// kb237_repro_test.go — paired regression tests for KB-237 at the
// engine/adapter boundary: the finalize comment the agent supplies to the
// copilot submit_outcome tool must reach the engine's require_comment
// evaluator.
//
// Incident shape (castle run 92bd1876-6770-4647-85ae-e64f0fdf3055, KB-236's
// refire on the live fleet): seq1278 carried tool.invocation submit_outcome
// with ALL THREE declared parameters — {"comment", "outcome", "reason"} —
// but the adapter's outcome.finalized verdict carries only {outcome, reason}
// and the KB-56 turn cut prevents the comment-bearing terminal ExecuteResult
// from ever being delivered. The engine evaluator then rejected every
// attempt with missing_comment; the repair loop exhausted and the outcome
// map defaulted to push_checkpoint; three identical (correct!) resubmissions
// were each swallowed the same way; max_visits=3 burned and the run parked
// awaiting_human with a COMPLETE branch it would not open a PR for.
//
// The engine now sees the comment because the host reconstructs it in the
// KB-53 rescue (internal/adapterhost, the KB-237 fix). These tests drive a
// copilot-shaped v2 client through NewRPCHandle so the full
// SessionManager → rpcHandle → ExecuteViaClient → contract evaluator path
// is exercised, and pin the no-op-resume shape end-to-end.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	structpb "google.golang.org/protobuf/types/known/structpb"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapterhost"
)

// kb237Comment is the no-op-resume finalize comment from the incident.
const kb237Comment = "No-op resume: workstream complete, branch converges PR-ready"

// kb237Workflow is the pair_programming_loop shape: develop finalizes
// ready_for_review into the post-develop leg; the exhausted/default and
// checkpoint arms route back around push_checkpoint (max_visits-bounded),
// and failure parks the run. On a no-op resume the loop must never reach
// push_checkpoint.
const kb237Workflow = `
workflow {
  name          = "kb237"
  version       = "0.1"
  initial_state = "develop"
  target_state  = "awaiting_human"

  policy { max_step_retries = 2 }
}

adapter "copilot" "default" {}
adapter "pipeline" "default" {}

step "develop" {
  target = adapter.copilot.default
  outcome "ready_for_review" {
    require_comment = true
    next            = step.create_pr
  }
  outcome "checkpoint" { next = step.push_checkpoint }
  outcome "failure"    { next = state.awaiting_human }
  outcome "default"    { next = step.push_checkpoint }
}
step "push_checkpoint" {
  target     = adapter.pipeline.default
  max_visits = 3
  outcome "success" { next = step.develop }
}
step "create_pr" {
  target = adapter.pipeline.default
  outcome "success" { next = state.awaiting_human }
}
state "awaiting_human" {
  terminal = true
  success  = true
}`

// kb237TurnClient replays the incident's wire shape on every develop turn:
// the accepted submit_outcome tool invocation ({"outcome", "comment",
// "reason"} — comment present only when enabled), the adapter's
// outcome.finalized observation ({outcome, reason} only), and a stream that
// ends before the terminal ExecuteResult can be delivered. The remaining
// client surface embeds the KB-53 stub.
type kb237TurnClient struct {
	kb53FinalizingClient
	comment     string
	submissions int
}

func (c *kb237TurnClient) Execute(_ context.Context, _ *v2.ExecuteRequest, sink adapterhost.ExecuteEventSink) error {
	c.submissions++
	args := map[string]any{
		"outcome": "ready_for_review",
		"reason":  "KB-237 repro: findings ride reason",
	}
	if c.comment != "" {
		args["comment"] = c.comment
	}
	subArgs, _ := structpb.NewStruct(args)
	if err := sink.Emit(&v2.ExecuteEvent{Event: &v2.ExecuteEvent_Tool{
		Tool: &v2.ToolInvocation{ToolName: "submit_outcome", Args: subArgs},
	}}); err != nil {
		return err
	}
	payload, _ := structpb.NewStruct(map[string]any{"outcome": "ready_for_review", "reason": "KB-237 COMPLETE"})
	if err := sink.Emit(&v2.ExecuteEvent{Event: &v2.ExecuteEvent_Adapter{
		Adapter: &v2.AdapterEvent{EventKind: "outcome.finalized", Payload: payload},
	}}); err != nil {
		return err
	}
	return errors.New("rpc error: code = Canceled desc = grpc: the client connection is closing")
}

// kb237Loader wires the copilot RPC handle (full
// SessionManager → rpcHandle → ExecuteViaClient path) and the plain
// pipeline success adapter for the loop/bookkeeping steps.
func kb237Loader(comment string) adapterhost.Loader {
	client := &kb237TurnClient{comment: comment}
	return &fakeLoader{adapters: map[string]adapterhost.Handle{
		"copilot":          adapterhost.NewRPCHandle("copilot.default", nil, client),
		"copilot.default":  adapterhost.NewRPCHandle("copilot.default", nil, client),
		"pipeline":         &fakeAdapter{name: "pipeline", outcome: "success"},
		"pipeline.default": &fakeAdapter{name: "pipeline", outcome: "success"},
	}}
}

// TestKB237_NoOpResumeFinalizesReadyForReviewWithoutCheckpoint pins
// acceptance #1+#2: the identical no-op-resume resubmission (the branch is
// already converged; the resubmission is byte-equal) must reach the
// evaluator WITH the comment — zero stepOutcomeInvalid events, the
// ready_for_review arm taken with the comment surfaced verbatim on the
// step outcome event, the post-develop leg runs, and push_checkpoint is
// never visited.
func TestKB237_NoOpResumeFinalizesReadyForReviewWithoutCheckpoint(t *testing.T) {
	g := compile(t, kb237Workflow)
	sink := &contractSink{}

	err := NewTestEngine(g, kb237Loader(kb237Comment), sink).Run(context.Background())

	require.NoError(t, err, "the accepted submit's comment must satisfy require_comment on the first visit")
	require.Equal(t, "awaiting_human", sink.terminal)
	require.True(t, sink.terminalOK)

	require.Empty(t, sink.invalid, "stepOutcomeInvalid must never fire for a comment-bearing submit")
	require.Empty(t, sink.defaulted, "the outcome map must never default to push_checkpoint")

	visited := 0
	for _, s := range sink.stepsRun {
		if s == "push_checkpoint" {
			visited++
		}
	}
	require.Zero(t, visited, "the pair-programming loop must exit before push_checkpoint")
	entered := 0
	for _, s := range sink.stepsRun {
		if s == "develop" {
			entered++
		}
	}
	require.Equal(t, 1, entered, "the converged branch must resolve on the first visit")

	var ready []string
	for _, o := range sink.outcome {
		if o.step == "develop" && o.outcome == "ready_for_review" && o.err == nil {
			ready = append(ready, o.comment)
		}
	}
	require.Equal(t, []string{kb237Comment}, ready, "the comment must surface verbatim on the workflow-level outcome")

	var failed []string
	for _, o := range sink.outcome {
		if o.step == "create_pr" {
			failed = append(failed, o.outcome)
		}
	}
	require.Equal(t, []string{"success"}, failed, "the post-develop leg must run")
}

// TestKB237_SwallowedCommentBurnsVisitsThenDefaults is the negative control:
// WITHOUT a comment source the same wire shape replays the incident — each
// develop turn bounces missing_comment, the repair loop exhausts, the map
// defaults to push_checkpoint, the loop re-enters develop, and
// max_visits burns (the run fails with exceeded max_visits). This proves the
// positive case above exits because the comment satisfied require_comment,
// not because the workflow shape itself avoids the loop.
func TestKB237_SwallowedCommentBurnsVisitsThenDefaults(t *testing.T) {
	g := compile(t, kb237Workflow)
	sink := &contractSink{}

	err := NewTestEngine(g, kb237Loader(""), sink).Run(context.Background())

	require.Error(t, err, "the incident shape must burn the visit budget")
	require.Contains(t, err.Error(), "exceeded max_visits")

	visited := 0
	for _, s := range sink.stepsRun {
		if s == "push_checkpoint" {
			visited++
		}
	}
	require.GreaterOrEqual(t, visited, 3, "max_visits=3 must burn before the run fails")
	require.Contains(t, err.Error(), `"push_checkpoint"`)

	require.NotEmpty(t, sink.invalid, "each swallowed attempt must emit stepOutcomeInvalid")
	for _, inv := range sink.invalid {
		require.Equal(t, "develop", inv.step)
		require.Equal(t, "ready_for_review", inv.outcome)
		require.Contains(t, inv.issues, `missing_comment: outcome "ready_for_review" requires a comment (require_comment)`)
	}

	defaults := 0
	for _, d := range sink.defaulted {
		if d[0] == "develop" && d[1] == "default" && d[2] == "default" {
			defaults++
		}
	}
	require.GreaterOrEqual(t, defaults, 1, "the exhausted map must default to push_checkpoint (via the default outcome)")
}

// TestKB237_SwallowedCommentExhaustionStillFailsRun pins that a
// require_comment rejection that exhausts the repair loop without a default
// outcome fails the run with the pinned issues (the pre-fix engine behavior
// is fail-visible, never silently accepted).
func TestKB237_SwallowedCommentExhaustionStillFailsRun(t *testing.T) {
	g := compile(t, `
workflow {
  name          = "kb237-nodefault"
  version       = "0.1"
  initial_state = "develop"
  target_state  = "awaiting_human"

  policy { max_step_retries = 1 }
}
adapter "copilot" "default" {}
step "develop" {
  target = adapter.copilot.default
  outcome "ready_for_review" {
    require_comment = true
    next            = state.awaiting_human
  }
}
state "awaiting_human" {
  terminal = true
  success  = true
}`)
	client := &kb237TurnClient{}
	sink := &contractSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"copilot":         adapterhost.NewRPCHandle("copilot.default", nil, client),
		"copilot.default": adapterhost.NewRPCHandle("copilot.default", nil, client),
	}}

	err := NewTestEngine(g, loader, sink).Run(context.Background())

	require.Error(t, err, "a comment-less submit must fail the run with the pinned issues")
	require.Len(t, sink.invalid, 2, "one event per rejected attempt (budget 1 + initial)")
	require.Contains(t, sink.invalid[0].issues, `missing_comment: outcome "ready_for_review" requires a comment (require_comment)`)
	require.Equal(t, 2, client.submissions, "the repair loop must have retried once")
}

// TestKB237_ContinueStreamingShapeResolvesWithComment pins the KB-56
// continue-past-submit variant at the engine boundary: the turn keeps
// streaming wrap-up deltas after the submit; the cut ends it at the outcome
// and the comment still satisfies require_comment.
func TestKB237_ContinueStreamingShapeResolvesWithComment(t *testing.T) {
	g := compile(t, kb237Workflow)

	client := &kb56StreamingSubmitClient{comment: kb237Comment}
	sink := &contractSink{}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"copilot":          adapterhost.NewRPCHandle("copilot.default", nil, client),
		"copilot.default":  adapterhost.NewRPCHandle("copilot.default", nil, client),
		"pipeline":         &fakeAdapter{name: "pipeline", outcome: "success"},
		"pipeline.default": &fakeAdapter{name: "pipeline", outcome: "success"},
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := NewTestEngine(g, loader, sink).Run(ctx)

	require.NoError(t, err)
	require.Empty(t, sink.invalid)
	require.Equal(t, "awaiting_human", sink.terminal)
	require.True(t, sink.terminalOK)
	entered := 0
	for _, s := range sink.stepsRun {
		if s == "push_checkpoint" {
			entered++
		}
	}
	require.Zero(t, entered)
}

// kb56StreamingSubmitClient reuses the KB-56 continuing-turn shape with the
// KB-237 submit arguments on top.
type kb56StreamingSubmitClient struct {
	kb53FinalizingClient
	comment string
}

func (c *kb56StreamingSubmitClient) Execute(ctx context.Context, _ *v2.ExecuteRequest, sink adapterhost.ExecuteEventSink) error {
	args, _ := structpb.NewStruct(map[string]any{
		"outcome": "ready_for_review",
		"comment": c.comment,
		"reason":  "KB-237 repro: findings ride reason",
	})
	if err := sink.Emit(&v2.ExecuteEvent{Event: &v2.ExecuteEvent_Tool{
		Tool: &v2.ToolInvocation{ToolName: "submit_outcome", Args: args},
	}}); err != nil {
		return err
	}
	payload, _ := structpb.NewStruct(map[string]any{"outcome": "ready_for_review", "reason": "wrap-up"})
	if err := sink.Emit(&v2.ExecuteEvent{Event: &v2.ExecuteEvent_Adapter{
		Adapter: &v2.AdapterEvent{EventKind: "outcome.finalized", Payload: payload},
	}}); err != nil {
		return err
	}
	// The model keeps streaming past its submit (KB-56): deltas flow until
	// the host ends the turn at the outcome.
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		delta, _ := structpb.NewStruct(map[string]any{"text": "wrapping up"})
		if err := sink.Emit(&v2.ExecuteEvent{Event: &v2.ExecuteEvent_Adapter{
			Adapter: &v2.AdapterEvent{EventKind: "message.delta", Payload: delta},
		}}); err != nil {
			return err
		}
	}
}
