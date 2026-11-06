// Step outcome and state finalization test suite.
// Unifies:
//   - KB-53: adapter-level outcome finalization produced workflow step outcome (kb53_repro_test.go)
//   - KB-56: mid-stream outcome finalization and turn cut on continued streaming (kb56_repro_test.go)
//   - KB-57: outcome preservation during teardown race & failure visibility (kb57_repro_test.go)
//   - KB-237: finalize comment propagation to require_comment evaluators (kb237_repro_test.go)
package engine

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	structpb "google.golang.org/protobuf/types/known/structpb"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow/workflowtest"
)

// -----------------------------------------------------------------------------
// KB-53: Adapter-Level Outcome Finalization
// -----------------------------------------------------------------------------

var kb53Workflow = workflowtest.New().
	WithHeader("kb53", "0.1", "develop", "need_assistance").
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
		workflowtest.Outcome("success", "state.need_assistance"),
		workflowtest.Outcome("error", "state.need_assistance"),
	).
	WithStep("comment_handler_failed",
		workflowtest.Target("adapter.pipeline.default"),
		workflowtest.Outcome("success", "state.need_assistance"),
		workflowtest.Outcome("error", "state.need_assistance"),
	).
	WithState("need_assistance", true, nil).
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

func (b *baseFakeClient) Log(context.Context, *v2.LogRequest, adapterhost.LogEventSink) error {
	return nil
}

func (b *baseFakeClient) Permissions(context.Context, <-chan *v2.PermissionEvent) error {
	return nil
}

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

// TestKB53_FinalizedAdapterOutcomeProducesWorkflowLevelStepOutcome pins KB-53:
// an adapter that resolves its turn at the adapter level (outcome.finalized) must
// produce a workflow-level step outcome instead of being dropped by an abnormal
// Execute stream, so the subsequent workflow pipeline runs.
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
	if sink.terminal != "need_assistance" || !sink.terminalOK {
		t.Errorf("terminal state %q success=%v; want need_assistance/true", sink.terminal, sink.terminalOK)
	}

	outcomes, transitions, events := sink.snapshot()

	// The develop step MUST have a workflow-level step outcome matching the rescued verdict
	if !slices.Contains(outcomes, "develop=ready_for_review") {
		t.Fatalf("step outcomes = %v; want develop=ready_for_review", outcomes)
	}

	// The workflow transitions through the post-develop pipeline to need_assistance
	for _, wantEdge := range []string{"develop->create_pr", "create_pr->reviewer_loop", "reviewer_loop->set_review_state", "set_review_state->need_assistance"} {
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
	if sink.terminal != "need_assistance" || !sink.terminalOK {
		t.Errorf("terminal state %q success=%v; want need_assistance/true", sink.terminal, sink.terminalOK)
	}
	outcomes, transitions, events := sink.snapshot()
	if !slices.Contains(outcomes, "develop=ready_for_review") {
		t.Fatalf("step outcomes = %v; want develop=ready_for_review despite the session loss", outcomes)
	}
	for _, wantEdge := range []string{"develop->create_pr", "create_pr->reviewer_loop", "reviewer_loop->set_review_state", "set_review_state->need_assistance"} {
		if !slices.Contains(transitions, wantEdge) {
			t.Errorf("transitions = %v; want edge %q", transitions, wantEdge)
		}
	}
	if !slices.Contains(events, "outcome.finalized") {
		t.Errorf("adapter events = %v; want outcome.finalized", events)
	}
}

// -----------------------------------------------------------------------------
// KB-56: Outcome Mid-Stream with Continued Streaming & Turn Cut
// -----------------------------------------------------------------------------

// kb56TurnTerminateBudget bounds how quickly the engine-level turn must end
// after the outcome lands.
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

// TestKB56_OutcomeMidStreamContinuedStreamingResolvesDevelopAndRunsNextStep
// pins KB-56: an adapter turn that resolves its outcome at the adapter level
// and then KEEPS STREAMING past the submit must resolve the develop step to
// the submitted verdict and continue the workflow, with the host ending the
// turn at the outcome so the stream's continuation cannot mask it.
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
	if sink.terminal != "need_assistance" || !sink.terminalOK {
		t.Errorf("terminal state %q success=%v; want need_assistance/true", sink.terminal, sink.terminalOK)
	}

	outcomes, transitions, events := sink.snapshot()

	found := false
	for _, o := range outcomes {
		if o == "develop=ready_for_review" {
			found = true
		}
	}
	if !found {
		t.Fatalf("step outcomes = %v; want develop=ready_for_review", outcomes)
	}

	joined := ""
	for _, tr := range transitions {
		joined += tr + " "
	}
	for _, wantEdge := range []string{"develop->create_pr", "create_pr->reviewer_loop", "reviewer_loop->set_review_state", "set_review_state->need_assistance"} {
		if !contains(joined, wantEdge) {
			t.Errorf("transitions %v; want edge %q", transitions, wantEdge)
		}
	}

	if client.tailSent != 0 {
		t.Errorf("tail stream forwarded %d post-outcome events, want 0 (the stream must end at the outcome)", client.tailSent)
	}
	if client.finalizeAt.IsZero() {
		t.Fatalf("the finalize event did not register; test wiring bug")
	}
	if latency := time.Since(client.finalizeAt); latency > kb56TurnTerminateBudget {
		t.Errorf("develop turn ended %v after outcome submission, want <%v", latency, kb56TurnTerminateBudget)
	}

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

