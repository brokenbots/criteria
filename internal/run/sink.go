// Package run wires the engine, dispatcher, and server transport into a
// single Sink that publishes events upstream.
package run

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/brokenbots/criteria/events"
	"github.com/brokenbots/criteria/internal/adapter"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// Publisher is the minimal transport interface required by Sink. The server
// transport (*servertrans.Client) satisfies this interface; test fakes may
// implement it directly without importing the transport package.
type Publisher interface {
	Publish(ctx context.Context, env *pb.Envelope)
}

// Sink implements engine.Sink by forwarding to a server client.
//
// Envelope identity is assigned by the transport: Client.Publish overwrites
// CorrelationId with a per-envelope UUID so the server can dedup on
// (run_id, correlation_id) across reconnects. Sink therefore carries only
// the RunID; no run-scoped trace id is stamped here.
type Sink struct {
	RunID  string
	Client Publisher
	Log    *slog.Logger
	// CheckpointFn, if non-nil, is called synchronously inside OnStepEntered
	// before the event is published. Use this to write a durable step
	// checkpoint for crash recovery.
	CheckpointFn func(step string, attempt int)
	// pausedNode is set by OnRunPaused when the engine pauses execution (W05).
	// Protected by pauseMu to guard against future concurrent access (e.g. W06
	// parallel branch execution).
	pauseMu    sync.Mutex
	pausedNode string
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

// OnVariableSet emits a variable.set event when a workflow variable is
// established (W04). source is "default" for HCL-declared defaults.
func (s *Sink) OnVariableSet(name, value, source string) {
	s.publish(&pb.VariableSet{Name: name, Value: value, Source: source})
}

// OnStepOutputCaptured emits a step.output_captured event after a step
// records outputs (W04).
func (s *Sink) OnStepOutputCaptured(step string, outputs map[string]string) {
	s.publish(&pb.StepOutputCaptured{Step: step, Outputs: outputs})
}

// OnRunPaused is called by the engine loop when execution pauses at a wait
// or approval node (W05). The server sink does not publish a separate event
// here because WaitEntered / ApprovalRequested were already emitted; this
// hook exists so the CLI pause/resume loop can detect the paused node.
func (s *Sink) OnRunPaused(node, mode, signal string) {
	s.Log.Info("run paused", "run_id", s.RunID, "node", node, "mode", mode, "signal", signal)
	s.pauseMu.Lock()
	s.pausedNode = node
	s.pauseMu.Unlock()
}

// IsPaused returns true if the engine paused at a node waiting for a signal (W05).
func (s *Sink) IsPaused() bool {
	s.pauseMu.Lock()
	defer s.pauseMu.Unlock()
	return s.pausedNode != ""
}

// PausedAt returns the node name the engine is paused at (W05).
func (s *Sink) PausedAt() string {
	s.pauseMu.Lock()
	defer s.pauseMu.Unlock()
	return s.pausedNode
}

// ClearPaused clears the paused state after the resume signal is delivered (W05).
func (s *Sink) ClearPaused() {
	s.pauseMu.Lock()
	s.pausedNode = ""
	s.pauseMu.Unlock()
}

// OnWaitEntered emits a wait.entered event when the engine enters a wait node (W05).
func (s *Sink) OnWaitEntered(node, mode, duration, signal string) {
	s.publish(&pb.WaitEntered{Node: node, Mode: mode, Duration: duration, Signal: signal})
}

// OnWaitResumed emits a wait.resumed event when a wait node resolves (W05).
func (s *Sink) OnWaitResumed(node, mode, signal string, payload map[string]string) {
	s.publish(&pb.WaitResumed{Node: node, Mode: mode, Signal: signal, Payload: payload})
}

// OnApprovalRequested emits an approval.requested event (W05).
func (s *Sink) OnApprovalRequested(node string, approvers []string, reason string) {
	s.publish(&pb.ApprovalRequested{Node: node, Approvers: approvers, Reason: reason})
}

// OnApprovalDecision emits an approval.decision event (W05).
func (s *Sink) OnApprovalDecision(node, decision, actor string, payload map[string]string) {
	s.publish(&pb.ApprovalDecision{Node: node, Decision: decision, Actor: actor, Payload: payload})
}

// OnBranchEvaluated emits a branch.evaluated event when a branch node selects a transition arm (W06).
func (s *Sink) OnBranchEvaluated(node, matchedArm, target, condition string) {
	s.publish(&pb.BranchEvaluated{Node: node, MatchedArm: matchedArm, Target: target, Condition: condition})
}

// OnForEachEntered emits a for_each.entered event when a for_each node begins iterating (W07).
func (s *Sink) OnForEachEntered(node string, count int) {
	s.publish(&pb.ForEachEntered{Node: node, Count: int32(count)})
}

// OnForEachIteration emits a for_each.iteration event at the start of each per-item iteration (W07).
func (s *Sink) OnForEachIteration(node string, index int, value string, anyFailed bool) {
	s.publish(&pb.ForEachIteration{Node: node, Index: int32(index), Value: value, AnyFailed: anyFailed})
}

// OnForEachOutcome emits a for_each.outcome event when a for_each node finishes iterating (W07).
func (s *Sink) OnForEachOutcome(node, outcome, target string) {
	s.publish(&pb.ForEachOutcome{Node: node, Outcome: outcome, Target: target})
}

// OnScopeIterCursorSet emits a scope.iter_cursor_set event when the for_each
// cursor is created, advanced, or cleared (W07). The server stores cursorJSON
// verbatim without interpreting field names.
func (s *Sink) OnScopeIterCursorSet(cursorJSON string) {
	s.publish(&pb.ScopeIterCursorSet{CursorJson: cursorJSON})
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
