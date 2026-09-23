// Package main is the prompt-capable conformance fixture (ADR-0006 D3). It
// mirrors the noop fixture's serving shape and additionally registers the
// adapter v2 Prompt RPC on an extended copy of the AdapterService
// ServiceDesc, declaring the supports_prompt capability. It exists so the
// prompt delivery path can be exercised end to end in-tree; production
// prompt-capable adapters (e.g. copilot) live outside this repository.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	adapterhost "github.com/brokenbots/criteria-go-adapter-sdk/adapterhost"
	hplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	promptwire "github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/adapterhost/heartbeatutil"
)

// callLogEnv, when set, names a file the fixture appends one line to per v2
// method invocation (info, open_session, execute, prompt, close_session).
// The live-seam tests read it to assert the host→adapter call sequence.
const callLogEnv = "PROMPTABLE_CALL_LOG"

type promptableService struct {
	adapterhost.UnimplementedPermissions

	mu       sync.Mutex
	sessions map[string]struct{}
}

func (s *promptableService) Info(_ context.Context, _ *v2.InfoRequest) (*v2.InfoResponse, error) {
	s.record("info")
	return &v2.InfoResponse{
		Name:               "promptable",
		Version:            "0.1.0",
		SourceUrl:          "https://github.com/brokenbots/criteria",
		SdkProtocolVersion: "2",
		Platforms:          []string{"linux/amd64", "linux/arm64", "darwin/arm64"},
		Capabilities:       []string{"parallel_safe", promptwire.PromptCapability},
	}, nil
}

func (s *promptableService) OpenSession(_ context.Context, req *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	s.record("open_session")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[req.GetSessionId()] = struct{}{}
	return &v2.OpenSessionResponse{}, nil
}

func (s *promptableService) Execute(ctx context.Context, req *v2.ExecuteRequest, sink adapterhost.ExecuteEventSender) error {
	s.record("execute")
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
	return sink.Send(&v2.ExecuteEvent{
		Event: &v2.ExecuteEvent_Result{
			Result: &v2.ExecuteResult{Outcome: "success"},
		},
	})
}

func (s *promptableService) Log(ctx context.Context, _ *v2.LogRequest, sender adapterhost.LogEventSender) error {
	return heartbeatutil.RunLogHeartbeat(ctx, sender)
}

func (s *promptableService) CloseSession(_ context.Context, req *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
	s.record("close_session")
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, req.GetSessionId())
	return &v2.CloseSessionResponse{}, nil
}

func (s *promptableService) record(method string) {
	path := os.Getenv(callLogEnv)
	if path == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintln(f, method)
}

// promptableServer bridges the generated AdapterServiceServer to the fixture
// service. The explicit methods carry the stock v2 surface; Prompt is the
// extension point, dispatched to the in-tree PromptService contract
// (promptwire.PromptMethodDesc) registered alongside the stock methods.
type promptableServer struct {
	v2.UnimplementedAdapterServiceServer
	svc *promptableService
}

func (s *promptableServer) Prompt(ctx context.Context, req *promptwire.PromptRequest) (*promptwire.PromptResponse, error) {
	s.svc.record("prompt")
	return &promptwire.PromptResponse{Accepted: true}, nil
}

func (s *promptableServer) Info(ctx context.Context, req *v2.InfoRequest) (*v2.InfoResponse, error) {
	return s.svc.Info(ctx, req)
}

func (s *promptableServer) OpenSession(ctx context.Context, req *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	return s.svc.OpenSession(ctx, req)
}

func (s *promptableServer) Execute(req *v2.ExecuteRequest, stream v2.AdapterService_ExecuteServer) error {
	return s.svc.Execute(stream.Context(), req, stream)
}

func (s *promptableServer) Log(req *v2.LogRequest, stream v2.AdapterService_LogServer) error {
	return s.svc.Log(stream.Context(), req, stream)
}

func (s *promptableServer) Permissions(stream v2.AdapterService_PermissionsServer) error {
	return s.svc.Permissions(stream.Context(), stream)
}

func (s *promptableServer) CloseSession(ctx context.Context, req *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
	return s.svc.CloseSession(ctx, req)
}

// promptPlugin is the go-plugin wrapper that registers the extended
// ServiceDesc carrying both the stock AdapterService methods and Prompt. It
// registers once, in place of the SDK's stock grpcAdapter.
type promptPlugin struct {
	hplugin.NetRPCUnsupportedPlugin
	svc *promptableService
}

func (p *promptPlugin) GRPCServer(_ *hplugin.GRPCBroker, server *grpc.Server) error {
	desc := v2.AdapterService_ServiceDesc
	methods := make([]grpc.MethodDesc, 0, len(desc.Methods)+1)
	methods = append(methods, desc.Methods...)
	methods = append(methods, promptwire.PromptMethodDesc())
	desc.Methods = methods
	server.RegisterService(&desc, &promptableServer{svc: p.svc})
	return nil
}

func (p *promptPlugin) GRPCClient(_ context.Context, _ *hplugin.GRPCBroker, _ *grpc.ClientConn) (interface{}, error) {
	return nil, errors.New("GRPCClient is not implemented in the adapter process")
}

func main() {
	hplugin.Serve(&hplugin.ServeConfig{
		HandshakeConfig: adapterhost.HandshakeConfig,
		Plugins: map[string]hplugin.Plugin{
			adapterhost.AdapterName: &promptPlugin{svc: &promptableService{sessions: map[string]struct{}{}}},
		},
		GRPCServer: hplugin.DefaultGRPCServer,
	})
}
