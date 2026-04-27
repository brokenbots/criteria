package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/brokenbots/overseer/internal/adapter/conformance"
)

var (
	testPluginBin string
	testFakeBin   string
)

func TestMain(m *testing.M) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("resolve caller path")
	}
	moduleRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	testPluginBin = buildBinary(moduleRoot, "./cmd/overseer-adapter-copilot", "overseer-adapter-copilot-test")
	testFakeBin = buildBinary(moduleRoot, "./cmd/overseer-adapter-copilot/testfixtures/fake-copilot", "fake-copilot-test")
	os.Exit(m.Run())
}

// TestCopilotPluginConformance runs the full conformance suite against the
// copilot plugin binary.
//
// By default it uses the deterministic fake-copilot stub so no real copilot
// CLI or network access is required. Set COPILOT_E2E=1 to use the real copilot
// CLI instead (requires copilot CLI on PATH or OVERSEER_COPILOT_BIN set):
//
//	COPILOT_E2E=1 go test ./cmd/overseer-adapter-copilot/... -run Conformance
func TestCopilotPluginConformance(t *testing.T) {
	opts := conformance.Options{
		StepConfig: map[string]string{
			"prompt": "Reply with only: RESULT: success",
		},
		PermissionConfig: map[string]string{
			// Force a fetch tool invocation so the fake emits a permission
			// request. The host deny policy drives the outcome to needs_review.
			"prompt": "Use the fetch tool (web/URL fetcher) to retrieve the contents of https://api.github.com/zen — you must invoke the tool to fetch the URL, not guess. After the fetch completes, end your response with `RESULT: success`.",
		},
		AllowedOutcomes: []string{"success", "failure", "needs_review"},
		Streaming:       true,
	}

	applyFakeIfNeeded(t)
	conformance.RunPlugin(t, "copilot", testPluginBin, opts)
}

// applyFakeIfNeeded sets OVERSEER_COPILOT_BIN to the deterministic fake
// binary unless COPILOT_E2E=1 is set, in which case whatever binary is at
// OVERSEER_COPILOT_BIN (or "copilot" on PATH) is used instead.
func applyFakeIfNeeded(t *testing.T) {
	t.Helper()
	if os.Getenv("COPILOT_E2E") != "1" {
		t.Setenv("OVERSEER_COPILOT_BIN", testFakeBin)
	}
}

// TestCopilotE2ERouting verifies the routing invariant: when COPILOT_E2E=1 is
// set, applyFakeIfNeeded must NOT override OVERSEER_COPILOT_BIN to the fake
// binary, so a real CLI (or any caller-supplied binary) is used instead.
//
// This test protects against a future refactor accidentally inverting or
// removing the guard so the fake always runs regardless of the env var.
func TestCopilotE2ERouting(t *testing.T) {
	t.Run("fake_used_when_e2e_unset", func(t *testing.T) {
		t.Setenv("COPILOT_E2E", "")
		t.Setenv("OVERSEER_COPILOT_BIN", "/some/other/binary")
		applyFakeIfNeeded(t)
		if got := os.Getenv("OVERSEER_COPILOT_BIN"); got != testFakeBin {
			t.Fatalf("expected OVERSEER_COPILOT_BIN=%q (fake), got %q", testFakeBin, got)
		}
	})

	t.Run("fake_not_used_when_e2e_set", func(t *testing.T) {
		sentinel := testPluginBin + "-real"
		t.Setenv("COPILOT_E2E", "1")
		t.Setenv("OVERSEER_COPILOT_BIN", sentinel)
		applyFakeIfNeeded(t)
		if got := os.Getenv("OVERSEER_COPILOT_BIN"); got != sentinel {
			t.Fatalf("COPILOT_E2E=1 must not override OVERSEER_COPILOT_BIN: got %q, want %q (fake is %q)", got, sentinel, testFakeBin)
		}
	})
}

// TestCopilotPluginBuilds verifies the plugin binary exists and is executable.
func TestCopilotPluginBuilds(t *testing.T) {
	if _, err := os.Stat(testPluginBin); err != nil {
		t.Fatalf("plugin binary not found at %q: %v", testPluginBin, err)
	}
}

// buildBinary compiles the package at pkgPath in moduleRoot and writes the
// binary to os.TempDir(). It panics if compilation fails.
func buildBinary(moduleRoot, pkgPath, binName string) string {
	out := filepath.Join(os.TempDir(), binName)
	cmd := exec.Command("go", "build", "-o", out, pkgPath)
	cmd.Dir = moduleRoot
	if output, err := cmd.CombinedOutput(); err != nil {
		panic("build " + pkgPath + ": " + err.Error() + "\n" + string(output))
	}
	return out
}

