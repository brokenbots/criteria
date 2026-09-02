package conformance

// secret_channel_test.go — adapter-level demonstration of the two secret
// delivery modes required by CRI-88.
//
// One fixture adapter behaves as an env-based consumer: it maps the secret
// delivered through the OpenSession secrets channel into its own child-process
// environment. The other mode behaves as a structured-payload consumer: it
// reads the secret only from the dedicated secrets channel and confirms that
// no secret value leaked into its process environment.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapterhost"
)

func TestSecretChannel_EnvBasedConsumer(t *testing.T) {
	bin := buildSecretConsumer(t)
	report := runSecretConsumer(t, bin, "env", map[string]string{"SECRET": "env-consumer-secret"})

	if report.OpenSecret != "env-consumer-secret" {
		t.Fatalf("expected structured secret value in OpenSession channel, got %q", report.OpenSecret)
	}
	if report.EnvSecret != "env-consumer-secret" {
		t.Fatalf("expected env-based consumer to expose secret in its environment, got %q", report.EnvSecret)
	}
}

func TestSecretChannel_StructuredPayloadConsumer(t *testing.T) {
	bin := buildSecretConsumer(t)
	report := runSecretConsumer(t, bin, "structured", map[string]string{"SECRET": "structured-consumer-secret"})

	if report.OpenSecret != "structured-consumer-secret" {
		t.Fatalf("expected structured secret value in OpenSession channel, got %q", report.OpenSecret)
	}
	if report.EnvSecret != "" {
		t.Fatalf("structured-payload consumer must not leak secret into process environment, got %q", report.EnvSecret)
	}
}

type secretReport struct {
	Mode       string `json:"mode"`
	EnvSecret  string `json:"env_secret"`
	OpenSecret string `json:"open_secret"`
}

func runSecretConsumer(t *testing.T, bin, mode string, secrets map[string]string) secretReport {
	t.Helper()

	outputPath := filepath.Join(t.TempDir(), "secret-report.json")
	loader := adapterhost.NewLoaderWithDiscovery(func(requested string) (string, error) {
		if requested != "secretconsumer" {
			return "", fmt.Errorf("unexpected adapter request %q (expected %q)", requested, "secretconsumer")
		}
		return bin, nil
	})
	defer func() {
		_ = loader.Shutdown(context.Background())
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	plug, sessionID := resolveAndOpen(t, ctx, loader, "secretconsumer", map[string]string{
		"mode":        mode,
		"output_path": outputPath,
	}, secrets)
	defer plug.Kill()

	step := baseStep("secret-channel", "secretconsumer", nil)
	sink := &recordingSink{}
	if _, err := plug.Execute(ctx, sessionID, step, sink); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if err := plug.CloseSession(ctx, sessionID); err != nil {
		t.Fatalf("close session: %v", err)
	}

	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	var report secretReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}
	return report
}

func buildSecretConsumer(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve caller path")
	}
	moduleRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
	adapterBin := filepath.Join(t.TempDir(), "criteria-adapter-secretconsumer")

	cmd := exec.Command("go", "build", "-o", adapterBin, "./internal/adapter/conformance/testfixtures/secretconsumer")
	cmd.Dir = moduleRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build secretconsumer adapter: %v\n%s", err, string(output))
	}
	return adapterBin
}
