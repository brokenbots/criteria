// Package main is a minimal adapter plugin that imports only the public
// sdk/pluginhost surface plus sdk/pb. It exists to prove that an external
// author needs no internal/ reach-through to write a functioning Overseer
// plugin, and is exercised by the adapter conformance harness.
package main

import (
	"context"
	"fmt"
	"sync"

	pluginhost "github.com/brokenbots/overseer/sdk/pluginhost"
	pb "github.com/brokenbots/overseer/sdk/pb/overseer/v1"
)

// publicSDKPlugin is the reference implementation that exercises every method
// in pluginhost.Service using only the public SDK.
type publicSDKPlugin struct {
	mu       sync.Mutex
	sessions map[string]struct{}
}

func (p *publicSDKPlugin) Info(_ context.Context, _ *pb.InfoRequest) (*pb.InfoResponse, error) {
	return &pb.InfoResponse{
		Name:    "public-sdk-fixture",
		Version: "0.1.0",
	}, nil
}

func (p *publicSDKPlugin) OpenSession(_ context.Context, req *pb.OpenSessionRequest) (*pb.OpenSessionResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sessions[req.GetSessionId()] = struct{}{}
	return &pb.OpenSessionResponse{}, nil
}

func (p *publicSDKPlugin) Execute(_ context.Context, req *pb.ExecuteRequest, sink pluginhost.ExecuteEventSender) error {
	p.mu.Lock()
	_, ok := p.sessions[req.GetSessionId()]
	p.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown session %q", req.GetSessionId())
	}
	return sink.Send(&pb.ExecuteEvent{
		Event: &pb.ExecuteEvent_Result{
			Result: &pb.ExecuteResult{Outcome: "success"},
		},
	})
}

func (p *publicSDKPlugin) Permit(_ context.Context, _ *pb.PermitRequest) (*pb.PermitResponse, error) {
	return &pb.PermitResponse{}, nil
}

func (p *publicSDKPlugin) CloseSession(_ context.Context, req *pb.CloseSessionRequest) (*pb.CloseSessionResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.sessions, req.GetSessionId())
	return &pb.CloseSessionResponse{}, nil
}

func main() {
	pluginhost.Serve(&publicSDKPlugin{sessions: map[string]struct{}{}})
}
