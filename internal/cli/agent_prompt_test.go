package cli

// agent_prompt_test.go — R2 agent-loop prompt routing tests (CRI-259,
// ADR-0006 D1.3/R6). Pins the three handlePrompt outcomes: delivery to the
// active run's prompt channel, a typed NOT_FOUND record for a prompt
// addressed to a non-active run (no cross-run delivery), and a typed
// DELIVERY_ERROR record when the run's prompt channel is full — the drop
// must be recorded, never silent.

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

func promptRoutingLoop(t *testing.T, runID string, promptBuf int) (*agentLoop, chan *pb.AgentPrompt, *bytes.Buffer) {
	t.Helper()
	ch := make(chan *pb.AgentPrompt, promptBuf)
	a := &activeRun{}
	a.mu.Lock()
	a.runID = runID
	a.promptCh = ch
	a.mu.Unlock()
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &agentLoop{log: logger, active: a}, ch, &logBuf
}

func promptRoutingMessage(runID string) *pb.AgentPrompt {
	return &pb.AgentPrompt{
		RunId:            runID,
		Step:             "a",
		Prompt:           "nudge",
		CallerCriteriaId: "agent-owner",
	}
}

// TestAgentLoop_PromptRoutedToActiveRun covers the matching path: the prompt
// lands on the active run's prompt channel and the routing is logged.
func TestAgentLoop_PromptRoutedToActiveRun(t *testing.T) {
	l, ch, logBuf := promptRoutingLoop(t, "run-1", 1)

	l.handlePrompt(promptRoutingMessage("run-1"))

	select {
	case got := <-ch:
		if got.GetRunId() != "run-1" || got.GetStep() != "a" || got.GetPrompt() != "nudge" {
			t.Fatalf("routed prompt = %+v, want the sent message", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("prompt never reached the active run's prompt channel")
	}
	if !strings.Contains(logBuf.String(), "routing agent prompt to active run") {
		t.Fatalf("routing log missing:\n%s", logBuf.String())
	}
}

// TestAgentLoop_PromptForInactiveRunRecordedNotFound pins criterion 2 at the
// CLI layer: a prompt addressed to a non-active run is recorded as typed
// NOT_FOUND and is never delivered to the active run's channel.
func TestAgentLoop_PromptForInactiveRunRecordedNotFound(t *testing.T) {
	l, ch, logBuf := promptRoutingLoop(t, "run-1", 1)

	l.handlePrompt(promptRoutingMessage("run-other"))

	select {
	case got := <-ch:
		t.Fatalf("cross-run delivery: prompt %+v reached the active run's channel", got)
	default:
	}
	for _, want := range []string{"class=NOT_FOUND", "run-other", "agent-owner"} {
		if !strings.Contains(logBuf.String(), want) {
			t.Fatalf("NOT_FOUND record missing %q:\n%s", want, logBuf.String())
		}
	}
}

// TestAgentLoop_PromptFullBufferRecordedDeliveryError pins the R6 record on
// the drop path: when the run's prompt channel is full (router pump busy),
// the prompt is recorded as typed DELIVERY_ERROR with the run/step/caller
// ids — never discarded without a record.
func TestAgentLoop_PromptFullBufferRecordedDeliveryError(t *testing.T) {
	l, ch, logBuf := promptRoutingLoop(t, "run-1", 1)

	// Pre-fill the slot: the router pump is busy delivering this one.
	queued := &pb.AgentPrompt{RunId: "run-1", Step: "a", Prompt: "queued"}
	ch <- queued

	l.handlePrompt(promptRoutingMessage("run-1"))

	for _, want := range []string{"class=DELIVERY_ERROR", "run-1", "step=a", "caller_criteria_id=agent-owner"} {
		if !strings.Contains(logBuf.String(), want) {
			t.Fatalf("DELIVERY_ERROR record missing %q:\n%s", want, logBuf.String())
		}
	}
	// The queued prompt was left untouched (not overwritten or corrupted).
	select {
	case got := <-ch:
		if got.GetPrompt() != "queued" {
			t.Fatalf("queued prompt = %+v, want the original sentinel", got)
		}
	default:
		t.Fatal("pre-filled prompt vanished from the channel")
	}
}
