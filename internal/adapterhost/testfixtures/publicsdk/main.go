// Package main is a minimal adapter that imports only the public
// sdk/adapterhost surface plus proto/criteria/v2. It exists to prove that an
// external author needs no internal/ reach-through to write a functioning
// Criteria adapter, and is exercised by the adapter conformance harness.
package main

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	v2 "github.com/brokenbots/criteria/sdk/pb/criteria/v2"
	adapterhost "github.com/brokenbots/criteria/sdk/adapterhost"
)

// publicSDKAdapter is the reference implementation that exercises every method
// in adapterhost.Service using only the public SDK.
type publicSDKAdapter struct {
	adapterhost.UnimplementedPermissions
	mu       sync.Mutex
	sessions map[string]struct{}
}

func (p *publicSDKAdapter) Info(_ context.Context, _ *v2.InfoRequest) (*v2.InfoResponse, error) {
	return &v2.InfoResponse{
		Name:    "public-sdk-fixture",
		Version: "0.1.0",
	}, nil
}

func (p *publicSDKAdapter) OpenSession(_ context.Context, req *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sessions[req.GetSessionId()] = struct{}{}
	return &v2.OpenSessionResponse{}, nil
}

func (p *publicSDKAdapter) Execute(ctx context.Context, req *v2.ExecuteRequest, sink adapterhost.ExecuteEventSender) error {
	p.mu.Lock()
	_, ok := p.sessions[req.GetSessionId()]
	p.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown session %q", req.GetSessionId())
	}
	// delay_ms support allows context_cancellation and step_timeout conformance
	// tests to exercise cross-process cancellation propagation.
	if raw := req.GetInput()["delay_ms"]; raw != "" {
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
	return sink.Send(&v2.ExecuteEvent{
		Event: &v2.ExecuteEvent_Result{
			Result: &v2.ExecuteResult{Outcome: "success"},
		},
	})
}

func (p *publicSDKAdapter) Log(_ context.Context, _ *v2.LogRequest, _ adapterhost.LogEventSender) error {
	return nil
}

func (p *publicSDKAdapter) Pause(_ context.Context, _ *v2.PauseRequest) (*v2.PauseResponse, error) {
	return &v2.PauseResponse{}, nil
}

func (p *publicSDKAdapter) Resume(_ context.Context, _ *v2.ResumeRequest) (*v2.ResumeResponse, error) {
	return &v2.ResumeResponse{}, nil
}

func (p *publicSDKAdapter) Snapshot(_ context.Context, _ *v2.SnapshotRequest) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{}, nil
}

func (p *publicSDKAdapter) Restore(_ context.Context, _ *v2.RestoreRequest) (*v2.RestoreResponse, error) {
	return &v2.RestoreResponse{}, nil
}

func (p *publicSDKAdapter) Inspect(_ context.Context, _ *v2.InspectRequest) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}

func (p *publicSDKAdapter) CloseSession(_ context.Context, req *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.sessions, req.GetSessionId())
	return &v2.CloseSessionResponse{}, nil
}

func main() {
	adapterhost.Serve(&publicSDKAdapter{sessions: map[string]struct{}{}})
}
