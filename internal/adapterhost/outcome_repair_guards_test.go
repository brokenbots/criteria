package adapterhost

// KB-45 repair-loop guards surfaced by the PR #496 contract review:
//
//  1. A non-chunked wire result whose outputs_json decodeOutputsJSON rejects
//     on a contract-bearing step must settle the captured verdict (like the
//     chunked path already does), so the pinned evaluator sees the verbatim
//     payload and issues the payload_schema issue instead of the fallback lane
//     silently synthesizing the fallback outcome.
//  2. A host-synthesized fallback verdict is validated at synthesis; the
//     session-level re-validation (SessionManager.execute and the respawn
//     retry) must skip it instead of applying the fallback contract's own
//     schema/require_comment to the empty payload the engine itself invented.

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/workflow"
)

// kb45UndecodablePayload is valid JSON but not an object: the pinned
// evaluator vocabulary keys the rejection as
// "payload_schema: outputs_json does not decode to a JSON object".
var kb45UndecodablePayload = []byte(`[]`)

const kb45PinnedNonObjectIssue = "payload_schema: outputs_json does not decode to a JSON object"

func kb45ResultEvent(outcome, comment string, outputsJSON []byte) *v2.ExecuteEvent {
	return &v2.ExecuteEvent{
		Event: &v2.ExecuteEvent_Result{
			Result: &v2.ExecuteResult{Outcome: outcome, Comment: comment, OutputsJson: outputsJSON},
		},
	}
}

// kb45ReplayClient replays a scripted Execute event stream and then returns
// execErr, with no other session activity.
type kb45ReplayClient struct {
	recordingClient

	events  []*v2.ExecuteEvent
	execErr error
}

func (c *kb45ReplayClient) Execute(_ context.Context, req *v2.ExecuteRequest, sink ExecuteEventSink) error {
	c.lastExecuteReq = req
	for _, ev := range c.events {
		if err := sink.Emit(ev); err != nil {
			return err
		}
	}
	return c.execErr
}

func (c *kb45ReplayClient) Log(_ context.Context, _ *v2.LogRequest, _ LogEventSink) error {
	return io.EOF
}

// TestExecuteViaClient_UndecodablePayloadOnContractStepSurfacesPinnedIssue
// pins the non-chunked capture: a delivered ExecuteResult whose outputs_json
// is not a JSON object ends with the captured verdict (done=true), so the
// contract evaluator issues the pinned payload_schema issue against the
// verbatim bytes.
func TestExecuteViaClient_UndecodablePayloadOnContractStepSurfacesPinnedIssue(t *testing.T) {
	client := &kb45ReplayClient{
		events: []*v2.ExecuteEvent{kb45ResultEvent("success", "shipped", kb45UndecodablePayload)},
	}

	result, err := ExecuteViaClient(context.Background(), client, "fake", "s1", true, contractStep(), &adapterEventCollector{}, nil)

	require.Equal(t, adapter.Result{}, result, "an invalid delivered payload must not resolve to any verdict")
	var invErr *OutcomeInvalidError
	require.True(t, errors.As(err, &invErr), "ExecuteViaClient err = %v, want *OutcomeInvalidError from contract validation", err)
	require.Equal(t, "success", invErr.Outcome)
	require.Contains(t, invErr.Issues, kb45PinnedNonObjectIssue)
}

