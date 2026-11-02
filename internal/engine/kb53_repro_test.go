package engine

// kb53_repro_test.go — regression test for KB-53: an adapter that resolves
// its turn at the adapter level (outcome.finalized) must produce a workflow-level
// step outcome instead of being dropped by an abnormal Execute stream, so the
// subsequent workflow pipeline runs.

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	structpb "google.golang.org/protobuf/types/known/structpb"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow/workflowtest"
)

var kb53Workflow = workflowtest.New().
	WithHeader("kb53", "0.1", "develop", "awaiting_human").
	WithAdapter("copilot", "default").
	WithAdapter("pipeline", "default").
	WithStep("develop",
		workflowtest.Target("adapter.copilot.default"),
		workflowtest.Outcome("ready_for_review", "step.create_pr"),
		workflowtest.Outcome("failure", "step.comment_handler_failed"),
	).
	WithStep("create_pr",
		workflowtest.Target("adapter.pipeline.default"),
		workflowtest.Outcome("success", "step.reviewer_loop"),
		workflowtest.Outcome("error", "step.reviewer_loop"),
	).
	WithStep("reviewer_loop",
		workflowtest.Target("adapter.pipeline.default"),
		workflowtest.Outcome("success", "step.set_review_state"),
		workflowtest.Outcome("changes_requested", "step.develop"),
		workflowtest.Outcome("error", "step.comment_handler_failed"),
	).
	WithStep("set_review_state",
		workflowtest.Target("adapter.pipeline.default"),
		workflowtest.Outcome("success", "state.awaiting_human"),
		workflowtest.Outcome("error", "state.awaiting_human"),
	).
	WithStep("comment_handler_failed",
		workflowtest.Target("adapter.pipeline.default"),
		workflowtest.Outcome("success", "state.awaiting_human"),
		workflowtest.Outcome("error", "state.awaiting_human"),
	).
	WithState("awaiting_human", true, nil).
	Build()

type finalizedTestSink struct {
	*fakeSink

	mu          sync.Mutex
	outcomes    []string
	transitions []string
	events      []string
}

func (s *finalizedTestSink) OnStepOutcome(step, outcome string, _ time.Duration, _ error, _ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outcomes = append(s.outcomes, step+"="+outcome)
}

func (s *finalizedTestSink) OnStepTransition(from, to, _ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.transitions = append(s.transitions, from+"->"+to)
}

func (s *finalizedTestSink) StepEventSink(string) adapter.EventSink {
	return &finalizedTestEventRecorder{parent: s}
}

type finalizedTestEventRecorder struct {
	parent *finalizedTestSink
}

func (r *finalizedTestEventRecorder) Log(string, []byte) {}

func (r *finalizedTestEventRecorder) Adapter(kind string, _ any) {
	r.parent.mu.Lock()
	defer r.parent.mu.Unlock()
	r.parent.events = append(r.parent.events, kind)
}

func (s *finalizedTestSink) snapshot() (outcomes, transitions, events []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.outcomes...),
		append([]string(nil), s.transitions...),
		append([]string(nil), s.events...)
}

type kb53CopilotSink = finalizedTestSink
type kb53CopilotEventRecorder = finalizedTestEventRecorder

type baseFakeClient struct{}

func (b *baseFakeClient) Info(context.Context, *v2.InfoRequest) (*v2.InfoResponse, error) {
	return &v2.InfoResponse{Name: "test", Version: "1.0.0"}, nil
}
func (b *baseFakeClient) OpenSession(context.Context, *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	return &v2.OpenSessionResponse{}, nil
}
func (b *baseFakeClient) CloseSession(context.Context, *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
	return &v2.CloseSessionResponse{}, nil
}
func (b *baseFakeClient) Log(context.Context, *v2.LogRequest, adapterhost.LogEventSink) error { return nil }
func (b *baseFakeClient) Permissions(context.Context, <-chan *v2.PermissionEvent) error       { return nil }
func (b *baseFakeClient) Prompt(context.Context, *adapterhost.PromptRequest) (*adapterhost.PromptResponse, error) {
	return &adapterhost.PromptResponse{}, nil
}
func (b *baseFakeClient) Pause(context.Context, *v2.PauseRequest) (*v2.PauseResponse, error) {
	return &v2.PauseResponse{}, nil
}
func (b *baseFakeClient) Resume(context.Context, *v2.ResumeRequest) (*v2.ResumeResponse, error) {
	return &v2.ResumeResponse{}, nil
}
func (b *baseFakeClient) Snapshot(context.Context, *v2.SnapshotRequest) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{}, nil
}
func (b *baseFakeClient) Restore(context.Context, *v2.RestoreRequest) (*v2.RestoreResponse, error) {
	return &v2.RestoreResponse{}, nil
}
func (b *baseFakeClient) Inspect(context.Context, *v2.InspectRequest) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}

