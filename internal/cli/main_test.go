package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	// Do not inherit a CRITERIA_HOME or CRITERIA_STATE_DIR from the outer test
	// runner; tests that need an explicit state directory set it via t.Setenv.
	_ = os.Unsetenv("CRITERIA_HOME")
	_ = os.Unsetenv("CRITERIA_STATE_DIR")

	// Build the noop adapter once and expose it via CRITERIA_ADAPTERS so tests
	// that execute real workflows can discover an adapter out-of-process (shell
	// and other adapters are external binaries, not in-process builtins). Tests
	// that need a different plugin set override CRITERIA_ADAPTERS via t.Setenv.
	dir, err := buildNoopPluginsDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = os.Setenv("CRITERIA_ADAPTERS", dir)

	// IgnoreCurrent captures goroutines started before tests run (e.g. race
	// detector, test infrastructure) so they do not trigger false positives.
	// Engine+fake-harness tests use per-test goleak.VerifyNone(t) via
	// requireNoGoroutineLeak to assert that HTTP/2 transport goroutines are
	// cleaned up individually for each test.
	goleak.VerifyTestMain(m, goleak.IgnoreCurrent())
}

// buildNoopPluginsDir compiles cmd/criteria-adapter-noop into a fresh temp
// directory and returns that directory (suitable for CRITERIA_ADAPTERS).
func buildNoopPluginsDir() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("resolve caller path")
	}
	moduleRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	dir, err := os.MkdirTemp("", "criteria-cli-plugins")
	if err != nil {
		return "", err
	}
	cmd := exec.Command("go", "build", "-o", filepath.Join(dir, "criteria-adapter-noop"), "./internal/adapter/conformance/testdata/noop")
	cmd.Dir = moduleRoot
	if out, buildErr := cmd.CombinedOutput(); buildErr != nil {
		return "", fmt.Errorf("build noop adapter: %w\n%s", buildErr, out)
	}
	return dir, nil
}
