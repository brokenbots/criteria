package engine

// KB-45 wire-path engine companions to the adapterhost-level guards. These
// drive the run through an out-of-process handle shape (adapterhost rpcHandle
// over a fake v2 client) so the fixes exercise the full
// SessionManager.execute → resolvedOutcome → attempt-loop route:
//
//  1. A non-chunked ExecuteResult whose outputs_json the host cannot decode
//     on a contract-bearing step settles the captured verdict, so the pinned
//     payload_schema issue reaches the engine's repair loop instead of the
//     never-finalized lane silently synthesizing the fallback over the
//     invalid payload.
//  2. A host-synthesized fallback verdict is not re-validated by the second
//     local pass (nor by the respawn retry), even when the fallback outcome
//     itself declares an unsatisfiable schema + require_comment.

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	criteriav2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapterhost"
)

// kb45WireWorkflow declares a contract-armed step executed through a wire
// handle: a schema+require_comment success, a fallback failure that itself
// carries a schema and require_comment (unsatisfiable against the empty
// synthesis by construction), and a default mapping.
func kb45WireWorkflow(adapterOpts string) string {
	return `
workflow {
  name          = "kb45wire"
  version       = "0.1"
  initial_state = "ship"
  target_state  = "done"
  policy {
    max_step_retries = 1
  }
}

type "audit" {
  schema = object({
    attempts = number
    summary  = optional(string, "(no summary)")
  })
}

adapter "fake" "default" {` + adapterOpts + `}

step "ship" {
  target = adapter.fake.default
  outcome "success" {
    schema          = type.audit
    require_comment = true
    next            = state.done
  }
  outcome "failure" {
    fallback        = true
    schema          = object({ reason = string })
    require_comment = true
    next            = state.failed
  }
  outcome "default" {
    next = state.defaulted
  }
}

state "done" { terminal = true }
state "failed" {
  terminal = true
  success  = false
}
state "defaulted" {
  terminal = true
  success  = false
}
`
}

// kb45WireLoader hands the wire handle for any requested adapter.
type kb45WireLoader struct {
	handle adapterhost.Handle
}

func (l *kb45WireLoader) Resolve(_ context.Context, _ string) (adapterhost.Handle, error) {
	if l.handle == nil {
		return nil, fmt.Errorf("kb45WireLoader: no handle")
	}
	return l.handle, nil
}

func (l *kb45WireLoader) Shutdown(context.Context) error { return nil }

// kb45WireClient is a fake v2 client whose Execute behavior is scripted per
// attempt; the remaining session surface is inert.
type kb45WireClient struct {
	mu       sync.Mutex
	attempts int
	events   func(attempt int) []*criteriav2.ExecuteEvent
	execErr  func(attempt int) error
	lastReq  *criteriav2.ExecuteRequest
}

func (c *kb45WireClient) attempt() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.attempts + 1
}

func (c *kb45WireClient) Info(context.Context, *criteriav2.InfoRequest) (*criteriav2.InfoResponse, error) {
	return &criteriav2.InfoResponse{Name: "kb45-wire-stub"}, nil
}

func (c *kb45WireClient) OpenSession(context.Context, *criteriav2.OpenSessionRequest) (*criteriav2.OpenSessionResponse, error) {
	return &criteriav2.OpenSessionResponse{}, nil
}

