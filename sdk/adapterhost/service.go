package adapterhost

import (
	"context"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// AdapterName is the dispenser key shared between the host and every plugin
// process. Plugin authors do not need to reference this constant directly;
// [Serve] registers it automatically.
const AdapterName = "adapter"

// Service is the contract an out-of-process adapter plugin must implement.
// The Criteria host creates one subprocess per plugin binary and calls these
// methods over a local gRPC transport managed by hashicorp/go-plugin.
//
// All methods receive a context that is cancelled when the host initiates
// teardown. Implementations must respect context cancellation.
type Service interface {
	Info(context.Context, *pb.InfoRequest) (*pb.InfoResponse, error)
	OpenSession(context.Context, *pb.OpenSessionRequest) (*pb.OpenSessionResponse, error)
	// Execute streams events back to the host via [ExecuteEventSender]. It must
	// send exactly one [pb.ExecuteResult] event before returning nil, or return a
	// non-nil error.
	Execute(context.Context, *pb.ExecuteRequest, ExecuteEventSender) error
	Permit(context.Context, *pb.PermitRequest) (*pb.PermitResponse, error)
	CloseSession(context.Context, *pb.CloseSessionRequest) (*pb.CloseSessionResponse, error)
}

// ExecuteEventSender pushes Execute stream events from the plugin to the host.
// Implementations must not retain the sender beyond the Execute call.
// Send must be safe for concurrent use: adapters may call it from event-handler
// goroutines concurrently with the main Execute goroutine.
type ExecuteEventSender interface {
	Send(*pb.ExecuteEvent) error
}
