package overseer

import pb "github.com/brokenbots/overseer/sdk/pb/v1"

// Envelope is the top-level event container sent over the wire.
// Every event published by an overseer is wrapped in an Envelope.
type Envelope = pb.Envelope

// Oneof wrapper types for Envelope.Payload. These are the concrete types
// stored in the Payload field; use a type switch or proto reflection to
// discriminate them.
type (
	Envelope_RunStarted        = pb.Envelope_RunStarted
	Envelope_RunCompleted      = pb.Envelope_RunCompleted
	Envelope_RunFailed         = pb.Envelope_RunFailed
	Envelope_StepEntered       = pb.Envelope_StepEntered
	Envelope_StepOutcome       = pb.Envelope_StepOutcome
	Envelope_StepTransition    = pb.Envelope_StepTransition
	Envelope_StepLog           = pb.Envelope_StepLog
	Envelope_AdapterEvent      = pb.Envelope_AdapterEvent
	Envelope_OverseerHeartbeat = pb.Envelope_OverseerHeartbeat
	Envelope_OverseerDisconnected = pb.Envelope_OverseerDisconnected
	Envelope_StepResumed       = pb.Envelope_StepResumed
	Envelope_VariableSet       = pb.Envelope_VariableSet
	Envelope_StepOutputCaptured = pb.Envelope_StepOutputCaptured
	Envelope_WaitEntered       = pb.Envelope_WaitEntered
	Envelope_WaitResumed       = pb.Envelope_WaitResumed
	Envelope_ApprovalRequested = pb.Envelope_ApprovalRequested
	Envelope_ApprovalDecision  = pb.Envelope_ApprovalDecision
	Envelope_BranchEvaluated   = pb.Envelope_BranchEvaluated
	Envelope_ForEachEntered    = pb.Envelope_ForEachEntered
	Envelope_ForEachIteration  = pb.Envelope_ForEachIteration
	Envelope_ForEachOutcome    = pb.Envelope_ForEachOutcome
	Envelope_ScopeIterCursorSet = pb.Envelope_ScopeIterCursorSet
	Envelope_WatchReady        = pb.Envelope_WatchReady
)