// TestExecuteViaClient_FallbackDoesNotFireOnUndecodablePayload pins that a
// declared fallback contract never rescue-synthesizes over an invalid
// delivered payload: the invalid verdict still routes to the repair loop,
// never to the fallback outcome.
func TestExecuteViaClient_FallbackDoesNotFireOnUndecodablePayload(t *testing.T) {
	step := &workflow.StepNode{
		Name: "ship",
		Outcomes: map[string]*workflow.CompiledOutcome{
			"success": {Name: "success", Next: "done", Schema: auditType(), SchemaJSON: auditSchema()},
			"failed": {
				Name: "failed",
				// The fallback contract carries schema + require_comment on
				// purpose: synthesis has no payload, so acceptance here must
				// never come from the fallback lane for a delivered verdict.
				Schema:         auditType(),
				SchemaJSON:     auditSchema(),
				RequireComment: true,
				Fallback:       true,
			},
		},
	}
	client := &kb45ReplayClient{
		events: []*v2.ExecuteEvent{kb45ResultEvent("success", "shipped", kb45UndecodablePayload)},
	}

	result, err := ExecuteViaClient(context.Background(), client, "fake", "s1", true, step, &adapterEventCollector{}, nil)

	require.Equal(t, adapter.Result{}, result, "an invalid delivered payload must not resolve to any verdict")
	var invErr *OutcomeInvalidError
	require.True(t, errors.As(err, &invErr), "err = %v, want the pinned payload_schema rejection (fallback must not fire)", err)
	require.Equal(t, "success", invErr.Outcome)
	require.Contains(t, invErr.Issues, kb45PinnedNonObjectIssue)
}

// kb45FallbackStep declares a schema+require_comment success outcome and a
// fallback failure outcome that is itself contract-armed: the synthesized
// verdict {outcome: failed} has no payload, so any second validation pass
// applying the fallback contract is unsatisfiable by construction.
func kb45FallbackFallbackStep() *workflow.StepNode {
	return &workflow.StepNode{
		Name: "ship",
		Outcomes: map[string]*workflow.CompiledOutcome{
			"success": {Name: "success", Next: "done", Schema: auditType(), SchemaJSON: auditSchema()},
			"failed": {
				Name:           "failed",
				SchemaJSON:     auditSchema(),
				Schema:         auditType(),
				RequireComment: true,
				Fallback:       true,
			},
		},
	}
}

// kb45NeverFinalizingClient delivers a clean stream end without any result
// event: a contract-bearing attempt enters the fallback lane.
type kb45NeverFinalizingClient struct {
	recordingClient
}

func (c *kb45NeverFinalizingClient) Execute(_ context.Context, _ *v2.ExecuteRequest, _ ExecuteEventSink) error {
	return nil
}

func (c *kb45NeverFinalizingClient) Log(_ context.Context, _ *v2.LogRequest, _ LogEventSink) error {
	return io.EOF
}

// TestSessionManager_SynthesizedFallbackSkipsContractRecheck pins the direct
// execute path: the handle synthesized the fallback verdict (validated at
// synthesis, zero results + fallback contract), and the second local
// validation pass must not reject the empty synthesis against the fallback's
// own schema/require_comment.
func TestSessionManager_SynthesizedFallbackSkipsContractRecheck(t *testing.T) {
	sm := &SessionManager{sessions: map[string]*Session{}}
	handle := NewRPCHandle("fake", nil, &kb45NeverFinalizingClient{})
	sess := &Session{Name: "fake.default", Adapter: "fake", handle: handle}
	sm.mu.Lock()
	sm.sessions["fake.default"] = sess
	sm.mu.Unlock()

	coll := &adapterEventCollector{}
	result, err := sm.Execute(context.Background(), "fake.default", kb45FallbackFallbackStep(), coll, nil)

	require.NoError(t, err, "the host-synthesized fallback must not be re-validated against its own contract")
	require.Equal(t, "failed", result.Outcome)
	require.Empty(t, result.Comment)
	require.Empty(t, result.Outputs, "the synthesized fallback carries no payload")
	require.True(t, result.SynthesizedFallback, "the result must be marked as the host synthesis")
	require.False(t, sess.crashed.Load())
}

// kb45ReplacementLoader hands the replacement generation's handle on every
// Resolve: the session executes on a pre-bound crashing gen1 handle, and the
// loader is only consulted by the respawn machinery.
type kb45ReplacementLoader struct {
	mu       sync.Mutex
	resolves int
}

