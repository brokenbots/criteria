package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/brokenbots/criteria/internal/testutil"
)

var (
	cliNoopBinOnce sync.Once
	cliNoopBinPath string
	cliFailBinOnce sync.Once
	cliFailBinPath string
)

func buildNoopAdapterBinary(t *testing.T) string {
	t.Helper()
	testutil.RequireUnixSockets(t)
	cliNoopBinOnce.Do(func() {
		_, file, _, ok := runtime.Caller(0)
		if !ok {
			t.Fatal("resolve caller path")
		}
		moduleRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
		dir, err := os.MkdirTemp("", "criteria-cli-noop-*")
		if err != nil {
			t.Fatalf("create temp dir: %v", err)
		}
		binary := filepath.Join(dir, "criteria-adapter-noop")

		cmd := exec.Command("go", "build", "-o", binary, "./internal/adapter/conformance/testdata/noop")
		cmd.Dir = moduleRoot
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("build noop adapter: %v\n%s", err, string(out))
		}
		cliNoopBinPath = binary
	})
	return cliNoopBinPath
}

// buildFailAdapterBinary builds a test adapter that always returns outcome
// "failure". The binary name is "criteria-adapter-fail" so workflows can
// target adapter.fail.default.
func buildFailAdapterBinary(t *testing.T) string {
	t.Helper()
	testutil.RequireUnixSockets(t)
	cliFailBinOnce.Do(func() {
		_, file, _, ok := runtime.Caller(0)
		if !ok {
			t.Fatal("resolve caller path")
		}
		moduleRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
		dir, err := os.MkdirTemp("", "criteria-cli-fail-*")
		if err != nil {
			t.Fatalf("create temp dir: %v", err)
		}
		binary := filepath.Join(dir, "criteria-adapter-fail")

		cmd := exec.Command("go", "build", "-o", binary, "./internal/adapter/conformance/testdata/fail")
		cmd.Dir = moduleRoot
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("build fail adapter: %v\n%s", err, string(out))
		}
		cliFailBinPath = binary
	})
	return cliFailBinPath
}

func writeWorkflowFile(t *testing.T, contents string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "workflow.hcl")
	if err := os.WriteFile(p, []byte(strings.TrimSpace(contents)+"\n"), 0o600); err != nil {
		t.Fatalf("write workflow file: %v", err)
	}
	return p
}

// stripHCLExt removes a recognised HCL extension (.chcl or .hcl) from name.
func stripHCLExt(name string) string {
	for _, ext := range HCLExtensions {
		if strings.HasSuffix(name, ext) {
			return strings.TrimSuffix(name, ext)
		}
	}
	return name
}

// splitNDJSONLines splits a newline-delimited JSON string into non-empty lines.
func splitNDJSONLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// ndjsonEnvelope is a generic payload envelope for criteria events in ND-JSON format.
type ndjsonEnvelope struct {
	PayloadType string          `json:"payload_type"`
	Payload     json.RawMessage `json:"payload"`
}
