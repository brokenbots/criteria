package main

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/brokenbots/criteria/internal/adapter/conformance"
)

// TestShellAdapterConformance runs the full adapter conformance suite against
// the out-of-process shell adapter binary, proving it speaks protocol v2
// correctly end-to-end (handshake, session lifecycle, Execute result, logs).
func TestShellAdapterConformance(t *testing.T) {
	adapterBin := buildShellAdapter(t)
	conformance.RunAdapter(
		t,
		"shell",
		adapterBin,
		conformance.Options{
			StepConfig:      map[string]string{"command": "echo conformance"},
			AllowedOutcomes: []string{"success", "failure"},
		},
	)
}

func buildShellAdapter(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve caller path")
	}
	moduleRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	adapterBin := filepath.Join(t.TempDir(), "criteria-adapter-shell")

	cmd := exec.Command("go", "build", "-o", adapterBin, "./cmd/criteria-adapter-shell")
	cmd.Dir = moduleRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build shell adapter: %v\n%s", err, string(output))
	}
	return adapterBin
}
