package plugin

import (
	"context"
	"errors"
	"io"

	pb "github.com/brokenbots/overseer/sdk/pb/v1"
	hplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
)

const (
	// PluginName is the dispenser key shared between host and plugin process.
	PluginName = "adapter"

	adapterPluginServiceName        = "overseer.v1.AdapterPluginService"
	adapterPluginInfoMethod         = "/overseer.v1.AdapterPluginService/Info"
	adapterPluginOpenSessionMethod  = "/overseer.v1.AdapterPluginService/OpenSession"
	adapterPluginExecuteMethod      = "/overseer.v1.AdapterPluginService/Execute"
	adapterPluginPermitMethod       = "/overseer.v1.AdapterPluginService/Permit"
	adapterPluginCloseSessionMethod = "/overseer.v1.AdapterPluginService/CloseSession"
)

// Service is the host-facing contract for an adapter plugin implementation.
type Service interface {
	Info(context.Context, *pb.InfoRequest) (*pb.InfoResponse, error)
	OpenSession(context.Context, *pb.OpenSessionRequest) (*pb.OpenSessionResponse, error)
	Execute(context.Context, *pb.ExecuteRequest, ExecuteEventSender) error
	Permit(context.Context, *pb.PermitRequest) (*pb.PermitResponse, error)
	CloseSession(context.Context, *pb.CloseSessionRequest) (*pb.CloseSessionResponse, error)
}

// Client is the host-side typed client returned from go-plugin dispense.
type Client interface {
	Info(context.Context, *pb.InfoRequest) (*pb.InfoResponse, error)
	OpenSession(context.Context, *pb.OpenSessionRequest) (*pb.OpenSessionResponse, error)
	Execute(context.Context, *pb.ExecuteRequest) (ExecuteEventReceiver, error)
	Permit(context.Context, *pb.PermitRequest) (*pb.PermitResponse, error)
	CloseSession(context.Context, *pb.CloseSessionRequest) (*pb.CloseSessionResponse, error)
}

// ExecuteEventSender pushes Execute stream events from plugin to host.
type ExecuteEventSender interface {
	Send(*pb.ExecuteEvent) error
}

// ExecuteEventReceiver reads Execute stream events from plugin.
type ExecuteEventReceiver interface {
	Recv() (*pb.ExecuteEvent, error)
}

// GRPCPlugin adapts Service implementations to hashicorp/go-plugin.
type GRPCPlugin struct {
	hplugin.NetRPCUnsupportedPlugin
	Impl Service
}

// PluginMap returns the plugin registry map shared by host and plugin.
func PluginMap(impl Service) map[string]hplugin.Plugin {
	return map[string]hplugin.Plugin{PluginName: &GRPCPlugin{Impl: impl}}
}

// Serve starts a plugin process using the shared handshake and plugin map.
func Serve(impl Service) {
	hplugin.Serve(&hplugin.ServeConfig{
		HandshakeConfig: HandshakeConfig,
		Plugins:         PluginMap(impl),
		GRPCServer:      hplugin.DefaultGRPCServer,
	})
}

func (p *GRPCPlugin) GRPCServer(_ *hplugin.GRPCBroker, s *grpc.Server) error {
	if p.Impl == nil {
		return errors.New("adapter plugin implementation is nil")
	}
	s.RegisterService(&adapterPluginServiceDesc, &grpcAdapterServer{impl: p.Impl})
	return nil
}

func (p *GRPCPlugin) GRPCClient(_ context.Context, _ *hplugin.GRPCBroker, cc *grpc.ClientConn) (interface{}, error) {
	return &grpcAdapterClient{cc: cc}, nil
}

type grpcAdapterServer struct {
	impl Service
}

type adapterPluginGRPCServer interface {
	Info(context.Context, *pb.InfoRequest) (*pb.InfoResponse, error)
	OpenSession(context.Context, *pb.OpenSessionRequest) (*pb.OpenSessionResponse, error)
	Execute(context.Context, *pb.ExecuteRequest, ExecuteEventSender) error
	Permit(context.Context, *pb.PermitRequest) (*pb.PermitResponse, error)
	CloseSession(context.Context, *pb.CloseSessionRequest) (*pb.CloseSessionResponse, error)
}

func (s *grpcAdapterServer) Info(ctx context.Context, req *pb.InfoRequest) (*pb.InfoResponse, error) {
	return s.impl.Info(ctx, req)
}

func (s *grpcAdapterServer) OpenSession(ctx context.Context, req *pb.OpenSessionRequest) (*pb.OpenSessionResponse, error) {
	return s.impl.OpenSession(ctx, req)
}

