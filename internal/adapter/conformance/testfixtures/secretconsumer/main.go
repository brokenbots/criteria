package main

// secretconsumer is a fixture adapter used by the adapter conformance suite to
// demonstrate the two supported secret-delivery patterns: an env-based
// consumer that maps the structured secret into its own child-process
// environment, and a structured-payload consumer that reads the value only from
// the dedicated OpenSession secrets channel.

import (
	"context"
	"encoding/json"
	"os"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	adapterhost "github.com/brokenbots/criteria-go-adapter-sdk/adapterhost"
)

type secretReport struct {
	Mode       string `json:"mode"`
	EnvSecret  string `json:"env_secret"`
	OpenSecret string `json:"open_secret"`
}

type secretConsumerService struct {
	adapterhost.UnimplementedPermissions
}

func (s *secretConsumerService) Info(context.Context, *v2.InfoRequest) (*v2.InfoResponse, error) {
	return &v2.InfoResponse{Name: "secretconsumer", Version: "0.1.0"}, nil
}

func (s *secretConsumerService) OpenSession(_ context.Context, req *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	mode := req.GetConfig()["mode"]
	outputPath := req.GetConfig()["output_path"]
	secretValue := req.GetSecrets()["SECRET"]

	report := secretReport{
		Mode:       mode,
		OpenSecret: secretValue,
	}

	if mode == "env" {
		// Env-based consumer: the adapter chooses to expose the secret to any
		// child process it spawns. This is adapter-specific behavior, not a
		// universal engine guarantee.
		_ = os.Setenv("ADAPTER_SECRET", secretValue)
		report.EnvSecret = secretValue
	} else {
		// Structured-payload consumer: the secret arrived through the dedicated
		// secret channel and must NOT leak into the process environment.
		report.EnvSecret = os.Getenv("ADAPTER_SECRET")
	}

	data, _ := json.Marshal(report)
	if outputPath != "" {
		_ = os.WriteFile(outputPath, data, 0o644)
	}

	return &v2.OpenSessionResponse{}, nil
}

func (s *secretConsumerService) Execute(_ context.Context, _ *v2.ExecuteRequest, sink adapterhost.ExecuteEventSender) error {
	return sink.Send(&v2.ExecuteEvent{
		Event: &v2.ExecuteEvent_Result{Result: &v2.ExecuteResult{Outcome: "success"}},
	})
}

func (s *secretConsumerService) Log(_ context.Context, _ *v2.LogRequest, _ adapterhost.LogEventSender) error {
	return nil
}

func (s *secretConsumerService) CloseSession(_ context.Context, _ *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
	return &v2.CloseSessionResponse{}, nil
}

func main() {
	adapterhost.Serve(&secretConsumerService{})
}
