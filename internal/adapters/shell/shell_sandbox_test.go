package shell_test

// shell_sandbox_test.go — hardening tests for W05 sandbox defaults.
//
// Tests follow the Step 3 specification verbatim. All six tests run under
// `make test` with no network access and no external binaries beyond what
// is available on a standard CI runner (sh, sleep, python3).

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapters/shell"
	"github.com/brokenbots/criteria/workflow"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// adapterEvents collects Adapter-kind events emitted via sink.Adapter.
type adapterEvents struct {
	events []map[string]any
}

func (s *adapterEvents) Log(string, []byte) {}
func (s *adapterEvents) Adapter(_ string, data any) {
	if m, ok := data.(map[string]any); ok {
		s.events = append(s.events, m)
	}
}

func (s *adapterEvents) findByType(eventType string) (map[string]any, bool) {
	for _, ev := range s.events {
		if ev["event_type"] == eventType {
			return ev, true
		}
	}
	return nil, false
}

// makeSandboxStep builds a StepNode for sandbox tests. The step uses only the
// fields that the sandbox hardening reads; caller supplies the full input map.
func makeSandboxStep(input map[string]string) *workflow.StepNode {
	return &workflow.StepNode{
		Name:       "sandbox-test",
		TargetKind: workflow.StepTargetAdapter,
		AdapterRef: shell.Name,
		Input:      input,
		Outcomes: map[string]string{
			"success": "__done__",
			"failure": "__done__",
		},
	}
}

// ── Test 1: env allowlist ────────────────────────────────────────────────────

func TestSandbox_EnvAllowlist_SecretDropped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell adapter uses sh; skip on Windows")
	}
	t.Setenv("SECRET", "super-secret-value")
	// No env declaration in step input — SECRET must not reach the child.
	a := shell.New()
	result, err := a.Execute(context.Background(), makeSandboxStep(map[string]string{
		"command": `printf '%s' "$SECRET"`,
	}), noopSink{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.TrimSpace(result.Outputs["stdout"]) != "" {
		t.Errorf("expected empty stdout (SECRET must not leak); got %q", result.Outputs["stdout"])
	}
}

func TestSandbox_EnvAllowlist_DeclaredSecretPropagated(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell adapter uses sh; skip on Windows")
	}
	t.Setenv("SECRET", "super-secret-value")
	// env = jsonencode({"SECRET": "$SECRET"}) → inherit SECRET from parent.
	envJSON, _ := json.Marshal(map[string]string{"SECRET": "$SECRET"})
	a := shell.New()
	result, err := a.Execute(context.Background(), makeSandboxStep(map[string]string{
		"command": `printf '%s' "$SECRET"`,
		"env":     string(envJSON),
	}), noopSink{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := strings.TrimSpace(result.Outputs["stdout"])
	if got != "super-secret-value" {
		t.Errorf("expected stdout %q; got %q", "super-secret-value", got)
	}
}

func TestSandbox_EnvAllowlist_PATHInEnvRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell adapter uses sh; skip on Windows")
	}
	envJSON, _ := json.Marshal(map[string]string{"PATH": "/tmp"})
	a := shell.New()
	_, err := a.Execute(context.Background(), makeSandboxStep(map[string]string{
		"command": "true",
		"env":     string(envJSON),
	}), noopSink{})
	if err == nil {
		t.Fatal("expected error when PATH is set via env; got nil")
	}
	if !strings.Contains(err.Error(), "PATH") {
		t.Errorf("expected error message to mention PATH; got %v", err)
	}
}

// ── Test 2: command path hygiene ─────────────────────────────────────────────

