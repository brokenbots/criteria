// Package main implements a simple test adapter that returns an outcome based on
// the step input configuration. In v2, permission requests are emitted as
// AdapterEvent(kind="permission.request") on the Execute stream; the host
// evaluates them and emits permission.granted / permission.denied events.
//
// # Configuration
//
// Set "outcome" in the step input to control the returned outcome.
// Defaults to "success" if unset.
//
// Set "perm_tools" to a comma-separated list of "tool[|fingerprint]" entries
// to trigger permission request events before returning the outcome.
//
// This adapter is only built and used by tests. It is NOT registered with
// `make plugins` and must not be installed in ~/.criteria/plugins/.
package main

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"google.golang.org/protobuf/types/known/structpb"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	adapterhost "github.com/brokenbots/criteria-go-adapter-sdk/adapterhost"
)

type permissiveService struct {
	adapterhost.UnimplementedPermissions
	mu       sync.Mutex
	sessions map[string]struct{}
}

func (s *permissiveService) Info(_ context.Context, _ *v2.InfoRequest) (*v2.InfoResponse, error) {
	return &v2.InfoResponse{
		Name:    "permissive",
		Version: "0.1.0",
	}, nil
}

func (s *permissiveService) OpenSession(_ context.Context, req *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[req.GetSessionId()] = struct{}{}
	return &v2.OpenSessionResponse{}, nil
}

func (s *permissiveService) emitPermissionRequests(req *v2.ExecuteRequest, sink adapterhost.ExecuteEventSender) error {
	permTools := req.GetInput()["perm_tools"]
	if permTools == "" {
		return nil
	}
	for i, entry := range strings.Split(permTools, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		var tool, fingerprint string
		if idx := strings.Index(entry, "|"); idx >= 0 {
			tool = entry[:idx]
			fingerprint = entry[idx+1:]
		} else {
			tool = entry
		}
		fields := map[string]any{
			"request_id": fmt.Sprintf("perm-%d", i),
			"tool":       tool,
		}
		if fingerprint != "" {
			fields["full_command_text"] = fingerprint
		}
		payload, err := structpb.NewStruct(fields)
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
	return nil
}

func (s *permissiveService) Execute(_ context.Context, req *v2.ExecuteRequest, sink adapterhost.ExecuteEventSender) error {
	s.mu.Lock()
	_, ok := s.sessions[req.GetSessionId()]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown session %q", req.GetSessionId())
	}

	if err := s.emitPermissionRequests(req, sink); err != nil {
		return err
	}

	outcome := req.GetInput()["outcome"]
	if outcome == "" {
		outcome = "success"
	}
	return sink.Send(&v2.ExecuteEvent{
		Event: &v2.ExecuteEvent_Result{
			Result: &v2.ExecuteResult{Outcome: outcome},
		},
	})
}

func (s *permissiveService) Log(_ context.Context, _ *v2.LogRequest, _ adapterhost.LogEventSender) error {
	return nil
}

func (s *permissiveService) Pause(_ context.Context, _ *v2.PauseRequest) (*v2.PauseResponse, error) {
	return &v2.PauseResponse{}, nil
}

func (s *permissiveService) Resume(_ context.Context, _ *v2.ResumeRequest) (*v2.ResumeResponse, error) {
	return &v2.ResumeResponse{}, nil
}

func (s *permissiveService) Snapshot(_ context.Context, _ *v2.SnapshotRequest) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{}, nil
}

func (s *permissiveService) Restore(_ context.Context, _ *v2.RestoreRequest) (*v2.RestoreResponse, error) {
	return &v2.RestoreResponse{}, nil
}

func (s *permissiveService) Inspect(_ context.Context, _ *v2.InspectRequest) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}

func (s *permissiveService) CloseSession(_ context.Context, req *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, req.GetSessionId())
	return &v2.CloseSessionResponse{}, nil
}

func main() {
	adapterhost.Serve(&permissiveService{
		sessions: map[string]struct{}{},
	})
}
