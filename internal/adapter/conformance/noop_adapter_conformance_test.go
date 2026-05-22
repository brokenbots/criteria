package conformance_test

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/brokenbots/criteria/internal/adapter/conformance"
)

// TestNoopAdapterConformance builds the in-tree noop conformance fixture and
// runs the full conformance suite against it. This is the v2 reference adapter
// for WS03: it must pass every applicable sub-test.
func TestNoopAdapterConformance(t *testing.T) {
	adapterBin := buildNoopAdapter(t)
	conformance.RunAdapter(
		t,
		"noop",
		adapterBin,
		conformance.Options{
			StepConfig:      map[string]string{"prompt": "hello"},
			AllowedOutcomes: []string{"success", "failure", "needs_review"},
		},
	)
}

func buildNoopAdapter(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve caller path")
	}
	moduleRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
	adapterBin := filepath.Join(t.TempDir(), "criteria-adapter-noop-conformance")

	cmd := exec.Command("go", "build", "-o", adapterBin, "./internal/adapter/conformance/testfixtures/noop")
	cmd.Dir = moduleRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build noop adapter: %v\n%s", err, string(output))
	}
	return adapterBin
}
