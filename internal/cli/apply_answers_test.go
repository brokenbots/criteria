package cli

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/cli/localresume"
	"github.com/brokenbots/criteria/internal/run"
)

// All tests in this file exercise the two designed human-in-the-loop paths
// for local runs (CRI-256): the interactive TTY prompt (path 1, default) and
// the pre-populated --answers JSON file (path 2), plus their interaction with
// the CRI-255 control-RPC surface (first resolution wins).

func writeAnswersFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "answers.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write answers file: %v", err)
	}
	return path
}

func answersApplyOpts(workflowPath, answersPath string, tty bool, stdin io.Reader, stderr io.Writer, log *slog.Logger) applyOptions {
	return applyOptions{
		workflowPath: workflowPath,
		answersPath:  answersPath,
		tty:          func() bool { return tty },
		stdin:        stdin,
		stderr:       stderr,
		log:          log,
	}
}

// lockedBuf is a goroutine-safe wrapper around bytes.Buffer for use as the
// Stderr capture writer when runApply is driven from a separate goroutine
// (runApplyAsync). bytes.Buffer.String/Write are not safe for concurrent use,
// and the race detector flags the test-side poll loop against the run
// goroutine's fmt.Fprintf into the prompt writer.
type lockedBuf struct {
	sync.Mutex
	bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.Lock()
	defer l.Unlock()
	return l.Buffer.Write(p)
}

func (l *lockedBuf) String() string {
	l.Lock()
	defer l.Unlock()
	return l.Buffer.String()
}

func TestApplyLocal_AnswersApproved(t *testing.T) {
	t.Setenv("CRITERIA_ADAPTERS", filepath.Dir(buildNoopAdapterBinary(t)))
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	t.Setenv("CRITERIA_LOCAL_APPROVAL", "")

	var logBuf bytes.Buffer
	captLog := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	var stderrBuf bytes.Buffer

	wf := filepath.Join("testdata", "local_approval_simple")
	opts := answersApplyOpts(wf, writeAnswersFile(t, `{"review": {"decision": "approved"}}`), false, nil, &stderrBuf, captLog)
	if err := runApply(context.Background(), opts); err != nil {
		t.Fatalf("expected non-interactive approved run, got: %v", err)
	}
	if strings.Contains(stderrBuf.String(), "Approval required") || strings.Contains(stderrBuf.String(), "Approve?") {
		t.Fatalf("answers mode must never prompt; stderr: %s", stderrBuf.String())
	}
	if !strings.Contains(logBuf.String(), "--answers") {
		t.Errorf("expected the answers selection log line, got: %s", logBuf.String())
	}
}

func TestApplyLocal_AnswersRejectedCarriesReason(t *testing.T) {
	t.Setenv("CRITERIA_ADAPTERS", filepath.Dir(buildNoopAdapterBinary(t)))
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	t.Setenv("CRITERIA_LOCAL_APPROVAL", "")

	wf := filepath.Join("testdata", "local_approval_simple")
	opts := answersApplyOpts(wf, writeAnswersFile(t, `{"review": {"decision": "rejected", "reason": "not shipping this"}}`), false, nil, nil, nil)
	if err := runApply(context.Background(), opts); err == nil {
		t.Fatal("rejected approval: expected non-nil error for terminal failed run")
	} else if !strings.Contains(err.Error(), `approval "review" was rejected with reason: not shipping this`) {
		t.Fatalf("rejection reason must surface in the run failure, got: %v", err)
	}
}

func TestApplyLocal_AnswersUnknownNodeFailsBeforeExecution(t *testing.T) {
	t.Setenv("CRITERIA_ADAPTERS", filepath.Dir(buildNoopAdapterBinary(t)))
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)
	t.Setenv("CRITERIA_LOCAL_APPROVAL", "")

	wf := filepath.Join("testdata", "local_approval_simple")
	opts := answersApplyOpts(wf, writeAnswersFile(t, `{"nonsense": {"decision": "approved"}}`), false, nil, nil, nil)
	if err := runApply(context.Background(), opts); err == nil {
		t.Fatal("expected unknown-node error before the run starts")
	} else if !strings.Contains(err.Error(), "unknown node(s) nonsense") || !strings.Contains(err.Error(), "review (approval)") {
		t.Fatalf("error must name the unknown entry and the declared nodes, got: %v", err)
	}
	entries, readErr := os.ReadDir(filepath.Join(stateDir, "runs"))
	if readErr == nil && len(entries) > 0 {
		t.Fatalf("no run may start when the answers file has an unknown node; found %d run dirs", len(entries))
	}
}

