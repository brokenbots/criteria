package engine

// prompt_held_test.go — the between-attempts hold→flush path driven through
// the real router windows and a live promptable adapter session (CRI-259 R5,
// ADR-0006 D2). A prompt arriving while the step is between attempts
// (inFlight>0, executing==0 — the state a retry backoff leaves behind) must
// be held and then delivered into the next attempt's live session with
// exactly one AgentPromptInjected; if the attempt loop exits first, it must
// resolve as NO_ACTIVE_SESSION with no event and no Prompt RPC.

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/brokenbots/criteria/internal/adapterhost"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

func heldPromptFixture(t *testing.T) (
	rt *PromptRouter, promptCh chan *pb.AgentPrompt, sink *promptInjectSink, logOut *bytes.Buffer, callLog string,
) {
	t.Helper()
	adapterBin := buildPromptableAdapter(t)
	callLogPath := filepath.Join(t.TempDir(), "promptable-held-calls.log")
	t.Setenv("PROMPTABLE_CALL_LOG", callLogPath)

	loader := adapterhost.NewLoaderWithDiscovery(func(string) (string, error) {
		return adapterBin, nil
	})
	t.Cleanup(func() { _ = loader.Shutdown(context.Background()) })

	// Open the step's live adapter session the way the engine does, so the
	// held prompt has a real prompt-capable session to flush into.
	sm := adapterhost.NewSessionManager(loader)
	if err := sm.Open(context.Background(), "promptable.default", "promptable", "fail", nil, nil); err != nil {
		t.Fatalf("open promptable session: %v", err)
	}

	g := compile(t, `
workflow {
  name = "prompt-held"
  version = "0.1"
  initial_state = "a"
  target_state  = "done"
}
step "a" {
  target = adapter.promptable.default
  input { delay_ms = "0" }
  outcome "success" { next = step.done }
}
state "done" { terminal = true }
adapter "promptable" "default" {}`)

	runID, ownerID := "run-held", "agent-cri-259"
	promptCh = make(chan *pb.AgentPrompt, 4)
	sink = &promptInjectSink{promptCh: promptCh, runID: runID, owner: ownerID}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelError}))

	rt = NewPromptRouter(context.Background(), promptCh, runID, ownerID, g, sm, sink, logger)
	if rt == nil {
		t.Fatal("NewPromptRouter returned nil")
	}
	return rt, promptCh, sink, &logBuf, callLogPath
}

// waitForHeld waits for the pump to park want prompt(s) for step via the
// real channel→route→hold path. A router that silently drops parked prompts
// never fills held, so the wait times out and the test fails.
func waitForHeld(t *testing.T, r *PromptRouter, step string, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		r.mu.Lock()
		got := len(r.held[step])
		r.mu.Unlock()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("router never parked %d prompt(s) for step %q (held=%d)", want, step, got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestPromptRouterHeldPromptFlushedIntoNextAttemptSession pins the R5
// hold→flush path: a prompt parked between attempts is delivered into the
// next attempt's live session, exactly once, with the AgentPromptInjected
// event and the adapter-side Prompt RPC.
func TestPromptRouterHeldPromptFlushedIntoNextAttemptSession(t *testing.T) {
	r, promptCh, sink, logBuf, callLog := heldPromptFixture(t)
	defer r.Stop()

	// Attempt 1 opens and closes its delivery window; the step is now
	// between attempts (inFlight=1, executing=0).
	r.beginExecute("a")
	r.endExecute("a")

	msg := &pb.AgentPrompt{
		RunId:            "run-held",
		Step:             "a",
		Prompt:           "between attempts",
		CallerCriteriaId: "agent-cri-259",
		IssuedAt:         timestamppb.Now(),
	}
	promptCh <- msg
	waitForHeld(t, r, "a", 1, 5*time.Second)

	// The attempt loop re-enters: beginExecute flushes the held prompt into
	// the next attempt's live session (synchronously, like the engine's
	// per-attempt window open).
	r.beginExecute("a")

	if n := sink.promptInjectionCount(); n != 1 {
		t.Fatalf("AgentPromptInjected events = %d, want exactly 1 (log:\n%s)", n, logBuf.String())
	}
	rec, ok := sink.firstInjection()
	if !ok {
		t.Fatal("injection record missing")
	}
	if rec.step != "a" {
		t.Errorf("injected step = %q, want %q", rec.step, "a")
	}
	if rec.sessionID != "promptable.default" {
		t.Errorf("injected session_id = %q, want the live session", rec.sessionID)
	}
	if rec.prompt != "between attempts" {
		t.Errorf("injected prompt = %q", rec.prompt)
	}
	if rec.caller != "agent-cri-259" {
		t.Errorf("injected caller = %q", rec.caller)
	}
	if rec.deliveredAt.IsZero() {
		t.Error("injected delivered_at is zero")
	}

	// The flush reached the adapter: exactly one Prompt call on the wire.
	logged := readCallLog(t, callLog)
	promptCalls := 0
	for _, m := range logged {
		if m == "prompt" {
			promptCalls++
		}
	}
	if promptCalls != 1 {
		t.Fatalf("call log has %d prompt calls, want exactly 1: %v", promptCalls, logged)
	}
	if strings.Contains(logBuf.String(), "agent prompt delivery failed") {
		t.Errorf("unexpected delivery failure log:\n%s", logBuf.String())
	}

	// Mirror the engine's deferred close so Stop has nothing held.
	r.endStep("a")
}

// TestPromptRouterHeldPromptResolvedNoActiveSessionWhenAttemptLoopExits pins
// the other R5 branch: if the attempt loop exits before another attempt, the
// held prompt resolves as NO_ACTIVE_SESSION — no retroactive injection into
// the still-registered session, no event, no Prompt RPC.
func TestPromptRouterHeldPromptResolvedNoActiveSessionWhenAttemptLoopExits(t *testing.T) {
	r, promptCh, sink, logBuf, callLog := heldPromptFixture(t)
	defer r.Stop()

	r.beginExecute("a")
	r.endExecute("a")

	msg := &pb.AgentPrompt{
		RunId:            "run-held",
		Step:             "a",
		Prompt:           "missed the window",
		CallerCriteriaId: "agent-cri-259",
		IssuedAt:         timestamppb.Now(),
	}
	promptCh <- msg
	waitForHeld(t, r, "a", 1, 5*time.Second)

	// The attempt loop exits (step completed) before another attempt.
	r.endStep("a")

	if n := sink.promptInjectionCount(); n != 0 {
		t.Fatalf("retroactive injection: %d AgentPromptInjected event(s)", n)
	}
	if !strings.Contains(logBuf.String(), "class=NO_ACTIVE_SESSION") {
		t.Fatalf("held prompt not resolved as NO_ACTIVE_SESSION; log:\n%s", logBuf.String())
	}
	for _, m := range readCallLog(t, callLog) {
		if m == "prompt" {
			t.Fatalf("Prompt RPC issued after the attempt loop exited: %v", readCallLog(t, callLog))
		}
	}
}
