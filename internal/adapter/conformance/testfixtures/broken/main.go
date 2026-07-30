package main

import (
	"context"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	adapterhost "github.com/brokenbots/criteria-go-adapter-sdk/adapterhost"

	"github.com/brokenbots/criteria/internal/adapterhost/testfixtures/heartbeatutil"
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

func (brokenService) Log(ctx context.Context, _ *v2.LogRequest, sender adapterhost.LogEventSender) error {
	// The log stream must remain open for the lifetime of the session. Returning
	// immediately would stop the SDK heartbeat ticker and break the host's
	// liveness contract. This fixture is intentionally broken only on the
	// outcome-domain contract, so it keeps the stream alive with heartbeats.
	//
	// heartbeatutil.RunLogHeartbeat is a transitional shim: the Go SDK should
	// own session-lifetime heartbeats (see PR #283 Follow-ups). Remove this once
	// the SDK fix lands.
	return heartbeatutil.RunLogHeartbeat(ctx, sender)
}

func (brokenService) CloseSession(context.Context, *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
	return &v2.CloseSessionResponse{}, nil
}

func main() {
	adapterhost.Serve(brokenService{})
}
