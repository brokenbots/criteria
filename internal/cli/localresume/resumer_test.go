package localresume_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/cli/localresume"
)

// --- ParseMode ---

func TestParseMode_Valid(t *testing.T) {
	cases := []struct {
		input string
		want  localresume.Mode
	}{
		{"stdin", localresume.ModeStdin},
		{"file", localresume.ModeFile},
		{"env", localresume.ModeEnv},
		{"auto-approve", localresume.ModeAutoApprove},
	}
	for _, tc := range cases {
		got, err := localresume.ParseMode(tc.input)
		if err != nil {
			t.Errorf("ParseMode(%q): unexpected error: %v", tc.input, err)
		}
		if got != tc.want {
			t.Errorf("ParseMode(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestParseMode_Invalid(t *testing.T) {
	_, err := localresume.ParseMode("interactive")
	if err == nil {
		t.Fatal("expected error for unknown mode, got nil")
	}
}

// --- auto-approve mode ---

func TestAutoApprove_ResumeApproval(t *testing.T) {
	stateDir := t.TempDir()
	r := localresume.New(localresume.ModeAutoApprove, localresume.Options{StateDir: stateDir})
	payload, err := r.ResumeApproval(context.Background(), "run-1", "review", []string{"alice"}, "ship it")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if payload["decision"] != "approved" {
		t.Errorf("expected decision=approved, got %v", payload)
	}
	// Decision should be persisted.
	assertDecisionPersisted(t, stateDir, "run-1", "review", "approved", "")
}

func TestAutoApprove_ResumeSignal(t *testing.T) {
	stateDir := t.TempDir()
	r := localresume.New(localresume.ModeAutoApprove, localresume.Options{StateDir: stateDir})
	payload, err := r.ResumeSignal(context.Background(), "run-1", "gate", "proceed", []string{"success", "failure"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if payload["outcome"] != "success" {
		t.Errorf("expected outcome=success, got %v", payload)
	}
	assertDecisionPersisted(t, stateDir, "run-1", "gate", "", "success")
}

// --- env mode ---

func TestEnvMode_Approval_Approved(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_APPROVAL_REVIEW", "approved")
	r := localresume.New(localresume.ModeEnv, localresume.Options{StateDir: stateDir})
	payload, err := r.ResumeApproval(context.Background(), "run-2", "review", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if payload["decision"] != "approved" {
		t.Errorf("expected decision=approved, got %v", payload)
	}
}

func TestEnvMode_Approval_Rejected(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_APPROVAL_REVIEW", "rejected")
	r := localresume.New(localresume.ModeEnv, localresume.Options{StateDir: stateDir})
	payload, err := r.ResumeApproval(context.Background(), "run-2", "review", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if payload["decision"] != "rejected" {
		t.Errorf("expected decision=rejected, got %v", payload)
	}
}

func TestEnvMode_Approval_Unset(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_APPROVAL_REVIEW", "")
	r := localresume.New(localresume.ModeEnv, localresume.Options{StateDir: stateDir})
	_, err := r.ResumeApproval(context.Background(), "run-2", "review", nil, "")
	if err == nil {
		t.Fatal("expected error when env var is unset")
	}
	if !contains(err.Error(), "CRITERIA_APPROVAL_REVIEW") {
		t.Errorf("error should mention the env var: %v", err)
	}
}

func TestEnvMode_Approval_InvalidValue(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_APPROVAL_REVIEW", "maybe")
	r := localresume.New(localresume.ModeEnv, localresume.Options{StateDir: stateDir})
	_, err := r.ResumeApproval(context.Background(), "run-2", "review", nil, "")
	if err == nil {
		t.Fatal("expected error for invalid value")
	}
}

func TestEnvMode_Signal(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_SIGNAL_GATE", "received")
	r := localresume.New(localresume.ModeEnv, localresume.Options{StateDir: stateDir})
	payload, err := r.ResumeSignal(context.Background(), "run-3", "gate", "proceed", []string{"received", "timeout"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if payload["outcome"] != "received" {
		t.Errorf("expected outcome=received, got %v", payload)
	}
}

func TestEnvMode_Signal_Unset(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_SIGNAL_GATE", "")
	r := localresume.New(localresume.ModeEnv, localresume.Options{StateDir: stateDir})
	_, err := r.ResumeSignal(context.Background(), "run-3", "gate", "proceed", []string{"received", "timeout"})
	if err == nil {
		t.Fatal("expected error when signal env var is unset")
	}
	if !contains(err.Error(), "CRITERIA_SIGNAL_GATE") {
		t.Errorf("error should mention the env var: %v", err)
	}
}

func TestEnvMode_NodeNameWithDotAndHyphen(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_APPROVAL_MY_NODE_NAME", "approved")
	r := localresume.New(localresume.ModeEnv, localresume.Options{StateDir: stateDir})
	// Node "my-node.name" → env key "CRITERIA_APPROVAL_MY_NODE_NAME"
	payload, err := r.ResumeApproval(context.Background(), "run-4", "my-node.name", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if payload["decision"] != "approved" {
		t.Errorf("expected decision=approved, got %v", payload)
	}
}

// --- stdin mode ---

func TestStdinMode_Approval_Yes(t *testing.T) {
	stateDir := t.TempDir()
	stdin := bytes.NewBufferString("y\n")
	var stderr bytes.Buffer
	r := localresume.New(localresume.ModeStdin, localresume.Options{
		Stdin:    stdin,
		Stderr:   &stderr,
		StateDir: stateDir,
	})
	payload, err := r.ResumeApproval(context.Background(), "run-5", "review", []string{"alice"}, "ship it?")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if payload["decision"] != "approved" {
		t.Errorf("expected decision=approved, got %v", payload)
	}
	if !contains(stderr.String(), "review") {
		t.Errorf("stderr should contain node name, got: %q", stderr.String())
	}
}

func TestStdinMode_Approval_No(t *testing.T) {
	stateDir := t.TempDir()
	r := localresume.New(localresume.ModeStdin, localresume.Options{
		Stdin:    bytes.NewBufferString("n\n"),
		Stderr:   &bytes.Buffer{},
		StateDir: stateDir,
	})
	payload, err := r.ResumeApproval(context.Background(), "run-6", "review", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if payload["decision"] != "rejected" {
		t.Errorf("expected decision=rejected, got %v", payload)
	}
}

func TestStdinMode_Approval_Yes_CaseVariants(t *testing.T) {
	for _, input := range []string{"Y\n", "YES\n", "yes\n"} {
		stateDir := t.TempDir()
		r := localresume.New(localresume.ModeStdin, localresume.Options{
			Stdin:    bytes.NewBufferString(input),
			Stderr:   &bytes.Buffer{},
			StateDir: stateDir,
		})
		payload, err := r.ResumeApproval(context.Background(), "run-yes", "review", nil, "")
		if err != nil {
			t.Fatalf("input=%q: unexpected error: %v", input, err)
		}
		if payload["decision"] != "approved" {
			t.Errorf("input=%q: expected approved, got %v", input, payload)
		}
	}
}

func TestStdinMode_Approval_EOF_Rejects(t *testing.T) {
	stateDir := t.TempDir()
	// Empty reader → EOF → rejected with reason "non-interactive input" (the EOF
	// path in resolveApprovalStdin, not the unrecognized-input path in parseApprovalInput).
	r := localresume.New(localresume.ModeStdin, localresume.Options{
		Stdin:    bytes.NewBufferString(""),
		Stderr:   &bytes.Buffer{},
		StateDir: stateDir,
	})
	payload, err := r.ResumeApproval(context.Background(), "run-7", "review", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if payload["decision"] != "rejected" {
		t.Errorf("expected rejected on EOF, got %v", payload)
	}
	if payload["reason"] != "non-interactive input" {
		t.Errorf("expected reason 'non-interactive input' on EOF, got %q", payload["reason"])
	}
}

func TestStdinMode_Approval_ReadError_Aborts(t *testing.T) {
	stateDir := t.TempDir()
	// A reader that returns a non-EOF error should cause the run to abort
	// (return an error) rather than silently persist a rejection.
	r := localresume.New(localresume.ModeStdin, localresume.Options{
		Stdin:    &errReader{err: errors.New("simulated I/O error")},
		Stderr:   &bytes.Buffer{},
		StateDir: stateDir,
	})
	_, err := r.ResumeApproval(context.Background(), "run-err", "review", nil, "")
	if err == nil {
		t.Fatal("expected error on stdin read failure, got nil")
	}
	if !strings.Contains(err.Error(), "simulated I/O error") {
		t.Errorf("error should mention the underlying cause, got: %v", err)
	}
}

// errReader is an io.Reader that always returns a configurable error.
type errReader struct{ err error }

func (e *errReader) Read(_ []byte) (int, error) { return 0, e.err }

func TestStdinMode_Approval_UnrecognizedInput_InvalidInputReason(t *testing.T) {
	stateDir := t.TempDir()
	r := localresume.New(localresume.ModeStdin, localresume.Options{
		Stdin:    bytes.NewBufferString("maybe\n"),
		Stderr:   &bytes.Buffer{},
		StateDir: stateDir,
	})
	payload, err := r.ResumeApproval(context.Background(), "run-ui", "review", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if payload["decision"] != "rejected" {
		t.Errorf("expected rejected, got %v", payload)
	}
	if payload["reason"] != "invalid input" {
		t.Errorf("expected reason 'invalid input', got %q", payload["reason"])
	}
}

func TestStdinMode_Signal_JSON(t *testing.T) {
	stateDir := t.TempDir()
	r := localresume.New(localresume.ModeStdin, localresume.Options{
		Stdin:    bytes.NewBufferString(`{"outcome":"received"}` + "\n"),
		Stderr:   &bytes.Buffer{},
		StateDir: stateDir,
	})
	payload, err := r.ResumeSignal(context.Background(), "run-8", "gate", "proceed", []string{"received", "timeout"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if payload["outcome"] != "received" {
		t.Errorf("expected outcome=received, got %v", payload)
	}
}

func TestStdinMode_Signal_InvalidJSON(t *testing.T) {
	stateDir := t.TempDir()
	r := localresume.New(localresume.ModeStdin, localresume.Options{
		Stdin:    bytes.NewBufferString("not-json\n"),
		Stderr:   &bytes.Buffer{},
		StateDir: stateDir,
	})
	_, err := r.ResumeSignal(context.Background(), "run-9", "gate", "proceed", []string{"received", "timeout"})
	if err == nil {
		t.Fatal("expected error for invalid JSON signal input")
	}
}

// --- file mode ---

func TestFileMode_Approval_WritesAndConsumes(t *testing.T) {
	stateDir := t.TempDir()
	runID := "run-file-1"
	nodeName := "review"

	r := localresume.New(localresume.ModeFile, localresume.Options{
		FilePollingInterval: 20 * time.Millisecond,
		FileTimeout:         5 * time.Second,
		StateDir:            stateDir,
		Stderr:              &bytes.Buffer{},
	})

	// Write decision file from a goroutine after a short delay.
	reqPath := filepath.Join(stateDir, "runs", runID, "approval-"+nodeName+".json")
	go func() {
		time.Sleep(80 * time.Millisecond)
		if err := os.MkdirAll(filepath.Dir(reqPath), 0o700); err != nil {
			return
		}
		_ = os.WriteFile(reqPath, []byte(`{"decision":"approved"}`), 0o600)
	}()

	payload, err := r.ResumeApproval(context.Background(), runID, nodeName, []string{"alice"}, "reason")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if payload["decision"] != "approved" {
		t.Errorf("expected decision=approved, got %v", payload)
	}
	// Request file should be deleted after consumption.
	if _, statErr := os.Stat(reqPath); !os.IsNotExist(statErr) {
		t.Error("expected request file to be deleted after consumption")
	}
	// Decision should be persisted.
	assertDecisionPersisted(t, stateDir, runID, nodeName, "approved", "")
}

func TestFileMode_Signal_WritesAndConsumes(t *testing.T) {
	stateDir := t.TempDir()
	runID := "run-file-sig"
	nodeName := "gate"

	r := localresume.New(localresume.ModeFile, localresume.Options{
		FilePollingInterval: 20 * time.Millisecond,
		FileTimeout:         5 * time.Second,
		StateDir:            stateDir,
		Stderr:              &bytes.Buffer{},
	})

	reqPath := filepath.Join(stateDir, "runs", runID, "approval-"+nodeName+".json")
	go func() {
		time.Sleep(80 * time.Millisecond)
		if err := os.MkdirAll(filepath.Dir(reqPath), 0o700); err != nil {
			return
		}
		_ = os.WriteFile(reqPath, []byte(`{"outcome":"received"}`), 0o600)
	}()

	payload, err := r.ResumeSignal(context.Background(), runID, nodeName, "proceed", []string{"received", "success"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if payload["outcome"] != "received" {
		t.Errorf("expected outcome=received, got %v", payload)
	}
	assertDecisionPersisted(t, stateDir, runID, nodeName, "", "received")
}

func TestFileMode_Timeout(t *testing.T) {
	stateDir := t.TempDir()
	r := localresume.New(localresume.ModeFile, localresume.Options{
		FilePollingInterval: 20 * time.Millisecond,
		FileTimeout:         100 * time.Millisecond,
		StateDir:            stateDir,
		Stderr:              &bytes.Buffer{},
	})
	_, err := r.ResumeApproval(context.Background(), "run-timeout", "review", nil, "")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !contains(err.Error(), "timed out") {
		t.Errorf("expected timeout error, got: %v", err)
	}
}

func TestFileMode_InvalidJSON(t *testing.T) {
	stateDir := t.TempDir()
	runID := "run-badjson"
	nodeName := "review"
	reqPath := filepath.Join(stateDir, "runs", runID, "approval-"+nodeName+".json")

	r := localresume.New(localresume.ModeFile, localresume.Options{
		FilePollingInterval: 20 * time.Millisecond,
		FileTimeout:         5 * time.Second,
		StateDir:            stateDir,
		Stderr:              &bytes.Buffer{},
	})

	go func() {
		time.Sleep(50 * time.Millisecond)
		if err := os.MkdirAll(filepath.Dir(reqPath), 0o700); err != nil {
			return
		}
		_ = os.WriteFile(reqPath, []byte(`not-json`), 0o600)
	}()

	_, err := r.ResumeApproval(context.Background(), runID, nodeName, nil, "")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestFileMode_MissingDecisionKey(t *testing.T) {
	stateDir := t.TempDir()
	runID := "run-nokey"
	nodeName := "review"
	reqPath := filepath.Join(stateDir, "runs", runID, "approval-"+nodeName+".json")

	r := localresume.New(localresume.ModeFile, localresume.Options{
		FilePollingInterval: 20 * time.Millisecond,
		FileTimeout:         5 * time.Second,
		StateDir:            stateDir,
		Stderr:              &bytes.Buffer{},
	})

	go func() {
		time.Sleep(50 * time.Millisecond)
		if err := os.MkdirAll(filepath.Dir(reqPath), 0o700); err != nil {
			return
		}
		_ = os.WriteFile(reqPath, []byte(`{"outcome":"approved"}`), 0o600) // missing "decision"
	}()

	_, err := r.ResumeApproval(context.Background(), runID, nodeName, nil, "")
	if err == nil {
		t.Fatal("expected error for missing decision key")
	}
}

// --- reattach safety: persisted decision reused ---

func TestReattach_Approval_PersistedDecisionReused(t *testing.T) {
	stateDir := t.TempDir()
	runID := "run-reattach"
	nodeName := "review"

	// Pre-write a persisted decision (simulates a previous run capturing it).
	decPath := filepath.Join(stateDir, "runs", runID, "approvals", nodeName+".json")
	if err := os.MkdirAll(filepath.Dir(decPath), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(decPath, []byte(`{"decision":"rejected","decided_at":"2024-01-01T00:00:00Z"}`), 0o600)

	// Use auto-approve mode — but the persisted decision should take precedence.
	r := localresume.New(localresume.ModeAutoApprove, localresume.Options{StateDir: stateDir})
	payload, err := r.ResumeApproval(context.Background(), runID, nodeName, nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Must return the persisted "rejected", not auto-approve's "approved".
	if payload["decision"] != "rejected" {
		t.Errorf("expected persisted rejected decision, got %v", payload)
	}
}

func TestReattach_Signal_PersistedOutcomeReused(t *testing.T) {
	stateDir := t.TempDir()
	runID := "run-reattach-sig"
	nodeName := "gate"

	decPath := filepath.Join(stateDir, "runs", runID, "approvals", nodeName+".json")
	if err := os.MkdirAll(filepath.Dir(decPath), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(decPath, []byte(`{"outcome":"received","decided_at":"2024-01-01T00:00:00Z"}`), 0o600)

	r := localresume.New(localresume.ModeAutoApprove, localresume.Options{StateDir: stateDir})
	payload, err := r.ResumeSignal(context.Background(), runID, nodeName, "proceed", []string{"received", "success"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if payload["outcome"] != "received" {
		t.Errorf("expected persisted outcome=received, got %v", payload)
	}
}

// --- context cancellation ---

func TestStdinMode_ContextCancelled(t *testing.T) {
	stateDir := t.TempDir()
	// Use a pipe that won't produce data — context cancellation should interrupt.
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	defer pw.Close()

	r := localresume.New(localresume.ModeStdin, localresume.Options{
		Stdin:    pr,
		Stderr:   &bytes.Buffer{},
		StateDir: stateDir,
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err = r.ResumeApproval(ctx, "run-cancel", "review", nil, "")
	// Context cancellation must propagate as an error — never manufacture a
	// persisted rejection when the operator cancelled the run.
	if err == nil {
		t.Fatal("expected error on context cancel, got nil")
	}
	// Confirm no decision was persisted (reattach safety).
	decPath := filepath.Join(stateDir, "runs", "run-cancel", "approvals", "review.json")
	if _, statErr := os.Stat(decPath); !os.IsNotExist(statErr) {
		t.Error("decision file must not be persisted on context cancellation")
	}
}

func TestFileMode_ContextCancelled(t *testing.T) {
	stateDir := t.TempDir()
	r := localresume.New(localresume.ModeFile, localresume.Options{
		FilePollingInterval: 20 * time.Millisecond,
		FileTimeout:         30 * time.Second,
		StateDir:            stateDir,
		Stderr:              &bytes.Buffer{},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	_, err := r.ResumeApproval(ctx, "run-ctx", "review", nil, "")
	if err == nil {
		t.Fatal("expected error on context cancellation")
	}
}

func TestStdinMode_Signal_EmptyOutcome_Error(t *testing.T) {
	stateDir := t.TempDir()
	r := localresume.New(localresume.ModeStdin, localresume.Options{
		// JSON payload with empty outcome string.
		Stdin:    bytes.NewBufferString(`{"outcome":""}` + "\n"),
		Stderr:   &bytes.Buffer{},
		StateDir: stateDir,
	})
	_, err := r.ResumeSignal(context.Background(), "run-empty-outcome", "gate", "proceed", []string{"received"})
	if err == nil {
		t.Fatal("expected error for empty outcome key, got nil")
	}
	if !strings.Contains(err.Error(), "outcome") {
		t.Errorf("error should mention 'outcome', got: %v", err)
	}
}

func TestStdinMode_Signal_MissingOutcome_Error(t *testing.T) {
	stateDir := t.TempDir()
	r := localresume.New(localresume.ModeStdin, localresume.Options{
		// JSON payload with no outcome key.
		Stdin:    bytes.NewBufferString(`{}` + "\n"),
		Stderr:   &bytes.Buffer{},
		StateDir: stateDir,
	})
	_, err := r.ResumeSignal(context.Background(), "run-missing-outcome", "gate", "proceed", []string{"received"})
	if err == nil {
		t.Fatal("expected error for missing outcome key, got nil")
	}
	if !strings.Contains(err.Error(), "outcome") {
		t.Errorf("error should mention 'outcome', got: %v", err)
	}
}

func TestStdinMode_Approval_ContextCancel_NoPersist(t *testing.T) {
	stateDir := t.TempDir()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	defer pw.Close()

	r := localresume.New(localresume.ModeStdin, localresume.Options{
		Stdin:    pr,
		Stderr:   &bytes.Buffer{},
		StateDir: stateDir,
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	_, err = r.ResumeApproval(ctx, "run-nopersist", "review", nil, "")
	if err == nil {
		t.Fatal("expected error on context cancel")
	}
	// No decision file should be written.
	decPath := filepath.Join(stateDir, "runs", "run-nopersist", "approvals", "review.json")
	if _, statErr := os.Stat(decPath); !os.IsNotExist(statErr) {
		t.Error("decision file must not be written on context cancellation")
	}
}

func TestReattach_Signal_PersistedInvalidOutcome_Error(t *testing.T) {
	stateDir := t.TempDir()
	runID := "run-reattach-invalid"
	nodeName := "gate"

	// Pre-write a persisted signal decision with an outcome that is NOT declared.
	decPath := filepath.Join(stateDir, "runs", runID, "approvals", nodeName+".json")
	if err := os.MkdirAll(filepath.Dir(decPath), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(decPath, []byte(`{"outcome":"bogus","decided_at":"2024-01-01T00:00:00Z"}`), 0o600)

	r := localresume.New(localresume.ModeAutoApprove, localresume.Options{StateDir: stateDir})
	_, err := r.ResumeSignal(context.Background(), runID, nodeName, "proceed", []string{"received", "success"})
	if err == nil {
		t.Fatal("expected error for invalid persisted signal outcome, got nil")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error should mention the invalid outcome 'bogus', got: %v", err)
	}
	if !strings.Contains(err.Error(), "not declared") {
		t.Errorf("error should say 'not declared', got: %v", err)
	}
}

// --- outcome validation ---

func TestStdinMode_Signal_UnknownOutcome_Error(t *testing.T) {
	stateDir := t.TempDir()
	r := localresume.New(localresume.ModeStdin, localresume.Options{
		Stdin:    bytes.NewBufferString(`{"outcome":"bogus"}` + "\n"),
		Stderr:   &bytes.Buffer{},
		StateDir: stateDir,
	})
	_, err := r.ResumeSignal(context.Background(), "run-bogus", "gate", "proceed", []string{"received", "timeout"})
	if err == nil {
		t.Fatal("expected error for unknown signal outcome, got nil")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error should mention the unknown outcome 'bogus', got: %v", err)
	}
	if !strings.Contains(err.Error(), "not declared") {
		t.Errorf("error should say 'not declared', got: %v", err)
	}
}

func TestEnvMode_Signal_UnknownOutcome_Error(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_SIGNAL_GATE", "bogus")
	r := localresume.New(localresume.ModeEnv, localresume.Options{StateDir: stateDir})
	_, err := r.ResumeSignal(context.Background(), "run-env-bogus", "gate", "proceed", []string{"received", "timeout"})
	if err == nil {
		t.Fatal("expected error for unknown env signal outcome, got nil")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error should mention the unknown outcome 'bogus', got: %v", err)
	}
}

func TestFileMode_Signal_UnknownOutcome_Error(t *testing.T) {
	stateDir := t.TempDir()
	runID := "run-file-bogus"
	nodeName := "gate"
	reqPath := filepath.Join(stateDir, "runs", runID, "approval-"+nodeName+".json")

	r := localresume.New(localresume.ModeFile, localresume.Options{
		FilePollingInterval: 20 * time.Millisecond,
		FileTimeout:         5 * time.Second,
		StateDir:            stateDir,
		Stderr:              &bytes.Buffer{},
	})
	go func() {
		time.Sleep(50 * time.Millisecond)
		if err := os.MkdirAll(filepath.Dir(reqPath), 0o700); err != nil {
			return
		}
		_ = os.WriteFile(reqPath, []byte(`{"outcome":"bogus"}`), 0o600)
	}()
	_, err := r.ResumeSignal(context.Background(), runID, nodeName, "proceed", []string{"received", "timeout"})
	if err == nil {
		t.Fatal("expected error for unknown file signal outcome, got nil")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error should mention the unknown outcome 'bogus', got: %v", err)
	}
}

// --- helpers ---

func assertDecisionPersisted(t *testing.T, stateDir, runID, nodeName, wantDecision, wantOutcome string) {
	t.Helper()
	path := filepath.Join(stateDir, "runs", runID, "approvals", nodeName+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("persisted decision file not found at %s: %v", path, err)
	}
	var d map[string]string
	if err := json.Unmarshal(data, &d); err != nil {
		t.Fatalf("decode persisted decision: %v", err)
	}
	if wantDecision != "" && d["decision"] != wantDecision {
		t.Errorf("persisted decision=%q, want %q", d["decision"], wantDecision)
	}
	if wantOutcome != "" && d["outcome"] != wantOutcome {
		t.Errorf("persisted outcome=%q, want %q", d["outcome"], wantOutcome)
	}
	if d["decided_at"] == "" {
		t.Error("persisted decision missing decided_at")
	}
}

func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}
