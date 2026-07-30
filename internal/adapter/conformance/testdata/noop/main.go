package main

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	adapterhost "github.com/brokenbots/criteria-go-adapter-sdk/adapterhost"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/brokenbots/criteria/internal/adapterhost/testfixtures/heartbeatutil"
)

type noopService struct {
	adapterhost.UnimplementedPermissions
	mu       sync.Mutex
	sessions map[string]struct{}
}

func (s *noopService) Info(_ context.Context, _ *v2.InfoRequest) (*v2.InfoResponse, error) {
	return &v2.InfoResponse{
		Name:               "noop",
		Version:            "0.1.0",
		SourceUrl:          "https://github.com/brokenbots/criteria",
		SdkProtocolVersion: "2",
		Platforms:          []string{"linux/amd64", "linux/arm64", "darwin/arm64"},
		Capabilities:       []string{"parallel_safe", "permission_gating", "permission_request_forwarding"},
	}, nil
}

func (s *noopService) OpenSession(_ context.Context, req *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[req.GetSessionId()] = struct{}{}
	return &v2.OpenSessionResponse{}, nil
}

func (s *noopService) Execute(ctx context.Context, req *v2.ExecuteRequest, sink adapterhost.ExecuteEventSender) error {
	s.mu.Lock()
	_, ok := s.sessions[req.GetSessionId()]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown session %q", req.GetSessionId())
	}
	if rawDelay := req.GetInput()["delay_ms"]; rawDelay != "" {
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
	if req.GetInput()["emit_permission_request"] == "true" {
		payload, err := structpb.NewStruct(map[string]any{
			"kind":       "shell",
			"request_id": "noop-perm-1",
			"tool":       "shell",
		})
		if err != nil {
			return fmt.Errorf("build permission.request payload: %w", err)
		}
		if err := sink.Send(&v2.ExecuteEvent{
			Event: &v2.ExecuteEvent_Adapter{
				Adapter: &v2.AdapterEvent{
					EventKind: "permission.request",
					Payload:   payload,
				},
			},
		}); err != nil {
			return err
		}
	}
	return sink.Send(&v2.ExecuteEvent{
		Event: &v2.ExecuteEvent_Result{Result: &v2.ExecuteResult{Outcome: "success"}},
	})
}

func (s *noopService) Log(ctx context.Context, _ *v2.LogRequest, sender adapterhost.LogEventSender) error {
	// The log stream must remain open for the lifetime of the session. Returning
	// immediately would stop the SDK heartbeat ticker and break the host's
	// liveness contract, so block until the host cancels the stream.
	//
	// heartbeatutil.RunLogHeartbeat is a transitional shim: the Go SDK should
	// own session-lifetime heartbeats (see PR #283 Follow-ups). Remove this once
	// the SDK fix lands.
	return heartbeatutil.RunLogHeartbeat(ctx, sender)
}

func (s *noopService) CloseSession(_ context.Context, req *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, req.GetSessionId())
	return &v2.CloseSessionResponse{}, nil
}

func main() {
	adapterhost.Serve(&noopService{sessions: map[string]struct{}{}})
}
