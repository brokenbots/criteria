package adapterhost

// kb53_outcome_finalize_test.go — regression tests for KB-53: an adapter that
// resolves its turn at the adapter level (outcome.finalized, e.g. the copilot
// submit_outcome tool with the KB-42 no-outcome guard) must produce that
// workflow-level outcome even when the Execute stream then ends abnormally —
// a lost session shim (copilot "grpc: the client connection is closing"), a
// mid-stream error, or a clean close that never carries a result event.
//
// The incident (castle run kb-41-1790653828): the develop turn finalized
// ready_for_review at the adapter level, the stream broke, the host threw the
// verdict away and synthesized Outcome "failure", and the post-develop
// pipeline (create_pr, reviewer loop, set_review_state) never ran. These
// tests pin the rescued-verdict behavior at the ExecuteViaClient layer.

import (
	"context"
	"errors"
	"testing"

	structpb "google.golang.org/protobuf/types/known/structpb"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/workflow"
)

// kb53CopilotCrashErr is the copilot session-loss signature from the KB-53
// incident (develop session shim, "grpc: the client connection is closing").
const kb53CopilotCrashErr = "rpc error: code = Canceled desc = grpc: the client connection is closing"

// kb53Step is a minimal develop-shaped step exercising the rescue paths.
func kb53Step() *workflow.StepNode {
	return &workflow.StepNode{Name: "develop", AllowTools: []string{"edit", "shell"}}
}

// snapshot returns a copy of the collected adapter events (read-safe here:
// these tests drive the sink single-threaded).
func (c *adapterEventCollector) snapshot() []adapterEvent {
	return c.events
}

// kb53Client replays a scripted Execute event stream and then returns
// execErr, modelling a copilot turn that finalized its outcome at the adapter
// level before the transport died.
type kb53Client struct {
	recordingClient

	events  []*v2.ExecuteEvent
	execErr error
}

func (c *kb53Client) Execute(_ context.Context, req *v2.ExecuteRequest, sink ExecuteEventSink) error {
	c.lastExecuteReq = req
	for _, ev := range c.events {
		if err := sink.Emit(ev); err != nil {
			return err
		}
	}
	return c.execErr
}

func kb53Event(kind string, payload map[string]any) *v2.ExecuteEvent {
	var p *structpb.Struct
	if payload != nil {
		p, _ = structpb.NewStruct(payload)
	}
	return &v2.ExecuteEvent{Event: &v2.ExecuteEvent_Adapter{
		Adapter: &v2.AdapterEvent{EventKind: kind, Payload: p},
	}}
}

func kb53FinalizedEvent(outcome, reason string) *v2.ExecuteEvent {
	return kb53Event(adapterEventFinalizedOutcome, map[string]any{"outcome": outcome, "reason": reason})
}

func TestExecuteViaClient_KB53FinalizedOutcomeStreamEndsWithoutResult(t *testing.T) {
	client := &kb53Client{events: []*v2.ExecuteEvent{kb53FinalizedEvent("ready_for_review", "pr ready")}}
	collector := &adapterEventCollector{}

	result, err := ExecuteViaClient(context.Background(), client, "copilot", "s1", true, kb53Step(), collector, nil)
	if err != nil {
		t.Fatalf("ExecuteViaClient: %v (a finalized verdict must survive a stream that ends without a result)", err)
	}
	if result.Outcome != "ready_for_review" {
		t.Fatalf("outcome = %q, want ready_for_review", result.Outcome)
	}
	events := collector.snapshot()
	if len(events) != 1 || events[0].kind != adapterEventFinalizedOutcome {
		t.Fatalf("adapter events = %+v, want the outcome.finalized event forwarded", events)
	}
}

func TestExecuteViaClient_KB53FinalizedOutcomeStreamError(t *testing.T) {
	client := &kb53Client{
		events:  []*v2.ExecuteEvent{kb53FinalizedEvent("ready_for_review", "pr ready")},
		execErr: errors.New(kb53CopilotCrashErr),
	}
	collector := &adapterEventCollector{}

	result, err := ExecuteViaClient(context.Background(), client, "copilot", "s1", true, kb53Step(), collector, nil)
	if err != nil {
		t.Fatalf("ExecuteViaClient: %v (the finalized verdict must survive the lost stream)", err)
	}
	if result.Outcome != "ready_for_review" {
		t.Fatalf("outcome = %q, want ready_for_review", result.Outcome)
	}
	if events := collector.snapshot(); len(events) != 1 {
		t.Fatalf("adapter events = %+v, want the finalized event forwarded", events)
	}
}

