// Package run wires the engine, dispatcher, and Castle transport into a
// single Sink that publishes events upstream.
package run

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/brokenbots/overlord/overseer/internal/adapter"
	castletrans "github.com/brokenbots/overlord/overseer/internal/transport/castle"
	"github.com/brokenbots/overlord/shared/events"
	pb "github.com/brokenbots/overlord/shared/pb/overlord/v1"
)

// Sink implements engine.Sink by forwarding to a Castle client.
//
// Envelope identity is assigned by the transport: Client.Publish overwrites
// CorrelationId with a per-envelope UUID so Castle can dedup on
// (run_id, correlation_id) across reconnects. Sink therefore carries only
// the RunID; no run-scoped trace id is stamped here.
type Sink struct {
	RunID  string
	Client *castletrans.Client
	Log    *slog.Logger
	// CheckpointFn, if non-nil, is called synchronously inside OnStepEntered
	// before the event is published. Use this to write a durable step
	// checkpoint for crash recovery.
	CheckpointFn func(step string, attempt int)
}

func (s *Sink) publish(payload any) {
	env := events.NewEnvelope(s.RunID, payload)
	// Always publish on a fresh background context — engine cancellation must
	// not prevent terminal events from leaving the buffer.
	s.Client.Publish(context.Background(), env)
}

func (s *Sink) OnRunStarted(workflowName, initialStep string) {
	s.publish(&pb.RunStarted{WorkflowName: workflowName, InitialStep: initialStep})
}

func (s *Sink) OnRunCompleted(finalState string, success bool) {
	s.publish(&pb.RunCompleted{FinalState: finalState, Success: success})
}

func (s *Sink) OnRunFailed(reason, step string) {
	s.publish(&pb.RunFailed{Reason: reason, Step: step})
}

func (s *Sink) OnStepEntered(step, adapterName string, attempt int) {
	if s.CheckpointFn != nil {
		s.CheckpointFn(step, attempt)
	}
	s.publish(&pb.StepEntered{Step: step, Adapter: adapterName, Attempt: int32(attempt)})
}

func (s *Sink) OnStepOutcome(step, outcome string, duration time.Duration, err error) {
	p := &pb.StepOutcome{Step: step, Outcome: outcome, DurationMs: duration.Milliseconds()}
	if err != nil {
		p.Error = err.Error()
	}
	s.publish(p)
}

func (s *Sink) OnStepTransition(from, to, viaOutcome string) {
	s.publish(&pb.StepTransition{From: from, To: to, ViaOutcome: viaOutcome})
}

func (s *Sink) OnStepResumed(step string, attempt int, reason string) {
	s.publish(&pb.StepResumed{Step: step, Attempt: int32(attempt), Reason: reason})
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
	ss.parent.publish(&pb.StepLog{Step: ss.step, Stream: logStreamFromString(stream), Chunk: string(chunk)})
}

// Adapter records a structured adapter-specific event. `data` must serialise
// to a JSON value; non-object values are wrapped under a `value` key before
// being stored as a google.protobuf.Struct, matching the Phase 0 adapter
// compatibility contract documented on AdapterEvent in events.proto.
//
// If `data` cannot be encoded (json.Marshal fails or the resulting JSON
// cannot be decoded into a google.protobuf.Struct) the failure is captured
// under an `_encode_error` key on the event so adapter diagnostics are not
// silently dropped.
func (ss *stepSink) Adapter(kind string, data any) {
	msg := &pb.AdapterEvent{Step: ss.step, Kind: kind}
	if data != nil {
		msg.Data = encodeAdapterData(data)
	}
	ss.parent.publish(msg)
}

// encodeAdapterData converts an arbitrary Go value into a
// google.protobuf.Struct by round-tripping through JSON. Non-object JSON
// values (scalars, arrays) are wrapped under `value`. On any encode/decode
// failure the returned struct carries `_encode_error` with the failure
// reason and `raw` with the best-effort JSON string (when available).
func encodeAdapterData(data any) *structpb.Struct {
	raw, err := json.Marshal(data)
	if err != nil {
		return errorStruct("marshal: "+err.Error(), "")
	}
	if len(raw) == 0 {
		return nil
	}
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		raw = []byte(`{"value":` + string(raw) + `}`)
	}
	st := &structpb.Struct{}
	if err := protojson.Unmarshal(raw, st); err != nil {
		return errorStruct("unmarshal: "+err.Error(), string(raw))
	}
	return st
}

func errorStruct(reason, raw string) *structpb.Struct {
	fields := map[string]*structpb.Value{
		"_encode_error": structpb.NewStringValue(reason),
	}
	if raw != "" {
		fields["raw"] = structpb.NewStringValue(raw)
	}
	return &structpb.Struct{Fields: fields}
}

func logStreamFromString(s string) pb.LogStream {
	switch s {
	case "stdout":
		return pb.LogStream_LOG_STREAM_STDOUT
	case "stderr":
		return pb.LogStream_LOG_STREAM_STDERR
	case "agent":
		return pb.LogStream_LOG_STREAM_AGENT
	default:
		return pb.LogStream_LOG_STREAM_UNSPECIFIED
	}
}
