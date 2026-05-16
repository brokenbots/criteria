package adapterhost

import (
	"context"
	"errors"
	"io"

	hplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// AdapterName is the dispenser key shared between host and adapter process.
// Adapter authors should use sdk/adapterhost.AdapterName; this constant is kept
// here for the host-side loader.
const AdapterName = "adapter"

// These wire-name constants must match the proto service descriptor.
// Validated by TestAdapterPluginWireNames against the compiled descriptor.
const (
	adapterServiceName        = "criteria.v1.AdapterService"
	adapterInfoMethod         = "/criteria.v1.AdapterService/Info"
	adapterOpenSessionMethod  = "/criteria.v1.AdapterService/OpenSession"
	adapterExecuteMethod      = "/criteria.v1.AdapterService/Execute"
	adapterPermitMethod       = "/criteria.v1.AdapterService/Permit"
	adapterCloseSessionMethod = "/criteria.v1.AdapterService/CloseSession"
)

// Client is the host-side typed client returned from go-plugin dispense.
type Client interface {
	Info(context.Context, *pb.InfoRequest) (*pb.InfoResponse, error)
	OpenSession(context.Context, *pb.OpenSessionRequest) (*pb.OpenSessionResponse, error)
	Execute(context.Context, *pb.ExecuteRequest) (ExecuteEventReceiver, error)
	Permit(context.Context, *pb.PermitRequest) (*pb.PermitResponse, error)
	CloseSession(context.Context, *pb.CloseSessionRequest) (*pb.CloseSessionResponse, error)
}

// ExecuteEventReceiver reads Execute stream events from an adapter process.
type ExecuteEventReceiver interface {
	Recv() (*pb.ExecuteEvent, error)
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
	return &grpcAdapterClient{cc: cc}, nil
}

type grpcAdapterClient struct {
	cc *grpc.ClientConn
}

func (c *grpcAdapterClient) Info(ctx context.Context, req *pb.InfoRequest) (*pb.InfoResponse, error) {
	out := new(pb.InfoResponse)
	if err := c.cc.Invoke(ctx, adapterInfoMethod, req, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *grpcAdapterClient) OpenSession(ctx context.Context, req *pb.OpenSessionRequest) (*pb.OpenSessionResponse, error) {
	out := new(pb.OpenSessionResponse)
	if err := c.cc.Invoke(ctx, adapterOpenSessionMethod, req, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *grpcAdapterClient) Execute(ctx context.Context, req *pb.ExecuteRequest) (ExecuteEventReceiver, error) {
	sd := &grpc.StreamDesc{ServerStreams: true}
	stream, err := c.cc.NewStream(ctx, sd, adapterExecuteMethod)
	if err != nil {
		return nil, err
	}
	if err := stream.SendMsg(req); err != nil {
		return nil, err
	}
	if err := stream.CloseSend(); err != nil {
		return nil, err
	}
	return &grpcExecuteEventClient{stream: stream}, nil
}

func (c *grpcAdapterClient) Permit(ctx context.Context, req *pb.PermitRequest) (*pb.PermitResponse, error) {
	out := new(pb.PermitResponse)
	if err := c.cc.Invoke(ctx, adapterPermitMethod, req, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *grpcAdapterClient) CloseSession(ctx context.Context, req *pb.CloseSessionRequest) (*pb.CloseSessionResponse, error) {
	out := new(pb.CloseSessionResponse)
	if err := c.cc.Invoke(ctx, adapterCloseSessionMethod, req, out); err != nil {
		return nil, err
	}
	return out, nil
}

type grpcExecuteEventClient struct {
	stream grpc.ClientStream
}

func (c *grpcExecuteEventClient) Recv() (*pb.ExecuteEvent, error) {
	out := new(pb.ExecuteEvent)
	if err := c.stream.RecvMsg(out); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		return nil, err
	}
	return out, nil
}
