package conformance_test

// noop_adapter_test.go — v2 reference conformance check.
// Builds the in-tree noop adapter from testdata/noop/ and runs the full
// conformance suite against it. This is the WS03 Step 8 deliverable: a
// v2-protocol adapter that passes every applicable sub-test.

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/brokenbots/criteria/internal/adapter/conformance"
)

func TestNoopAdapterConformance(t *testing.T) {
	adapterBin := buildNoopAdapter(t)
	conformance.RunAdapter(
		t,
		"noop",
		adapterBin,
		conformance.Options{
			StepConfig:       map[string]string{"delay_ms": "0"},
			AllowedOutcomes:  []string{"success"},
			PermissionConfig: map[string]string{"emit_permission_request": "true"},
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

	cmd := exec.Command("go", "build", "-o", adapterBin, "./internal/adapter/conformance/testdata/noop")
	cmd.Dir = moduleRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build noop adapter: %v\n%s", err, string(output))
	}
	return adapterBin
}
