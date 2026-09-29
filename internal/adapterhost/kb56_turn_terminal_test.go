package adapterhost

// kb56_turn_terminal_test.go — regression tests for KB-56: an adapter-level
// outcome.finalized is turn-terminal. When the model keeps streaming past its
// submit_outcome tool invocation (the incident's 85 trailing message_delta
// events in run kb-54-1790714369), the host must end the Execute turn at the
// outcome and resolve the step to the submitted verdict instead of leaving
// the turn open until the stall watchdog tears the sessions down (which is
// how the incident dropped the develop step's ready_for_review and routed the
// workflow to comment_handler_failed).

import (
	"context"
	"testing"
	"time"

	structpb "google.golang.org/protobuf/types/known/structpb"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
)

const (
	// kb56StreamingPace is the inter-event pace of the "the model keeps
	// streaming its wrap-up summary" tail. Absent the KB-56 cut the tail
	// never ends on its own.
	kb56StreamingPace = 10 * time.Millisecond

	// kb56TurnTerminateBudget caps how long ExecuteViaClient may take to
	// return after the outcome landed (the workstream's "turn must terminate
	// within N ms of outcome submission"). Generous for loaded CI; without
	// the cut the stream runs for the whole test deadline (2s), so the
	// budget only has to separate "immediate" from "never".
	kb56TurnTerminateBudget = 500 * time.Millisecond

	// kb56TestDeadline is the caller deadline backing these tests: with the
	// fix the turn ends in microseconds; without it the stream is cancelled
	// here and the assertions fail cleanly instead of hanging.
	kb56TestDeadline = 2 * time.Second
)

// kb56ContinuingClient models an adapter turn that submits its outcome and
// then keeps streaming a wrap-up summary: after replaying `events` it emits
// message_delta events on a ticker until its Execute context is cancelled. It
// never sends a result event, replicating the incident shape at the
// ExecuteViaClient layer.
type kb56ContinuingClient struct {
	recordingClient

	events []*v2.ExecuteEvent

	// tailEvents > 0 caps the tail: that many deltas are streamed and then
	// Execute returns clean nil (models "the adapter gives up streaming").
	// 0 streams until the context is cancelled (models the incident).
	tailEvents int

	finalizeAt time.Time
	sentAfter  int // deltas forwarded after the scripted events
}

func (c *kb56ContinuingClient) Execute(ctx context.Context, req *v2.ExecuteRequest, sink ExecuteEventSink) error {
	c.lastExecuteReq = req
	finalizeSeen := false
	for _, ev := range c.events {
		if err := sink.Emit(ev); err != nil {
			return err
		}
		if ev.GetAdapter() != nil && ev.GetAdapter().GetEventKind() == adapterEventFinalizedOutcome {
			finalizeSeen = true
		}
	}
	if finalizeSeen {
		// The scripted events carry the submit; any cut fires while the last
		// of them is being emitted. The tail below must not run.
		c.finalizeAt = time.Now()
	}
	ticker := time.NewTicker(kb56StreamingPace)
	defer ticker.Stop()
	for i := 0; c.tailEvents == 0 || i < c.tailEvents; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		c.sentAfter++
		payload, _ := structpb.NewStruct(map[string]any{"text": "wrapping up", "n": i})
		if err := sink.Emit(&v2.ExecuteEvent{Event: &v2.ExecuteEvent_Adapter{
			Adapter: &v2.AdapterEvent{EventKind: "message.delta", Payload: payload},
		}}); err != nil {
			return err
		}
	}
	return nil
}

// runKB56FinalizedMidStream drives a finalize-then-continuing-stream turn
// through ExecuteViaClient with the given permission-stream mode and asserts
// the turn terminates at the outcome.
func runKB56FinalizedMidStream(t *testing.T, hasPermStream bool, client *kb56ContinuingClient) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), kb56TestDeadline)
	defer cancel()
	collector := &adapterEventCollector{}

	start := time.Now()
	result, err := ExecuteViaClient(ctx, client, "copilot", "s1", hasPermStream, kb53Step(), collector)
	returned := time.Since(start)

	if err != nil {
		t.Fatalf("ExecuteViaClient: %v (the submitted verdict must resolve the step; the continuing stream must not mask it)", err)
	}
	if result.Outcome != "ready_for_review" {
		t.Fatalf("outcome = %q, want ready_for_review", result.Outcome)
	}
	// Turn-terminal: the host must stop consuming at the outcome (KB-56) —
	// no wrap-up deltas were sent (the cut cancelled the stream before the
	// first tick) and ExecuteViaClient returned well within budget.
	if client.sentAfter != 0 {
		t.Errorf("tail stream forwarded %d post-outcome events, want 0 (the stream must end at the outcome)", client.sentAfter)
	}
	if client.finalizeAt.IsZero() {
		t.Fatalf("finalize event replay did not register; test wiring bug")
	}
	if latency := time.Since(client.finalizeAt); latency > kb56TurnTerminateBudget {
		t.Errorf("turn terminated %v after outcome submission, want <%v (outcome must be turn-terminal)", latency, kb56TurnTerminateBudget)
	}
	if returned > kb56TurnTerminateBudget {
		t.Errorf("ExecuteViaClient returned after %v, want <%v", returned, kb56TurnTerminateBudget)
	}
	if !collector.saw(adapterEventFinalizedOutcome) {
		t.Errorf("collector did not see the outcome.finalized event")
	}
}

