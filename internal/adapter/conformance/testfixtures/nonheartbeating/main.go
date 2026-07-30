package main

import (
	"context"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	adapterhost "github.com/brokenbots/criteria-go-adapter-sdk/adapterhost"
)

// nonHeartbeatingService is intentionally broken on the log-stream heartbeat
// contract: its Log implementation returns immediately instead of holding the
// stream open. The conformance harness uses this fixture to prove that a
// non-heartbeating adapter fails the heartbeat suite rather than skipping it.
//
// The fixture otherwise behaves correctly: it tracks open sessions so that
// Execute rejects a closed session and only the heartbeats suite fails.
type nonHeartbeatingService struct {
	adapterhost.UnimplementedPermissions
	mu       sync.RWMutex
	sessions map[string]struct{}
}

func (s *nonHeartbeatingService) Info(context.Context, *v2.InfoRequest) (*v2.InfoResponse, error) {
	return &v2.InfoResponse{Name: "nonheartbeating", Version: "0.1.0"}, nil
}

func (s *nonHeartbeatingService) OpenSession(_ context.Context, req *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions == nil {
		s.sessions = make(map[string]struct{})
	}
	s.sessions[req.GetSessionId()] = struct{}{}
	return &v2.OpenSessionResponse{}, nil
}

func (s *nonHeartbeatingService) Execute(_ context.Context, req *v2.ExecuteRequest, sink adapterhost.ExecuteEventSender) error {
	s.mu.RLock()
	_, ok := s.sessions[req.GetSessionId()]
	s.mu.RUnlock()
	if !ok {
		return status.Error(codes.FailedPrecondition, "session is not open")
	}
	return sink.Send(&v2.ExecuteEvent{
		Event: &v2.ExecuteEvent_Result{Result: &v2.ExecuteResult{Outcome: "success"}},
	})
}

func (s *nonHeartbeatingService) Log(_ context.Context, _ *v2.LogRequest, _ adapterhost.LogEventSender) error {
	// Intentionally returns immediately, breaking the log-stream-liveness
	// contract that the stream must stay open for the lifetime of the session.
	return nil
}

func (s *nonHeartbeatingService) CloseSession(_ context.Context, req *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, req.GetSessionId())
	return &v2.CloseSessionResponse{}, nil
}

func main() {
	adapterhost.Serve(&nonHeartbeatingService{})
}
