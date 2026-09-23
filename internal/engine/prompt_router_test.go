package engine

// prompt_router_test.go — in-memory PromptRouter routing and typed
// failure-class tests (CRI-259 R2/R6/R10, ADR-0006 D1.4/D4/D9). The live
// out-of-process delivery path is covered by prompt_live_test.go; these
// tests pin the deterministic routing decisions and the typed failure
// taxonomy without touching an adapter process.

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/brokenbots/criteria/internal/adapterhost"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	"github.com/brokenbots/criteria/workflow"
)

func promptTestGraph() *workflow.FSMGraph {
	g := &workflow.FSMGraph{
		Name:  "prompt_router_test",
		Steps: map[string]*workflow.StepNode{},
		States: map[string]*workflow.StateNode{
			"done": {Name: "done", Terminal: true, Success: true},
		},
	}
	g.InitialState = "a"
	g.TargetState = "done"
	return g
}

func promptTestMessage(runID, step, caller string) *pb.AgentPrompt {
	return &pb.AgentPrompt{
		RunId:            runID,
		Step:             step,
		Prompt:           "nudge",
		CallerCriteriaId: caller,
	}
}

// promptRouterTest builds a router around an unbound graph (no live
// sessions), so every routed prompt resolves through the non-delivery
// branches and lands as a typed failure in the captured log.
func promptRouterTest(t *testing.T, runID string) (*PromptRouter, *bytes.Buffer) {
	t.Helper()
	ch := make(chan *pb.AgentPrompt, 4)
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelError}))
	r := NewPromptRouter(context.Background(), ch, runID, "owner-1", promptTestGraph(), adapterhost.NewSessionManager(nil), &fakeSink{}, logger)
	if r == nil {
		t.Fatal("NewPromptRouter returned nil")
	}
	return r, &logBuf
}

// TestPromptRouterRunIDMismatchIsTypedNotFound pins criterion 2: a prompt
// addressed to a non-active run id is recorded as typed NOT_FOUND and is
// never routed to the active run.
func TestPromptRouterRunIDMismatchIsTypedNotFound(t *testing.T) {
	r, logBuf := promptRouterTest(t, "run-active")
	defer r.Stop()

	r.route(promptTestMessage("run-other", "a", "owner-1"))

	if !strings.Contains(logBuf.String(), PromptFailNotFound) {
		t.Fatalf("routing failure log missing %s:\n%s", PromptFailNotFound, logBuf.String())
	}
}

// TestPromptRouterCallerRecheckIsTypedAuthorization pins R10: the
// delivery-side caller re-check fails closed for non-owner callers.
func TestPromptRouterCallerRecheckIsTypedAuthorization(t *testing.T) {
	r, logBuf := promptRouterTest(t, "run-1")
	defer r.Stop()

	r.route(promptTestMessage("run-1", "a", "impostor"))

	if !strings.Contains(logBuf.String(), PromptFailAuthorization) {
		t.Fatalf("authorization rejection log missing %s:\n%s", PromptFailAuthorization, logBuf.String())
	}
}

// TestPromptCallerRejectionTable pins the re-check decision table.
func TestPromptCallerRejectionTable(t *testing.T) {
	cases := []struct {
		name   string
		caller string
		owner  string
		wantOK bool
	}{
		{"owner match", "owner-1", "owner-1", true},
		{"console sentinel", "console", "owner-1", true},
		{"empty caller", "", "owner-1", false},
		{"whitespace caller", "  ", "owner-1", false},
		{"foreign caller", "agent-9", "owner-1", false},
	}
	for _, tc := range cases {
		reason := promptCallerRejection(tc.caller, tc.owner)
		if tc.wantOK && reason != "" {
			t.Errorf("%s: expected delivery, got rejection %q", tc.name, reason)
		}
		if !tc.wantOK && reason == "" {
			t.Errorf("%s: expected rejection, got none", tc.name)
		}
	}
}

// TestPromptRouterStepWithoutWindowIsTypedNoActiveSession pins the
// NO_ACTIVE_SESSION branch for a step with no live execution window
// (prompt arrives before the step is entered or after it completed).
func TestPromptRouterStepWithoutWindowIsTypedNoActiveSession(t *testing.T) {
	r, logBuf := promptRouterTest(t, "run-1")
	defer r.Stop()

	r.route(promptTestMessage("run-1", "never-entered", "owner-1"))

	if !strings.Contains(logBuf.String(), PromptFailNoActiveSession) {
		t.Fatalf("no-window prompt log missing %s:\n%s", PromptFailNoActiveSession, logBuf.String())
	}
}

// TestPromptRouterStopFlushesHeldAsNoActiveSession pins the flush path: a
// prompt held between attempts becomes NO_ACTIVE_SESSION when the run stops
// — never silence.
func TestPromptRouterStopFlushesHeldAsNoActiveSession(t *testing.T) {
	r, logBuf := promptRouterTest(t, "run-1")

	msg := promptTestMessage("run-1", "a", "owner-1")
	r.mu.Lock()
	r.inFlight["a"] = 1
	r.held["a"] = append(r.held["a"], msg)
	r.mu.Unlock()

	r.Stop()

	if !strings.Contains(logBuf.String(), PromptFailNoActiveSession) {
		t.Fatalf("held prompt not flushed as %s:\n%s", PromptFailNoActiveSession, logBuf.String())
	}
}
