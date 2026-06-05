package main

import (
	"context"
	"os"
	"sync"

	adapterhost "github.com/brokenbots/criteria-go-adapter-sdk/adapterhost"
	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
)

type dropperService struct {
	adapterhost.UnimplementedPermissions
	mu     sync.Mutex
	opened bool
}

func (s *dropperService) Info(context.Context, *v2.InfoRequest) (*v2.InfoResponse, error) {
	return &v2.InfoResponse{
		Name:    "handshake-dropper",
		Version: "0.1.0",
	}, nil
}

func (s *dropperService) OpenSession(context.Context, *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opened = true
	return &v2.OpenSessionResponse{}, nil
}

func (s *dropperService) Execute(context.Context, *v2.ExecuteRequest, adapterhost.ExecuteEventSender) error {
	s.mu.Lock()
	_ = s.opened
	s.mu.Unlock()
	// Crash the process immediately so the host sees a connection drop.
	os.Exit(1)
	return nil
}

func (s *dropperService) Log(context.Context, *v2.LogRequest, adapterhost.LogEventSender) error {
	return nil
}

func (s *dropperService) CloseSession(context.Context, *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
	return &v2.CloseSessionResponse{}, nil
}

func main() {
	adapterhost.Serve(&dropperService{})
}
