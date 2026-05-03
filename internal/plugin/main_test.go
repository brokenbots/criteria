package plugin

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"go.uber.org/goleak"
)

// testNoopPluginBin is the path to the noop adapter binary compiled once for
// the entire test-binary lifetime. Building in TestMain means concurrent
// -count=N runs share the same binary instead of racing N parallel `go build`
// invocations, which was the root cause of TestHandshakeInfo flaking under
// -race -count=3 on loaded hosts.
var testNoopPluginBin string

func TestMain(m *testing.M) {
	testNoopPluginBin = buildTestNoopPlugin()
	// IgnoreCurrent captures any goroutines started by the Go runtime or
	// test infrastructure before our tests run (e.g. the race detector's own
	// goroutines). This avoids false-positive leak reports for goroutines that
	// exist before any test code executes.
	goleak.VerifyTestMain(m, goleak.IgnoreCurrent())
}

// buildTestNoopPlugin compiles the noop adapter into an OS temp dir that
// persists for the duration of the test binary. The binary is built once;
// every test reads testNoopPluginBin rather than triggering a fresh build.
func buildTestNoopPlugin() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("plugin/main_test.go: resolve caller path")
	}
	moduleRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	dir, err := os.MkdirTemp("", "criteria-plugin-tests-")
	if err != nil {
		panic("plugin/main_test.go: create temp dir: " + err.Error())
	}
	bin := filepath.Join(dir, "criteria-adapter-noop")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/criteria-adapter-noop")
	cmd.Dir = moduleRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		panic("plugin/main_test.go: build noop plugin: " + err.Error() + "\n" + string(out))
	}
	return bin
}
