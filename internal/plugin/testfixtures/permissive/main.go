// Package main implements a configurable test plugin that emits N permission
// requests and returns an outcome based on the grant/deny decisions it receives.
//
// # Configuration
//
// The plugin reads perm_tools from the step config (comma-separated list of
// permission specs) or from the PERM_TOOLS environment variable.
//
// Supported spec forms:
//   - "read_file" -> Permission="read_file"
//   - "shell|git status" -> Permission="shell", Details["commands"]="git status"
//
// If neither source is set the plugin emits no permission requests and returns
// "success".
//
// Outcome semantics:
//   - All requests granted → "success"
//   - Any request denied   → "needs_review"
//
// This plugin is only built and used by tests. It is NOT registered with
// `make plugins` and must not be installed in ~/.overlord/plugins/.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/google/uuid"

	pluginpkg "github.com/brokenbots/overseer/internal/plugin"
	pb "github.com/brokenbots/overseer/sdk/pb/v1"
)

type permitDecision struct {
	allow  bool
	reason string
}

type permissionSpec struct {
	tool    string
	details map[string]string
}

type permissiveService struct {
	mu       sync.Mutex
	sessions map[string]struct{}
	pending  map[string]chan permitDecision
}

func (s *permissiveService) Info(_ context.Context, _ *pb.InfoRequest) (*pb.InfoResponse, error) {
	return &pb.InfoResponse{
		Name:         "permissive",
		Version:      "0.1.0",
		Capabilities: []string{"permission_gating"},
	}, nil
}

func (s *permissiveService) OpenSession(_ context.Context, req *pb.OpenSessionRequest) (*pb.OpenSessionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[req.GetSessionId()] = struct{}{}
	return &pb.OpenSessionResponse{}, nil
}

// Execute emits one PermissionRequest event per configured tool and waits for
// the host to respond via Permit before proceeding to the next tool. The final
// result is "needs_review" if any request was denied, "success" otherwise.
func (s *permissiveService) Execute(ctx context.Context, req *pb.ExecuteRequest, sink pluginpkg.ExecuteEventSender) error {
	s.mu.Lock()
	_, ok := s.sessions[req.GetSessionId()]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown session %q", req.GetSessionId())
	}

	requests := requestsFromConfig(req.GetConfig())

	anyDenied := false
	for _, requested := range requests {
		id := uuid.New().String()
		ch := make(chan permitDecision, 1)

		s.mu.Lock()
		s.pending[id] = ch
		s.mu.Unlock()

		if err := sink.Send(&pb.ExecuteEvent{
			Event: &pb.ExecuteEvent_Permission{
				Permission: &pb.PermissionRequest{
					Id:         id,
					Permission: requested.tool,
					Details:    requested.details,
				},
			},
		}); err != nil {
			s.mu.Lock()
			delete(s.pending, id)
			s.mu.Unlock()
			return fmt.Errorf("send permission request: %w", err)
		}

		var decision permitDecision
		select {
		case decision = <-ch:
		case <-ctx.Done():
			s.mu.Lock()
			delete(s.pending, id)
			s.mu.Unlock()
			return ctx.Err()
		}

		if !decision.allow {
			anyDenied = true
		}
	}

	outcome := "success"
	if anyDenied {
		outcome = "needs_review"
	}
	return sink.Send(&pb.ExecuteEvent{
		Event: &pb.ExecuteEvent_Result{
			Result: &pb.ExecuteResult{Outcome: outcome},
		},
	})
}

func (s *permissiveService) Permit(_ context.Context, req *pb.PermitRequest) (*pb.PermitResponse, error) {
	s.mu.Lock()
	ch, ok := s.pending[req.GetPermissionId()]
	if ok {
		delete(s.pending, req.GetPermissionId())
	}
	s.mu.Unlock()
	if ok {
		ch <- permitDecision{allow: req.GetAllow(), reason: req.GetReason()}
	}
	return &pb.PermitResponse{}, nil
}

func (s *permissiveService) CloseSession(_ context.Context, req *pb.CloseSessionRequest) (*pb.CloseSessionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, req.GetSessionId())
	return &pb.CloseSessionResponse{}, nil
}

// requestsFromConfig returns the list of permission requests to emit.
func requestsFromConfig(config map[string]string) []permissionSpec {
	if v := config["perm_tools"]; v != "" {
		return parsePermissionSpecs(v)
	}
	if v := os.Getenv("PERM_TOOLS"); v != "" {
		return parsePermissionSpecs(v)
	}
	return nil
}

func parsePermissionSpecs(s string) []permissionSpec {
	parts := strings.Split(s, ",")
	out := make([]permissionSpec, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if strings.Contains(p, "|") {
			chunk := strings.SplitN(p, "|", 2)
			tool := strings.TrimSpace(chunk[0])
			fingerprint := strings.TrimSpace(chunk[1])
			if tool != "" {
				spec := permissionSpec{tool: tool}
				if fingerprint != "" {
					spec.details = map[string]string{"commands": fingerprint}
				}
				out = append(out, spec)
			}
			continue
		}
		out = append(out, permissionSpec{tool: p})
	}
	return out
}

func main() {
	pluginpkg.Serve(&permissiveService{
		sessions: map[string]struct{}{},
		pending:  map[string]chan permitDecision{},
	})
}