func (l *kb45ReplacementLoader) Resolve(_ context.Context, _ string) (Handle, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.resolves++
	return NewRPCHandle("fake", nil, &kb45NeverFinalizingClient{}), nil
}

func (l *kb45ReplacementLoader) Shutdown(context.Context) error { return nil }

// kb45CrashingClient reproduces the CRI-271 transport-close crash on Execute.
type kb45CrashingClient struct {
	recordingClient
}

func (c *kb45CrashingClient) Execute(context.Context, *v2.ExecuteRequest, ExecuteEventSink) error {
	return cri271CrashErr
}

func (c *kb45CrashingClient) Log(_ context.Context, _ *v2.LogRequest, _ LogEventSink) error {
	return io.EOF
}

// TestSessionManager_SynthesizedFallbackSkipsRecheckAfterRespawn pins the
// respawn retry path: the crash respawn re-runs the step on the replacement
// generation, the replacement never finalizes, the synthesized fallback is
// accepted, and the second local validation pass (which previously applied
// the fallback's schema/require_comment to the empty synthesis and exhausted
// the step's retries) does not run against the synthesized verdict.
func TestSessionManager_SynthesizedFallbackSkipsRecheckAfterRespawn(t *testing.T) {
	loader := &kb45ReplacementLoader{}
	sm := &SessionManager{loader: loader, sessions: map[string]*Session{}}
	sess := &Session{
		Name:    "fake.default",
		Adapter: "fake",
		// Gen1 dies with the CRI-271 transport signature on Execute; the
		// crash machinery resolves the replacement from the loader.
		handle:  NewRPCHandle("fake", nil, &kb45CrashingClient{}),
		OnCrash: OnCrashRespawn,
	}
	sm.mu.Lock()
	sm.sessions["fake.default"] = sess
	sm.mu.Unlock()

	coll := &adapterEventCollector{}
	result, err := sm.Execute(context.Background(), "fake.default", kb45FallbackFallbackStep(), coll, nil)

	require.NoError(t, err, "the respawn retry's synthesized fallback must not be re-validated")
	require.Equal(t, "failed", result.Outcome)
	require.True(t, result.SynthesizedFallback)
	require.Empty(t, result.Outputs)

	// The crash path ran through exactly one respawn: the loader handed out
	// the replacement generation and the session recovered. Gen1 was
	// pre-bound, so Resolve is consulted only by the respawn machinery.
	loader.mu.Lock()
	resolves := loader.resolves
	loader.mu.Unlock()
	require.Equal(t, 1, resolves, "one respawn must resolve one replacement handle")

	events := coll.snapshot()
	var respawned bool
	for _, ev := range events {
		if ev.kind == "session.respawned" {
			respawned = true
		}
	}
	require.True(t, respawned, "the respawn path must emit session.respawned")
}

// TestExecuteViaClient_RespawnRetryNeverRewritesWireResult is a defense-in-depth
// pin: a wire-delivered valid retry verdict after a respawn is returned
// verbatim (outputs survive) — guard against the skip flag leaking into
// non-synthesized results.
func TestExecuteViaClient_RespawnRetryNeverRewritesWireResult(t *testing.T) {
	client := &kb45ReplayClient{
		events: []*v2.ExecuteEvent{kb45ResultEvent("success", "clean retry", []byte(`{"attempts":3}`))},
	}

	result, err := ExecuteViaClient(context.Background(), client, "fake", "s1", true, kb45FallbackFallbackStep(), &adapterEventCollector{}, nil)

	require.NoError(t, err)
	require.Equal(t, "success", result.Outcome)
	require.Equal(t, "clean retry", result.Comment)
	require.False(t, result.SynthesizedFallback, "a wire-delivered verdict is not a synthesis")
	require.True(t, result.Outputs["attempts"].RawEquals(cty.NumberIntVal(3)))
}
