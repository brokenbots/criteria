package testutil

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// BuildPermissiveAdapter builds the permissive adapter test fixture and returns
// the output binary path.
func BuildPermissiveAdapter(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve caller path")
	}
	moduleRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	pluginBin := filepath.Join(t.TempDir(), "criteria-adapter-permissive")

	cmd := exec.Command("go", "build", "-o", pluginBin, "./internal/adapterhost/testfixtures/permissive")
	cmd.Dir = moduleRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build permissive adapter: %v\n%s", err, string(output))
	}

	return pluginBin
}
