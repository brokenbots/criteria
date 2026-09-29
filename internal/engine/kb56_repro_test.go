package engine

// kb56_repro_test.go — regression test for KB-56: an adapter turn that
// resolves its outcome at the adapter level (outcome.finalized, the copilot
// submit_outcome tool) and then KEEPS STREAMING past the submit must resolve
// the develop step to the submitted verdict and continue the workflow, with
// the host ending the turn at the outcome so the stream's continuation cannot
// mask it.
//
// Incident shape (castle run b1f42cd7 in run kb-54-1790714369): the develop
// turn invoked the outcome tool (seq 1172/1173), outcome.finalized
// ready_for_review landed (seq 1174), then 85 more message_delta events
// streamed (seq 1175..1259) while the turn stayed open on the host. The stall
// watchdog eventually tore the sessions down, the develop step resolved
// failure via comment_handler_failed, and the post-develop pipeline (create_pr,
// reviewer loop, set_review_state) never ran. The KB-53 rescue only covers an
// Execute stream that ENDS abnormally; this test covers a stream that
// CONTINUES after the verdict and pins the turn-terminal cut engine-side.

import (
	"context"
	"testing"
	"time"

	structpb "google.golang.org/protobuf/types/known/structpb"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapterhost"
)

// kb56TurnTerminateBudget bounds how quickly the engine-level turn must end
// after the outcome lands. The turn-terminal cut at the ExecuteViaClient
// layer makes this microseconds; the budget is only generous enough for
// session/rpc scheduling on loaded CI. Without the cut the turn runs until
// the test deadline (15s), failing these assertions cleanly.
const kb56TurnTerminateBudget = 2 * time.Second

// kb56ContinuingTurnClient is the copilot-shaped client for the incident: the
// turn submits its outcome mid-stream and then keeps generating wrap-up
// deltas until the host ends the turn at the submitted outcome.
type kb56ContinuingTurnClient struct {
	kb53FinalizingClient

	finalizeAt time.Time
	tailSent   int
}

func (c *kb56ContinuingTurnClient) Execute(ctx context.Context, _ *v2.ExecuteRequest, sink adapterhost.ExecuteEventSink) error {
	payload, _ := structpb.NewStruct(map[string]any{"outcome": "ready_for_review", "reason": "pr ready"})
	if err := sink.Emit(&v2.ExecuteEvent{Event: &v2.ExecuteEvent_Adapter{
		Adapter: &v2.AdapterEvent{EventKind: "outcome.finalized", Payload: payload},
	}}); err != nil {
		return err
	}
	c.finalizeAt = time.Now()

	// The model keeps streaming past its submit: deltas flow until the host
	// ends the turn at the outcome (KB-56 cut). No result event ever arrives.
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for i := 0; ; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		c.tailSent++
		delta, _ := structpb.NewStruct(map[string]any{"text": "wrapping up", "n": i})
		if err := sink.Emit(&v2.ExecuteEvent{Event: &v2.ExecuteEvent_Adapter{
			Adapter: &v2.AdapterEvent{EventKind: "message.delta", Payload: delta},
		}}); err != nil {
			return err
		}
	}
}

func TestKB56_OutcomeMidStreamContinuedStreamingResolvesDevelopAndRunsNextStep(t *testing.T) {
	client := &kb56ContinuingTurnClient{}
	g := compile(t, kb53Workflow)
	sink := &kb53CopilotSink{fakeSink: &fakeSink{}}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"copilot":          adapterhost.NewRPCHandle("copilot.default", nil, client),
		"copilot.default":  adapterhost.NewRPCHandle("copilot.default", nil, client),
		"pipeline":         &fakeAdapter{name: "pipeline", outcome: "success"},
		"pipeline.default": &fakeAdapter{name: "pipeline", outcome: "success"},
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := NewTestEngine(g, loader, sink).Run(ctx); err != nil {
		t.Fatalf("run: %v (the submitted verdict must end the turn; the streaming tail must not mask it)", err)
	}
	if sink.terminal != "awaiting_human" || !sink.terminalOK {
		t.Errorf("terminal state %q success=%v; want awaiting_human/true", sink.terminal, sink.terminalOK)
	}

	outcomes, transitions, events := sink.snapshot()

	// The develop step resolved to the submitted verdict — in the incident it
	// resolved failure via comment_handler_failed and the pipeline never ran.
	found := false
	for _, o := range outcomes {
		if o == "develop=ready_for_review" {
			found = true
		}
	}
	if !found {
		t.Fatalf("step outcomes = %v; want develop=ready_for_review", outcomes)
	}

	// The next workflow steps ran: the full post-develop pipeline to terminal.
	joined := ""
	for _, tr := range transitions {
		joined += tr + " "
	}
	for _, wantEdge := range []string{"develop->create_pr", "create_pr->reviewer_loop", "reviewer_loop->set_review_state", "set_review_state->awaiting_human"} {
		if !contains(joined, wantEdge) {
			t.Errorf("transitions %v; want edge %q", transitions, wantEdge)
		}
	}

	// The turn terminated at the outcome: no wrap-up deltas were consumed and
	// the turn ended promptly after outcome.finalized.
	if client.tailSent != 0 {
		t.Errorf("tail stream forwarded %d post-outcome events, want 0 (the stream must end at the outcome)", client.tailSent)
	}
	if client.finalizeAt.IsZero() {
		t.Fatalf("the finalize event did not register; test wiring bug")
	}
	if latency := time.Since(client.finalizeAt); latency > kb56TurnTerminateBudget {
		t.Errorf("develop turn ended %v after outcome submission, want <%v", latency, kb56TurnTerminateBudget)
	}

	// The finalized verdict stayed visible on the step event sink, and no
	// tail delta was forwarded to the event stream.
	seenFinalized := false
	for _, ev := range events {
		switch ev {
		case "outcome.finalized":
			seenFinalized = true
		case "message.delta":
			t.Errorf("a post-outcome message.delta reached the event stream: %v", events)
		}
	}
	if !seenFinalized {
		t.Errorf("adapter events = %v; want outcome.finalized", events)
	}

	if sink.failure != "" {
		t.Errorf("OnRunFailed(%q); the run must complete successfully on the submitted verdict", sink.failure)
	}
}
