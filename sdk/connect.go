package overseer

import (
	"net/http"

	connect "connectrpc.com/connect"
	"github.com/brokenbots/overseer/sdk/pb/v1/overseerv1connect"
)

// ServiceClient is the Connect client interface for the OverseerService.
// Use [NewServiceClient] to construct an implementation.
type ServiceClient = overseerv1connect.OverseerServiceClient

// ServiceHandler is the server-side handler interface for the OverseerService.
// Orchestrators implement this interface to receive calls from an overseer.
type ServiceHandler = overseerv1connect.OverseerServiceHandler

// OverseerServiceClient is an alias for [ServiceClient] for compatibility
// with code that uses the fully-qualified form before migrating to the SDK path.
type OverseerServiceClient = ServiceClient

// OverseerServiceHandler is an alias for [ServiceHandler] for compatibility
// with code that uses the fully-qualified form before migrating to the SDK path.
type OverseerServiceHandler = ServiceHandler

// NewServiceClient constructs a [ServiceClient] that speaks to baseURL.
// By default it uses the Connect protocol with binary Protobuf encoding.
// Pass connect.WithGRPC() or connect.WithGRPCWeb() to use those protocols.
var NewServiceClient = overseerv1connect.NewOverseerServiceClient

// NewServiceHandler builds an HTTP handler from a [ServiceHandler] implementation.
// It returns the URL path prefix and the handler itself, ready to mount on an
// http.ServeMux. The handler supports Connect, gRPC, and gRPC-Web protocols.
func NewServiceHandler(svc ServiceHandler, opts ...connect.HandlerOption) (string, http.Handler) {
	return overseerv1connect.NewOverseerServiceHandler(svc, opts...)
}

// Service name and procedure path constants forwarded from the generated package.
const (
	ServiceName = overseerv1connect.OverseerServiceName

	RegisterProcedure     = overseerv1connect.OverseerServiceRegisterProcedure
	HeartbeatProcedure    = overseerv1connect.OverseerServiceHeartbeatProcedure
	CreateRunProcedure    = overseerv1connect.OverseerServiceCreateRunProcedure
	ReattachRunProcedure  = overseerv1connect.OverseerServiceReattachRunProcedure
	ResumeProcedure       = overseerv1connect.OverseerServiceResumeProcedure
	SubmitEventsProcedure = overseerv1connect.OverseerServiceSubmitEventsProcedure
	ControlProcedure      = overseerv1connect.OverseerServiceControlProcedure
)
