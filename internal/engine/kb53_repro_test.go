package engine

// kb53_repro_test.go — regression test for KB-53: an adapter that resolves
// its turn at the adapter level (outcome.finalized, the copilot
// submit_outcome tool with the KB-42 guard) must produce a workflow-level
// step outcome instead of being dropped by a broken Execute stream, so the
// post-develop pipeline (create_pr, reviewer loop, set_review_state) runs.
//
// Incident shape (castle run kb-41-1790653828): the develop turn finalized
// ready_for_review at the adapter level, the Execute stream ended without a
// result event, and the run routed develop → comment_handler_failed; no
// create_pr, no reviewer loop, and no step.outcome event after step.entered.
// The engine now receives the adapter's own verdict because the loader
// rescues it (internal/adapterhost); this test drives a copilot-shaped v2
// client through NewRPCHandle so the full SessionManager → rpcHandle →
// ExecuteViaClient path is exercised end-to-end, and additionally pins the
// engine-side behavior when the handle surfaces the rescued verdict.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	structpb "google.golang.org/protobuf/types/known/structpb"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
)

// kb53CopilotCrashErr is the copilot session-loss signature from the KB-41/53
// incident (develop session shim, "grpc: the client connection is closing").
const kb53CopilotCrashErr = "rpc error: code = Canceled desc = grpc: the client connection is closing"

// kb53Workflow mirrors the castle workflow tail: develop resolves its
// outcome, the post-develop pipeline runs (create_pr, reviewer_loop,
// set_review_state), and a tail comment step exists on the failure branch for
// parity with the incident's bookkeeping path.
const kb53Workflow = `
workflow {
  name = "kb53"
  version = "0.1"
  initial_state = "develop"
  target_state  = "awaiting_human"
}
step "develop" {
  target = adapter.copilot
  outcome "ready_for_review" { next = step.create_pr }
  outcome "failure"          { next = step.comment_handler_failed }
}
step "create_pr" {
  target = adapter.pipeline
  outcome "success" { next = step.reviewer_loop }
}
step "reviewer_loop" {
  target = adapter.pipeline
  outcome "success"          { next = step.set_review_state }
  outcome "changes_requested" { next = step.develop }
}
step "set_review_state" {
  target = adapter.pipeline
  outcome "success" { next = state.awaiting_human }
}
step "comment_handler_failed" {
  target = adapter.pipeline
  outcome "success" { next = state.awaiting_human }
}
state "awaiting_human" {
  terminal = true
  success  = true
}`

// kb53CopilotSink records step outcomes and transitions (fakeSink ignores
// them) plus adapter events, so the test can assert the develop step's
// workflow-level outcome exists and the finalized adapter event is visible.
type kb53CopilotSink struct {
	*fakeSink

	mu          sync.Mutex
	outcomes    []string
	transitions []string
	events      []string
}

func (s *kb53CopilotSink) OnStepOutcome(step, outcome string, _ time.Duration, _ error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outcomes = append(s.outcomes, step+"="+outcome)
}

func (s *kb53CopilotSink) OnStepTransition(from, to, via string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.transitions = append(s.transitions, from+"->"+to)
}

func (s *kb53CopilotSink) StepEventSink(string) adapter.EventSink {
	return &kb53CopilotEventRecorder{parent: s}
}

type kb53CopilotEventRecorder struct {
	parent *kb53CopilotSink
}

func (r *kb53CopilotEventRecorder) Log(string, []byte) {}

func (r *kb53CopilotEventRecorder) Adapter(kind string, _ any) {
	r.parent.mu.Lock()
	defer r.parent.mu.Unlock()
	r.parent.events = append(r.parent.events, kind)
}

func (s *kb53CopilotSink) snapshot() (outcomes, transitions, events []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.outcomes...),
		append([]string(nil), s.transitions...),
		append([]string(nil), s.events...)
}

// kb53FinalizingClient is the copilot-shaped v2 client for the incident: the
// turn resolves its outcome at the adapter level mid-turn
// (outcome.finalized), then the Execute stream ends abnormally without a
// result event (session shim lost after the long develop turn).
type kb53FinalizingClient struct{}

func (c *kb53FinalizingClient) Info(_ context.Context, _ *v2.InfoRequest) (*v2.InfoResponse, error) {
	return &v2.InfoResponse{Name: "copilot", Version: "0.5.9-kb42"}, nil
}

func (c *kb53FinalizingClient) OpenSession(_ context.Context, _ *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	return &v2.OpenSessionResponse{}, nil
}

func (c *kb53FinalizingClient) Execute(_ context.Context, _ *v2.ExecuteRequest, sink adapterhost.ExecuteEventSink) error {
	payload, _ := structpb.NewStruct(map[string]any{"outcome": "ready_for_review", "reason": "pr ready"})
	if err := sink.Emit(&v2.ExecuteEvent{Event: &v2.ExecuteEvent_Adapter{
		Adapter: &v2.AdapterEvent{EventKind: "outcome.finalized", Payload: payload},
	}}); err != nil {
		return err
	}
	// The stream ends abnormally: the copilot session shim dies right after
	// the turn settled and no result event is delivered.
	return nil
}

func (c *kb53FinalizingClient) Log(_ context.Context, _ *v2.LogRequest, _ adapterhost.LogEventSink) error {
	return nil
}

func (c *kb53FinalizingClient) Permissions(_ context.Context, _ <-chan *v2.PermissionEvent) error {
	return nil
}

