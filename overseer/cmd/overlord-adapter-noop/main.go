package main

import (
	"context"

	pluginpkg "github.com/brokenbots/overlord/overseer/internal/plugin"
	pb "github.com/brokenbots/overlord/shared/pb/overlord/v1"
)

type noopService struct{}

func (noopService) Info(context.Context, *pb.InfoRequest) (*pb.InfoResponse, error) {
	return &pb.InfoResponse{Name: "noop", Version: "0.1.0"}, nil
}

func (noopService) OpenSession(context.Context, *pb.OpenSessionRequest) (*pb.OpenSessionResponse, error) {
	return &pb.OpenSessionResponse{}, nil
}

func (noopService) Execute(_ *pb.ExecuteRequest, sink pluginpkg.ExecuteEventSender) error {
	return sink.Send(&pb.ExecuteEvent{
		Event: &pb.ExecuteEvent_Result{Result: &pb.ExecuteResult{Outcome: "success"}},
	})
}

func (noopService) Permit(context.Context, *pb.PermitRequest) (*pb.PermitResponse, error) {
	return &pb.PermitResponse{}, nil
}

func (noopService) CloseSession(context.Context, *pb.CloseSessionRequest) (*pb.CloseSessionResponse, error) {
	return &pb.CloseSessionResponse{}, nil
}

func main() {
	pluginpkg.Serve(noopService{})
}
