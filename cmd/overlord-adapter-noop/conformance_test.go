package main

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/brokenbots/overseer/internal/adapter/conformance"
)

func TestNoopPluginConformance(t *testing.T) {
	pluginBin := buildNoopPlugin(t)
	conformance.RunPlugin(
		t,
		"noop",
		pluginBin,
		conformance.Options{
			StepConfig:      map[string]string{"prompt": "hello", "delay_ms": "10"},
			AllowedOutcomes: []string{"success", "failure", "needs_review"},
		},
	)
}

func buildNoopPlugin(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve caller path")
	}
	moduleRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	pluginBin := filepath.Join(t.TempDir(), "overlord-adapter-noop")

	cmd := exec.Command("go", "build", "-o", pluginBin, "./cmd/overlord-adapter-noop")
	cmd.Dir = moduleRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build noop plugin: %v\n%s", err, string(output))
	}
	return pluginBin
}