func TestExecuteViaClient_KB53CapturedResultWins(t *testing.T) {
	client := &kb53Client{
		events: []*v2.ExecuteEvent{
			kb53FinalizedEvent("ready_for_review", "obsolete"),
			{Event: &v2.ExecuteEvent_Result{Result: &v2.ExecuteResult{
				Outcome:     "ready_for_review",
				OutputsJson: []byte(`{"reason":"final result"}`),
			}}},
		},
		execErr: errors.New(kb53CopilotCrashErr),
	}
	collector := &adapterEventCollector{}

	result, err := ExecuteViaClient(context.Background(), client, "copilot", "s1", true, kb53Step(), collector, nil)
	if err != nil {
		t.Fatalf("ExecuteViaClient: %v", err)
	}
	if result.Outcome != "ready_for_review" {
		t.Fatalf("outcome = %q, want ready_for_review", result.Outcome)
	}
	if got := result.Outputs["reason"].AsString(); got != "final result" {
		t.Fatalf("reason output = %q, want \"final result\" (the captured result must win verbatim)", got)
	}
}

func TestExecuteViaClient_KB53NoEvidenceStaysFailure(t *testing.T) {
	client := &kb53Client{}
	collector := &adapterEventCollector{}

	result, err := ExecuteViaClient(context.Background(), client, "copilot", "s1", true, kb53Step(), collector, nil)
	if err == nil {
		t.Fatal("expected an error when the stream ends without any verdict evidence")
	}
	if result.Outcome != "failure" {
		t.Fatalf("outcome = %q, want failure", result.Outcome)
	}
}