func TestSandbox_CommandPathHygiene_DotInPathDropped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell adapter uses sh; skip on Windows")
	}
	// Create a temp dir containing an executable named 'evil'.
	binDir := t.TempDir()
	evilPath := filepath.Join(binDir, "evil")
	if err := os.WriteFile(evilPath, []byte("#!/bin/sh\necho EVIL_RAN\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Parent PATH includes "." so that if CWD == binDir, "evil" would be found
	// via ".". The sandbox strips "." from PATH for the child, so "evil" must
	// not run. CRITERIA_SHELL_ALLOWED_PATHS is set so working_directory=binDir
	// passes the confinement check.
	t.Setenv("PATH", ".:/bin:/usr/bin:/usr/local/bin")
	t.Setenv("CRITERIA_SHELL_ALLOWED_PATHS", binDir)

	a := shell.New()
	result, err := a.Execute(context.Background(), makeSandboxStep(map[string]string{
		"command":           "evil",
		"working_directory": binDir,
	}), noopSink{})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	// "." was stripped from PATH; "evil" must not have run.
	if strings.Contains(result.Outputs["stdout"], "EVIL_RAN") {
		t.Error("expected 'evil' to not run; sandbox did not strip '.' from PATH")
	}
	// Missing command should produce a failure outcome with a non-zero exit code.
	if result.Outcome != "failure" {
		t.Errorf("expected outcome 'failure' for missing command; got %q", result.Outcome)
	}
	exitCode, ok := result.Outputs["exit_code"]
	if !ok || exitCode == "" {
		t.Fatalf("expected exit_code output for missing command; outputs=%v", result.Outputs)
	}
	if exitCode == "0" {
		t.Fatalf("expected non-zero exit_code for missing command; outputs=%v", result.Outputs)
	}
}

func TestSandbox_CommandPathHygiene_ExplicitPathRuns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell adapter uses sh; skip on Windows")
	}
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	evilPath := filepath.Join(binDir, "mybin")
	if err := os.WriteFile(evilPath, []byte("#!/bin/sh\necho MYBIN_RAN\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	a := shell.New()
	// Explicit command_path that includes binDir → mybin should run.
	result, err := a.Execute(context.Background(), makeSandboxStep(map[string]string{
		"command":      "mybin",
		"command_path": binDir,
	}), noopSink{})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !strings.Contains(result.Outputs["stdout"], "MYBIN_RAN") {
		t.Errorf("expected 'mybin' to run with explicit command_path; stdout=%q", result.Outputs["stdout"])
	}
}

// ── Test 3: hard timeout ──────────────────────────────────────────────────────

func TestSandbox_Timeout_ShortCommandFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal-based timeout test not supported on Windows")
	}

	ev := &adapterEvents{}
	a := shell.New()

	// Wall-clock budget: 1s timeout + 5s kill grace + 3s buffer = 9s.
	start := time.Now()
	deadline := 9 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	result, err := a.Execute(ctx, makeSandboxStep(map[string]string{
		"command": "sleep 60",
		"timeout": "1s",
	}), ev)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Execute returned unexpected error: %v", err)
	}
	if result.Outcome != "failure" {
		t.Errorf("expected outcome 'failure'; got %q", result.Outcome)
	}
	if elapsed > deadline {
		t.Errorf("test took %v, expected < %v (timeout + grace + buffer)", elapsed, deadline)
	}
	if _, found := ev.findByType("timeout"); !found {
		t.Errorf("expected 'timeout' adapter event; got events: %v", ev.events)
	}
}

// ── Test 4: bounded output capture ───────────────────────────────────────────

func TestSandbox_BoundedOutput_TruncatesAtLimit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell adapter uses sh; skip on Windows")
	}

	const limitBytes = 1024 * 1024 // 1 MiB

	ev := &adapterEvents{}
	a := shell.New()
	// Generate 10 MiB of stdout.
	result, err := a.Execute(context.Background(), makeSandboxStep(map[string]string{
		"command":            `python3 -c "import sys; sys.stdout.write('x' * (10 * 1024 * 1024))"`,
		"output_limit_bytes": "1048576",
	}), ev)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Outcome != "success" {
		t.Errorf("expected success (truncation is non-fatal); got %q", result.Outcome)
	}

	stdoutLen := len(result.Outputs["stdout"])
	if stdoutLen != limitBytes {
		t.Errorf("stdout length %d; expected exactly %d (limit)", stdoutLen, limitBytes)
	}

	if result.Outputs["_truncated_stdout"] != "true" {
		t.Error("expected _truncated_stdout sentinel to be set")
	}

	ev2, found := ev.findByType("output_truncated")
	if !found {
		t.Fatalf("expected output_truncated adapter event; got: %v", ev.events)
	}
	droppedAny, ok := ev2["dropped_bytes"]
	if !ok {
		t.Fatal("output_truncated event missing dropped_bytes field")
	}
	// JSON numbers decode as float64.
	dropped, ok := droppedAny.(int64)
	if !ok {
		// Accept float64 (JSON round-trip in tests using collectSink).
		if f, ok2 := droppedAny.(float64); ok2 {
			dropped = int64(f)
		} else {
			t.Fatalf("dropped_bytes has unexpected type %T: %v", droppedAny, droppedAny)
		}
	}
	// We emitted ~10 MiB, capped at 1 MiB → ~9 MiB dropped.
	expectedDroppedMin := int64(8 * 1024 * 1024)
	if dropped < expectedDroppedMin {
		t.Errorf("dropped_bytes %d < expected minimum %d", dropped, expectedDroppedMin)
	}
}

