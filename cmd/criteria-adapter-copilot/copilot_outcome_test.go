// copilot_outcome_test.go — unit tests for handleSubmitOutcome, reprompt loop,
// and supporting helpers.

package main

import (
	"context"
	"strings"
	"testing"

	copilot "github.com/github/copilot-sdk/go"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// ── helpers ──────────────────────────────────────────────────────────────────

func outcomePlugin(s *sessionState) *copilotPlugin {
	return &copilotPlugin{sessions: map[string]*sessionState{"s1": s}}
}

func stateWithOutcomes(allowed ...string) *sessionState {
	set := make(map[string]struct{}, len(allowed))
	for _, o := range allowed {
		set[o] = struct{}{}
	}
	return &sessionState{
		session:               &fakeSession{},
		pending:               make(map[string]chan permDecision),
		activeAllowedOutcomes: set,
	}
}

// ── handleSubmitOutcome unit tests ───────────────────────────────────────────

// Test 5.1: Valid outcome sets finalizedOutcome, returns success ToolResult.
func TestHandleSubmitOutcomeSuccess(t *testing.T) {
	s := stateWithOutcomes("success", "failure")
	p := outcomePlugin(s)

	res, err := p.handleSubmitOutcome("s1", SubmitOutcomeArgs{Outcome: "success", Reason: "looks good"})
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.ResultType != "success" {
		t.Errorf("ResultType = %q, want %q", res.ResultType, "success")
	}
	if !strings.Contains(res.TextResultForLLM, "success") {
		t.Errorf("TextResultForLLM %q should mention the outcome", res.TextResultForLLM)
	}

	s.mu.Lock()
	outcome, reason := s.finalizedOutcome, s.finalizedReason
	attempts := s.finalizeAttempts
	s.mu.Unlock()

	if outcome != "success" {
		t.Errorf("finalizedOutcome = %q, want %q", outcome, "success")
	}
	if reason != "looks good" {
		t.Errorf("finalizedReason = %q, want %q", reason, "looks good")
	}
	if attempts != 1 {
		t.Errorf("finalizeAttempts = %d, want 1", attempts)
	}
}

// Test 5.2: Invalid outcome not in allowed set returns failure ToolResult.
func TestHandleSubmitOutcomeInvalidOutcome(t *testing.T) {
	s := stateWithOutcomes("success", "failure")
	p := outcomePlugin(s)

	res, err := p.handleSubmitOutcome("s1", SubmitOutcomeArgs{Outcome: "unknown"})
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.ResultType != "failure" {
		t.Errorf("ResultType = %q, want %q", res.ResultType, "failure")
	}
	if !strings.Contains(res.TextResultForLLM, "not in the allowed set") {
		t.Errorf("TextResultForLLM %q should mention allowed set constraint", res.TextResultForLLM)
	}
	if !strings.Contains(res.TextResultForLLM, "failure") || !strings.Contains(res.TextResultForLLM, "success") {
		t.Errorf("TextResultForLLM %q should list allowed outcomes", res.TextResultForLLM)
	}

	s.mu.Lock()
	outcome := s.finalizedOutcome
	attempts := s.finalizeAttempts
	kind := s.finalizeFailureKind
	s.mu.Unlock()

	if outcome != "" {
		t.Errorf("finalizedOutcome should be empty on invalid call, got %q", outcome)
	}
	if attempts != 1 {
		t.Errorf("finalizeAttempts = %d, want 1 (invalid attempts still count)", attempts)
	}
	if kind != "invalid_outcome" {
		t.Errorf("finalizeFailureKind = %q, want %q", kind, "invalid_outcome")
	}
}

// Test 5.3: Empty outcome string returns failure ToolResult.
func TestHandleSubmitOutcomeEmptyOutcome(t *testing.T) {
	s := stateWithOutcomes("success")
	p := outcomePlugin(s)

	res, err := p.handleSubmitOutcome("s1", SubmitOutcomeArgs{Outcome: "   "})
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.ResultType != "failure" {
		t.Errorf("ResultType = %q, want %q", res.ResultType, "failure")
	}
	if !strings.Contains(res.TextResultForLLM, "outcome is required") {
		t.Errorf("TextResultForLLM %q should mention outcome is required", res.TextResultForLLM)
	}
	// Empty-string case: attempts still incremented.
	s.mu.Lock()
	attempts := s.finalizeAttempts
	kind := s.finalizeFailureKind
	s.mu.Unlock()
	if attempts != 1 {
		t.Errorf("finalizeAttempts = %d, want 1", attempts)
	}
	if kind != "missing" {
		t.Errorf("finalizeFailureKind = %q, want %q", kind, "missing")
	}
}

// Test 5.4: Duplicate call in same turn returns failure ToolResult preserving
// the first outcome.
func TestHandleSubmitOutcomeDuplicate(t *testing.T) {
	s := stateWithOutcomes("success", "failure")
	p := outcomePlugin(s)

	if _, err := p.handleSubmitOutcome("s1", SubmitOutcomeArgs{Outcome: "success"}); err != nil {
		t.Fatalf("first call: unexpected Go error: %v", err)
	}

	res, err := p.handleSubmitOutcome("s1", SubmitOutcomeArgs{Outcome: "failure"})
	if err != nil {
		t.Fatalf("second call: unexpected Go error: %v", err)
	}
	if res.ResultType != "failure" {
		t.Errorf("ResultType = %q, want %q (duplicate must be rejected)", res.ResultType, "failure")
	}
	if !strings.Contains(res.TextResultForLLM, "already finalized") {
		t.Errorf("TextResultForLLM %q should mention already finalized", res.TextResultForLLM)
	}

	// The first outcome must be preserved.
	s.mu.Lock()
	outcome := s.finalizedOutcome
	kind := s.finalizeFailureKind
	s.mu.Unlock()
	if outcome != "success" {
		t.Errorf("finalizedOutcome = %q, want %q (first call wins)", outcome, "success")
	}
	// The second (duplicate) call must record the failure kind.
	if kind != "duplicate" {
		t.Errorf("finalizeFailureKind = %q, want %q (second call is a duplicate)", kind, "duplicate")
	}
}

// Test 5.5: Unknown session ID returns failure ToolResult (not a Go error).
func TestHandleSubmitOutcomeUnknownSession(t *testing.T) {
	p := &copilotPlugin{sessions: map[string]*sessionState{}}
	res, err := p.handleSubmitOutcome("no-such-session", SubmitOutcomeArgs{Outcome: "success"})
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.ResultType != "failure" {
		t.Errorf("ResultType = %q, want %q", res.ResultType, "failure")
	}
}

// Test 5.6: outcome.finalized event is emitted to the sink on success.
func TestHandleSubmitOutcomeEmitsEvent(t *testing.T) {
	s := stateWithOutcomes("success")
	sink := &recordingSender{}
	s.mu.Lock()
	s.active = true
	s.sink = sink
	s.mu.Unlock()
	p := outcomePlugin(s)

	if _, err := p.handleSubmitOutcome("s1", SubmitOutcomeArgs{Outcome: "success", Reason: "done"}); err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}

	found := false
	for _, ev := range sink.snapshot() {
		if a := ev.GetAdapter(); a != nil && a.GetKind() == "outcome.finalized" {
			found = true
			d := a.GetData().AsMap()
			if d["outcome"] != "success" {
				t.Errorf("outcome.finalized event outcome = %v, want %q", d["outcome"], "success")
			}
		}
	}
	if !found {
		t.Fatal("expected outcome.finalized adapter event")
	}
}

