package main

import (
	"context"

	pluginhost "github.com/brokenbots/overseer/sdk/pluginhost"
	pb "github.com/brokenbots/overseer/sdk/pb/overseer/v1"
)

type brokenService struct{}

func (brokenService) Info(context.Context, *pb.InfoRequest) (*pb.InfoResponse, error) {
	return &pb.InfoResponse{Name: "broken", Version: "0.1.0"}, nil
}

func (brokenService) OpenSession(context.Context, *pb.OpenSessionRequest) (*pb.OpenSessionResponse, error) {
	return &pb.OpenSessionResponse{}, nil
}

func (brokenService) Execute(_ context.Context, _ *pb.ExecuteRequest, sink pluginhost.ExecuteEventSender) error {
	return sink.Send(&pb.ExecuteEvent{
		Event: &pb.ExecuteEvent_Result{Result: &pb.ExecuteResult{Outcome: ""}},
	})
}

func (brokenService) Permit(context.Context, *pb.PermitRequest) (*pb.PermitResponse, error) {
	return &pb.PermitResponse{}, nil
}

func (brokenService) CloseSession(context.Context, *pb.CloseSessionRequest) (*pb.CloseSessionResponse, error) {
	return &pb.CloseSessionResponse{}, nil
}

func main() {
	pluginhost.Serve(brokenService{})
}