// ── Test 5: working-directory confinement ────────────────────────────────────

func TestSandbox_WorkingDirectory_OutsideHomeRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("path confinement test uses Unix paths")
	}
	// Temporarily override HOME to a temp dir so we can control the confinement
	// boundary without depending on the actual home directory structure.
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("CRITERIA_SHELL_ALLOWED_PATHS", "")

	a := shell.New()
	result, err := a.Execute(context.Background(), makeSandboxStep(map[string]string{
		"command":           "pwd",
		"working_directory": "/etc",
	}), noopSink{})

	// Runtime rejection: Execute must return outcome "failure".
	if result.Outcome != "failure" {
		t.Errorf("expected outcome 'failure' for working_directory=/etc outside HOME; got %q", result.Outcome)
	}
	if err == nil {
		t.Errorf("expected a non-nil error for working_directory=/etc outside HOME")
	}
	// The error message should carry the new guidance and must not retain the
	// removed CRITERIA_SHELL_LEGACY=1 suggestion.
	var errMsg string
	if err != nil {
		errMsg = err.Error()
	}
	if !strings.Contains(errMsg, "working_directory") {
		t.Errorf("error message should mention 'working_directory'; got: %q", errMsg)
	}
	if !strings.Contains(errMsg, "CRITERIA_SHELL_ALLOWED_PATHS") {
		t.Errorf("error message should mention 'CRITERIA_SHELL_ALLOWED_PATHS'; got: %q", errMsg)
	}
	if strings.Contains(errMsg, "CRITERIA_SHELL_LEGACY") {
		t.Errorf("error message must not mention removed CRITERIA_SHELL_LEGACY; got: %q", errMsg)
	}
}

func TestSandbox_WorkingDirectory_AllowedPathAccepted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("path confinement test uses Unix paths")
	}
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("CRITERIA_SHELL_ALLOWED_PATHS", "/etc")

	a := shell.New()
	result, err := a.Execute(context.Background(), makeSandboxStep(map[string]string{
		"command":           "pwd",
		"working_directory": "/etc",
	}), noopSink{})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if result.Outcome != "success" {
		t.Errorf("expected success for /etc in CRITERIA_SHELL_ALLOWED_PATHS; outcome=%q", result.Outcome)
	}
	if !strings.Contains(result.Outputs["stdout"], "/etc") {
		t.Errorf("expected stdout to contain /etc; got %q", result.Outputs["stdout"])
	}
}

// ── Test 6: CRITERIA_SHELL_LEGACY=1 is no longer recognized ──────────────────

// TestSandbox_LegacyEnvVarIgnored asserts that CRITERIA_SHELL_LEGACY is no
// longer recognized after v0.2.0 removal (W10). Setting it has no effect on
// sandbox enforcement: the env allowlist still applies.
func TestSandbox_LegacyEnvVarIgnored(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell adapter uses sh; skip on Windows")
	}
	t.Setenv("CRITERIA_SHELL_LEGACY", "1")
	t.Setenv("SECRET", "should-not-leak")

	// With the legacy mode removed, setting CRITERIA_SHELL_LEGACY=1 must have
	// no effect. The env allowlist is unconditional: SECRET must not reach the
	// child even though the var is set.
	a := shell.New()
	result, err := a.Execute(context.Background(), makeSandboxStep(map[string]string{
		"command": `printf '%s' "$SECRET"`,
	}), noopSink{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := strings.TrimSpace(result.Outputs["stdout"]); got != "" {
		t.Errorf("env allowlist must be enforced even with CRITERIA_SHELL_LEGACY=1; SECRET leaked: %q", got)
	}
}
