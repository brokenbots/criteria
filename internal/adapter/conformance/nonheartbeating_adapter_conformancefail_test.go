//go:build conformancefail

package conformance_test

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/brokenbots/criteria/internal/adapter/conformance"
)

func TestNonHeartbeatingAdapterConformanceFixture(t *testing.T) {
	adapterBin := buildNonHeartbeatingAdapter(t)
	conformance.RunAdapter(
		t,
		"nonheartbeating",
		adapterBin,
		conformance.Options{
			StepConfig:      map[string]string{"prompt": "hello"},
			AllowedOutcomes: []string{"success"},
		},
	)
}

func buildNonHeartbeatingAdapter(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve caller path")
	}
	moduleRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
	adapterBin := filepath.Join(t.TempDir(), "criteria-adapter-nonheartbeating")

	cmd := exec.Command("go", "build", "-o", adapterBin, "./internal/adapter/conformance/testfixtures/nonheartbeating")
	cmd.Dir = moduleRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build nonheartbeating adapter: %v\n%s", err, string(output))
	}
	return adapterBin
}
