// Package main is a minimal adapter that imports only the public
// sdk/adapterhost surface plus proto/criteria/v2. It exists to prove that an
// external author needs no internal/ reach-through to write a functioning
// Criteria adapter, and is exercised by the adapter conformance harness.
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	adapterhost "github.com/brokenbots/criteria-go-adapter-sdk/adapterhost"
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
	// emit_typed exercises the native outputs_json channel end to end: structured
	// and scalar values are emitted with their JSON-native type so the host can
	// decode them to native cty types.
	if req.GetInput()["emit_typed"] == "true" {
		ev, err := v2.NewExecuteResultEvent("success", map[string]any{
			"meta":  map[string]any{"id": 7, "name": "widget"},
			"count": 42,
			"ok":    true,
		})
		if err != nil {
			return err
		}
		return sink.Send(ev)
	}

	return sink.Send(&v2.ExecuteEvent{
		Event: &v2.ExecuteEvent_Result{
			Result: &v2.ExecuteResult{Outcome: "success"},
		},
	})
}

func (p *publicSDKAdapter) Log(ctx context.Context, _ *v2.LogRequest, sender adapterhost.LogEventSender) error {
	// The log stream must remain open for the lifetime of the session. Returning
	// immediately would stop the SDK heartbeat ticker and break the host's
	// liveness contract, so block until the host cancels the stream.
	//
	// For fast conformance tests the host uses a short stall threshold, so we
	// emit heartbeats at a configurable cadence (defaulting to the protocol
	// 30 s). This lets the idle-survival test prove liveness without waiting
	// the full production interval.
	interval := 30 * time.Second
	if raw := os.Getenv("CRITERIA_TEST_HEARTBEAT_INTERVAL_MS"); raw != "" {
		if ms, err := strconv.Atoi(raw); err == nil && ms > 0 {
			interval = time.Duration(ms) * time.Millisecond
		}
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case t := <-ticker.C:
			if err := sender.Send(&v2.LogEvent{Heartbeat: &v2.Heartbeat{StreamName: "log", SentAt: timestamppb.New(t)}}); err != nil {
				return err
			}
		}
	}
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
