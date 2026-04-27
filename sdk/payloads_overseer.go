package overseer

import pb "github.com/brokenbots/overseer/sdk/pb/v1"

// OverseerHeartbeat is emitted periodically by the overseer to signal liveness.
type OverseerHeartbeat = pb.OverseerHeartbeat

// OverseerDisconnected is emitted when the overseer disconnects from the orchestrator.
type OverseerDisconnected = pb.OverseerDisconnected

// WatchReady is emitted by the orchestrator to signal that a WatchRun stream
// is live and ready to receive events.
type WatchReady = pb.WatchReady
