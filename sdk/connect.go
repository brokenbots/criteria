package criteria

import (
	"net/http"

	connect "connectrpc.com/connect"

	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
)

// ServiceClient is the Connect client interface for the CriteriaService.
// Use [NewServiceClient] to construct an implementation.
type ServiceClient = criteriav1connect.CriteriaServiceClient

// ServiceHandler is the server-side handler interface for the CriteriaService.
// Orchestrators implement this interface to receive calls from a criteria agent.
type ServiceHandler = criteriav1connect.CriteriaServiceHandler

// CriteriaServiceClient is an alias for [ServiceClient].
type CriteriaServiceClient = ServiceClient

// CriteriaServiceHandler is an alias for [ServiceHandler].
type CriteriaServiceHandler = ServiceHandler

// OrchestratorServiceClient is the Connect client interface for the
// OrchestratorService (CRI-133): the operator-facing observation/reconcile
// API (SubscribeRunEvents / ListActiveRuns / CancelRun) implemented by
// orchestrators.
type OrchestratorServiceClient = criteriav1connect.OrchestratorServiceClient

// OrchestratorServiceHandler is the server-side handler interface for the
// OrchestratorService. Orchestrators implement this interface to expose the
// operator-facing run reconcile API.
type OrchestratorServiceHandler = criteriav1connect.OrchestratorServiceHandler

// NewOrchestratorServiceClient constructs an [OrchestratorServiceClient] that
// speaks to baseURL. Encoding/protocol options match [NewServiceClient].
var NewOrchestratorServiceClient = criteriav1connect.NewOrchestratorServiceClient

// NewOrchestratorServiceHandler builds an HTTP handler from an
// [OrchestratorServiceHandler] implementation. It returns the URL path prefix
// and the handler itself, ready to mount on an http.ServeMux. The handler
// supports Connect, gRPC, and gRPC-Web protocols.
func NewOrchestratorServiceHandler(svc OrchestratorServiceHandler, opts ...connect.HandlerOption) (string, http.Handler) {
	return criteriav1connect.NewOrchestratorServiceHandler(svc, opts...)
}

// NewServiceClient constructs a [ServiceClient] that speaks to baseURL.
// By default it uses the Connect protocol with binary Protobuf encoding.
// Pass connect.WithGRPC() or connect.WithGRPCWeb() to use those protocols.
var NewServiceClient = criteriav1connect.NewCriteriaServiceClient

// NewServiceHandler builds an HTTP handler from a [ServiceHandler] implementation.
// It returns the URL path prefix and the handler itself, ready to mount on an
// http.ServeMux. The handler supports Connect, gRPC, and gRPC-Web protocols.
func NewServiceHandler(svc ServiceHandler, opts ...connect.HandlerOption) (string, http.Handler) {
	return criteriav1connect.NewCriteriaServiceHandler(svc, opts...)
}

// Service name and procedure path constants forwarded from the generated package.
const (
	ServiceName = criteriav1connect.CriteriaServiceName

	RegisterProcedure     = criteriav1connect.CriteriaServiceRegisterProcedure
	HeartbeatProcedure    = criteriav1connect.CriteriaServiceHeartbeatProcedure
	CreateRunProcedure    = criteriav1connect.CriteriaServiceCreateRunProcedure
	ReattachRunProcedure  = criteriav1connect.CriteriaServiceReattachRunProcedure
	ResumeProcedure       = criteriav1connect.CriteriaServiceResumeProcedure
	SubmitEventsProcedure = criteriav1connect.CriteriaServiceSubmitEventsProcedure
	ControlProcedure      = criteriav1connect.CriteriaServiceControlProcedure

	OrchestratorServiceName = criteriav1connect.OrchestratorServiceName

	OrchestratorServiceSubscribeRunEventsProcedure = criteriav1connect.OrchestratorServiceSubscribeRunEventsProcedure
	OrchestratorServiceListActiveRunsProcedure     = criteriav1connect.OrchestratorServiceListActiveRunsProcedure
	OrchestratorServiceCancelRunProcedure          = criteriav1connect.OrchestratorServiceCancelRunProcedure
)
