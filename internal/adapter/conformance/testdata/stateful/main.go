package main

import (
	"context"
	"fmt"
	"sync"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	adapterhost "github.com/brokenbots/criteria-go-adapter-sdk/adapterhost"

	"github.com/brokenbots/criteria/internal/adapterhost/heartbeatutil"
)

// stateful is a conformance test fixture adapter that declares a blob state
// descriptor in its InfoResponse. It is used to verify host-side propagation
// of the adapter state declaration (CRI-201); it performs no persistence of
// its own.
type statefulService struct {
	adapterhost.UnimplementedPermissions
	mu       sync.Mutex
	sessions map[string]struct{}
}

func (s *statefulService) Info(_ context.Context, _ *v2.InfoRequest) (*v2.InfoResponse, error) {
	return &v2.InfoResponse{
		Name:               "stateful",
		Version:            "0.1.0",
		SourceUrl:          "https://github.com/brokenbots/criteria",
		SdkProtocolVersion: "2",
		Platforms:          []string{"linux/amd64", "linux/arm64", "darwin/arm64"},
		Capabilities:       []string{"parallel_safe"},
		State: &v2.StateDescriptor{
			Mode:        "blob",
			Schema:      "stateful.v1",
			MaxBytes:    64 * 1024,
			Granularity: "per-turn",
		},
	}, nil
}

func (s *statefulService) OpenSession(_ context.Context, req *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[req.GetSessionId()] = struct{}{}
	return &v2.OpenSessionResponse{}, nil
}

func (s *statefulService) Execute(ctx context.Context, req *v2.ExecuteRequest, sink adapterhost.ExecuteEventSender) error {
	s.mu.Lock()
	_, ok := s.sessions[req.GetSessionId()]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown session %q", req.GetSessionId())
	}
	return sink.Send(&v2.ExecuteEvent{
		Event: &v2.ExecuteEvent_Result{Result: &v2.ExecuteResult{Outcome: "success"}},
	})
}

func (s *statefulService) Log(ctx context.Context, req *v2.LogRequest, sender adapterhost.LogEventSender) error {
	return heartbeatutil.RunLogHeartbeat(ctx, sender)
}

func (s *statefulService) CloseSession(_ context.Context, req *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, req.GetSessionId())
	return &v2.CloseSessionResponse{}, nil
}

func main() {
	adapterhost.Serve(&statefulService{sessions: map[string]struct{}{}})
}