package adapterhost

import (
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

// testSandboxShimBin is the path to a dedicated Linux sandbox shim helper
// binary. It is only built on Linux; on other platforms it remains empty and
// the Linux-only sandbox tests that need it are skipped.
var testSandboxShimBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "criteria-adapter-tests-")
	if err != nil {
		panic("adapterhost/main_test.go: create temp dir: " + err.Error())
	}

	testNoopAdapterBin = buildTestNoopAdapter(dir)
	if runtime.GOOS == "linux" {
		testSandboxShimBin = buildTestSandboxShim(dir)
	}

	// IgnoreCurrent captures any goroutines started by the Go runtime or
	// test infrastructure before our tests run (e.g. the race detector's own
	// goroutines). This avoids false-positive leak reports for goroutines that
	// exist before any test code executes.
	goleak.VerifyTestMain(m, goleak.IgnoreCurrent())
}

// buildTestNoopAdapter compiles the noop adapter into dir. The binary is built
// once; every test reads testNoopAdapterBin rather than triggering a fresh
// build.
func buildTestNoopAdapter(dir string) string {
	bin := filepath.Join(dir, "criteria-adapter-noop")
	cmd := exec.Command("go", "build", "-o", bin, "./internal/adapter/conformance/testdata/noop")
	cmd.Dir = moduleRootFromCaller()
	if out, err := cmd.CombinedOutput(); err != nil {
		panic("adapterhost/main_test.go: build noop adapter: " + err.Error() + "\n" + string(out))
	}
	return bin
}

// buildTestSandboxShim compiles the Linux-only sandbox shim helper into dir.
func buildTestSandboxShim(dir string) string {
	bin := filepath.Join(dir, "criteria-sandbox-shim")
	cmd := exec.Command("go", "build", "-o", bin, "./internal/adapterhost/testdata/sandboxshim")
	cmd.Dir = moduleRootFromCaller()
	if out, err := cmd.CombinedOutput(); err != nil {
		panic("adapterhost/main_test.go: build sandbox shim: " + err.Error() + "\n" + string(out))
	}
	return bin
}

func moduleRootFromCaller() string {
	_, file, _, ok := runtime.Caller(1)
	if !ok {
		panic("adapterhost/main_test.go: resolve caller path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}