// ── sortedAllowedOutcomes helper ─────────────────────────────────────────────

// Test 5.7: sortedAllowedOutcomes returns deterministically sorted slice.
func TestSortedAllowedOutcomes(t *testing.T) {
	set := map[string]struct{}{
		"success":      {},
		"needs_review": {},
		"failure":      {},
	}
	got := sortedAllowedOutcomes(set)
	want := []string{"failure", "needs_review", "success"}
	if len(got) != len(want) {
		t.Fatalf("sortedAllowedOutcomes len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sortedAllowedOutcomes[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// ── reprompt loop integration tests ──────────────────────────────────────────

// Test 5.8: Submit on first turn → success result, single Send call.
func TestAwaitOutcomeSuccessOnFirstTurn(t *testing.T) {
	s := stateWithOutcomes("success", "failure")
	fake := s.session.(*fakeSession)
	fake.emitOnSend = []copilot.SessionEvent{
		{Type: copilot.SessionEventTypeAssistantMessage, Data: &copilot.AssistantMessageData{MessageID: "m1", Content: "done"}},
		{Type: copilot.SessionEventTypeSessionIdle, Data: &copilot.SessionIdleData{}},
	}
	// Simulate tool call via onSend hook.
	fake.onSend = func(_ int, _ copilot.MessageOptions) {
		s.mu.Lock()
		s.finalizedOutcome = "success"
		s.mu.Unlock()
	}
	p := outcomePlugin(s)
	sender := &recordingSender{}

	if err := p.Execute(context.Background(), &pb.ExecuteRequest{
		SessionId:       "s1",
		Config:          map[string]string{"prompt": "do work"},
		AllowedOutcomes: []string{"failure", "success"},
	}, sender); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	assertOutcome(t, sender, "success")
	if fake.sendCount != 1 {
		t.Errorf("sendCount = %d, want 1 (no reprompts)", fake.sendCount)
	}
}

// Test 5.9: Missing submit on first turn → reprompt → submit on second turn → success.
func TestAwaitOutcomeSuccessAfterOneReprompt(t *testing.T) {
	s := stateWithOutcomes("success", "failure")
	fake := s.session.(*fakeSession)
	// Turn 1: no tool call (session goes idle without finalize).
	// Turn 2: tool call simulated, then idle.
	fake.sendSequence = [][]copilot.SessionEvent{
		{ // turn 1: just idle, no outcome
			{Type: copilot.SessionEventTypeSessionIdle, Data: &copilot.SessionIdleData{}},
		},
		{ // turn 2: message + idle
			{Type: copilot.SessionEventTypeAssistantMessage, Data: &copilot.AssistantMessageData{MessageID: "m2", Content: "ok"}},
			{Type: copilot.SessionEventTypeSessionIdle, Data: &copilot.SessionIdleData{}},
		},
	}
	// Simulate submit_outcome on second Send call.
	fake.onSend = func(callIndex int, _ copilot.MessageOptions) {
		if callIndex == 1 {
			s.mu.Lock()
			s.finalizedOutcome = "success"
			s.mu.Unlock()
		}
	}
	p := outcomePlugin(s)
	sender := &recordingSender{}

	if err := p.Execute(context.Background(), &pb.ExecuteRequest{
		SessionId:       "s1",
		Config:          map[string]string{"prompt": "do work"},
		AllowedOutcomes: []string{"failure", "success"},
	}, sender); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	assertOutcome(t, sender, "success")
	if fake.sendCount != 2 {
		t.Errorf("sendCount = %d, want 2 (1 initial + 1 reprompt)", fake.sendCount)
	}
	// Second Send must contain the reprompt wording.
	opts := fake.getSentOpts()
	if !strings.Contains(opts[1].Prompt, "submit_outcome") {
		t.Errorf("reprompt message should mention submit_outcome, got: %q", opts[1].Prompt)
	}
	if !strings.Contains(opts[1].Prompt, "success") {
		t.Errorf("reprompt message should list allowed outcomes, got: %q", opts[1].Prompt)
	}
}

// Test 5.10: All 3 turns exhausted without submit → failure result.
func TestAwaitOutcomeExhaustedReturnsFailure(t *testing.T) {
	s := stateWithOutcomes("success", "failure")
	fake := s.session.(*fakeSession)
	// All 3 turns: idle without any submit_outcome call.
	idle := []copilot.SessionEvent{
		{Type: copilot.SessionEventTypeSessionIdle, Data: &copilot.SessionIdleData{}},
	}
	fake.sendSequence = [][]copilot.SessionEvent{idle, idle, idle}
	p := outcomePlugin(s)
	sender := &recordingSender{}

	if err := p.Execute(context.Background(), &pb.ExecuteRequest{
		SessionId:       "s1",
		Config:          map[string]string{"prompt": "do work"},
		AllowedOutcomes: []string{"failure", "success"},
	}, sender); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	assertOutcome(t, sender, "failure")
	if fake.sendCount != maxFinalizeAttempts {
		t.Errorf("sendCount = %d, want %d (1 initial + 2 reprompts)", fake.sendCount, maxFinalizeAttempts)
	}

	// A structured outcome.failure adapter event should be present with
	// required diagnostic fields.
	hasFailureEvent := false
	for _, ev := range sender.snapshot() {
		a := ev.GetAdapter()
		if a == nil || a.GetKind() != "outcome.failure" {
			continue
		}
		hasFailureEvent = true
		d := a.GetData().AsMap()
		// Verify the structured fields are present.
		if _, ok := d["kind"]; !ok {
			t.Error("outcome.failure event missing 'kind' field")
		}
		if _, ok := d["allowed_outcomes"]; !ok {
			t.Error("outcome.failure event missing 'allowed_outcomes' field")
		}
		if _, ok := d["attempts"]; !ok {
			t.Error("outcome.failure event missing 'attempts' field")
		}
		if _, ok := d["reason"]; !ok {
			t.Error("outcome.failure event missing 'reason' field")
		}
	}
	if !hasFailureEvent {
		t.Fatal("expected outcome.failure adapter event on exhaustion")
	}
}

// Test 5.11: max_turns reached with needs_review in allowed set → needs_review result.
func TestMaxTurnsWithNeedsReviewAllowed(t *testing.T) {
	s := stateWithOutcomes("success", "failure", "needs_review")
	fake := s.session.(*fakeSession)
	fake.emitOnSend = []copilot.SessionEvent{
		{Type: copilot.SessionEventTypeAssistantMessage, Data: &copilot.AssistantMessageData{MessageID: "m1", Content: "thinking"}},
	}
	p := outcomePlugin(s)
	sender := &recordingSender{}

	if err := p.Execute(context.Background(), &pb.ExecuteRequest{
		SessionId:       "s1",
		Config:          map[string]string{"prompt": "do work", "max_turns": "1"},
		AllowedOutcomes: []string{"failure", "needs_review", "success"},
	}, sender); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	assertOutcome(t, sender, "needs_review")
}

// ── assertion helper ──────────────────────────────────────────────────────────

func assertOutcome(t *testing.T, sender *recordingSender, want string) {
	t.Helper()
	for _, ev := range sender.snapshot() {
		if r := ev.GetResult(); r != nil {
			if r.GetOutcome() == want {
				return
			}
			t.Fatalf("outcome = %q, want %q", r.GetOutcome(), want)
		}
	}
	t.Fatalf("no result event found; want outcome %q", want)
}

// ── additional Step 5.1 matrix tests ─────────────────────────────────────────

// Test 5.12: Success on 3rd attempt (2 reprompts before success).
func TestSubmitOutcome_RepromptTwice(t *testing.T) {
	s := stateWithOutcomes("success", "failure")
	fake := s.session.(*fakeSession)
	idle := []copilot.SessionEvent{{Type: copilot.SessionEventTypeSessionIdle, Data: &copilot.SessionIdleData{}}}
	withIdle := []copilot.SessionEvent{
		{Type: copilot.SessionEventTypeAssistantMessage, Data: &copilot.AssistantMessageData{MessageID: "m3", Content: "ok"}},
		{Type: copilot.SessionEventTypeSessionIdle, Data: &copilot.SessionIdleData{}},
	}
	// Turn 1, 2: no outcome. Turn 3: outcome submitted.
	fake.sendSequence = [][]copilot.SessionEvent{idle, idle, withIdle}
	fake.onSend = func(callIndex int, _ copilot.MessageOptions) {
		if callIndex == 2 {
			s.mu.Lock()
			s.finalizedOutcome = "success"
			s.mu.Unlock()
		}
	}

	sender := &recordingSender{}
	if err := outcomePlugin(s).Execute(context.Background(), &pb.ExecuteRequest{
		SessionId:       "s1",
		Config:          map[string]string{"prompt": "do work"},
		AllowedOutcomes: []string{"failure", "success"},
	}, sender); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	assertOutcome(t, sender, "success")
	if fake.sendCount != 3 {
		t.Errorf("sendCount = %d, want 3 (1 initial + 2 reprompts)", fake.sendCount)
	}
}

// Test 5.13: Invalid outcome on turn 1 (handler rejects it), success on turn 2.
// This validates that invalid enum rejections don't prevent eventual success.
// The real handleSubmitOutcome is called directly from onSend so the actual
// handler validation path (not manual state mutation) is exercised.
func TestSubmitOutcome_InvalidEnumThenSuccess(t *testing.T) {
	s := stateWithOutcomes("success", "failure")
	fake := s.session.(*fakeSession)
	idle := []copilot.SessionEvent{{Type: copilot.SessionEventTypeSessionIdle, Data: &copilot.SessionIdleData{}}}
	withIdle := []copilot.SessionEvent{
		{Type: copilot.SessionEventTypeSessionIdle, Data: &copilot.SessionIdleData{}},
	}
	fake.sendSequence = [][]copilot.SessionEvent{idle, withIdle}

	p := outcomePlugin(s)
	fake.onSend = func(callIndex int, _ copilot.MessageOptions) {
		switch callIndex {
		case 0:
			// Turn 1: model calls submit_outcome with a value outside the allowed
			// set. The real handler rejects it, increments finalizeAttempts, and
			// sets finalizeFailureKind = "invalid_outcome".
			if _, err := p.handleSubmitOutcome("s1", SubmitOutcomeArgs{Outcome: "not-valid"}); err != nil {
				t.Errorf("handleSubmitOutcome turn 1: unexpected error: %v", err)
			}
		case 1:
			// Turn 2: model corrects the call; handler accepts and sets
			// finalizedOutcome = "success".
			if _, err := p.handleSubmitOutcome("s1", SubmitOutcomeArgs{Outcome: "success"}); err != nil {
				t.Errorf("handleSubmitOutcome turn 2: unexpected error: %v", err)
			}
		}
	}

	sender := &recordingSender{}
	if err := p.Execute(context.Background(), &pb.ExecuteRequest{
		SessionId:       "s1",
		Config:          map[string]string{"prompt": "do work"},
		AllowedOutcomes: []string{"failure", "success"},
	}, sender); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	assertOutcome(t, sender, "success")

	s.mu.Lock()
	kind := s.finalizeFailureKind
	s.mu.Unlock()
	// After a successful outcome, finalizeFailureKind retains the category of
	// the last rejection ("invalid_outcome" from the first call). This is
	// correct: the field is only reset by beginExecution at the start of each
	// step and set on failure paths — successful calls do not clear it.
	if kind != "invalid_outcome" {
		t.Errorf("finalizeFailureKind = %q after invalid-then-success, want %q (last rejection category preserved)", kind, "invalid_outcome")
	}
}

// Test 5.14: Permission denied during execution → "failure" outcome.
// The permissionDeny flag is set synchronously in onSend (before SessionIdle
// fires), so awaitOutcome sees the flag and returns "failure" without exhausting
// the reprompt loop.
func TestSubmitOutcome_PermissionDeniedFailure(t *testing.T) {
	s := stateWithOutcomes("success", "failure")
	fake := s.session.(*fakeSession)
	// Turn 1: permission denied → just idle.
	fake.emitOnSend = []copilot.SessionEvent{
		{Type: copilot.SessionEventTypeSessionIdle, Data: &copilot.SessionIdleData{}},
	}
	fake.onSend = func(_ int, _ copilot.MessageOptions) {
		s.mu.Lock()
		s.permissionDeny = true
		s.mu.Unlock()
	}

	sender := &recordingSender{}
	if err := outcomePlugin(s).Execute(context.Background(), &pb.ExecuteRequest{
		SessionId:       "s1",
		Config:          map[string]string{"prompt": "do work"},
		AllowedOutcomes: []string{"failure", "success"},
	}, sender); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	assertOutcome(t, sender, "failure")
}

// Test 5.15: max_turns hit without "needs_review" in allowed set → "failure".
// Regression guard for the handleMaxTurnsReached path; distinct from
// TestMaxTurnsWithNeedsReviewAllowed (Test 5.11) which expects "needs_review".
func TestSubmitOutcome_MaxTurnsReached_NoNeedsReviewInAllowed(t *testing.T) {
	s := stateWithOutcomes("success", "failure")
	fake := s.session.(*fakeSession)
	fake.emitOnSend = []copilot.SessionEvent{
		{Type: copilot.SessionEventTypeAssistantMessage, Data: &copilot.AssistantMessageData{MessageID: "m1", Content: "thinking..."}},
	}

	sender := &recordingSender{}
	if err := outcomePlugin(s).Execute(context.Background(), &pb.ExecuteRequest{
		SessionId:       "s1",
		Config:          map[string]string{"prompt": "do work", "max_turns": "1"},
		AllowedOutcomes: []string{"failure", "success"},
	}, sender); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	assertOutcome(t, sender, "failure")
}

// Test 5.16: Empty allowed set → every submit_outcome call is rejected, exhaustion
// returns "failure" after maxFinalizeAttempts. Validates closed-set semantics
// even when the step declares no outcomes.
func TestSubmitOutcome_EmptyAllowedSetFailsClosed(t *testing.T) {
	s := stateWithOutcomes() // no outcomes declared
	fake := s.session.(*fakeSession)
	idle := []copilot.SessionEvent{{Type: copilot.SessionEventTypeSessionIdle, Data: &copilot.SessionIdleData{}}}
	fake.sendSequence = [][]copilot.SessionEvent{idle, idle, idle}

	sender := &recordingSender{}
	if err := outcomePlugin(s).Execute(context.Background(), &pb.ExecuteRequest{
		SessionId: "s1",
		Config:    map[string]string{"prompt": "do work"},
		// No AllowedOutcomes: empty set.
	}, sender); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	assertOutcome(t, sender, "failure")
}

// Test 5.17: Initial Send contains a preamble listing the allowed outcomes.
// Verifies that the model receives the submit_outcome instruction on the first
// turn, not just in reprompts.
func TestSubmitOutcome_PreamblePresentInPrompt(t *testing.T) {
	s := stateWithOutcomes("success", "failure")
	fake := s.session.(*fakeSession)
	// Use a single idle turn; we only care about the sent prompt, not the result.
	fake.emitOnSend = []copilot.SessionEvent{
		{Type: copilot.SessionEventTypeSessionIdle, Data: &copilot.SessionIdleData{}},
	}
	// Turn 2 and 3 produce idle too (for exhaustion path).
	idle := []copilot.SessionEvent{{Type: copilot.SessionEventTypeSessionIdle, Data: &copilot.SessionIdleData{}}}
	fake.sendSequence = [][]copilot.SessionEvent{idle, idle, idle}

	sender := &recordingSender{}
	_ = outcomePlugin(s).Execute(context.Background(), &pb.ExecuteRequest{
		SessionId:       "s1",
		Config:          map[string]string{"prompt": "do the task"},
		AllowedOutcomes: []string{"failure", "success"},
	}, sender)

	opts := fake.getSentOpts()
	if len(opts) == 0 {
		t.Fatal("expected at least one Send call")
	}
	firstPrompt := opts[0].Prompt
	if !strings.Contains(firstPrompt, "allowed outcomes are") {
		t.Errorf("first prompt should contain allowed-outcomes preamble, got: %q", firstPrompt)
	}
	if !strings.Contains(firstPrompt, "success") {
		t.Errorf("first prompt should list 'success' in allowed outcomes, got: %q", firstPrompt)
	}
	if !strings.Contains(firstPrompt, "failure") {
		t.Errorf("first prompt should list 'failure' in allowed outcomes, got: %q", firstPrompt)
	}
	if !strings.Contains(firstPrompt, "do the task") {
		t.Errorf("first prompt should include the step prompt, got: %q", firstPrompt)
	}
}
