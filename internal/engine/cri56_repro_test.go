package engine

// Regression tests for CRI-56: approval and signal-wait nodes used to clear
// ResumePayload/PendingSignal before validating the supplied decision/outcome,
// so an invalid resume destroyed the retry/audit context. The nodes must
// validate the resume input first and only consume the payload once committed
// to their outcome; on invalid input the payload stays intact so a retry or
// reattach re-evaluates against the same context. The documented single-outcome
// fallback for payload-free resumes is unchanged.

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/brokenbots/criteria/workflow"
)

// resumeTrackingSink extends fakeSink with approval-decision and wait-resumed
// event capture for direct node evaluation.
type resumeTrackingSink struct {
	fakeSink
	approvalDecisions []approvalDecisionEvent
	waitResumes       []waitResumedEvent
}

type approvalDecisionEvent struct {
	node, decision, actor string
	payload               map[string]string
}

type waitResumedEvent struct {
	node, mode, signal string
	payload            map[string]string
}

func (s *resumeTrackingSink) OnApprovalDecision(node, decision, actor string, payload map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.approvalDecisions = append(s.approvalDecisions, approvalDecisionEvent{node, decision, actor, payload})
}

func (s *resumeTrackingSink) OnWaitResumed(node, mode, signal string, payload map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.waitResumes = append(s.waitResumes, waitResumedEvent{node, mode, signal, payload})
}

func (s *resumeTrackingSink) decisionCount() int {
	return len(s.approvalDecisions)
}

func (s *resumeTrackingSink) waitResumedCount() int {
	return len(s.waitResumes)
}

func newTestRunState(current string, payload map[string]string, pendingSignal string) *RunState {
	return &RunState{
		Current:       current,
		PendingSignal: pendingSignal,
		ResumePayload: payload,
	}
}

func testApprovalNode() *approvalNode {
	return &approvalNode{node: &workflow.ApprovalNode{
		Name:      "check",
		Approvers: []string{"alice"},
		Reason:    "needs review",
		Outcomes: map[string]string{
			"approved": "done",
			"rejected": "fail",
		},
	}}
}

func testMultiOutcomeWaitNode() *waitNode {
	return &waitNode{node: &workflow.WaitNode{
		Name:   "gate",
		Signal: "resume",
		Outcomes: map[string]string{
			"ok":  "done_ok",
			"err": "done_err",
		},
	}}
}

func TestCRI56_Approval_InvalidDecisionKeepsResumeContext(t *testing.T) {
	ctx := context.Background()
	n := testApprovalNode()
	deps := Deps{Sink: &resumeTrackingSink{}}
	st := newTestRunState("check", map[string]string{"decision": "bogus", "actor": "mallory"}, "check")

	next, err := n.Evaluate(ctx, st, deps)
	if err == nil {
		t.Fatalf("expected error for unknown decision, got nil (next=%q)", next)
	}
	const wantErr = `approval "check": unknown decision "bogus" (expected "approved" or "rejected")`
	if err.Error() != wantErr {
		t.Errorf("error text changed:\n got: %s\nwant: %s", err.Error(), wantErr)
	}
	if st.ResumePayload == nil {
		t.Fatal("ResumePayload was consumed on invalid decision; expected it intact for retry")
	}
	want := map[string]string{"decision": "bogus", "actor": "mallory"}
	if !reflect.DeepEqual(st.ResumePayload, want) {
		t.Errorf("ResumePayload changed: got %v, want %v", st.ResumePayload, want)
	}
	if st.PendingSignal != "check" {
		t.Errorf("PendingSignal was cleared on invalid decision: got %q, want %q", st.PendingSignal, "check")
	}

	// Retry with a corrected decision resolves against the preserved context.
	st.ResumePayload = map[string]string{"decision": "approved", "actor": "mallory"}
	next, err = n.Evaluate(ctx, st, deps)
	if err != nil {
		t.Fatalf("valid retry failed: %v", err)
	}
	if next != "done" {
		t.Errorf("expected target %q for approved decision, got %q", "done", next)
	}
	if st.ResumePayload != nil || st.PendingSignal != "" {
		t.Errorf("valid resume must consume the payload: ResumePayload=%v PendingSignal=%q", st.ResumePayload, st.PendingSignal)
	}
}

func TestCRI56_Approval_ValidResumeEmitsDecisionOnceCommitted(t *testing.T) {
	ctx := context.Background()
	n := testApprovalNode()
	sink := &resumeTrackingSink{}
	deps := Deps{Sink: sink}
	st := newTestRunState("check", map[string]string{"decision": "rejected", "actor": "mallory", "reason": "no"}, "check")

	next, err := n.Evaluate(ctx, st, deps)
	if err != nil {
		t.Fatalf("valid resume failed: %v", err)
	}
	if next != "fail" {
		t.Errorf("expected target %q for rejected decision, got %q", "fail", next)
	}
	if st.ResumePayload != nil || st.PendingSignal != "" {
		t.Errorf("valid resume must clear ResumePayload/PendingSignal: got %v / %q", st.ResumePayload, st.PendingSignal)
	}
	if got := sink.decisionCount(); got != 1 {
		t.Fatalf("expected exactly one OnApprovalDecision event, got %d", got)
	}
	ev := sink.approvalDecisions[0]
	if ev.node != "check" || ev.decision != "rejected" || ev.actor != "mallory" {
		t.Errorf("unexpected OnApprovalDecision event: %+v", ev)
	}
	if !reflect.DeepEqual(ev.payload, map[string]string{"decision": "rejected", "actor": "mallory", "reason": "no"}) {
		t.Errorf("OnApprovalDecision payload changed: got %v", ev.payload)
	}
}

