package overseer

import pb "github.com/brokenbots/overlord/shared/pb/overlord/v1"

// RunStarted is emitted when a run begins execution.
type RunStarted = pb.RunStarted

// RunCompleted is emitted when a run finishes successfully.
type RunCompleted = pb.RunCompleted

// RunFailed is emitted when a run terminates with an error.
type RunFailed = pb.RunFailed
