// Package events provides thin helpers over the generated criteria event
// envelope type. The wire contract itself lives in proto/criteria/v1/*.proto
// and its generated Go code is the single source of truth for payload shapes.
//
// Callers that need to read or construct envelope payloads should work with
// the generated types in sdk/pb/criteria/v1 directly; the helpers here
// cover the few cross-cutting concerns (schema version, envelope builder,
// type discriminator, terminal-event check) that aren't generated.
//
// Standalone-import note (KB-118): the helpers' single source of truth lives
// IN the sdk (github.com/brokenbots/criteria/sdk root package) — this root
// package re-exports it. The sdk must never import this module back (its
// go.mod require of the criteria root broke every external `go mod tidy`
// against a released sdk pseudo-version while the dependency pointed this
// direction).
package events

import (
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"

	criteria "github.com/brokenbots/criteria/sdk"
)

// SchemaVersion is the current event protocol version. Bump only with a new
// criteria.vN proto package. Single source of truth: the sdk root package
// (events re-exports it — bump nothing here by hand).
const SchemaVersion = criteria.SchemaVersion

// NewEnvelope builds a *pb.Envelope for runID with the given payload message.
// Single source of truth: the sdk root package (events re-exports it).
// Compile-time twin: the root-local setPayload switch used to double
// as the failsafe when a new payload type landed in the proto; the sdk's
// conformance suite (sdk/conformance) now carries that coverage — run
// `make test-conformance` after adding envelope payload types.
func NewEnvelope(runID string, payload any) *pb.Envelope {
	return criteria.NewEnvelope(runID, payload)
}

// switch; kept as a compile-time twin to fail the build when a new payload
// type lands in the proto without bumping the switch in the sdk) assigns a
// payload message to env.Payload by concrete type.
// Unknown non-nil payloads panic to surface caller bugs at construction
// time rather than producing an empty envelope that looks valid on the wire.

// TypeString returns a stable discriminator string for env's payload (e.g.
// "step.log"). It is used as the `type` column in the server's event store and
// by tests that want to inspect events without reaching into the oneof.
// Envelopes with no payload return the empty string.
func TypeString(env *pb.Envelope) string { //nolint:funlen,gocyclo // discriminator switch must cover every concrete payload type in the oneof
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
	case *pb.Envelope_StepOutcomeInvalid:
		return "step.outcome_invalid"
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
	case *pb.Envelope_RunPaused:
		return "run.paused"
	case *pb.Envelope_RunResumed:
		return "run.resumed"
	case *pb.Envelope_ApprovalRequested:
		return "approval.requested"
	case *pb.Envelope_ApprovalDecision:
		return "approval.decision"
	case *pb.Envelope_BranchEvaluated:
		return "branch.evaluated"
	case *pb.Envelope_ForEachEntered:
		return "for_each.entered"
	case *pb.Envelope_StepIterationStarted:
		return "step.iteration_started"
	case *pb.Envelope_StepIterationCompleted:
		return "step.iteration_completed"
	case *pb.Envelope_ScopeIterCursorSet:
		return "scope.iter_cursor_set"
	case *pb.Envelope_StepIterationItem:
		return "step.iteration_item"
	case *pb.Envelope_RunOutputs:
		return "run.outputs"
	case *pb.Envelope_RunMetadata:
		return "run.metadata"
	case *pb.Envelope_AdapterLifecycleProvisionWanted:
		return "adapter.lifecycle.provision_wanted"
	case *pb.Envelope_AdapterLifecycleReleased:
		return "adapter.lifecycle.released"
	case *pb.Envelope_WorkflowGraphs:
		return "workflow.graphs"
	case *pb.Envelope_AgentPromptInjected:
		return "agent.prompt_injected"
	case *pb.Envelope_CheckpointPointer:
		return "checkpoint.pointer"
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
