package main

import (
	"context"

	v2 "github.com/brokenbots/criteria/proto/criteria/v2"
	adapterhost "github.com/brokenbots/criteria/sdk/adapterhost"
)

type brokenService struct {
	adapterhost.UnimplementedPermissions
	adapterhost.UnimplementedLifecycle
}

func (brokenService) Info(context.Context, *v2.InfoRequest) (*v2.InfoResponse, error) {
	return &v2.InfoResponse{Name: "broken", Version: "0.1.0"}, nil
}

func (brokenService) OpenSession(context.Context, *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	return &v2.OpenSessionResponse{}, nil
}

func (brokenService) Execute(_ context.Context, _ *v2.ExecuteRequest, sink adapterhost.ExecuteEventSender) error {
	return sink.Send(&v2.ExecuteEvent{
		Event: &v2.ExecuteEvent_Result{Result: &v2.ExecuteResult{Outcome: ""}},
	})
}

func (brokenService) Log(_ context.Context, _ *v2.LogRequest, _ adapterhost.LogEventSender) error {
	return nil
}

func (brokenService) CloseSession(context.Context, *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
	return &v2.CloseSessionResponse{}, nil
}

func main() {
	adapterhost.Serve(brokenService{})
}
