package adapterhost

// kb57_teardown_result_test.go — regression tests for KB-57: a fully
// delivered ExecuteResult must survive an engine- or run-initiated teardown
// that cancels the host context around the delivery, and a finalize-only
// verdict must stay behind the KB-56/CRI-275 live-context gate.
//
// Incident shape (castle run 4246be92 in run kb-55-1790727148): the shell
// step push_wip_checkpoint completed its work, the Execute call observed both
// the captured result and the cancelled host context, and the previously
// unconditional ctx-cancel gate turned the delivered verdict into a synthetic
// failure that the engine then dropped — step.entered was the last event
// before the sessions were torn down.

import (
	"context"
	"testing"

	structpb "google.golang.org/protobuf/types/known/structpb"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/workflow"
)

// kb57Step is a shell-bookkeeping-shaped step exercising the teardown paths.
func kb57Step() *workflow.StepNode {
	return &workflow.StepNode{Name: "push_wip_checkpoint", AllowTools: []string{"shell"}}
}

// kb57TeardownClient delivers the full ExecuteResult and then cancels the
// host context from inside Execute (the engine tearing the session's
// execution context down around the step), returning the cancellation error
// the transport would surface.
type kb57TeardownClient struct {
	recordingClient

	teardown func()
}

func (c *kb57TeardownClient) Execute(ctx context.Context, _ *v2.ExecuteRequest, sink ExecuteEventSink) error {
	if err := sink.Emit(&v2.ExecuteEvent{
		Event: &v2.ExecuteEvent_Result{
			Result: &v2.ExecuteResult{Outcome: "success"},
		},
	}); err != nil {
		return err
	}
	c.teardown()
	return ctx.Err()
}

// kb57TeardownFinalizeClient finalizes the turn at the adapter level without
// ever delivering a result event, then races the teardown. The finalize-only
// verdict must still lose to the CRI-275 teardown semantics.
type kb57TeardownFinalizeClient struct {
	recordingClient

	teardown func()
}

func (c *kb57TeardownFinalizeClient) Execute(ctx context.Context, _ *v2.ExecuteRequest, sink ExecuteEventSink) error {
	payload, _ := structpb.NewStruct(map[string]any{"outcome": "success", "reason": "work done"})
	if err := sink.Emit(&v2.ExecuteEvent{
		Event: &v2.ExecuteEvent_Adapter{
			Adapter: &v2.AdapterEvent{EventKind: "outcome.finalized", Payload: payload},
		},
	}); err != nil {
		return err
	}
	c.teardown()
	return ctx.Err()
}

// kb57DeliveredEmptyResultClient delivers a result event whose outcome is
// empty (an adapter contract violation) and then races the teardown.
type kb57DeliveredEmptyResultClient struct {
	recordingClient

	teardown func()
}

func (c *kb57DeliveredEmptyResultClient) Execute(ctx context.Context, _ *v2.ExecuteRequest, sink ExecuteEventSink) error {
	if err := sink.Emit(&v2.ExecuteEvent{
		Event: &v2.ExecuteEvent_Result{
			Result: &v2.ExecuteResult{Outcome: ""},
		},
	}); err != nil {
		return err
	}
	c.teardown()
	return ctx.Err()
}

// TestKB57_DeliveredResultSurvivesTeardown_ActiveStream: the shell step
// completed and its ExecuteResult was fully delivered; the host context was
// cancelled during the same window (session teardown). The adapter's verdict
// is completed work and must be reported — pre-KB-57 the gate discarded it
// for a synthetic failure, and the engine dropped the step outcome entirely.
func TestKB57_DeliveredResultSurvivesTeardown_ActiveStream(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := &kb57TeardownClient{teardown: cancel}
	result, err := ExecuteViaClient(parent, client, "shell", "s1", true, kb57Step(), &adapterEventCollector{}, nil)

	if err != nil {
		t.Fatalf("ExecuteViaClient: %v (a delivered ExecuteResult must survive the teardown race)", err)
	}
	if result.Outcome != "success" {
		t.Errorf("result.Outcome = %q, want the adapter's delivered %q", result.Outcome, "success")
	}
}

// TestKB57_DeliveredResultSurvivesTeardown_FallbackStream: same race on the
// per-Execute permission-stream path that bypasses SessionManager.
func TestKB57_DeliveredResultSurvivesTeardown_FallbackStream(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := &kb57TeardownClient{teardown: cancel}
	result, err := ExecuteViaClient(parent, client, "shell", "s1", false, kb57Step(), &adapterEventCollector{}, nil)

	if err != nil {
		t.Fatalf("ExecuteViaClient: %v (a delivered ExecuteResult must survive the teardown race)", err)
	}
	if result.Outcome != "success" {
		t.Errorf("result.Outcome = %q, want the adapter's delivered %q", result.Outcome, "success")
	}
}

// TestKB57_DeliveredResultWinsBeforeTeardown pins the clean-delivery path:
// the same client without the teardown must behave exactly as before (result
// returned, no rescue involved).
func TestKB57_DeliveredResultWinsBeforeTeardown(t *testing.T) {
	client := &kb57TeardownClient{teardown: func() {}}
	result, err := ExecuteViaClient(context.Background(), client, "shell", "s1", true, kb57Step(), &adapterEventCollector{}, nil)

	if err != nil {
		t.Fatalf("ExecuteViaClient: %v", err)
	}
	if result.Outcome != "success" {
		t.Errorf("result.Outcome = %q, want %q", result.Outcome, "success")
	}
}

// TestKB57_FinalizeOnlyVerdictUnderTeardownStaysSynthetic pins the KB-57
// boundary: an outcome.finalized event with NO delivered result must NOT be
// resurrected under an already-cancelled host context. The finalize cut
// (KB-56) and the CRI-275 step-ceiling teardown rely on the failure
// semantics; only fully delivered results survive the race.
func TestKB57_FinalizeOnlyVerdictUnderTeardownStaysSynthetic(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := &kb57TeardownFinalizeClient{teardown: cancel}
	result, err := ExecuteViaClient(parent, client, "shell", "s1", true, kb57Step(), &adapterEventCollector{}, nil)

	if err == nil {
		t.Fatalf("ExecuteViaClient: err = nil, want the teardown cancellation")
	}
	if result.Outcome != "failure" {
		t.Errorf("result.Outcome = %q, want synthetic %q (finalize-only verdicts need a live host context)", result.Outcome, "failure")
	}
}

// TestKB57_DeliveredEmptyResultUnderTeardownStaysSynthetic pins the
// done-without-outcome case: a delivered result with an empty outcome is not
// completed work and does not rescue, matching the KB-53 evidence rule.
func TestKB57_DeliveredEmptyResultUnderTeardownStaysSynthetic(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := &kb57DeliveredEmptyResultClient{teardown: cancel}
	result, err := ExecuteViaClient(parent, client, "shell", "s1", true, kb57Step(), &adapterEventCollector{}, nil)

	if err == nil {
		t.Fatalf("ExecuteViaClient: err = nil, want the teardown cancellation")
	}
	if result.Outcome != "failure" {
		t.Errorf("result.Outcome = %q, want synthetic %q (empty-outcome results are not evidence)", result.Outcome, "failure")
	}
}