// -----------------------------------------------------------------------------
// KB-57: Teardown Racing Step Completion & Failure Visibility
// -----------------------------------------------------------------------------

const kb57Workflow = `
workflow {
  name = "kb57"
  version = "0.1"
  initial_state = "develop"
  target_state  = "done"
}
step "develop" {
  target = adapter.copilot
  outcome "ready_for_review" { next = step.write_work_log }
  outcome "failure"          { next = step.comment_handler_failed }
}
step "write_work_log" {
  target = adapter.pipeline
  outcome "success" { next = step.commit_work_log }
  outcome "error" { next = step.commit_work_log }
}
step "commit_work_log" {
  target = adapter.pipeline
  outcome "success" { next = step.push_wip_checkpoint }
  outcome "error" { next = step.push_wip_checkpoint }
}
step "push_wip_checkpoint" {
  target = adapter.shell
  timeout = "40ms"
  outcome "success" { next = state.done }
  outcome "error" { next = state.done }
}
step "comment_handler_failed" {
  target = adapter.pipeline
  outcome "success" { next = state.done }
  outcome "error" { next = state.done }
}
state "done" {
  terminal = true
  success  = true
}
`

const kb57EmptyOutcomeWorkflow = `
workflow {
  name = "kb57-empty"
  version = "0.1"
  initial_state = "push_wip_checkpoint"
  target_state  = "done"
}
step "push_wip_checkpoint" {
  target = adapter.shell
  outcome "success" { next = state.done }
  outcome "error" { next = state.done }
}
state "done" {
  terminal = true
  success  = true
}
`

type kb57TeardownShellClient struct {
	kb53FinalizingClient
}

func (c *kb57TeardownShellClient) Execute(ctx context.Context, _ *v2.ExecuteRequest, sink adapterhost.ExecuteEventSink) error {
	select {
	case <-ctx.Done():
	case <-time.After(200 * time.Millisecond):
	}
	if err := sink.Emit(&v2.ExecuteEvent{
		Event: &v2.ExecuteEvent_Result{
			Result: &v2.ExecuteResult{Outcome: "success"},
		},
	}); err != nil {
		return err
	}
	return ctx.Err()
}

func kb57LoaderWithShell(client adapterhost.Client) *fakeLoader {
	return &fakeLoader{adapters: map[string]adapterhost.Handle{
		"copilot":          &fakeAdapter{name: "copilot", outcome: "ready_for_review"},
		"copilot.default":  &fakeAdapter{name: "copilot", outcome: "ready_for_review"},
		"pipeline":         &fakeAdapter{name: "pipeline", outcome: "success"},
		"pipeline.default": &fakeAdapter{name: "pipeline", outcome: "success"},
		"shell":            adapterhost.NewRPCHandle("shell.default", nil, client),
		"shell.default":    adapterhost.NewRPCHandle("shell.default", nil, client),
	}}
}

// TestKB57_TeardownRacingStepCompletionPreservesOutcome: a step whose Execute
// context (session teardown / step ceiling) is already cancelled when the
// result is delivered resolves to the adapter's verdict and the chain continues.
func TestKB57_TeardownRacingStepCompletionPreservesOutcome(t *testing.T) {
	g := compile(t, kb57Workflow)
	sink := &kb53CopilotSink{fakeSink: &fakeSink{}}

	if err := NewTestEngine(g, kb57LoaderWithShell(&kb57TeardownShellClient{}), sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v (the delivered ExecuteResult must survive the teardown race)", err)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Errorf("terminal state %q success=%v; want done/true", sink.terminal, sink.terminalOK)
	}

	outcomes, transitions, _ := sink.snapshot()

	found := false
	for _, o := range outcomes {
		if o == "push_wip_checkpoint=success" {
			found = true
		}
		if o == "push_wip_checkpoint=" || o == "push_wip_checkpoint=failure" {
			t.Errorf("step outcomes = %v; the teardown race must not erase or invert the delivered outcome", outcomes)
		}
	}
	if !found {
		t.Fatalf("step outcomes = %v; want push_wip_checkpoint=success", outcomes)
	}

	joined := ""
	for _, tr := range transitions {
		joined += tr + " "
	}
	for _, wantEdge := range []string{
		"develop->write_work_log",
		"write_work_log->commit_work_log",
		"commit_work_log->push_wip_checkpoint",
		"push_wip_checkpoint->done",
	} {
		if !contains(joined, wantEdge) {
			t.Errorf("transitions %v; want edge %q", transitions, wantEdge)
		}
	}

	if sink.failure != "" {
		t.Errorf("OnRunFailed(%q); the run must complete successfully once the outcome lands", sink.failure)
	}
}

