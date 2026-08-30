package adapterhost

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"go.uber.org/goleak"
)

// testNoopAdapterBin is the path to the noop adapter binary compiled once for
// the entire test-binary lifetime. Building in TestMain means concurrent
// -count=N runs share the same binary instead of racing N parallel `go build`
// invocations, which was the root cause of TestHandshakeInfo flaking under
// -race -count=3 on loaded hosts.
var testNoopAdapterBin string

// sandboxShimEntry is replaced on Linux by a function that applies the
// sandbox restrictions and re-execs the real adapter when the test binary is
// invoked as a pre-exec shim (CRITERIA_SANDBOX_CONFIG_PATH is set). This lets
// integration tests that launch real adapters inside the Linux sandbox use the
// test binary as the shim, matching how the criteria CLI binary behaves.
var sandboxShimEntry = func() (ran bool, err error) { return false, nil }

func TestMain(m *testing.M) {
	if ran, err := sandboxShimEntry(); ran {
		if err != nil {
			fmt.Fprintln(os.Stderr, "sandbox shim failed:", err)
			os.Exit(125)
		}
		// RunIfEnv should have replaced this process via syscall.Exec; reaching
		// here is unexpected but safe to treat as success.
		os.Exit(0)
	} else if err != nil {
		fmt.Fprintln(os.Stderr, "sandbox shim check failed:", err)
		os.Exit(125)
	}

	testNoopAdapterBin = buildTestNoopAdapter()
	// IgnoreCurrent captures any goroutines started by the Go runtime or
	// test infrastructure before our tests run (e.g. the race detector's own
	// goroutines). This avoids false-positive leak reports for goroutines that
	// exist before any test code executes.
	goleak.VerifyTestMain(m, goleak.IgnoreCurrent())
}

// buildTestNoopAdapter compiles the noop adapter into an OS temp dir that
// persists for the duration of the test binary. The binary is built once;
// every test reads testNoopAdapterBin rather than triggering a fresh build.
func buildTestNoopAdapter() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("adapterhost/main_test.go: resolve caller path")
	}
	moduleRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	dir, err := os.MkdirTemp("", "criteria-adapter-tests-")
	if err != nil {
		panic("adapterhost/main_test.go: create temp dir: " + err.Error())
	}
	bin := filepath.Join(dir, "criteria-adapter-noop")
	cmd := exec.Command("go", "build", "-o", bin, "./internal/adapter/conformance/testdata/noop")
	cmd.Dir = moduleRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		panic("adapterhost/main_test.go: build noop adapter: " + err.Error() + "\n" + string(out))
	}
	return bin
}
