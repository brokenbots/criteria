package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func buildNoopPluginBinary(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve caller path")
	}
	moduleRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	binary := filepath.Join(t.TempDir(), "criteria-adapter-noop")

	cmd := exec.Command("go", "build", "-o", binary, "./cmd/criteria-adapter-noop")
	cmd.Dir = moduleRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build noop plugin: %v\n%s", err, string(out))
	}
	return binary
}

func writeWorkflowFile(t *testing.T, contents string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "workflow.hcl")
	if err := os.WriteFile(p, []byte(strings.TrimSpace(contents)+"\n"), 0o600); err != nil {
		t.Fatalf("write workflow file: %v", err)
	}
	return p
}