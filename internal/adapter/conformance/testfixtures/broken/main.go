package main

import (
"context"

adapterhost "github.com/brokenbots/criteria/sdk/adapterhost"
v2 "github.com/brokenbots/criteria/proto/criteria/v2"
)

type brokenService struct {
adapterhost.UnimplementedPermissions
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

func (brokenService) Pause(_ context.Context, _ *v2.PauseRequest) (*v2.PauseResponse, error) {
return &v2.PauseResponse{}, nil
}

func (brokenService) Resume(_ context.Context, _ *v2.ResumeRequest) (*v2.ResumeResponse, error) {
return &v2.ResumeResponse{}, nil
}

func (brokenService) Snapshot(_ context.Context, _ *v2.SnapshotRequest) (*v2.SnapshotResponse, error) {
return &v2.SnapshotResponse{}, nil
}

func (brokenService) Restore(_ context.Context, _ *v2.RestoreRequest) (*v2.RestoreResponse, error) {
return &v2.RestoreResponse{}, nil
}

func (brokenService) Inspect(_ context.Context, _ *v2.InspectRequest) (*v2.InspectResponse, error) {
return &v2.InspectResponse{}, nil
}

func (brokenService) CloseSession(context.Context, *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
return &v2.CloseSessionResponse{}, nil
}

func main() {
adapterhost.Serve(brokenService{})
}
