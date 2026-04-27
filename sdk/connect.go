package overseer

import (
	"net/http"

	connect "connectrpc.com/connect"
	"github.com/brokenbots/overlord/shared/pb/overlord/v1/overlordv1connect"
)

// ServiceClient is the Connect client interface for the OverseerService.
// Use [NewServiceClient] to construct an implementation.
type ServiceClient = overlordv1connect.OverseerServiceClient

// ServiceHandler is the server-side handler interface for the OverseerService.
// Orchestrators implement this interface to receive calls from an overseer.
type ServiceHandler = overlordv1connect.OverseerServiceHandler

// OverseerServiceClient is an alias for [ServiceClient] for compatibility
// with code that uses the fully-qualified form before migrating to the SDK path.
type OverseerServiceClient = ServiceClient

// OverseerServiceHandler is an alias for [ServiceHandler] for compatibility
// with code that uses the fully-qualified form before migrating to the SDK path.
type OverseerServiceHandler = ServiceHandler

// NewServiceClient constructs a [ServiceClient] that speaks to baseURL.
// By default it uses the Connect protocol with binary Protobuf encoding.
// Pass connect.WithGRPC() or connect.WithGRPCWeb() to use those protocols.
var NewServiceClient = overlordv1connect.NewOverseerServiceClient

// NewServiceHandler builds an HTTP handler from a [ServiceHandler] implementation.
// It returns the URL path prefix and the handler itself, ready to mount on an
// http.ServeMux. The handler supports Connect, gRPC, and gRPC-Web protocols.
func NewServiceHandler(svc ServiceHandler, opts ...connect.HandlerOption) (string, http.Handler) {
	return overlordv1connect.NewOverseerServiceHandler(svc, opts...)
}

// Service name and procedure path constants forwarded from the generated package.
const (
	ServiceName = overlordv1connect.OverseerServiceName

	RegisterProcedure     = overlordv1connect.OverseerServiceRegisterProcedure
	HeartbeatProcedure    = overlordv1connect.OverseerServiceHeartbeatProcedure
	CreateRunProcedure    = overlordv1connect.OverseerServiceCreateRunProcedure
	ReattachRunProcedure  = overlordv1connect.OverseerServiceReattachRunProcedure
	ResumeProcedure       = overlordv1connect.OverseerServiceResumeProcedure
	SubmitEventsProcedure = overlordv1connect.OverseerServiceSubmitEventsProcedure
	ControlProcedure      = overlordv1connect.OverseerServiceControlProcedure
)
