// Package run wires the engine, dispatcher, and Castle transport into a
// single Sink that publishes events upstream.
package run

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/brokenbots/overlord/overseer/internal/adapter"
	castletrans "github.com/brokenbots/overlord/overseer/internal/transport/castle"
	"github.com/brokenbots/overlord/shared/events"
)

// Sink implements engine.Sink by forwarding to a Castle client.
//
// Envelope identity is assigned by the transport: Client.Publish overwrites
// CorrelationID with a per-envelope UUID so Castle can dedup on
// (run_id, correlation_id) across reconnects. Sink therefore carries only
// the RunID; no run-scoped trace id is stamped here.
type Sink struct {
	RunID  string
	Client *castletrans.Client
	Log    *slog.Logger
}

func (s *Sink) publish(t events.Type, payload any) {
	env, err := events.New(s.RunID, t, payload)
	if err != nil {
		s.Log.Error("event marshal failed", "type", t, "error", err)
		return
	}
	// Always publish on a fresh background context — engine cancellation must
	// not prevent terminal events from leaving the buffer.
	s.Client.Publish(context.Background(), env)
}

func (s *Sink) OnRunStarted(workflowName, initialStep string) {
	s.publish(events.TypeRunStarted, events.RunStarted{WorkflowName: workflowName, InitialStep: initialStep})
}

func (s *Sink) OnRunCompleted(finalState string, success bool) {
	s.publish(events.TypeRunCompleted, events.RunCompleted{FinalState: finalState, Success: success})
}

func (s *Sink) OnRunFailed(reason, step string) {
	s.publish(events.TypeRunFailed, events.RunFailed{Reason: reason, Step: step})
}

func (s *Sink) OnStepEntered(step, adapterName string, attempt int) {
	s.publish(events.TypeStepEntered, events.StepEntered{Step: step, Adapter: adapterName, Attempt: attempt})
}

func (s *Sink) OnStepOutcome(step, outcome string, duration time.Duration, err error) {
	p := events.StepOutcome{Step: step, Outcome: outcome, DurationMS: duration.Milliseconds()}
	if err != nil {
		p.Error = err.Error()
	}
	s.publish(events.TypeStepOutcome, p)
}

func (s *Sink) OnStepTransition(from, to, viaOutcome string) {
	s.publish(events.TypeStepTransition, events.StepTransition{From: from, To: to, ViaOutcome: viaOutcome})
}

// StepEventSink returns a per-step adapter sink that wraps Log/Adapter into
// step.log / adapter.event envelopes.
func (s *Sink) StepEventSink(step string) adapter.EventSink {
	return &stepSink{parent: s, step: step}
}

type stepSink struct {
	parent *Sink
	step   string
}

func (ss *stepSink) Log(stream string, chunk []byte) {
	ss.parent.publish(events.TypeStepLog, events.StepLog{Step: ss.step, Stream: events.LogStream(stream), Chunk: string(chunk)})
}

func (ss *stepSink) Adapter(kind string, data any) {
	raw, _ := json.Marshal(data)
	ss.parent.publish(events.TypeAdapterEvent, events.AdapterEvent{Step: ss.step, Adapter: "", Kind: kind, Data: raw})
}