func TestExecuteViaClient_KB56FinalizedOutcomeEndsContinuingStream(t *testing.T) {
	// Session-scoped permission stream (SessionManager path):
	// executeWithActiveStream.
	client := &kb56ContinuingClient{events: []*v2.ExecuteEvent{kb53FinalizedEvent("ready_for_review", "pr ready")}}
	runKB56FinalizedMidStream(t, true, client)
}

func TestExecuteViaClient_KB56FinalizedOutcomeEndsContinuingStreamFallback(t *testing.T) {
	// Fallback per-Execute permission stream (conformance/direct path):
	// executeWithFallbackStream.
	client := &kb56ContinuingClient{events: []*v2.ExecuteEvent{kb53FinalizedEvent("ready_for_review", "pr ready")}}
	runKB56FinalizedMidStream(t, false, client)
}

func TestExecuteViaClient_KB56ChunkedFinalizedOutcomeEndsContinuingStream(t *testing.T) {
	// A chunk-reassembled finalize (payload > wire fragment size) must cut
	// the continuing stream exactly like an inline one.
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
	client := &kb56ContinuingClient{events: events}
	runKB56FinalizedMidStream(t, true, client)
}

func TestExecuteViaClient_KB56NilPayloadFinalizeDoesNotEndTurn(t *testing.T) {
	// An outcome.finalized without a usable outcome carries no verdict: the
	// turn stays open (KB-53 already pins that it must not rescue). The
	// client stops streaming on its own here; with no recorded verdict the
	// stream is consumed and ends without a result.
	client := &kb56ContinuingClient{
		events:     []*v2.ExecuteEvent{kb53Event(adapterEventFinalizedOutcome, nil)},
		tailEvents: 3,
	}
	ctx, cancel := context.WithTimeout(context.Background(), kb56TestDeadline)
	defer cancel()
	collector := &adapterEventCollector{}

	result, err := ExecuteViaClient(ctx, client, "copilot", "s1", true, kb53Step(), collector)
	if err == nil {
		t.Fatal("expected the stream to end without a result: a nil-payload finalize carries no verdict")
	}
	if result.Outcome != "failure" {
		t.Fatalf("outcome = %q, want failure", result.Outcome)
	}
	if client.sentAfter == 0 {
		t.Error("tail stream must keep running when no verdict was recorded (sentAfter != 0 expected)")
	}
}

func TestCutStreamAtFinalizedFiresOnceOnRecordedVerdict(t *testing.T) {
	cancels := 0
	s := &executeCaptureSink{
		sink:            &adapterEventCollector{},
		onTurnFinalized: func() { cancels++ },
	}

	if err := s.emitAdapter(kb53FinalizedEvent("ready_for_review", "pr ready").GetAdapter()); err != nil {
		t.Fatalf("emitAdapter: %v", err)
	}
	if cancels != 1 {
		t.Fatalf("cut fired %d times after the first verdict, want exactly 1", cancels)
	}
	if !s.finalizeKilled {
		t.Error("finalizeKilled must be set after the cut")
	}

	// A later finalize re-records the verdict (last wins) but must not re-cut.
	if err := s.emitAdapter(kb53FinalizedEvent("failure", "changed mind").GetAdapter()); err != nil {
		t.Fatalf("emitAdapter: %v", err)
	}
	if cancels != 1 {
		t.Fatalf("cut fired again on a second finalize, want exactly 1, got %d", cancels)
	}
	if s.finalizedOutcome != "failure" {
		t.Fatalf("outcome = %q, want failure (last wins)", s.finalizedOutcome)
	}
}

func TestCutStreamAtFinalizedSkipsUnrecordablePayloads(t *testing.T) {
	cancels := 0
	s := &executeCaptureSink{
		sink:            &adapterEventCollector{},
		onTurnFinalized: func() { cancels++ },
	}

	// Nil payload: no verdict, no cut.
	if err := s.emitAdapter(&v2.AdapterEvent{EventKind: adapterEventFinalizedOutcome}); err != nil {
		t.Fatalf("emitAdapter (nil payload): %v", err)
	}
	// Outcome-less payload: no verdict, no cut.
	if err := s.emitAdapter(kb53Event(adapterEventFinalizedOutcome, map[string]any{"reason": "outcome missing"}).GetAdapter()); err != nil {
		t.Fatalf("emitAdapter (outcomeless): %v", err)
	}
	if cancels != 0 {
		t.Fatalf("cut fired %d times on unrecordable payloads, want 0", cancels)
	}
	if s.finalizeKilled {
		t.Error("finalizeKilled must stay false when no verdict was recorded")
	}
	if s.finalizedOutcome != "" {
		t.Fatalf("finalizedOutcome = %q, want empty", s.finalizedOutcome)
	}

	// No cancel hook (theoretical in-proc shape): recording must not panic.
	hookless := &executeCaptureSink{sink: &adapterEventCollector{}}
	if err := hookless.emitAdapter(kb53FinalizedEvent("success", "no hook").GetAdapter()); err != nil {
		t.Fatalf("emitAdapter: %v", err)
	}
	if hookless.finalizeKilled {
		t.Error("finalizeKilled must stay false without a cancel hook")
	}
}