func TestExecuteViaClient_KB53CanceledContextNotRescued(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &kb53Client{events: []*v2.ExecuteEvent{kb53FinalizedEvent("ready_for_review", "pr ready")}}
	collector := &adapterEventCollector{}

	cancel() // host-initiated teardown: the rescue must not fire
	result, err := ExecuteViaClient(ctx, client, "copilot", "s1", true, kb53Step(), collector, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if result.Outcome != "failure" {
		t.Fatalf("outcome = %q, want failure", result.Outcome)
	}
}

func TestExecuteViaClient_KB53ChunkedFinalizedOutcome(t *testing.T) {
	frags := v2.ChunkAdapterEventPayload(
		&v2.AdapterEvent{EventKind: adapterEventFinalizedOutcome},
		[]byte(`{"outcome":"ready_for_review","reason":"chunked payload reason"}`), 6)
	if len(frags) < 2 {
		t.Fatalf("expected chunked fragments, got %d", len(frags))
	}
	events := make([]*v2.ExecuteEvent, 0, len(frags))
	for _, f := range frags {
		events = append(events, &v2.ExecuteEvent{Event: &v2.ExecuteEvent_Adapter{Adapter: f}})
	}
	client := &kb53Client{events: events}
	collector := &adapterEventCollector{}

	result, err := ExecuteViaClient(context.Background(), client, "copilot", "s1", true, kb53Step(), collector, nil)
	if err != nil {
		t.Fatalf("ExecuteViaClient: %v", err)
	}
	if result.Outcome != "ready_for_review" {
		t.Fatalf("outcome = %q, want ready_for_review", result.Outcome)
	}
	if got := result.Outputs["reason"].AsString(); got != "chunked payload reason" {
		t.Fatalf("reason output = %#v, want chunked reassembly", result.Outputs)
	}
}

func TestExecuteViaClient_KB53FinalizedSuccessWithDeniedPermissionBecomesNeedsReview(t *testing.T) {
	client := &kb53Client{
		events: []*v2.ExecuteEvent{
			kb53Event("permission.request", map[string]any{
				"request_id": "req-1", "tool": "browser", "full_command_text": "https://example.invalid",
			}),
			kb53FinalizedEvent("success", "asked to run"),
		},
	}
	collector := &adapterEventCollector{}

	// hasPermStream=false: the per-Execute fallback stream evaluates the host
	// policy locally; the request is denied (browser is not in allow_tools).
	result, err := ExecuteViaClient(context.Background(), client, "copilot", "s1", false, kb53Step(), collector, nil)
	if err != nil {
		t.Fatalf("ExecuteViaClient: %v", err)
	}
	if result.Outcome != "needs_review" {
		t.Fatalf("outcome = %q, want needs_review (a denied permission demotes success)", result.Outcome)
	}
}

func TestRecordFinalizedOutcomeLastWins(t *testing.T) {
	s := &executeCaptureSink{sink: &adapterEventCollector{}}
	if err := s.emitAdapter(&v2.AdapterEvent{EventKind: adapterEventFinalizedOutcome}); err != nil {
		t.Fatalf("emitAdapter (nil payload): %v", err)
	}
	if s.finalizedOutcome != "" {
		t.Fatalf("nil payload must not set an outcome, got %q", s.finalizedOutcome)
	}
	if err := s.emitAdapter(kb53FinalizedEvent("success", "first").GetAdapter()); err != nil {
		t.Fatalf("emitAdapter: %v", err)
	}
	if s.finalizedOutcome != "success" {
		t.Fatalf("outcome = %q, want success", s.finalizedOutcome)
	}
	if err := s.emitAdapter(kb53FinalizedEvent("ready_for_review", "second").GetAdapter()); err != nil {
		t.Fatalf("emitAdapter: %v", err)
	}
	if s.finalizedOutcome != "ready_for_review" {
		t.Fatalf("outcome = %q, want ready_for_review (last wins)", s.finalizedOutcome)
	}
	result, ok := s.rescueResult()
	if !ok || result.Outcome != "ready_for_review" {
		t.Fatalf("rescueResult = %+v %v, want ready_for_review", result, ok)
	}
	if got := result.Outputs["reason"].AsString(); got != "second" {
		t.Fatalf("reason = %#v, want \"second\"", result.Outputs["reason"])
	}
}

func TestRescueResultEmptySink(t *testing.T) {
	s := &executeCaptureSink{sink: &adapterEventCollector{}}
	if _, ok := s.rescueResult(); ok {
		t.Fatal("rescueResult must report no evidence on an idle sink")
	}
}

func TestRescueResultCapturedResultWins(t *testing.T) {
	s := &executeCaptureSink{sink: &adapterEventCollector{}}
	if err := s.emitAdapter(kb53FinalizedEvent("failure", "finalized first").GetAdapter()); err != nil {
		t.Fatalf("emitAdapter: %v", err)
	}
	if err := s.emitResult(&v2.ExecuteResult{Outcome: "ready_for_review"}); err != nil {
		t.Fatalf("emitResult: %v", err)
	}
	result, ok := s.rescueResult()
	if !ok {
		t.Fatal("rescueResult must hit when a result was captured")
	}
	if result.Outcome != "ready_for_review" {
		t.Fatalf("outcome = %q, want the captured result verbatim", result.Outcome)
	}
}

func TestRescueResultDoneWithoutOutcomeFallsBackToFinalized(t *testing.T) {
	s := &executeCaptureSink{sink: &adapterEventCollector{}}
	if err := s.emitAdapter(kb53FinalizedEvent("ready_for_review", "finalized").GetAdapter()); err != nil {
		t.Fatalf("emitAdapter: %v", err)
	}
	s.done = true // a result event with an empty outcome must not mask the finalized verdict
	result, ok := s.rescueResult()
	if !ok || result.Outcome != "ready_for_review" {
		t.Fatalf("rescueResult = %+v %v, want the finalized outcome", result, ok)
	}
}

func TestApplyNeedsReviewOverrideOnlyDemotesSuccess(t *testing.T) {
	s := &executeCaptureSink{sink: &adapterEventCollector{}, lastDecisionDenied: true}
	result := adapter.Result{Outcome: "ready_for_review"}
	s.applyNeedsReviewOverride(&result)
	if result.Outcome != "ready_for_review" {
		t.Fatalf("override must not touch non-success outcomes, got %q", result.Outcome)
	}
	result = adapter.Result{Outcome: "success"}
	s.applyNeedsReviewOverride(&result)
	if result.Outcome != "needs_review" {
		t.Fatalf("override must demote success, got %q", result.Outcome)
	}
}
