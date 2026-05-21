package adapterhost

import (
	"context"
	"errors"
	"io"

	hplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	v2 "github.com/brokenbots/criteria/proto/criteria/v2"
)

// AdapterName is the dispenser key shared between host and adapter process.
// Adapter authors should use sdk/adapterhost.AdapterName; this constant is kept
// here for the host-side loader.
const AdapterName = "adapter"

// Client is the host-side typed client returned from go-plugin dispense.
// All methods correspond 1:1 with the v2 AdapterService RPC surface.
type Client interface {
	Info(ctx context.Context, req *v2.InfoRequest) (*v2.InfoResponse, error)
	OpenSession(ctx context.Context, req *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error)
	// Execute drives the server-streaming Execute RPC, dispatching each event
	// to sink.Emit. It returns nil when the stream closes cleanly (after the
	// adapter has sent an ExecuteResult event).
	Execute(ctx context.Context, req *v2.ExecuteRequest, sink ExecuteEventSink) error
	// Log drives the server-streaming Log RPC, dispatching each log event to
	// sink.Emit. Callers typically invoke this concurrently with Execute.
	Log(ctx context.Context, req *v2.LogRequest, sink LogEventSink) error
	// Permissions drives the bidi Permissions RPC. The caller sends
	// PermissionEvent messages on requests and reads PermissionDecision
	// messages from decisions. Closing requests causes CloseSend; the call
	// returns when the response stream is exhausted.
	Permissions(ctx context.Context, requests <-chan *v2.PermissionEvent, decisions chan<- *v2.PermissionDecision) error
	Pause(ctx context.Context, req *v2.PauseRequest) (*v2.PauseResponse, error)
	Resume(ctx context.Context, req *v2.ResumeRequest) (*v2.ResumeResponse, error)
	Snapshot(ctx context.Context, req *v2.SnapshotRequest) (*v2.SnapshotResponse, error)
	Restore(ctx context.Context, req *v2.RestoreRequest) (*v2.RestoreResponse, error)
	Inspect(ctx context.Context, req *v2.InspectRequest) (*v2.InspectResponse, error)
	CloseSession(ctx context.Context, req *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error)
}

// ExecuteEventSink receives events from the adapter's Execute RPC stream.
// Emit is called once per received message; it must not be called concurrently.
type ExecuteEventSink interface {
	Emit(*v2.ExecuteEvent) error
}

// LogEventSink receives events from the adapter's Log RPC stream.
// Emit is called once per received message; it must not be called concurrently.
type LogEventSink interface {
	Emit(*v2.LogEvent) error
}

// GRPCAdapter is the host-side go-plugin adapter for the Criteria adapter
// protocol. It only implements GRPCClient; GRPCServer is a no-op stub because
// the host never acts as an adapter server.
type GRPCAdapter struct {
	hplugin.NetRPCUnsupportedPlugin
}

// AdapterMap returns the host-side adapter registry map used when creating a
// go-plugin client.
func AdapterMap() map[string]hplugin.Plugin {
	return map[string]hplugin.Plugin{AdapterName: &GRPCAdapter{}}
}

func (p *GRPCAdapter) GRPCServer(_ *hplugin.GRPCBroker, _ *grpc.Server) error {
	return errors.New("GRPCServer should not be called on the Criteria host")
}

func (p *GRPCAdapter) GRPCClient(_ context.Context, _ *hplugin.GRPCBroker, cc *grpc.ClientConn) (interface{}, error) {
	return &grpcClient{c: v2.NewAdapterServiceClient(cc)}, nil
}

type grpcClient struct {
	c v2.AdapterServiceClient
}

func (g *grpcClient) Info(ctx context.Context, req *v2.InfoRequest) (*v2.InfoResponse, error) {
	return g.c.Info(ctx, req)
}

func (g *grpcClient) OpenSession(ctx context.Context, req *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	return g.c.OpenSession(ctx, req)
}

func (g *grpcClient) Execute(ctx context.Context, req *v2.ExecuteRequest, sink ExecuteEventSink) error {
	stream, err := g.c.Execute(ctx, req)
	if err != nil {
		return err
	}
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := sink.Emit(ev); err != nil {
			return err
		}
	}
}

func (g *grpcClient) Log(ctx context.Context, req *v2.LogRequest, sink LogEventSink) error {
	stream, err := g.c.Log(ctx, req)
	if err != nil {
		return err
	}
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := sink.Emit(ev); err != nil {
			return err
		}
	}
}

func (g *grpcClient) Permissions(ctx context.Context, requests <-chan *v2.PermissionEvent, decisions chan<- *v2.PermissionDecision) error {
	stream, err := g.c.Permissions(ctx)
	if err != nil {
		return err
	}
	sendDone := make(chan error, 1)
	go func() {
		for req := range requests {
			if err := stream.Send(req); err != nil {
				sendDone <- err
				return
			}
		}
		sendDone <- stream.CloseSend()
	}()
	for {
		dec, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		select {
		case decisions <- dec:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return <-sendDone
}

func (g *grpcClient) Pause(ctx context.Context, req *v2.PauseRequest) (*v2.PauseResponse, error) {
	return g.c.Pause(ctx, req)
}

func (g *grpcClient) Resume(ctx context.Context, req *v2.ResumeRequest) (*v2.ResumeResponse, error) {
	return g.c.Resume(ctx, req)
}

func (g *grpcClient) Snapshot(ctx context.Context, req *v2.SnapshotRequest) (*v2.SnapshotResponse, error) {
	return g.c.Snapshot(ctx, req)
}

func (g *grpcClient) Restore(ctx context.Context, req *v2.RestoreRequest) (*v2.RestoreResponse, error) {
	return g.c.Restore(ctx, req)
}

func (g *grpcClient) Inspect(ctx context.Context, req *v2.InspectRequest) (*v2.InspectResponse, error) {
	return g.c.Inspect(ctx, req)
}

func (g *grpcClient) CloseSession(ctx context.Context, req *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
	return g.c.CloseSession(ctx, req)
}
