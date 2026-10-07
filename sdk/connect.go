package criteria

import (
	"net/http"
	"strings"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

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
func NewOrchestratorServiceClient(httpClient connecthttp.HTTPClient, baseURL string, opts ...connecthttp.Option) OrchestratorServiceClient {
	transport := connecthttp.NewTransport(httpClient, baseURL, opts...)
	return criteriav1connect.NewOrchestratorServiceClient(connect.NewClient(transport))
}

// NewOrchestratorServiceHandler builds an HTTP handler for the
// OrchestratorService from an [OrchestratorServiceHandler] implementation. It
// returns the URL path prefix ("/criteria.v1.OrchestratorService/") and the
// handler itself, ready to mount on an http.ServeMux. The handler supports
// Connect, gRPC, and gRPC-Web protocols.
func NewOrchestratorServiceHandler(svc OrchestratorServiceHandler, opts ...connecthttp.Option) (string, http.Handler) {
	server := connect.NewServer()
	criteriav1connect.RegisterOrchestratorServiceHandler(server, svc)
	subtree := serviceSubtree(criteriav1connect.OrchestratorServiceSubscribeRunEventsProcedure)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server, opts...)
	return subtree, mux
}

// NewServiceClient constructs a [ServiceClient] that speaks to baseURL.
// By default it uses the Connect protocol with binary Protobuf encoding.
// Pass connecthttp.WithGRPC() or connecthttp.WithGRPCWeb() to use those protocols.
func NewServiceClient(httpClient connecthttp.HTTPClient, baseURL string, opts ...connecthttp.Option) ServiceClient {
	transport := connecthttp.NewTransport(httpClient, baseURL, opts...)
	return criteriav1connect.NewCriteriaServiceClient(connect.NewClient(transport))
}

// NewServiceHandler builds an HTTP handler for the CriteriaService from a
// [ServiceHandler] implementation. It returns the URL path prefix
// ("/criteria.v1.CriteriaService/") and the handler itself, ready to mount on
// an http.ServeMux. The handler supports Connect, gRPC, and gRPC-Web protocols.
func NewServiceHandler(svc ServiceHandler, opts ...connecthttp.Option) (string, http.Handler) {
	server := connect.NewServer()
	criteriav1connect.RegisterCriteriaServiceHandler(server, svc)
	subtree := serviceSubtree(criteriav1connect.CriteriaServiceRegisterProcedure)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server, opts...)
	return subtree, mux
}

// serviceSubtree converts a procedure path ("/pkg.Service/Method") into the
// service's subtree pattern ("/pkg.Service/") for http.ServeMux mounting.
func serviceSubtree(procedure string) string {
	idx := strings.LastIndexByte(procedure, '/')
	if idx < 0 {
		return procedure
	}
	return procedure[:idx+1]
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