func (s *grpcAdapterServer) Execute(ctx context.Context, req *pb.ExecuteRequest, sink ExecuteEventSender) error {
	return s.impl.Execute(ctx, req, sink)
}

func (s *grpcAdapterServer) Permit(ctx context.Context, req *pb.PermitRequest) (*pb.PermitResponse, error) {
	return s.impl.Permit(ctx, req)
}

func (s *grpcAdapterServer) CloseSession(ctx context.Context, req *pb.CloseSessionRequest) (*pb.CloseSessionResponse, error) {
	return s.impl.CloseSession(ctx, req)
}

type grpcExecuteEventServer struct {
	stream grpc.ServerStream
}

func (s *grpcExecuteEventServer) Send(evt *pb.ExecuteEvent) error {
	return s.stream.SendMsg(evt)
}

var adapterPluginServiceDesc = grpc.ServiceDesc{
	ServiceName: adapterPluginServiceName,
	HandlerType: (*adapterPluginGRPCServer)(nil),
	Methods: []grpc.MethodDesc{
		{MethodName: "Info", Handler: adapterPluginInfoHandler},
		{MethodName: "OpenSession", Handler: adapterPluginOpenSessionHandler},
		{MethodName: "Permit", Handler: adapterPluginPermitHandler},
		{MethodName: "CloseSession", Handler: adapterPluginCloseSessionHandler},
	},
	Streams: []grpc.StreamDesc{
		{StreamName: "Execute", Handler: adapterPluginExecuteHandler, ServerStreams: true},
	},
}

func adapterPluginInfoHandler(srv interface{}, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(pb.InfoRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	handler := func(ctx context.Context, req any) (any, error) {
		return srv.(adapterPluginGRPCServer).Info(ctx, req.(*pb.InfoRequest))
	}
	if interceptor == nil {
		return handler(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: adapterPluginInfoMethod}
	return interceptor(ctx, in, info, handler)
}

func adapterPluginOpenSessionHandler(srv interface{}, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(pb.OpenSessionRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	handler := func(ctx context.Context, req any) (any, error) {
		return srv.(adapterPluginGRPCServer).OpenSession(ctx, req.(*pb.OpenSessionRequest))
	}
	if interceptor == nil {
		return handler(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: adapterPluginOpenSessionMethod}
	return interceptor(ctx, in, info, handler)
}

func adapterPluginPermitHandler(srv interface{}, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(pb.PermitRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	handler := func(ctx context.Context, req any) (any, error) {
		return srv.(adapterPluginGRPCServer).Permit(ctx, req.(*pb.PermitRequest))
	}
	if interceptor == nil {
		return handler(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: adapterPluginPermitMethod}
	return interceptor(ctx, in, info, handler)
}

func adapterPluginCloseSessionHandler(srv interface{}, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(pb.CloseSessionRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	handler := func(ctx context.Context, req any) (any, error) {
		return srv.(adapterPluginGRPCServer).CloseSession(ctx, req.(*pb.CloseSessionRequest))
	}
	if interceptor == nil {
		return handler(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: adapterPluginCloseSessionMethod}
	return interceptor(ctx, in, info, handler)
}

func adapterPluginExecuteHandler(srv interface{}, stream grpc.ServerStream) error {
	in := new(pb.ExecuteRequest)
	if err := stream.RecvMsg(in); err != nil {
		return err
	}
	return srv.(adapterPluginGRPCServer).Execute(stream.Context(), in, &grpcExecuteEventServer{stream: stream})
}

type grpcAdapterClient struct {
	cc *grpc.ClientConn
}

func (c *grpcAdapterClient) Info(ctx context.Context, req *pb.InfoRequest) (*pb.InfoResponse, error) {
	out := new(pb.InfoResponse)
	if err := c.cc.Invoke(ctx, adapterPluginInfoMethod, req, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *grpcAdapterClient) OpenSession(ctx context.Context, req *pb.OpenSessionRequest) (*pb.OpenSessionResponse, error) {
	out := new(pb.OpenSessionResponse)
	if err := c.cc.Invoke(ctx, adapterPluginOpenSessionMethod, req, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *grpcAdapterClient) Execute(ctx context.Context, req *pb.ExecuteRequest) (ExecuteEventReceiver, error) {
	sd := &grpc.StreamDesc{ServerStreams: true}
	stream, err := c.cc.NewStream(ctx, sd, adapterPluginExecuteMethod)
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
	if err := c.cc.Invoke(ctx, adapterPluginPermitMethod, req, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *grpcAdapterClient) CloseSession(ctx context.Context, req *pb.CloseSessionRequest) (*pb.CloseSessionResponse, error) {
	out := new(pb.CloseSessionResponse)
	if err := c.cc.Invoke(ctx, adapterPluginCloseSessionMethod, req, out); err != nil {
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
