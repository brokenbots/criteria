package shell

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// run executes the shell core for a test, discarding streamed chunks and
// returning the outcome, outputs, and adapter events.
func run(t *testing.T, input map[string]string) (outcome string, outputs map[string]any, events []adapterEvt, err error) {
	t.Helper()
	return runShell(context.Background(), input, nil, func(string, []byte) {})
}

func findEvent(events []adapterEvt, eventType string) (map[string]any, bool) {
	for _, ev := range events {
		if ev.payload["event_type"] == eventType {
			return ev.payload, true
		}
	}
	return nil, false
}

func TestShell_CapturesStdout(t *testing.T) {
	outcome, outputs, _, err := run(t, map[string]string{"command": "printf 'hello world'"})
	if err != nil {
		t.Fatalf("runShell: %v", err)
	}
	if outcome != "success" {
		t.Errorf("outcome = %q, want 'success'", outcome)
	}
	if outputs["stdout"] != "hello world" {
		t.Errorf("stdout = %q, want 'hello world'", outputs["stdout"])
	}
	if outputs["exit_code"] != "0" {
		t.Errorf("exit_code = %q, want '0'", outputs["exit_code"])
	}
}

func TestShell_CapturesExitCode(t *testing.T) {
	outcome, outputs, _, err := run(t, map[string]string{"command": "exit 2"})
	if err != nil {
		t.Fatalf("runShell: %v", err)
	}
	if outcome != "failure" {
		t.Errorf("outcome = %q, want 'failure'", outcome)
	}
	if outputs["exit_code"] != "2" {
		t.Errorf("exit_code = %q, want '2'", outputs["exit_code"])
	}
}

func TestShell_MissingCommand(t *testing.T) {
	_, _, _, err := run(t, map[string]string{})
	if err == nil {
		t.Error("expected error for missing command")
	}
}

func TestShell_OutputSchema(t *testing.T) {
	info := InfoResponse()
	fields := info.GetOutputSchema().GetFields()
	for _, k := range []string{"stdout", "stderr", "exit_code"} {
		if _, ok := fields[k]; !ok {
			t.Errorf("OutputSchema missing %q", k)
		}
	}
	if !info.GetInputSchema().GetFields()["command"].GetRequired() {
		t.Error("InputSchema: command should be required")
	}
}

func TestShell_SecretEnvInjected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell adapter uses sh; skip on Windows")
	}
	outcome, outputs, _, err := runShell(context.Background(),
		map[string]string{"command": `printf '%s' "$API_KEY"`},
		map[string]string{"API_KEY": "s3cr3t"},
		func(string, []byte) {})
	if err != nil {
		t.Fatalf("runShell: %v", err)
	}
	if outcome != "success" {
		t.Errorf("outcome = %q, want 'success'", outcome)
	}
	if outputs["stdout"] != "s3cr3t" {
		t.Errorf("stdout = %q, want injected secret 's3cr3t'", outputs["stdout"])
	}
}

func TestShell_StdoutCappedAtDefaultLimit(t *testing.T) {
	outcome, outputs, _, err := run(t, map[string]string{
		"command": `for i in $(seq 1 1100); do printf '%064d\n' "$i"; done`,
	})
	if err != nil {
		t.Fatalf("runShell: %v", err)
	}
	if outcome != "success" {
		t.Fatalf("outcome = %q", outcome)
	}
	stdout := outputs["stdout"].(string)
	const defaultCap = 4 * 1024 * 1024
	if len(stdout) > defaultCap || stdout == "" {
		t.Errorf("stdout len=%d unexpected", len(stdout))
	}
	if _, ok := outputs["_truncated_stdout"]; ok {
		t.Errorf("unexpected truncation sentinel; stdout len=%d", len(stdout))
	}
}

// ── sandbox hardening ────────────────────────────────────────────────────────

func TestSandbox_EnvAllowlist_SecretDropped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell adapter uses sh; skip on Windows")
	}
	t.Setenv("SECRET", "super-secret-value")
	_, outputs, _, err := run(t, map[string]string{"command": `printf '%s' "$SECRET"`})
	if err != nil {
		t.Fatalf("runShell: %v", err)
	}
	if strings.TrimSpace(outputs["stdout"].(string)) != "" {
		t.Errorf("expected empty stdout (SECRET must not leak); got %q", outputs["stdout"])
	}
}

func TestSandbox_EnvAllowlist_DeclaredSecretPropagated(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell adapter uses sh; skip on Windows")
	}
	t.Setenv("SECRET", "super-secret-value")
	envJSON, _ := json.Marshal(map[string]string{"SECRET": "$SECRET"})
	_, outputs, _, err := run(t, map[string]string{
		"command": `printf '%s' "$SECRET"`,
		"env":     string(envJSON),
	})
	if err != nil {
		t.Fatalf("runShell: %v", err)
	}
	if got := strings.TrimSpace(outputs["stdout"].(string)); got != "super-secret-value" {
		t.Errorf("expected stdout %q; got %q", "super-secret-value", got)
	}
}