func TestCRI56_Wait_InvalidOutcomeKeepsResumeContext(t *testing.T) {
	ctx := context.Background()
	n := testMultiOutcomeWaitNode()

	for name, payload := range map[string]map[string]string{
		"missing selector": {},
		"empty selector":   {"outcome": ""},
		"unknown selector": {"outcome": "unknown"},
	} {
		sink := &resumeTrackingSink{}
		deps := Deps{Sink: sink}
		st := newTestRunState("gate", clonePayload(payload), "resume")

		_, err := n.Evaluate(ctx, st, deps)
		if err == nil {
			t.Fatalf("%s: expected error, got nil", name)
		}
		wantErr := `wait "gate": missing or invalid outcome "` + payload["outcome"] + `"; valid outcomes: [err ok]`
		if !strings.Contains(err.Error(), wantErr) {
			t.Errorf("%s: error text changed:\n got: %s\nwant substring: %s", name, err.Error(), wantErr)
		}
		if !reflect.DeepEqual(st.ResumePayload, payload) {
			t.Errorf("%s: ResumePayload was consumed on invalid outcome: got %v, want %v", name, st.ResumePayload, payload)
		}
		if st.PendingSignal != "resume" {
			t.Errorf("%s: PendingSignal was cleared on invalid outcome: got %q, want %q", name, st.PendingSignal, "resume")
		}
		if got := sink.waitResumedCount(); got != 0 {
			t.Errorf("%s: OnWaitResumed emitted %d time(s) for an uncommitted resume; expected 0", name, got)
		}
	}
}

func TestCRI56_Wait_UnknownOutcomeRetrySucceeds(t *testing.T) {
	ctx := context.Background()
	n := testMultiOutcomeWaitNode()
	sink := &resumeTrackingSink{}
	deps := Deps{Sink: sink}
	st := newTestRunState("gate", map[string]string{"outcome": "unknown"}, "resume")

	if _, err := n.Evaluate(ctx, st, deps); err == nil {
		t.Fatal("expected error for unknown outcome selector, got nil")
	}

	// Retry with a corrected selector resolves against the preserved context.
	st.ResumePayload = map[string]string{"outcome": "ok"}
	next, err := n.Evaluate(ctx, st, deps)
	if err != nil {
		t.Fatalf("valid retry failed: %v", err)
	}
	if next != "done_ok" {
		t.Errorf("expected target %q for outcome \"ok\", got %q", "done_ok", next)
	}
	if st.ResumePayload != nil || st.PendingSignal != "" {
		t.Errorf("valid resume must consume the resume context: got %v / %q", st.ResumePayload, st.PendingSignal)
	}
	if got := sink.waitResumedCount(); got != 1 {
		t.Fatalf("expected exactly one OnWaitResumed event, got %d", got)
	}
	ev := sink.waitResumes[0]
	if ev.node != "gate" || ev.mode != "signal" || ev.signal != "resume" {
		t.Errorf("unexpected OnWaitResumed event: %+v", ev)
	}
	if !reflect.DeepEqual(ev.payload, map[string]string{"outcome": "ok"}) {
		t.Errorf("OnWaitResumed payload changed: got %v", ev.payload)
	}
}

func TestCRI56_Wait_SingleOutcomeFallbackUnchanged(t *testing.T) {
	ctx := context.Background()
	n := &waitNode{node: &workflow.WaitNode{
		Name:     "gate",
		Signal:   "resume",
		Outcomes: map[string]string{"ok": "done"},
	}}

	for name, payload := range map[string]map[string]string{
		"payload-free":     {},
		"empty selector":   {"outcome": ""},
		"unknown selector": {"outcome": "unknown"},
		"valid selector":   {"outcome": "ok"},
	} {
		sink := &resumeTrackingSink{}
		st := newTestRunState("gate", clonePayload(payload), "resume")

		next, err := n.Evaluate(ctx, st, Deps{Sink: sink})
		if err != nil {
			t.Fatalf("%s: single-outcome fallback must resume, got error: %v", name, err)
		}
		if next != "done" {
			t.Errorf("%s: expected target %q, got %q", name, "done", next)
		}
		if st.ResumePayload != nil || st.PendingSignal != "" {
			t.Errorf("%s: committed resume must clear ResumePayload/PendingSignal: got %v / %q", name, st.ResumePayload, st.PendingSignal)
		}
		if got := sink.waitResumedCount(); got != 1 {
			t.Errorf("%s: expected exactly one OnWaitResumed event, got %d", name, got)
		}
	}
}

func clonePayload(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}