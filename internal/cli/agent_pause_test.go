package cli

// agent_pause_test.go — CRI-254 agent-loop pause routing tests. Pins the
// three handlePause outcomes: delivery to the active run's pause channel, a
// structured run_not_running drop for a pause addressed to a non-active run
// (detectable, never silent — castle's ack timeout sees it), and the
// matched-but-not-routed drop when the run's pause channel is full (the
// router is still processing the previous latch; mid-execution pauses land
// idempotently at the next boundary).

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

func pauseRoutingLoop(t *testing.T, runID string, pauseBuf int) (*agentLoop, chan *pb.PauseRun, *bytes.Buffer) {
	t.Helper()
	ch := make(chan *pb.PauseRun, pauseBuf)
	a := &activeRun{}
	a.mu.Lock()
	a.runID = runID
	a.pauseCh = ch
	a.mu.Unlock()
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &agentLoop{log: logger, active: a}, ch, &logBuf
}

// TestAgentLoop_PauseRoutedToActiveRun covers the matching path: the pause
// lands on the active run's pause channel and the routing is logged.
func TestAgentLoop_PauseRoutedToActiveRun(t *testing.T) {
	l, ch, logBuf := pauseRoutingLoop(t, "run-1", 1)

	l.handlePause(&pb.PauseRun{RunId: "run-1", Reason: "castle hold"})

	select {
	case got := <-ch:
		if got.GetRunId() != "run-1" || got.GetReason() != "castle hold" {
			t.Fatalf("routed pause = %+v, want the sent message", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pause never reached the active run's pause channel")
	}
	if !strings.Contains(logBuf.String(), "routing pause to active run") {
		t.Fatalf("routing log missing:\n%s", logBuf.String())
	}
}

// TestAgentLoop_PauseForInactiveRunRecordedNotRunning pins the CRI-62 rule at
// the agent-loop layer: a pause addressed to a non-active run is dropped with
// the structured run_not_running reason and is never delivered to another
// run's channel.
func TestAgentLoop_PauseForInactiveRunRecordedNotRunning(t *testing.T) {
	l, ch, logBuf := pauseRoutingLoop(t, "run-1", 1)

	l.handlePause(&pb.PauseRun{RunId: "run-other", Reason: "hold"})

	select {
	case got := <-ch:
		t.Fatalf("cross-run delivery: pause %+v reached the active run's channel", got)
	default:
	}
	for _, want := range []string{"drop_reason=run_not_running", "run-other"} {
		if !strings.Contains(logBuf.String(), want) {
			t.Fatalf("run_not_running record missing %q:\n%s", want, logBuf.String())
		}
	}
}

// TestAgentLoop_PauseFullBufferRecordedDrop pins the slot-full path: when the
// run's pause channel is full (the router pump is busy), the drop is logged
// with the active_run full reason — the pause is not lost when the latch is
// already armed, and the log keeps the outcome detectable.
func TestAgentLoop_PauseFullBufferRecordedDrop(t *testing.T) {
	l, ch, logBuf := pauseRoutingLoop(t, "run-1", 1)

	// Pre-fill the slot: the router pump is busy processing this one.
	queued := &pb.PauseRun{RunId: "run-1", Reason: "in flight"}
	ch <- queued

	l.handlePause(&pb.PauseRun{RunId: "run-1", Reason: "double"})

	for _, want := range []string{"drop_reason=active_run_pause_ch_full", "active_run_pause_ch_full"} {
		if !strings.Contains(logBuf.String(), want) {
			t.Fatalf("slot-full record missing %q:\n%s", want, logBuf.String())
		}
	}
	// The queued pause was left untouched (not overwritten or corrupted).
	if got := <-ch; got.GetReason() != "in flight" {
		t.Fatalf("queued pause = %+v, want the original message", got)
	}
	if strings.Count(logBuf.String(), "active_run_pause_ch_full") != 1 {
		t.Fatalf("exactly one slot-full record expected:\n%s", logBuf.String())
	}
}