func (c *kb53FinalizingClient) CloseSession(_ context.Context, _ *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
	return &v2.CloseSessionResponse{}, nil
}

func (c *kb53FinalizingClient) Prompt(_ context.Context, _ *adapterhost.PromptRequest) (*adapterhost.PromptResponse, error) {
	return &adapterhost.PromptResponse{}, nil
}

func (c *kb53FinalizingClient) Pause(_ context.Context, _ *v2.PauseRequest) (*v2.PauseResponse, error) {
	return &v2.PauseResponse{}, nil
}

func (c *kb53FinalizingClient) Resume(_ context.Context, _ *v2.ResumeRequest) (*v2.ResumeResponse, error) {
	return &v2.ResumeResponse{}, nil
}

func (c *kb53FinalizingClient) Snapshot(_ context.Context, _ *v2.SnapshotRequest) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{}, nil
}

func (c *kb53FinalizingClient) Restore(_ context.Context, _ *v2.RestoreRequest) (*v2.RestoreResponse, error) {
	return &v2.RestoreResponse{}, nil
}

func (c *kb53FinalizingClient) Inspect(_ context.Context, _ *v2.InspectRequest) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}

// kb53Loader wires the copilot RPC handle (full SessionManager/rpcHandle
// path) and the plain pipeline success adapter for the post-develop steps.
func kb53Loader() adapterhost.Loader {
	return &fakeLoader{adapters: map[string]adapterhost.Handle{
		"copilot":          adapterhost.NewRPCHandle("copilot.default", nil, &kb53FinalizingClient{}),
		"copilot.default":  adapterhost.NewRPCHandle("copilot.default", nil, &kb53FinalizingClient{}),
		"pipeline":         &fakeAdapter{name: "pipeline", outcome: "success"},
		"pipeline.default": &fakeAdapter{name: "pipeline", outcome: "success"},
	}}
}

func TestKB53_FinalizedAdapterOutcomeProducesWorkflowLevelStepOutcome(t *testing.T) {
	g := compile(t, kb53Workflow)
	sink := &kb53CopilotSink{fakeSink: &fakeSink{}}

	if err := NewTestEngine(g, kb53Loader(), sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "awaiting_human" || !sink.terminalOK {
		t.Errorf("terminal state %q success=%v; want awaiting_human/true", sink.terminal, sink.terminalOK)
	}

	outcomes, transitions, events := sink.snapshot()

	// The develop step MUST have a workflow-level step outcome — the incident
	// saw none after step.entered develop.
	found := false
	for _, o := range outcomes {
		if o == "develop=ready_for_review" {
			found = true
		}
	}
	if !found {
		t.Fatalf("step outcomes = %v; want develop=ready_for_review", outcomes)
	}

	// The post-develop pipeline must run: create_pr, reviewer loop, and the
	// review-state write, then the terminal state.
	joined := ""
	for _, tr := range transitions {
		joined += tr + " "
	}
	for _, wantEdge := range []string{"develop->create_pr", "create_pr->reviewer_loop", "reviewer_loop->set_review_state", "set_review_state->awaiting_human"} {
		if !contains(joined, wantEdge) {
			t.Errorf("transitions %v; want edge %q", transitions, wantEdge)
		}
	}

	// The adapter-level outcome.finalized event was emitted by the turn and
	// stays visible on the step's event sink.
	seenFinalized := false
	for _, ev := range events {
		if ev == "outcome.finalized" {
			seenFinalized = true
		}
	}
	if !seenFinalized {
		t.Errorf("adapter events = %v; want outcome.finalized", events)
	}

	// The failure branch bookkeeping must NOT have run: the verdict was
	// ready_for_review.
	if sink.failure != "" {
		t.Errorf("OnRunFailed(%q); the run must complete successfully", sink.failure)
	}
}

func TestKB53_CopilotSessionLossAfterFinalizeStillYieldsOutcome(t *testing.T) {
	// Same incident, with the transport death surfacing as the copilot crash
	// error: the loader still rescues the finalized verdict.
	client := &kb53CrashingFinalizingClient{}
	g := compile(t, kb53Workflow)
	sink := &kb53CopilotSink{fakeSink: &fakeSink{}}
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
	outcomes, _, events := sink.snapshot()
	found := false
	for _, o := range outcomes {
		if o == "develop=ready_for_review" {
			found = true
		}
	}
	if !found {
		t.Fatalf("step outcomes = %v; want develop=ready_for_review despite the session loss", outcomes)
	}
	if !contains(eventsJoined(events), "outcome.finalized") {
		t.Errorf("adapter events = %v; want outcome.finalized", events)
	}
}

// kb53CrashingFinalizingClient finalizes the turn at the adapter level and
// then dies with the exact copilot crash error.
type kb53CrashingFinalizingClient struct {
	kb53FinalizingClient
}

func (c *kb53CrashingFinalizingClient) Execute(_ context.Context, _ *v2.ExecuteRequest, sink adapterhost.ExecuteEventSink) error {
	payload, _ := structpb.NewStruct(map[string]any{"outcome": "ready_for_review", "reason": "pr ready"})
	if err := sink.Emit(&v2.ExecuteEvent{Event: &v2.ExecuteEvent_Adapter{
		Adapter: &v2.AdapterEvent{EventKind: "outcome.finalized", Payload: payload},
	}}); err != nil {
		return err
	}
	return errors.New(kb53CopilotCrashErr)
}

func eventsJoined(events []string) string {
	joined := ""
	for _, ev := range events {
		joined += ev + " "
	}
	return joined
}