func TestSandbox_EnvAllowlist_PATHInEnvRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell adapter uses sh; skip on Windows")
	}
	envJSON, _ := json.Marshal(map[string]string{"PATH": "/tmp"})
	_, _, _, err := run(t, map[string]string{"command": "true", "env": string(envJSON)})
	if err == nil || !strings.Contains(err.Error(), "PATH") {
		t.Fatalf("expected PATH error; got %v", err)
	}
}

func TestSandbox_CommandPathHygiene_DotInPathDropped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell adapter uses sh; skip on Windows")
	}
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "evil"), []byte("#!/bin/sh\necho EVIL_RAN\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", ".:/bin:/usr/bin:/usr/local/bin")
	t.Setenv("CRITERIA_SHELL_ALLOWED_PATHS", binDir)

	outcome, outputs, _, err := run(t, map[string]string{"command": "evil", "working_directory": binDir})
	if err != nil {
		t.Fatalf("runShell: %v", err)
	}
	if strings.Contains(outputs["stdout"].(string), "EVIL_RAN") {
		t.Error("sandbox did not strip '.' from PATH")
	}
	if outcome != "failure" {
		t.Errorf("expected 'failure' for missing command; got %q", outcome)
	}
	if outputs["exit_code"] == "0" || outputs["exit_code"] == "" {
		t.Errorf("expected non-zero exit_code; got %v", outputs["exit_code"])
	}
}

func TestSandbox_CommandPathHygiene_ExplicitPathRuns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell adapter uses sh; skip on Windows")
	}
	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "mybin"), []byte("#!/bin/sh\necho MYBIN_RAN\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, outputs, _, err := run(t, map[string]string{"command": "mybin", "command_path": binDir})
	if err != nil {
		t.Fatalf("runShell: %v", err)
	}
	if !strings.Contains(outputs["stdout"].(string), "MYBIN_RAN") {
		t.Errorf("expected 'mybin' to run; stdout=%q", outputs["stdout"])
	}
}

func TestSandbox_Timeout_ShortCommandFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal-based timeout test not supported on Windows")
	}
	start := time.Now()
	deadline := 9 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	outcome, _, events, err := runShell(ctx, map[string]string{"command": "sleep 60", "timeout": "1s"}, nil, func(string, []byte) {})
	if err != nil {
		t.Fatalf("runShell: %v", err)
	}
	if outcome != "failure" {
		t.Errorf("expected 'failure'; got %q", outcome)
	}
	if time.Since(start) > deadline {
		t.Errorf("took too long: %v", time.Since(start))
	}
	if _, found := findEvent(events, "timeout"); !found {
		t.Errorf("expected 'timeout' event; got %v", events)
	}
}

func TestSandbox_BoundedOutput_TruncatesAtLimit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell adapter uses sh; skip on Windows")
	}
	const limitBytes = 1024 * 1024
	outcome, outputs, events, err := run(t, map[string]string{
		"command":            `python3 -c "import sys; sys.stdout.write('x' * (10 * 1024 * 1024))"`,
		"output_limit_bytes": "1048576",
	})
	if err != nil {
		t.Fatalf("runShell: %v", err)
	}
	if outcome != "success" {
		t.Errorf("expected success (truncation non-fatal); got %q", outcome)
	}
	if got := len(outputs["stdout"].(string)); got != limitBytes {
		t.Errorf("stdout length %d; want %d", got, limitBytes)
	}
	if outputs["_truncated_stdout"] != "true" {
		t.Error("expected _truncated_stdout sentinel")
	}
	ev, found := findEvent(events, "output_truncated")
	if !found {
		t.Fatalf("expected output_truncated event; got %v", events)
	}
	if dropped, _ := ev["dropped_bytes"].(int64); dropped < int64(8*1024*1024) {
		t.Errorf("dropped_bytes %v too small", ev["dropped_bytes"])
	}
}

func TestSandbox_WorkingDirectory_OutsideHomeRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("path confinement test uses Unix paths")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CRITERIA_SHELL_ALLOWED_PATHS", "")

	outcome, _, _, err := run(t, map[string]string{"command": "pwd", "working_directory": "/etc"})
	if outcome != "failure" {
		t.Errorf("expected 'failure'; got %q", outcome)
	}
	if err == nil {
		t.Fatal("expected error for working_directory=/etc outside HOME")
	}
	msg := err.Error()
	if !strings.Contains(msg, "working_directory") || !strings.Contains(msg, "CRITERIA_SHELL_ALLOWED_PATHS") {
		t.Errorf("error message missing guidance: %q", msg)
	}
	if strings.Contains(msg, "CRITERIA_SHELL_LEGACY") {
		t.Errorf("error must not mention removed CRITERIA_SHELL_LEGACY: %q", msg)
	}
}

func TestSandbox_WorkingDirectory_AllowedPathAccepted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("path confinement test uses Unix paths")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CRITERIA_SHELL_ALLOWED_PATHS", "/etc")

	outcome, outputs, _, err := run(t, map[string]string{"command": "pwd", "working_directory": "/etc"})
	if err != nil {
		t.Fatalf("runShell: %v", err)
	}
	if outcome != "success" {
		t.Errorf("expected success; got %q", outcome)
	}
	if !strings.Contains(outputs["stdout"].(string), "/etc") {
		t.Errorf("expected stdout to contain /etc; got %q", outputs["stdout"])
	}
}
