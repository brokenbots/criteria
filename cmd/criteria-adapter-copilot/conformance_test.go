package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapter/conformance"
	"github.com/brokenbots/criteria/internal/plugin"
	"github.com/brokenbots/criteria/workflow"
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
	testPluginBin = buildBinary(moduleRoot, "./cmd/criteria-adapter-copilot", "criteria-adapter-copilot-test")
	testFakeBin = buildBinary(moduleRoot, "./cmd/criteria-adapter-copilot/testfixtures/fake-copilot", "fake-copilot-test")
	os.Exit(m.Run())
}

// TestCopilotPluginConformance runs the full conformance suite against the
// copilot plugin binary.
//
// By default it uses the deterministic fake-copilot stub so no real copilot
// CLI or network access is required. Set COPILOT_E2E=1 to use the real copilot
// CLI instead (requires copilot CLI on PATH or CRITERIA_COPILOT_BIN set):
//
//	COPILOT_E2E=1 go test ./cmd/criteria-adapter-copilot/... -run Conformance
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

// applyFakeIfNeeded sets CRITERIA_COPILOT_BIN to the deterministic fake
// binary unless COPILOT_E2E=1 is set, in which case whatever binary is at
// CRITERIA_COPILOT_BIN (or "copilot" on PATH) is used instead.
func applyFakeIfNeeded(t *testing.T) {
	t.Helper()
	if os.Getenv("COPILOT_E2E") != "1" {
		t.Setenv("CRITERIA_COPILOT_BIN", testFakeBin)
	}
}

// TestCopilotE2ERouting verifies the routing invariant: when COPILOT_E2E=1 is
// set, applyFakeIfNeeded must NOT override CRITERIA_COPILOT_BIN to the fake
// binary, so a real CLI (or any caller-supplied binary) is used instead.
//
// This test protects against a future refactor accidentally inverting or
// removing the guard so the fake always runs regardless of the env var.
func TestCopilotE2ERouting(t *testing.T) {
	t.Run("fake_used_when_e2e_unset", func(t *testing.T) {
		t.Setenv("COPILOT_E2E", "")
		t.Setenv("CRITERIA_COPILOT_BIN", "/some/other/binary")
		applyFakeIfNeeded(t)
		if got := os.Getenv("CRITERIA_COPILOT_BIN"); got != testFakeBin {
			t.Fatalf("expected CRITERIA_COPILOT_BIN=%q (fake), got %q", testFakeBin, got)
		}
	})

	t.Run("fake_not_used_when_e2e_set", func(t *testing.T) {
		sentinel := testPluginBin + "-real"
		t.Setenv("COPILOT_E2E", "1")
		t.Setenv("CRITERIA_COPILOT_BIN", sentinel)
		applyFakeIfNeeded(t)
		if got := os.Getenv("CRITERIA_COPILOT_BIN"); got != sentinel {
			t.Fatalf("COPILOT_E2E=1 must not override CRITERIA_COPILOT_BIN: got %q, want %q (fake is %q)", got, sentinel, testFakeBin)
		}
	})
}

// TestCopilotPluginBuilds verifies the plugin binary exists and is executable.
func TestCopilotPluginBuilds(t *testing.T) {
	if _, err := os.Stat(testPluginBin); err != nil {
		t.Fatalf("plugin binary not found at %q: %v", testPluginBin, err)
	}
}

// TestCopilotReasoningEffortOverride (test 6.8) exercises the full
// agent-open → per-step reasoning_effort override → restore flow end-to-end
// against the fake copilot binary. It validates that:
//   - Opening a session with reasoning_effort succeeds.
//   - Executing a step with a per-step reasoning_effort override succeeds
//     and returns a valid outcome.
//   - Executing a follow-up step without per-step effort also succeeds
//     (verifying the restore did not break session state).
//
// Run by make test-conformance.
func TestCopilotReasoningEffortOverride(t *testing.T) {
	applyFakeIfNeeded(t)

	loader := plugin.NewLoaderWithDiscovery(func(requested string) (string, error) {
		if requested != "copilot" {
			return "", fmt.Errorf("unexpected plugin %q", requested)
		}
		return testPluginBin, nil
	})
	t.Cleanup(func() { _ = loader.Shutdown(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	plug, err := loader.Resolve(ctx, "copilot")
	if err != nil {
		t.Fatalf("resolve plugin: %v", err)
	}

	sessionID := "effort-override-test-session"

	// Open with agent-level reasoning_effort = "medium".
	if err := plug.OpenSession(ctx, sessionID, map[string]string{
		"reasoning_effort": "medium",
	}); err != nil {
		t.Fatalf("OpenSession with reasoning_effort=medium: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = plug.CloseSession(closeCtx, sessionID)
		cancel()
		plug.Kill()
	})

	// Step 1: execute with per-step reasoning_effort override "high".
	step1 := &workflow.StepNode{
		Name:  "planning",
		Agent: "bot",
		Input: map[string]string{
			"prompt":           "Reply with only: RESULT: success",
			"reasoning_effort": "high",
		},
	}
	result1, err := plug.Execute(ctx, sessionID, step1, &discardSink{})
	if err != nil {
		t.Fatalf("Execute step1 (per-step effort override): %v", err)
	}
	if result1.Outcome == "" {
		t.Fatal("step1 returned empty outcome")
	}

	// Step 2: execute without per-step effort (inherits agent default after restore).
	step2 := &workflow.StepNode{
		Name:  "execution",
		Agent: "bot",
		Input: map[string]string{
			"prompt": "Reply with only: RESULT: success",
		},
	}
	result2, err := plug.Execute(ctx, sessionID, step2, &discardSink{})
	if err != nil {
		t.Fatalf("Execute step2 (after effort restore): %v", err)
	}
	if result2.Outcome == "" {
		t.Fatal("step2 returned empty outcome after effort restore")
	}
}

// discardSink is a no-op adapter.EventSink used in conformance tests that only
// need to verify outcomes and errors rather than individual events.
type discardSink struct{}

func (*discardSink) Log(_ string, _ []byte)   {}
func (*discardSink) Adapter(_ string, _ any)  {}

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