// TestKB57_FailureWithoutDeclaredOutcomeIsVisible: a failed attempt for a
// step that declares no failure outcome must emit a WORKFLOW-LEVEL "failure"
// outcome, never an empty one.
func TestKB57_FailureWithoutDeclaredOutcomeIsVisible(t *testing.T) {
	g := compile(t, kb57EmptyOutcomeWorkflow)
	sink := &kb53CopilotSink{fakeSink: &fakeSink{}}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"shell":         &fakeAdapter{name: "shell", outcome: "success", err: errors.New("shell exit 1")},
		"shell.default": &fakeAdapter{name: "shell", outcome: "success", err: errors.New("shell exit 1")},
	}}

	if err := NewTestEngine(g, loader, sink).Run(context.Background()); err == nil {
		t.Fatalf("run: err = nil, want the attempt exhaustion error")
	}

	outcomes, _, _ := sink.snapshot()
	if len(outcomes) != 1 {
		t.Fatalf("step outcomes = %v; want exactly the push attempt outcome", outcomes)
	}
	if outcomes[0] != "push_wip_checkpoint=failure" {
		t.Errorf("step outcomes = %v; want push_wip_checkpoint=failure (an empty outcome is invisible to event consumers)", outcomes)
	}
}

// -----------------------------------------------------------------------------
// KB-237: Finalize Comment Propagation to require_comment Evaluator
// -----------------------------------------------------------------------------

const kb237Comment = "No-op resume: workstream complete, branch converges PR-ready"

const kb237Workflow = `
workflow {
  name          = "kb237"
  version       = "0.1"
  initial_state = "develop"
  target_state  = "need_assistance"

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
  outcome "failure"    { next = state.need_assistance }
  outcome "default"    { next = step.push_checkpoint }
}
step "push_checkpoint" {
  target     = adapter.pipeline.default
  max_visits = 3
  outcome "success" { next = step.develop }
  outcome "error" { next = step.develop }
}
step "create_pr" {
  target = adapter.pipeline.default
  outcome "success" { next = state.need_assistance }
  outcome "error" { next = state.need_assistance }
}
state "need_assistance" {
  terminal = true
  success  = true
}`

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

func kb237Loader(comment string) adapterhost.Loader {
	client := &kb237TurnClient{comment: comment}
	return &fakeLoader{adapters: map[string]adapterhost.Handle{
		"copilot":          adapterhost.NewRPCHandle("copilot.default", nil, client),
		"copilot.default":  adapterhost.NewRPCHandle("copilot.default", nil, client),
		"pipeline":         &fakeAdapter{name: "pipeline", outcome: "success"},
		"pipeline.default": &fakeAdapter{name: "pipeline", outcome: "success"},
	}}
}

// TestKB237_NoOpResumeFinalizesReadyForReviewWithoutCheckpoint: the finalize
// comment the agent supplies to the copilot submit_outcome tool reaches the
// engine's require_comment evaluator.
func TestKB237_NoOpResumeFinalizesReadyForReviewWithoutCheckpoint(t *testing.T) {
	g := compile(t, kb237Workflow)
	sink := &contractSink{}

	err := NewTestEngine(g, kb237Loader(kb237Comment), sink).Run(context.Background())

	require.NoError(t, err, "the accepted submit's comment must satisfy require_comment on the first visit")
	require.Equal(t, "need_assistance", sink.terminal)
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

// TestKB237_SwallowedCommentBurnsVisitsThenDefaults: negative control proving
// that without a comment, require_comment bounces and burns max_visits.
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
// outcome fails the run.
func TestKB237_SwallowedCommentExhaustionStillFailsRun(t *testing.T) {
	g := compile(t, `
workflow {
  name          = "kb237-nodefault"
  version       = "0.1"
  initial_state = "develop"
  target_state  = "need_assistance"

  policy { max_step_retries = 1 }
}
adapter "copilot" "default" {}
step "develop" {
  target = adapter.copilot.default
  outcome "ready_for_review" {
    require_comment = true
    next            = state.need_assistance
  }
  outcome "error" { next = state.need_assistance }
}
state "need_assistance" {
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
// continue-past-submit variant at the engine boundary.
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
	require.Equal(t, "need_assistance", sink.terminal)
	require.True(t, sink.terminalOK)
	entered := 0
	for _, s := range sink.stepsRun {
		if s == "push_checkpoint" {
			entered++
		}
	}
	require.Zero(t, entered)
}

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
