// Package main is a minimal adapter that imports only the public
// sdk/adapterhost surface plus sdk/pb. It exists to prove that an external
// author needs no internal/ reach-through to write a functioning Criteria
// adapter, and is exercised by the adapter conformance harness.
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

// publicSDKAdapter is the reference implementation that exercises every method
// in adapterhost.Service using only the public SDK.
type publicSDKAdapter struct {
	mu       sync.Mutex
	sessions map[string]struct{}
}

func (p *publicSDKAdapter) Info(_ context.Context, _ *pb.InfoRequest) (*pb.InfoResponse, error) {
	return &pb.InfoResponse{
		Name:    "public-sdk-fixture",
		Version: "0.1.0",
	}, nil
}

func (p *publicSDKAdapter) OpenSession(_ context.Context, req *pb.OpenSessionRequest) (*pb.OpenSessionResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sessions[req.GetSessionId()] = struct{}{}
	return &pb.OpenSessionResponse{}, nil
}

func (p *publicSDKAdapter) Execute(ctx context.Context, req *pb.ExecuteRequest, sink adapterhost.ExecuteEventSender) error {
	p.mu.Lock()
	_, ok := p.sessions[req.GetSessionId()]
	p.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown session %q", req.GetSessionId())
	}
	// delay_ms support allows context_cancellation and step_timeout conformance
	// tests to exercise cross-process cancellation propagation.
	if raw := req.GetConfig()["delay_ms"]; raw != "" {
		ms, err := strconv.Atoi(raw)
		if err != nil || ms < 0 {
			return fmt.Errorf("invalid delay_ms %q", raw)
		}
		if ms > 0 {
			timer := time.NewTimer(time.Duration(ms) * time.Millisecond)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return sink.Send(&pb.ExecuteEvent{
		Event: &pb.ExecuteEvent_Result{
			Result: &pb.ExecuteResult{Outcome: "success"},
		},
	})
}

func (p *publicSDKAdapter) Permit(_ context.Context, _ *pb.PermitRequest) (*pb.PermitResponse, error) {
	return &pb.PermitResponse{}, nil
}

func (p *publicSDKAdapter) CloseSession(_ context.Context, req *pb.CloseSessionRequest) (*pb.CloseSessionResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.sessions, req.GetSessionId())
	return &pb.CloseSessionResponse{}, nil
}

func main() {
	adapterhost.Serve(&publicSDKAdapter{sessions: map[string]struct{}{}})
}
