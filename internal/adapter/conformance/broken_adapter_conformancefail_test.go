//go:build conformancefail

package conformance_test

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/brokenbots/criteria/internal/adapter/conformance"
)

func TestBrokenAdapterConformanceFixture(t *testing.T) {
	adapterBin := buildBrokenAdapter(t)
	conformance.RunAdapter(
		t,
		"broken",
		adapterBin,
		conformance.Options{
			StepConfig:      map[string]string{"prompt": "hello"},
			AllowedOutcomes: []string{"success", "failure", "needs_review"},
		},
	)
}

func buildBrokenAdapter(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve caller path")
	}
	moduleRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
	adapterBin := filepath.Join(t.TempDir(), "criteria-adapter-broken")

	cmd := exec.Command("go", "build", "-o", adapterBin, "./internal/adapter/conformance/testfixtures/broken")
	cmd.Dir = moduleRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build broken adapter: %v\n%s", err, string(output))
	}
	return adapterBin
}
