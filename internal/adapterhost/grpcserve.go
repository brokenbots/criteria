package adapterhost

import (
	"context"
	"sync"

	"google.golang.org/grpc"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
)

// NewAdapterServiceServer adapts a host-side Client to the generated
// criteria v2 AdapterServiceServer interface, so the FULL client surface can
// be served back out over a gRPC server (the phone-home model: the criteria
// host dials in and becomes the gRPC client of the local adapter process).
// The bridge serializes stream sends (grpc streams are not goroutine-safe)
// and wraps the backend Log stream with adapter protocol heartbeats.
//
// The same bridge serves both the remote runner
// (cmd/criteria-adapter-remote-runner) and the criteria peer
// (internal/peer/serve.go); previously the runner carried a private copy.
func NewAdapterServiceServer(impl Client) v2.AdapterServiceServer {
	return &adapterServiceBridge{impl: impl}
}

// RegisterAdapterService registers the v2 AdapterService backed by impl on a
// gRPC server, extending the generated ServiceDesc with the dynamically
// encoded Prompt method (promptwire.go). This is the registration the runner
// and the peer both use for their phone-home server.
func RegisterAdapterService(srv *grpc.Server, impl Client) {
	desc := AdapterServiceDescWithPrompt()
	srv.RegisterService(desc, NewAdapterServiceServer(impl))
}

// AdapterServiceDescWithPrompt returns an extended copy of the generated
// AdapterService ServiceDesc carrying the Prompt method descriptor. The
// original generated descriptor is not mutated.
func AdapterServiceDescWithPrompt() *grpc.ServiceDesc {
	methods := make([]grpc.MethodDesc, 0, len(v2.AdapterService_ServiceDesc.Methods)+1)
	methods = append(methods, v2.AdapterService_ServiceDesc.Methods...)
	methods = append(methods, PromptMethodDesc())
	desc := v2.AdapterService_ServiceDesc
	desc.Methods = methods
	return &desc
}

// adapterServiceBridge forwards every AdapterService RPC to the local adapter
// process client. It implements the full internal Client surface, including
// Prompt through the dynamic promptwire encoding (PromptService).
type adapterServiceBridge struct {
	v2.UnimplementedAdapterServiceServer
	impl Client
}

var (
	_ v2.AdapterServiceServer = (*adapterServiceBridge)(nil)
	_ PromptService           = (*adapterServiceBridge)(nil)
)

func (b *adapterServiceBridge) Info(ctx context.Context, req *v2.InfoRequest) (*v2.InfoResponse, error) {
	return b.impl.Info(ctx, req)
}

func (b *adapterServiceBridge) OpenSession(ctx context.Context, req *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	return b.impl.OpenSession(ctx, req)
}

func (b *adapterServiceBridge) Execute(req *v2.ExecuteRequest, stream v2.AdapterService_ExecuteServer) error {
	return b.impl.Execute(stream.Context(), req, &bridgeExecuteEventServer{stream: stream})
}

func (b *adapterServiceBridge) Log(req *v2.LogRequest, stream v2.AdapterService_LogServer) error {
	sender := &bridgeLogEventServer{stream: stream}
	go func() {
		_ = v2.RunHeartbeat(stream.Context(), "log", func(hb *v2.Heartbeat) error {
			return sender.Emit(&v2.LogEvent{Heartbeat: hb})
		})
	}()

	errCh := make(chan error, 1)
	go func() { errCh <- b.impl.Log(stream.Context(), req, sender) }()

	err := <-errCh
	if err != nil {
		return err
	}
	// The host holds the Log stream open for the lifetime of the session and
	// cancels it when the session closes; keep the RPC open until then so
	// the host never treats a clean backend return as an adapter death.
	<-stream.Context().Done()
	return nil
}

func (b *adapterServiceBridge) Permissions(stream v2.AdapterService_PermissionsServer) error {
	ctx := stream.Context()
	requests := make(chan *v2.PermissionEvent, 64)
	go func() {
		defer close(requests)
		for {
			ev, err := stream.Recv()
			if err != nil {
				return
			}
			select {
			case requests <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()

	err := b.impl.Permissions(ctx, requests)
	// Unblock a pump still feeding the channel after an early backend return;
	// no-op when the backend drained requests until close.
	for range requests {
	}
	return err
}

func (b *adapterServiceBridge) Pause(ctx context.Context, req *v2.PauseRequest) (*v2.PauseResponse, error) {
	return b.impl.Pause(ctx, req)
}

func (b *adapterServiceBridge) Resume(ctx context.Context, req *v2.ResumeRequest) (*v2.ResumeResponse, error) {
	return b.impl.Resume(ctx, req)
}

func (b *adapterServiceBridge) Snapshot(ctx context.Context, req *v2.SnapshotRequest) (*v2.SnapshotResponse, error) {
	return b.impl.Snapshot(ctx, req)
}

func (b *adapterServiceBridge) Restore(ctx context.Context, req *v2.RestoreRequest) (*v2.RestoreResponse, error) {
	return b.impl.Restore(ctx, req)
}

func (b *adapterServiceBridge) Inspect(ctx context.Context, req *v2.InspectRequest) (*v2.InspectResponse, error) {
	return b.impl.Inspect(ctx, req)
}

func (b *adapterServiceBridge) CloseSession(ctx context.Context, req *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
	return b.impl.CloseSession(ctx, req)
}

// Prompt implements PromptService (ADR-0006 D3). promptDispatch finds it on
// the registered server implementation via the extended ServiceDesc.
func (b *adapterServiceBridge) Prompt(ctx context.Context, req *PromptRequest) (*PromptResponse, error) {
	return b.impl.Prompt(ctx, req)
}

// bridgeExecuteEventServer serializes Emit calls onto the Execute server
// stream; the backend's ExecuteEventSink contract forbids concurrent Emit.
type bridgeExecuteEventServer struct {
	mu     sync.Mutex
	stream v2.AdapterService_ExecuteServer
}

func (s *bridgeExecuteEventServer) Emit(evt *v2.ExecuteEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream.Send(evt)
}

// bridgeLogEventServer serializes Emit calls onto the Log server stream;
// heartbeats and log events share the stream.
type bridgeLogEventServer struct {
	mu     sync.Mutex
	stream v2.AdapterService_LogServer
}

func (s *bridgeLogEventServer) Emit(evt *v2.LogEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream.Send(evt)
}