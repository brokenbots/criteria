// Package main is a mock adapter used by the remote-runner regression test.
//
// It mimics real adapters (criteria-adapter-shell >= v0.5.3 and
// criteria-adapter-copilot >= v0.5.5) that inspect CRITERIA_REMOTE_HOST in their
// main() and, if present, switch into ServeRemote/phone-home mode instead of
// opening the local go-plugin socket. To make the regression test deterministic,
// this fixture fails fast when any of the runner's remote-connection variables
// leak through, rather than actually phoning home.
package main

import (
	"context"
	"fmt"
	"os"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	adapterhost "github.com/brokenbots/criteria-go-adapter-sdk/adapterhost"
)

type mockAdapter struct {
	adapterhost.UnimplementedPermissions
}

func (m *mockAdapter) Info(_ context.Context, _ *v2.InfoRequest) (*v2.InfoResponse, error) {
	return &v2.InfoResponse{Name: "remote-detecting-mock", Version: "0.1.0"}, nil
}

func (m *mockAdapter) OpenSession(_ context.Context, _ *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	return &v2.OpenSessionResponse{}, nil
}

func (m *mockAdapter) Execute(_ context.Context, _ *v2.ExecuteRequest, sink adapterhost.ExecuteEventSender) error {
	return sink.Send(&v2.ExecuteEvent{
		Event: &v2.ExecuteEvent_Result{
			Result: &v2.ExecuteResult{Outcome: "success"},
		},
	})
}

func (m *mockAdapter) Log(_ context.Context, _ *v2.LogRequest, _ adapterhost.LogEventSender) error {
	return nil
}

func (m *mockAdapter) CloseSession(_ context.Context, _ *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
	return &v2.CloseSessionResponse{}, nil
}

func main() {
	// remoteEnvVars must match the set filtered by the runner in runner.go.
	remoteEnvVars := []string{
		"CRITERIA_REMOTE_HOST",
		"CRITERIA_REMOTE_TOKEN",
		"CRITERIA_REMOTE_DIGEST",
		"CRITERIA_REMOTE_TLS_CERT",
		"CRITERIA_REMOTE_TLS_KEY",
		"CRITERIA_REMOTE_CA",
	}
	for _, name := range remoteEnvVars {
		if os.Getenv(name) != "" {
			fmt.Fprintf(os.Stderr, "mock adapter detected leaked runner variable %s\n", name)
			os.Exit(1)
		}
	}

	adapterhost.Serve(&mockAdapter{})
}
