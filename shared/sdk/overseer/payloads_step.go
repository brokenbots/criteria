package overseer

import pb "github.com/brokenbots/overlord/shared/pb/overlord/v1"

// StepEntered is emitted when execution enters a step node.
type StepEntered = pb.StepEntered

// StepOutcome is emitted when a step completes (success or failure).
type StepOutcome = pb.StepOutcome

// StepTransition is emitted when the FSM transitions between step states.
type StepTransition = pb.StepTransition

// StepLog carries a log line produced during step execution.
type StepLog = pb.StepLog

// StepResumed is emitted when a paused step resumes execution.
type StepResumed = pb.StepResumed

// StepOutputCaptured is emitted when a step's output is captured.
type StepOutputCaptured = pb.StepOutputCaptured
