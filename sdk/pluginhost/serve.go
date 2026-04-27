package pluginhost

import (
	"context"
	"errors"

	pb "github.com/brokenbots/overseer/sdk/pb/overseer/v1"
	hplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
)

const (
	adapterPluginServiceName        = "overseer.v1.AdapterPluginService"
	adapterPluginInfoMethod         = "/overseer.v1.AdapterPluginService/Info"
	adapterPluginOpenSessionMethod  = "/overseer.v1.AdapterPluginService/OpenSession"
	adapterPluginExecuteMethod      = "/overseer.v1.AdapterPluginService/Execute"
	adapterPluginPermitMethod       = "/overseer.v1.AdapterPluginService/Permit"
	adapterPluginCloseSessionMethod = "/overseer.v1.AdapterPluginService/CloseSession"
)

// Serve starts the adapter plugin process using the shared [HandshakeConfig].
// Call this from your plugin's main() function.
func Serve(impl Service) {
	hplugin.Serve(&hplugin.ServeConfig{
		HandshakeConfig: HandshakeConfig,
		Plugins: map[string]hplugin.Plugin{
			PluginName: &grpcPlugin{Impl: impl},
		},
		GRPCServer: hplugin.DefaultGRPCServer,
	})
}

// grpcPlugin adapts a Service implementation to hashicorp/go-plugin on the
// plugin (server) side. It is intentionally unexported: callers use [Serve].
type grpcPlugin struct {
	hplugin.NetRPCUnsupportedPlugin
	Impl Service
}

func (p *grpcPlugin) GRPCServer(_ *hplugin.GRPCBroker, s *grpc.Server) error {
	if p.Impl == nil {
		return errors.New("adapter plugin implementation is nil")
	}
	s.RegisterService(&adapterPluginServiceDesc, &grpcAdapterServer{impl: p.Impl})
	return nil
}

// GRPCClient is not used in the plugin process; the host-side client lives in
// internal/plugin. This stub satisfies the hplugin.GRPCPlugin interface.
func (p *grpcPlugin) GRPCClient(_ context.Context, _ *hplugin.GRPCBroker, _ *grpc.ClientConn) (interface{}, error) {
	return nil, errors.New("GRPCClient is not implemented in the plugin process")
}

// grpcAdapterServer is the server-side gRPC adapter that delegates to a Service.
type grpcAdapterServer struct {
	impl Service
}

// adapterPluginGRPCServer is the internal interface that the service desc
// handler functions cast to.
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

// grpcExecuteEventServer wraps a grpc.ServerStream to satisfy ExecuteEventSender.
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