func (c *kb45WireClient) Execute(_ context.Context, req *criteriav2.ExecuteRequest, sink adapterhost.ExecuteEventSink) error {
	c.mu.Lock()
	c.attempts++
	n := c.attempts
	c.mu.Unlock()
	c.lastReq = req
	if c.execErr != nil {
		if err := c.execErr(n); err != nil {
			return err
		}
	}
	if c.events != nil {
		for _, ev := range c.events(n) {
			if err := sink.Emit(ev); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *kb45WireClient) Log(context.Context, *criteriav2.LogRequest, adapterhost.LogEventSink) error {
	return nil
}

func (c *kb45WireClient) Permissions(context.Context, <-chan *criteriav2.PermissionEvent) error {
	return nil
}

func (c *kb45WireClient) Pause(context.Context, *criteriav2.PauseRequest) (*criteriav2.PauseResponse, error) {
	return &criteriav2.PauseResponse{}, nil
}

func (c *kb45WireClient) Resume(context.Context, *criteriav2.ResumeRequest) (*criteriav2.ResumeResponse, error) {
	return &criteriav2.ResumeResponse{}, nil
}

func (c *kb45WireClient) Snapshot(context.Context, *criteriav2.SnapshotRequest) (*criteriav2.SnapshotResponse, error) {
	return &criteriav2.SnapshotResponse{}, nil
}

func (c *kb45WireClient) Restore(context.Context, *criteriav2.RestoreRequest) (*criteriav2.RestoreResponse, error) {
	return &criteriav2.RestoreResponse{}, nil
}

func (c *kb45WireClient) Inspect(context.Context, *criteriav2.InspectRequest) (*criteriav2.InspectResponse, error) {
	return &criteriav2.InspectResponse{}, nil
}

func (c *kb45WireClient) CloseSession(context.Context, *criteriav2.CloseSessionRequest) (*criteriav2.CloseSessionResponse, error) {
	return &criteriav2.CloseSessionResponse{}, nil
}

func (c *kb45WireClient) Prompt(context.Context, *adapterhost.PromptRequest) (*adapterhost.PromptResponse, error) {
	return &adapterhost.PromptResponse{Accepted: false, Detail: "kb45 wire stub does not accept prompts"}, nil
}

// kb45ResultEvent builds a non-chunked ExecuteResult event.
func kb45ResultEvent(outcome, comment string, outputsJSON []byte) *criteriav2.ExecuteEvent {
	return &criteriav2.ExecuteEvent{
		Event: &criteriav2.ExecuteEvent_Result{
			Result: &criteriav2.ExecuteResult{Outcome: outcome, Comment: comment, OutputsJson: outputsJSON},
		},
	}
}

// kb45WireRun compiles the workflow, wires the fake client into the engine,
// and runs.
func kb45WireRun(t *testing.T, src string, client *kb45WireClient) (*contractSink, error) {
	t.Helper()
	g := compile(t, src)
	sink := &contractSink{}
	loader := &kb45WireLoader{handle: adapterhost.NewRPCHandle("fake", nil, client)}
	err := NewTestEngine(g, loader, sink).Run(context.Background())
	return sink, err
}

// TestRun_WireUndecodablePayloadEntersRepairLoop pins DEFECT 1 end to end:
// the wire verdict carries outputs_json that is not a JSON object, the host
// settles the captured verdict, and every attempt lands in the engine's
// repair loop with the pinned payload_schema issue — exhaustion then maps to
// the step's default outcome. (Pre-fix, the never-finalized lane silently
// synthesized the fallback "failure" outcome over the invalid payload, so
// no StepOutcomeInvalid event ever surfaced.)
func TestRun_WireUndecodablePayloadEntersRepairLoop(t *testing.T) {
	client := &kb45WireClient{
		events: func(int) []*criteriav2.ExecuteEvent {
			return []*criteriav2.ExecuteEvent{kb45ResultEvent("success", "shipped", []byte(`[]`))}
		},
		execErr: func(int) error { return nil },
	}

	sink, err := kb45WireRun(t, kb45WireWorkflow(""), client)
	require.NoError(t, err)
	require.Equal(t, "defaulted", sink.terminal, "exhausted repairs map to the default outcome")

	require.Len(t, sink.invalid, 2, "one StepOutcomeInvalid per rejected attempt (budget 1 + initial)")
	for i, inv := range sink.invalid {
		require.Equal(t, "ship", inv.step)
		require.Equal(t, "success", inv.outcome)
		require.Equal(t, i+1, inv.attempt)
		require.Contains(t, inv.issues, "payload_schema: outputs_json does not decode to a JSON object")
	}

	// The wire verdict's own payload diagnosis surfaced verbatim: no
	// synthesized fallback, no generic no_result error.
	for _, o := range sink.outcome {
		require.NotEqual(t, "failure", o.outcome, "the fallback must never fire over an invalid delivered payload")
	}
	require.Len(t, sink.defaulted, 1)
	require.Equal(t, [3]string{"ship", "default", "default"}, sink.defaulted[0])
}

// TestRun_WireNeverFinalizesSynthesizesArmedFallback pins DEFECT 2 end to end
// on the direct execute path: the adapter never finalizes, the host
// synthesizes the fallback outcome, and the fallback succeeds even though the
// fallback contract itself requires a payload property and a comment the
// synthesis cannot carry.
func TestRun_WireNeverFinalizesSynthesizesArmedFallback(t *testing.T) {
	client := &kb45WireClient{
		events:  func(int) []*criteriav2.ExecuteEvent { return nil },
		execErr: func(int) error { return nil },
	}

	sink, err := kb45WireRun(t, kb45WireWorkflow(""), client)
	require.NoError(t, err)
	require.Equal(t, "failed", sink.terminal, "the synthesized fallback outcome must be accepted")
	require.Empty(t, sink.invalid, "the synthesized fallback must never re-enter contract validation")

	var outcomes []string
	for _, o := range sink.outcome {
		if o.step == "ship" {
			outcomes = append(outcomes, o.outcome)
			require.NoError(t, o.err)
		}
	}
	require.Equal(t, []string{"failure"}, outcomes, "the fallback fired on the first attempt (no repair retries)")
	require.Empty(t, sink.captured["ship"], "the synthesized fallback carries no payload")
}

// TestRun_WireNeverFinalizesSynthesizesArmedFallbackAfterRespawn pins
// DEFECT 2 on the respawn retry path: the first adapter process dies with a
// transport close under on_crash=respawn, the replacement generation never
// finalizes, and the synthesized fallback is accepted without the retry
// re-validation unsatisfiably rejecting it.
func TestRun_WireNeverFinalizesSynthesizesArmedFallbackAfterRespawn(t *testing.T) {
	const cri271TransportClosed = "rpc error: code = Canceled desc = grpc: the client connection is closing"
	client := &kb45WireClient{
		events: func(int) []*criteriav2.ExecuteEvent { return nil },
		execErr: func(attempt int) error {
			if attempt == 1 {
				return fmt.Errorf("%s", cri271TransportClosed)
			}
			return nil
		},
	}

	sink, err := kb45WireRun(t, kb45WireWorkflow(" on_crash = \"respawn\" "), client)
	require.NoError(t, err)
	require.Equal(t, "failed", sink.terminal, "the respawn retry's synthesized fallback must be accepted")
	require.Empty(t, sink.invalid, "the respawn retry must not re-validate the synthesis against the fallback contract")

	var outcomes []string
	for _, o := range sink.outcome {
		if o.step == "ship" {
			outcomes = append(outcomes, o.outcome)
		}
	}
	require.Equal(t, []string{"failure"}, outcomes)
}

// TestRun_WireLegacyStepUndecodablePayloadKeepsStreamError preserves the
// legacy boundary: an undecodable payload on a step WITHOUT contracts still
// aborts the stream with the decode error (no contracts → no repair loop).
func TestRun_WireLegacyStepUndecodablePayloadKeepsStreamError(t *testing.T) {
	g := compile(t, `
workflow {
  name          = "kb45legacy"
  version       = "0.1"
  initial_state = "ship"
  target_state  = "done"
  policy {
    max_step_retries = 1
  }
}
adapter "fake" "default" {}
step "ship" {
  target = adapter.fake.default
  outcome "success" { next = state.done }
}
state "done" { terminal = true }
`)
	client := &kb45WireClient{
		events: func(int) []*criteriav2.ExecuteEvent {
			return []*criteriav2.ExecuteEvent{kb45ResultEvent("success", "shipped", []byte(`[]`))}
		},
	}
	sink := &contractSink{}
	loader := &kb45WireLoader{handle: adapterhost.NewRPCHandle("fake", nil, client)}
	err := NewTestEngine(g, loader, sink).Run(context.Background())
	require.Error(t, err, "legacy steps keep the pre-contract stream error for undecodable payloads")
	require.Empty(t, sink.invalid)
}
