// Package main is the greeter adapter — a minimal example of a
// third-party Criteria adapter that lives in its own module, imports only
// the public adapter SDK, and is discovered at runtime from CRITERIA_ADAPTERS
// or ~/.local/criteria/adapters/.
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

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	adapterhost "github.com/brokenbots/criteria-go-adapter-sdk/adapterhost"
)

type greeterService struct {
	adapterhost.UnimplementedPermissions
}

func (g *greeterService) Info(_ context.Context, _ *v2.InfoRequest) (*v2.InfoResponse, error) {
	return &v2.InfoResponse{
		Name:    "greeter",
		Version: "0.1.0",
	}, nil
}

func (g *greeterService) OpenSession(_ context.Context, _ *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	return &v2.OpenSessionResponse{}, nil
}

func (g *greeterService) Execute(_ context.Context, req *v2.ExecuteRequest, sink adapterhost.ExecuteEventSender) error {
	name := req.GetInput()["name"]
	if name == "" {
		name = "world"
	}
	greeting := fmt.Sprintf("hello, %s", name)

	// Return the greeting as a named output so downstream steps can reference
	// it via steps.<step_name>.greeting. Outputs travel on the typed outputs_json
	// channel; values keep their native JSON type.
	ev, err := v2.NewExecuteResultEvent("success", map[string]any{"greeting": greeting})
	if err != nil {
		return err
	}
	return sink.Send(ev)
}

// Log blocks until the host closes the stream; greeter has no log lines to emit.
func (g *greeterService) Log(ctx context.Context, _ *v2.LogRequest, _ adapterhost.LogEventSender) error {
	<-ctx.Done()
	return nil
}

func (g *greeterService) CloseSession(_ context.Context, _ *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
	return &v2.CloseSessionResponse{}, nil
}

func main() {
	adapterhost.Serve(&greeterService{})
}
