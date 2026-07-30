package main

import (
	"context"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	adapterhost "github.com/brokenbots/criteria-go-adapter-sdk/adapterhost"
)

// nonHeartbeatingService is intentionally broken on the log-stream heartbeat
// contract: its Log implementation returns immediately instead of holding the
// stream open. The conformance harness uses this fixture to prove that a
// non-heartbeating adapter fails the heartbeat suite rather than skipping it.
type nonHeartbeatingService struct {
	adapterhost.UnimplementedPermissions
}

func (nonHeartbeatingService) Info(context.Context, *v2.InfoRequest) (*v2.InfoResponse, error) {
	return &v2.InfoResponse{Name: "nonheartbeating", Version: "0.1.0"}, nil
}

func (nonHeartbeatingService) OpenSession(context.Context, *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	return &v2.OpenSessionResponse{}, nil
}

func (nonHeartbeatingService) Execute(_ context.Context, _ *v2.ExecuteRequest, sink adapterhost.ExecuteEventSender) error {
	return sink.Send(&v2.ExecuteEvent{
		Event: &v2.ExecuteEvent_Result{Result: &v2.ExecuteResult{Outcome: "success"}},
	})
}

func (nonHeartbeatingService) Log(_ context.Context, _ *v2.LogRequest, _ adapterhost.LogEventSender) error {
	// Intentionally returns immediately, breaking the log-stream-liveness
	// contract that the stream must stay open for the lifetime of the session.
	return nil
}

func (nonHeartbeatingService) CloseSession(context.Context, *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
	return &v2.CloseSessionResponse{}, nil
}

func main() {
	adapterhost.Serve(nonHeartbeatingService{})
}
