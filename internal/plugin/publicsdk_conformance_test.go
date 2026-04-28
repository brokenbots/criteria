package plugin_test

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/brokenbots/criteria/internal/adapter/conformance"
)

// TestPublicSDKFixtureConformance proves that a plugin built exclusively
// against sdk/pluginhost (no internal/ reach-through) passes the full adapter
// conformance harness. This is the golden signal that the public package
// surface is sufficient for external adapter authors.
func TestPublicSDKFixtureConformance(t *testing.T) {
	bin := buildPublicSDKFixture(t)
	conformance.RunPlugin(
		t,
		"public-sdk-fixture",
		bin,
		conformance.Options{
			// StepConfig with delay_ms enables context_cancellation and step_timeout
			// sub-tests, proving context propagation works across the plugin subprocess
			// boundary when using only the public sdk/pluginhost surface.
			StepConfig:      map[string]string{"delay_ms": "0"},
			AllowedOutcomes: []string{"success", "failure", "needs_review"},
		},
	)
}

func buildPublicSDKFixture(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve caller path")
	}
	moduleRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	pluginBin := filepath.Join(t.TempDir(), "criteria-adapter-public-sdk-fixture")

	cmd := exec.Command("go", "build", "-o", pluginBin,
		"./internal/plugin/testfixtures/publicsdk")
	cmd.Dir = moduleRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build public-sdk fixture: %v\n%s", err, string(output))
	}
	return pluginBin
}
