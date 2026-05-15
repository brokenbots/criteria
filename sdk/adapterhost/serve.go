package adapterhost

import (
	"context"
	"errors"
	"sync"

	hplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

const (
	adapterServiceName        = "criteria.v1.AdapterService"
	adapterInfoMethod         = "/criteria.v1.AdapterService/Info"
	adapterOpenSessionMethod  = "/criteria.v1.AdapterService/OpenSession"
	adapterExecuteMethod      = "/criteria.v1.AdapterService/Execute"
	adapterPermitMethod       = "/criteria.v1.AdapterService/Permit"
	adapterCloseSessionMethod = "/criteria.v1.AdapterService/CloseSession"
)

// Serve starts the adapter plugin process using the shared [HandshakeConfig].
// Call this from your plugin's main() function.
func Serve(impl Service) {
	hplugin.Serve(&hplugin.ServeConfig{
		HandshakeConfig: HandshakeConfig,
		Plugins: map[string]hplugin.Plugin{
			AdapterName: &grpcAdapter{Impl: impl},
		},
		GRPCServer: hplugin.DefaultGRPCServer,
	})
}

// grpcAdapter adapts a Service implementation to hashicorp/go-plugin on the
// plugin (server) side. It is intentionally unexported: callers use [Serve].
type grpcAdapter struct {
	hplugin.NetRPCUnsupportedPlugin
	Impl Service
}

func (p *grpcAdapter) GRPCServer(_ *hplugin.GRPCBroker, s *grpc.Server) error {
	if p.Impl == nil {
		return errors.New("adapter plugin implementation is nil")
	}
	s.RegisterService(&adapterServiceDesc, &grpcAdapterServer{impl: p.Impl})
	return nil
}

// GRPCClient is not used in the plugin process; the host-side client lives in
// internal/adapterhost. This stub satisfies the hplugin.GRPCPlugin interface.
func (p *grpcAdapter) GRPCClient(_ context.Context, _ *hplugin.GRPCBroker, _ *grpc.ClientConn) (interface{}, error) {
	return nil, errors.New("GRPCClient is not implemented in the plugin process")
}

// grpcAdapterServer is the server-side gRPC adapter that delegates to a Service.
type grpcAdapterServer struct {
	impl Service
}

// adapterGRPCServer is the internal interface that the service desc
// handler functions cast to.
type adapterGRPCServer interface {
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
// Send is safe for concurrent use: adapters may call it from event-handler
// goroutines concurrently with the main Execute goroutine. The mutex serialises
// all SendMsg calls because grpc.ServerStream.SendMsg is not goroutine-safe.
type grpcExecuteEventServer struct {
	mu     sync.Mutex
	stream grpc.ServerStream
}

func (s *grpcExecuteEventServer) Send(evt *pb.ExecuteEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream.SendMsg(evt)
}

var adapterServiceDesc = grpc.ServiceDesc{
	ServiceName: adapterServiceName,
	HandlerType: (*adapterGRPCServer)(nil),
	Methods: []grpc.MethodDesc{
		{MethodName: "Info", Handler: adapterInfoHandler},
		{MethodName: "OpenSession", Handler: adapterOpenSessionHandler},
		{MethodName: "Permit", Handler: adapterPermitHandler},
		{MethodName: "CloseSession", Handler: adapterCloseSessionHandler},
	},
	Streams: []grpc.StreamDesc{
		{StreamName: "Execute", Handler: adapterExecuteHandler, ServerStreams: true},
	},
}

func adapterInfoHandler(srv interface{}, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(pb.InfoRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	handler := func(ctx context.Context, req any) (any, error) {
		return srv.(adapterGRPCServer).Info(ctx, req.(*pb.InfoRequest))
	}
	if interceptor == nil {
		return handler(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: adapterInfoMethod}
	return interceptor(ctx, in, info, handler)
}

func adapterOpenSessionHandler(srv interface{}, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(pb.OpenSessionRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	handler := func(ctx context.Context, req any) (any, error) {
		return srv.(adapterGRPCServer).OpenSession(ctx, req.(*pb.OpenSessionRequest))
	}
	if interceptor == nil {
		return handler(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: adapterOpenSessionMethod}
	return interceptor(ctx, in, info, handler)
}

func adapterPermitHandler(srv interface{}, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(pb.PermitRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	handler := func(ctx context.Context, req any) (any, error) {
		return srv.(adapterGRPCServer).Permit(ctx, req.(*pb.PermitRequest))
	}
	if interceptor == nil {
		return handler(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: adapterPermitMethod}
	return interceptor(ctx, in, info, handler)
}

func adapterCloseSessionHandler(srv interface{}, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(pb.CloseSessionRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	handler := func(ctx context.Context, req any) (any, error) {
		return srv.(adapterGRPCServer).CloseSession(ctx, req.(*pb.CloseSessionRequest))
	}
	if interceptor == nil {
		return handler(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: adapterCloseSessionMethod}
	return interceptor(ctx, in, info, handler)
}

func adapterExecuteHandler(srv interface{}, stream grpc.ServerStream) error {
	in := new(pb.ExecuteRequest)
	if err := stream.RecvMsg(in); err != nil {
		return err
	}
	return srv.(adapterGRPCServer).Execute(stream.Context(), in, &grpcExecuteEventServer{stream: stream})
}