type kb53FinalizingClient struct {
	baseFakeClient
	outcome   string
	streamErr error
}

func (c *kb53FinalizingClient) Execute(_ context.Context, _ *v2.ExecuteRequest, sink adapterhost.ExecuteEventSink) error {
	outcome := c.outcome
	if outcome == "" {
		outcome = "ready_for_review"
	}
	payload, _ := structpb.NewStruct(map[string]any{"outcome": outcome, "reason": "turn finalized"})
	if err := sink.Emit(&v2.ExecuteEvent{Event: &v2.ExecuteEvent_Adapter{
		Adapter: &v2.AdapterEvent{EventKind: "outcome.finalized", Payload: payload},
	}}); err != nil {
		return err
	}
	return c.streamErr
}

func TestKB53_FinalizedAdapterOutcomeProducesWorkflowLevelStepOutcome(t *testing.T) {
	client := &kb53FinalizingClient{outcome: "ready_for_review"}
	g := compile(t, kb53Workflow)
	sink := &finalizedTestSink{fakeSink: &fakeSink{}}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"copilot":          adapterhost.NewRPCHandle("copilot.default", nil, client),
		"copilot.default":  adapterhost.NewRPCHandle("copilot.default", nil, client),
		"pipeline":         &fakeAdapter{name: "pipeline", outcome: "success"},
		"pipeline.default": &fakeAdapter{name: "pipeline", outcome: "success"},
	}}

	if err := NewTestEngine(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "awaiting_human" || !sink.terminalOK {
		t.Errorf("terminal state %q success=%v; want awaiting_human/true", sink.terminal, sink.terminalOK)
	}

	outcomes, transitions, events := sink.snapshot()

	// The develop step MUST have a workflow-level step outcome matching the rescued verdict
	if !slices.Contains(outcomes, "develop=ready_for_review") {
		t.Fatalf("step outcomes = %v; want develop=ready_for_review", outcomes)
	}

	// The workflow transitions through the post-develop pipeline to awaiting_human
	for _, wantEdge := range []string{"develop->create_pr", "create_pr->reviewer_loop", "reviewer_loop->set_review_state", "set_review_state->awaiting_human"} {
		if !slices.Contains(transitions, wantEdge) {
			t.Errorf("transitions = %v; want edge %q", transitions, wantEdge)
		}
	}

	// The adapter-level outcome.finalized event was forwarded
	if !slices.Contains(events, "outcome.finalized") {
		t.Errorf("adapter events = %v; want outcome.finalized", events)
	}
	if sink.failure != "" {
		t.Errorf("OnRunFailed(%q); the run must complete successfully", sink.failure)
	}
}

func TestKB53_CopilotSessionLossAfterFinalizeStillYieldsOutcome(t *testing.T) {
	client := &kb53FinalizingClient{
		outcome:   "ready_for_review",
		streamErr: errors.New("rpc error: code = Canceled desc = transport connection closed"),
	}
	g := compile(t, kb53Workflow)
	sink := &finalizedTestSink{fakeSink: &fakeSink{}}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"copilot":          adapterhost.NewRPCHandle("copilot.default", nil, client),
		"copilot.default":  adapterhost.NewRPCHandle("copilot.default", nil, client),
		"pipeline":         &fakeAdapter{name: "pipeline", outcome: "success"},
		"pipeline.default": &fakeAdapter{name: "pipeline", outcome: "success"},
	}}

	if err := NewTestEngine(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "awaiting_human" || !sink.terminalOK {
		t.Errorf("terminal state %q success=%v; want awaiting_human/true", sink.terminal, sink.terminalOK)
	}
	outcomes, transitions, events := sink.snapshot()
	if !slices.Contains(outcomes, "develop=ready_for_review") {
		t.Fatalf("step outcomes = %v; want develop=ready_for_review despite the session loss", outcomes)
	}
	for _, wantEdge := range []string{"develop->create_pr", "create_pr->reviewer_loop", "reviewer_loop->set_review_state", "set_review_state->awaiting_human"} {
		if !slices.Contains(transitions, wantEdge) {
			t.Errorf("transitions = %v; want edge %q", transitions, wantEdge)
		}
	}
	if !slices.Contains(events, "outcome.finalized") {
		t.Errorf("adapter events = %v; want outcome.finalized", events)
	}
}
