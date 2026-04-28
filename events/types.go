// Package events provides thin helpers over the generated criteria event
// envelope type. The wire contract itself lives in proto/criteria/v1/*.proto
// and its generated Go code is the single source of truth for payload shapes.
//
// Callers that need to read or construct envelope payloads should work with
// the generated types in sdk/pb/criteria/v1 directly; the helpers here
// cover the few cross-cutting concerns (schema version, envelope builder,
// type discriminator, terminal-event check) that aren't generated.
package events

import (
	"fmt"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// SchemaVersion is the current event protocol version. Bump only with a new
// criteria.vN proto package.
const SchemaVersion = 1

// NewEnvelope builds a *pb.Envelope for runID with the given payload message.
// The schema version is stamped and the timestamp is set to now (UTC).
// Seq is left at zero; the server assigns the real value on ingest.
//
// `payload` must be one of the generated payload message types (e.g.
// *pb.RunStarted, *pb.StepLog). Passing a nil payload leaves env.Payload
// unset. Passing a non-nil value of an unknown type panics rather than
// silently producing an empty envelope — callers are expected to hand in
// the concrete generated types.
//
// NewEnvelope does not set CorrelationId. The agent transport stamps a
// fresh UUID on every Publish so the server can deduplicate on
// (run_id, correlation_id) across reconnects; any caller-supplied
// correlation id would be overwritten there anyway.
func NewEnvelope(runID string, payload any) *pb.Envelope {
	env := &pb.Envelope{
		SchemaVersion: SchemaVersion,
		RunId:         runID,
		Ts:            timestamppb.New(time.Now().UTC()),
	}
	setPayload(env, payload)
	return env
}

// setPayload assigns a payload message to env.Payload by concrete type.
// Unknown non-nil payloads panic to surface caller bugs at construction
// time rather than producing an empty envelope that looks valid on the wire.
func setPayload(env *pb.Envelope, payload any) {
	switch p := payload.(type) {
	case nil:
		return
	case *pb.RunStarted:
		env.Payload = &pb.Envelope_RunStarted{RunStarted: p}
	case *pb.RunCompleted:
		env.Payload = &pb.Envelope_RunCompleted{RunCompleted: p}
	case *pb.RunFailed:
		env.Payload = &pb.Envelope_RunFailed{RunFailed: p}
	case *pb.StepEntered:
		env.Payload = &pb.Envelope_StepEntered{StepEntered: p}
	case *pb.StepOutcome:
		env.Payload = &pb.Envelope_StepOutcome{StepOutcome: p}
	case *pb.StepTransition:
		env.Payload = &pb.Envelope_StepTransition{StepTransition: p}
	case *pb.StepLog:
		env.Payload = &pb.Envelope_StepLog{StepLog: p}
	case *pb.AdapterEvent:
		env.Payload = &pb.Envelope_AdapterEvent{AdapterEvent: p}
	case *pb.CriteriaHeartbeat:
		env.Payload = &pb.Envelope_CriteriaHeartbeat{CriteriaHeartbeat: p}
	case *pb.CriteriaDisconnected:
		env.Payload = &pb.Envelope_CriteriaDisconnected{CriteriaDisconnected: p}
	case *pb.StepResumed:
		env.Payload = &pb.Envelope_StepResumed{StepResumed: p}
	case *pb.WatchReady:
		env.Payload = &pb.Envelope_WatchReady{WatchReady: p}
	case *pb.VariableSet:
		env.Payload = &pb.Envelope_VariableSet{VariableSet: p}
	case *pb.StepOutputCaptured:
		env.Payload = &pb.Envelope_StepOutputCaptured{StepOutputCaptured: p}
	case *pb.WaitEntered:
		env.Payload = &pb.Envelope_WaitEntered{WaitEntered: p}
	case *pb.WaitResumed:
		env.Payload = &pb.Envelope_WaitResumed{WaitResumed: p}
	case *pb.ApprovalRequested:
		env.Payload = &pb.Envelope_ApprovalRequested{ApprovalRequested: p}
	case *pb.ApprovalDecision:
		env.Payload = &pb.Envelope_ApprovalDecision{ApprovalDecision: p}
	case *pb.BranchEvaluated:
		env.Payload = &pb.Envelope_BranchEvaluated{BranchEvaluated: p}
	case *pb.ForEachEntered:
		env.Payload = &pb.Envelope_ForEachEntered{ForEachEntered: p}
	case *pb.ForEachIteration:
		env.Payload = &pb.Envelope_ForEachIteration{ForEachIteration: p}
	case *pb.ForEachOutcome:
		env.Payload = &pb.Envelope_ForEachOutcome{ForEachOutcome: p}
	case *pb.ScopeIterCursorSet:
		env.Payload = &pb.Envelope_ScopeIterCursorSet{ScopeIterCursorSet: p}
	default:
		panic(fmt.Sprintf("events.NewEnvelope: unsupported payload type %T", payload))
	}
}

// TypeString returns a stable discriminator string for env's payload (e.g.
// "step.log"). It is used as the `type` column in the server's event store and
// by tests that want to inspect events without reaching into the oneof.
// Envelopes with no payload return the empty string.
func TypeString(env *pb.Envelope) string {
	if env == nil {
		return ""
	}
	switch env.Payload.(type) {
	case *pb.Envelope_RunStarted:
		return "run.started"
	case *pb.Envelope_RunCompleted:
		return "run.completed"
	case *pb.Envelope_RunFailed:
		return "run.failed"
	case *pb.Envelope_StepEntered:
		return "step.entered"
	case *pb.Envelope_StepOutcome:
		return "step.outcome"
	case *pb.Envelope_StepTransition:
		return "step.transition"
	case *pb.Envelope_StepLog:
		return "step.log"
	case *pb.Envelope_AdapterEvent:
		return "adapter.event"
	case *pb.Envelope_CriteriaHeartbeat:
		return "criteria.heartbeat"
	case *pb.Envelope_CriteriaDisconnected:
		return "criteria.disconnected"
	case *pb.Envelope_StepResumed:
		return "step.resumed"
	case *pb.Envelope_WatchReady:
		return "watch.ready"
	case *pb.Envelope_VariableSet:
		return "variable.set"
	case *pb.Envelope_StepOutputCaptured:
		return "step.output_captured"
	case *pb.Envelope_WaitEntered:
		return "wait.entered"
	case *pb.Envelope_WaitResumed:
		return "wait.resumed"
	case *pb.Envelope_ApprovalRequested:
		return "approval.requested"
	case *pb.Envelope_ApprovalDecision:
		return "approval.decision"
	case *pb.Envelope_BranchEvaluated:
		return "branch.evaluated"
	case *pb.Envelope_ForEachEntered:
		return "for_each.entered"
	case *pb.Envelope_ForEachIteration:
		return "for_each.iteration"
	case *pb.Envelope_ForEachOutcome:
		return "for_each.outcome"
	case *pb.Envelope_ScopeIterCursorSet:
		return "scope.iter_cursor_set"
	default:
		return ""
	}
}

// IsTerminal reports whether env is a terminal run event (run.completed or
// run.failed). Used by WatchRun to close the stream after the final event.
func IsTerminal(env *pb.Envelope) bool {
	if env == nil {
		return false
	}
	switch env.Payload.(type) {
	case *pb.Envelope_RunCompleted, *pb.Envelope_RunFailed:
		return true
	default:
		return false
	}
}
