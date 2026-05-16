package main

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	adapterhost "github.com/brokenbots/criteria/sdk/adapterhost"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

type noopService struct {
	mu       sync.Mutex
	sessions map[string]struct{}
}

func (s *noopService) Info(context.Context, *pb.InfoRequest) (*pb.InfoResponse, error) {
	return &pb.InfoResponse{
		Name:         "noop",
		Version:      "0.1.0",
		Capabilities: []string{"parallel_safe"},
	}, nil
}

func (s *noopService) OpenSession(_ context.Context, request *pb.OpenSessionRequest) (*pb.OpenSessionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions == nil {
		s.sessions = map[string]struct{}{}
	}
	s.sessions[request.GetSessionId()] = struct{}{}
	return &pb.OpenSessionResponse{}, nil
}

func (s *noopService) Execute(ctx context.Context, request *pb.ExecuteRequest, sink adapterhost.ExecuteEventSender) error {
	s.mu.Lock()
	_, ok := s.sessions[request.GetSessionId()]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown session %q", request.GetSessionId())
	}
	if rawDelay := request.GetConfig()["delay_ms"]; rawDelay != "" {
		delayMS, err := strconv.Atoi(rawDelay)
		if err != nil || delayMS < 0 {
			return fmt.Errorf("invalid delay_ms %q", rawDelay)
		}
		if delayMS > 0 {
			timer := time.NewTimer(time.Duration(delayMS) * time.Millisecond)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}

	return sink.Send(&pb.ExecuteEvent{
		Event: &pb.ExecuteEvent_Result{Result: &pb.ExecuteResult{Outcome: "success"}},
	})
}

func (s *noopService) Permit(context.Context, *pb.PermitRequest) (*pb.PermitResponse, error) {
	return &pb.PermitResponse{}, nil
}

func (s *noopService) CloseSession(_ context.Context, request *pb.CloseSessionRequest) (*pb.CloseSessionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, request.GetSessionId())
	return &pb.CloseSessionResponse{}, nil
}

func main() {
	adapterhost.Serve(&noopService{sessions: map[string]struct{}{}})
}
