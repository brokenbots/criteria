// Package main is the greeter adapter plugin — a minimal example of a
// third-party Criteria adapter that lives in its own module, imports only
// the public plugin SDK, and is discovered at runtime from CRITERIA_PLUGINS
// or ~/.criteria/plugins/.
//
// The adapter accepts one input key, "name", and returns:
//   - outcome:           "success"
//   - output "greeting": "hello, <name>"
//
// See example.hcl for a workflow that exercises this adapter.
package main

import (
	"context"
	"fmt"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	pluginhost "github.com/brokenbots/criteria/sdk/pluginhost"
)

type greeterService struct{}

func (g *greeterService) Info(_ context.Context, _ *pb.InfoRequest) (*pb.InfoResponse, error) {
	return &pb.InfoResponse{
		Name:    "greeter",
		Version: "0.1.0",
	}, nil
}

func (g *greeterService) OpenSession(_ context.Context, _ *pb.OpenSessionRequest) (*pb.OpenSessionResponse, error) {
	return &pb.OpenSessionResponse{}, nil
}

func (g *greeterService) Execute(_ context.Context, req *pb.ExecuteRequest, sink pluginhost.ExecuteEventSender) error {
	name := req.GetConfig()["name"]
	if name == "" {
		name = "world"
	}
	greeting := fmt.Sprintf("hello, %s", name)

	// Emit the greeting as a log line so it is visible in the run output.
	if err := sink.Send(&pb.ExecuteEvent{
		Event: &pb.ExecuteEvent_Log{
			Log: &pb.LogEvent{
				Stream: "stdout",
				Chunk:  []byte(greeting + "\n"),
			},
		},
	}); err != nil {
		return err
	}

	// Return the greeting as a named output so downstream steps can reference
	// it via steps.<step_name>.greeting.
	return sink.Send(&pb.ExecuteEvent{
		Event: &pb.ExecuteEvent_Result{
			Result: &pb.ExecuteResult{
				Outcome: "success",
				Outputs: map[string]string{"greeting": greeting},
			},
		},
	})
}

func (g *greeterService) Permit(_ context.Context, _ *pb.PermitRequest) (*pb.PermitResponse, error) {
	return &pb.PermitResponse{}, nil
}

func (g *greeterService) CloseSession(_ context.Context, _ *pb.CloseSessionRequest) (*pb.CloseSessionResponse, error) {
	return &pb.CloseSessionResponse{}, nil
}

func main() {
	pluginhost.Serve(&greeterService{})
}