func TestApplyLocal_AnswersAmbiguousEntry(t *testing.T) {
	t.Setenv("CRITERIA_ADAPTERS", filepath.Dir(buildNoopAdapterBinary(t)))
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	t.Setenv("CRITERIA_LOCAL_APPROVAL", "")

	wf := filepath.Join("testdata", "local_approval_simple")
	opts := answersApplyOpts(wf, writeAnswersFile(t, `{"review": {"decision": "approved", "outcome": "success"}}`), false, nil, nil, nil)
	if err := runApply(context.Background(), opts); err == nil {
		t.Fatal("expected ambiguous-entry error")
	} else if !strings.Contains(err.Error(), "ambiguous: set decision or outcome, not both") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestApplyLocal_AnswersUndeclaredApprovalDecision(t *testing.T) {
	t.Setenv("CRITERIA_ADAPTERS", filepath.Dir(buildNoopAdapterBinary(t)))
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	t.Setenv("CRITERIA_LOCAL_APPROVAL", "")

	wf := filepath.Join("testdata", "local_approval_simple")
	opts := answersApplyOpts(wf, writeAnswersFile(t, `{"review": {"decision": "bogus"}}`), false, nil, nil, nil)
	if err := runApply(context.Background(), opts); err == nil {
		t.Fatal("expected undeclared-decision error")
	} else if !strings.Contains(err.Error(), `decision "bogus" is not declared`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestApplyLocal_AnswersUndeclaredWaitOutcome(t *testing.T) {
	t.Setenv("CRITERIA_ADAPTERS", filepath.Dir(buildNoopAdapterBinary(t)))
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	t.Setenv("CRITERIA_LOCAL_APPROVAL", "")

	wf := filepath.Join("testdata", "local_signal_wait")
	opts := answersApplyOpts(wf, writeAnswersFile(t, `{"gate": {"outcome": "bogus"}}`), false, nil, nil, nil)
	if err := runApply(context.Background(), opts); err == nil {
		t.Fatal("expected undeclared-outcome error")
	} else if !strings.Contains(err.Error(), `outcome "bogus" is not declared`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestApplyLocal_AnswersWaitOutcome(t *testing.T) {
	t.Setenv("CRITERIA_ADAPTERS", filepath.Dir(buildNoopAdapterBinary(t)))
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	t.Setenv("CRITERIA_LOCAL_APPROVAL", "")

	var stderrBuf bytes.Buffer
	wf := filepath.Join("testdata", "local_signal_wait")
	opts := answersApplyOpts(wf, writeAnswersFile(t, `{"gate": {"outcome": "success"}}`), false, nil, &stderrBuf, nil)
	if err := runApply(context.Background(), opts); err != nil {
		t.Fatalf("expected non-interactive signal run, got: %v", err)
	}
	if strings.Contains(stderrBuf.String(), "Enter JSON payload") {
		t.Fatalf("answers mode must never prompt; stderr: %s", stderrBuf.String())
	}
}

func TestApplyLocal_AnswersPrecedenceOverEnv(t *testing.T) {
	// The two paths are mutually exclusive per run: --answers wins over
	// CRITERIA_LOCAL_APPROVAL (logged), so auto-approve must NOT fire.
	t.Setenv("CRITERIA_ADAPTERS", filepath.Dir(buildNoopAdapterBinary(t)))
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	t.Setenv("CRITERIA_LOCAL_APPROVAL", "auto-approve")

	var logBuf bytes.Buffer
	captLog := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	wf := filepath.Join("testdata", "local_approval_simple")
	opts := answersApplyOpts(wf, writeAnswersFile(t, `{"review": {"decision": "rejected", "reason": "answers veto"}}`), false, nil, nil, captLog)
	if err := runApply(context.Background(), opts); err == nil {
		t.Fatal("answers rejection must beat the auto-approve env default")
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "--answers overrides CRITERIA_LOCAL_APPROVAL") {
		t.Errorf("expected the answers-override warning, got: %s", logs)
	}
	if strings.Contains(logs, "auto-approving approval node") {
		t.Errorf("ignored auto-approve env mode must not act; logs: %s", logs)
	}
}

func TestApplyLocal_AnswersMissingNodeNoTTYFailsLoudly(t *testing.T) {
	t.Setenv("CRITERIA_ADAPTERS", filepath.Dir(buildNoopAdapterBinary(t)))
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	t.Setenv("CRITERIA_LOCAL_APPROVAL", "")

	// Only second_review is answered; first_review is missing and the run is
	// non-interactive, so it must fail loudly naming the node.
	wf := filepath.Join("testdata", "local_approval_multi")
	opts := answersApplyOpts(wf, writeAnswersFile(t, `{"second_review": {"decision": "approved"}}`), false, nil, nil, nil)
	if err := runApply(context.Background(), opts); err == nil {
		t.Fatal("expected the missing-entry pause to fail loudly")
	} else if !strings.Contains(err.Error(), `no entry in answers file: approval node "first_review"`) {
		t.Fatalf("error must name the unanswered node, got: %v", err)
	}
}

func TestApplyLocal_AnswersMissingNodeTTYFallsBackToPrompt(t *testing.T) {
	t.Setenv("CRITERIA_ADAPTERS", filepath.Dir(buildNoopAdapterBinary(t)))
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	t.Setenv("CRITERIA_LOCAL_APPROVAL", "")

	var logBuf bytes.Buffer
	captLog := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Only first_review is answered; second_review falls back to the prompt
	// path because the session is interactive.
	wf := filepath.Join("testdata", "local_approval_multi")
	opts := answersApplyOpts(wf, writeAnswersFile(t, `{"first_review": {"decision": "approved"}}`), true, bytes.NewBufferString("y\n"), nil, captLog)
	if err := runApply(context.Background(), opts); err != nil {
		t.Fatalf("expected prompt fallback to complete the run, got: %v", err)
	}
	if !strings.Contains(logBuf.String(), "falling back to the interactive prompt") {
		t.Errorf("expected the prompt-fallback log line, got: %s", logBuf.String())
	}
}

func TestApplyLocal_TTYDefaultPromptApproved(t *testing.T) {
	// Path 1 is the default: no CRITERIA_LOCAL_APPROVAL set, a TTY attached,
	// no --answers → the run prompts at the pause instead of auto-approving.
	t.Setenv("CRITERIA_ADAPTERS", filepath.Dir(buildNoopAdapterBinary(t)))
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	t.Setenv("CRITERIA_LOCAL_APPROVAL", "")

	var stderrBuf bytes.Buffer
	wf := filepath.Join("testdata", "local_approval_simple")
	opts := answersApplyOpts(wf, "", true, bytes.NewBufferString("y\n"), &stderrBuf, nil)
	if err := runApply(context.Background(), opts); err != nil {
		t.Fatalf("expected approved run via the default prompt, got: %v", err)
	}
	out := stderrBuf.String()
	if !strings.Contains(out, `Approval required for node "review"`) ||
		!strings.Contains(out, "Approvers: alice") ||
		!strings.Contains(out, "Reason: needs review") ||
		!strings.Contains(out, "Approve? (y/n)") {
		t.Fatalf("prompt must show node, approvers, reason, and the confirm line; stderr: %s", out)
	}
}

func TestApplyLocal_TTYDefaultPromptRejectedCarriesReason(t *testing.T) {
	t.Setenv("CRITERIA_ADAPTERS", filepath.Dir(buildNoopAdapterBinary(t)))
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	t.Setenv("CRITERIA_LOCAL_APPROVAL", "")

	wf := filepath.Join("testdata", "local_approval_simple")
	opts := answersApplyOpts(wf, "", true, bytes.NewBufferString("n\nhold for security review\n"), nil, nil)
	if err := runApply(context.Background(), opts); err == nil {
		t.Fatal("explicit prompt rejection must fail the run")
	} else if !strings.Contains(err.Error(), "approval \"review\" was rejected with reason: hold for security review") {
		t.Fatalf("rejection reason must carry into the failure, got: %v", err)
	}
}

func TestApplyLocal_PromptLosesToControlRPC(t *testing.T) {
	// First resolution wins: when a prompt is pending and the control
	// listener delivers a decision, the prompt is dismissed cleanly with a
	// note on stderr and the run completes via the RPC decision.
	t.Setenv("CRITERIA_ADAPTERS", filepath.Dir(buildNoopAdapterBinary(t)))
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	t.Setenv("CRITERIA_LOCAL_APPROVAL", "")

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer stdinW.Close()
	defer stdinR.Close()

	var stderrBuf lockedBuf
	wf := filepath.Join("testdata", "local_approval_simple")
	opts := answersApplyOpts(wf, "", true, stdinR, &stderrBuf, nil)
	errCh := runApplyAsync(&opts)
	stateDir := os.Getenv("CRITERIA_STATE_DIR")
	addr := waitForControlEndpoint(t, stateDir)
	runID := singleRunID(t, stateDir)

	// Wait until the interactive prompt is actually active before delivering
	// the RPC, so the mid-prompt race (not the pre-poll) is what's exercised.
	timeout := time.After(10 * time.Second)
	for {
		if strings.Contains(stderrBuf.String(), "Approve? (y/n)") {
			break
		}
		select {
		case err := <-errCh:
			t.Fatalf("run ended before the prompt appeared: %v", err)
		case <-timeout:
			t.Fatalf("prompt never appeared; stderr: %s", stderrBuf.String())
		case <-time.After(50 * time.Millisecond):
		}
	}

	if accepted, reason := resolveApproval(t, addr, runID, "review", map[string]string{"decision": "approved"}); !accepted || reason != "ok" {
		t.Fatalf("ResolveResume = (accepted=%t, reason=%q), want accepted ok", accepted, reason)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("expected successful run after RPC decision beat the prompt, got: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("runApply did not return after the RPC decision")
	}
	if !strings.Contains(stderrBuf.String(), "already resolved via the run's control listener") {
		t.Fatalf("expected the prompt-loser note on stderr, got: %s", stderrBuf.String())
	}
}

func TestApplyLocal_HeadlessPauseWithoutListenerFailsLoudly(t *testing.T) {
	// No TTY, no --answers, no resumer, and the control listener refuses to
	// attach (non-loopback control addr): the pause can never be resolved,
	// so the run fails loudly naming the selection modes.
	t.Setenv("CRITERIA_ADAPTERS", filepath.Dir(buildNoopAdapterBinary(t)))
	t.Setenv("CRITERIA_STATE_DIR", t.TempDir())
	t.Setenv("CRITERIA_LOCAL_APPROVAL", "")

	wf := filepath.Join("testdata", "local_approval_simple")
	opts := applyOptions{
		workflowPath: wf,
		controlAddr:  "10.0.0.1:9876",
		tty:          func() bool { return false },
	}
	if err := runApply(context.Background(), opts); err == nil {
		t.Fatal("expected the unresolvable pause to fail loudly")
	} else if !strings.Contains(err.Error(), "approval pause is unresolvable") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveApprovalPause_ParkedRPCWinsOverAnswersResumer(t *testing.T) {
	t.Setenv("CRITERIA_ADAPTERS", filepath.Dir(buildNoopAdapterBinary(t)))

	runCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, graph, loader, err := compileForExecution(runCtx, filepath.Join("testdata", "local_approval_simple"), discardLogger(), false, false)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	defer func() { _ = loader.Shutdown(context.WithoutCancel(runCtx)) }()

	tracker := &pauseTracker{Sink: &run.LocalSink{RunID: "answers-parked-rpc", Out: io.Discard}}
	tracker.mu.Lock()
	tracker.pausedNode = "review"
	tracker.mu.Unlock()
	ctrl := newLocalRunControl("answers-parked-rpc", graph, tracker, nil)

	// A decision parked while the pause landed must win the race before the
	// answers resumer answers (first resolution wins).
	if accepted, reason := ctrl.resolveResume("review", map[string]string{"decision": "approved"}); !accepted || reason != "ok" {
		t.Fatalf("resolveResume = (accepted=%t, reason=%q), want accepted ok", accepted, reason)
	}

	entries, err := localresume.ParseAnswers([]byte(`{"review": {"decision": "rejected"}}`))
	if err != nil {
		t.Fatalf("parse answers: %v", err)
	}
	resolution := approvalResolution{resumer: localresume.NewAnswers(entries, localresume.Options{}), answersActive: true, answersPath: "answers.json", ttyOK: false}

	payload, err := resolveApprovalPause(context.Background(), discardLogger(), ctrl, &resolution, "answers-parked-rpc", "review")
	if err != nil {
		t.Fatalf("resolveApprovalPause: %v", err)
	}
	if payload["decision"] != "approved" {
		t.Fatalf("parked RPC decision must win over the answers resumer, got payload %v", payload)
	}
}
