package adapterhost_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/brokenbots/criteria/internal/adapter/conformance"
)

// TestPublicSDKFixtureConformance proves that a plugin built exclusively
// against sdk/adapterhost (no internal/ reach-through) passes the full adapter
// conformance harness. This is the golden signal that the public package
// surface is sufficient for external adapter authors.
func TestPublicSDKFixtureConformance(t *testing.T) {
	bin := buildPublicSDKFixture(t)
	conformance.RunAdapter(
		t,
		"public-sdk-fixture",
		bin,
		conformance.Options{
			// StepConfig with delay_ms enables context_cancellation and step_timeout
			// sub-tests, proving context propagation works across the plugin subprocess
			// boundary when using only the public sdk/adapterhost surface.
			StepConfig:      map[string]string{"delay_ms": "0"},
			AllowedOutcomes: []string{"success", "failure", "needs_review"},
		},
	)
}

var (
	buildPublicSDKOnce sync.Once
	publicSDKBinPath   string
)

// buildPublicSDKFixture returns the path to the public-sdk fixture binary,
// building it only once for the test-binary lifetime so that -count=N runs
// don't trigger N concurrent `go build` invocations.
func buildPublicSDKFixture(t *testing.T) string {
	t.Helper()
	buildPublicSDKOnce.Do(func() {
		_, file, _, ok := runtime.Caller(0)
		if !ok {
			panic("plugin_test: resolve caller path")
		}
		moduleRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
		dir, err := os.MkdirTemp("", "criteria-publicsdk-test-")
		if err != nil {
			panic("plugin_test: create temp dir: " + err.Error())
		}
		bin := filepath.Join(dir, "criteria-adapter-public-sdk-fixture")
		cmd := exec.Command("go", "build", "-o", bin,
			"./internal/adapterhost/testfixtures/publicsdk")
		cmd.Dir = moduleRoot
		if output, err := cmd.CombinedOutput(); err != nil {
			panic("plugin_test: build public-sdk fixture: " + err.Error() + "\n" + string(output))
		}
		publicSDKBinPath = bin
	})
	if publicSDKBinPath == "" {
		t.Fatal("buildPublicSDKFixture: binary path not set after sync.Once")
	}
	return publicSDKBinPath
}
