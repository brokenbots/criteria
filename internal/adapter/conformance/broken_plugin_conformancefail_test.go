//go:build conformancefail

package conformance_test

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/brokenbots/criteria/internal/adapter/conformance"
)

func TestBrokenPluginConformanceFixture(t *testing.T) {
	pluginBin := buildBrokenPlugin(t)
	conformance.RunPlugin(
		t,
		"broken",
		pluginBin,
		conformance.Options{
			StepConfig:      map[string]string{"prompt": "hello"},
			AllowedOutcomes: []string{"success", "failure", "needs_review"},
		},
	)
}

func buildBrokenPlugin(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve caller path")
	}
	moduleRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
	pluginBin := filepath.Join(t.TempDir(), "criteria-adapter-broken")

	cmd := exec.Command("go", "build", "-o", pluginBin, "./internal/adapter/conformance/testfixtures/broken")
	cmd.Dir = moduleRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build broken plugin: %v\n%s", err, string(output))
	}
	return pluginBin
}
