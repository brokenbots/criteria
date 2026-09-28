package servertrans

// CRI-62 regression harness. Before the fix, controlLoop forwarded run.cancel
// and resume_run commands into fixed 32-entry channels with non-blocking
// sends: a saturated buffer logged "dropping ... control message" and
// discarded the command with no ack, retry, metric, or backpressure. In
// production a paused run's resume could silently vanish when the consumer
// was busy. These tests reproduce that deterministically against the old
// behavior and assert the new invariant: control commands apply backpressure
// instead of vanishing; the only remaining drop path is a genuine shutdown
// race, which must be logged with structured evidence.

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// syncBuffer is a concurrency-safe bytes.Buffer; controlLoop logs from its own
// goroutine while the test reads the captured output.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startCapturingCtlClient registers and attaches the Control stream against
// the fake server with a logger that captures output for drop-evidence
// assertions. Callers own cleanup via t.Cleanup.
func startCapturingCtlClient(t *testing.T, f *fakeServer) (*Client, *syncBuffer) {
	t.Helper()
	url := startFakeServer(t, f)
	logBuf := &syncBuffer{}
	c, err := NewClient(url, slog.New(slog.NewTextHandler(logBuf, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Register(ctx, "n", "h", "v"); err != nil {
		t.Fatal(err)
	}
	if err := c.StartControl(ctx); err != nil {
		t.Fatalf("StartControl: %v", err)
	}
	waitForCtlAttach(t, f)
	return c, logBuf
}

// TestCRI62ControlSaturationNoSilentDrop sends one more distinct resume
// command than the resume channel can hold while the consumer deliberately
// does not drain. With the old fire-and-forget sends the 33rd command was
// dropped with only a warning; with backpressure the control loop blocks
// delivering it and every command must arrive once the consumer drains. The
// assignment marker acts as a dispatch barrier: it can only be forwarded
// after every resume ahead of it has been dispatched, so under the old
// behavior its arrival proves the saturated resumes were dropped.
func TestCRI62ControlSaturationNoSilentDrop(t *testing.T) {
	f := newFakeServer()
	c, logBuf := startCapturingCtlClient(t, f)

	// Consumer deliberately does not drain: pre-fill ResumeCh to capacity.
	for i := 0; i < cap(c.resumeCh); i++ {
		c.resumeCh <- &pb.ResumeRun{RunId: fmt.Sprintf("filler-%02d", i)}
	}

	// 33+ distinct commands over the fake Control stream.
	want := cap(c.resumeCh) + 1
	for i := 1; i <= want; i++ {
		f.controls <- &pb.ControlMessage{Command: &pb.ControlMessage_ResumeRun{ResumeRun: &pb.ResumeRun{
			RunId:  fmt.Sprintf("run-%02d", i),
			Signal: "resume",
		}}}
	}
	// Dispatch barrier: on a fire-and-forget transport the control loop
	// discards every saturated resume instantly and forwards the marker;
	// with backpressure the loop is still blocked on the first resume.
	f.controls <- &pb.ControlMessage{Command: &pb.ControlMessage_WorkflowAssignment{WorkflowAssignment: &pb.WorkflowAssignment{
		RunId: "barrier",
	}}}

	select {
	case <-c.AssignmentCh():
		t.Fatalf("CRI-62 defect reproduced: control loop dispatched past %d saturated resume commands (they were dropped); log:\n%s",
			want, logBuf.String())
	case <-time.After(2 * time.Second):
		// Backpressure applied: the control loop is still blocked
		// delivering run-01.
	}

	// Drain: every distinct command must arrive, none may be dropped.
	got := make(map[string]bool, want)
	deadline := time.After(5 * time.Second)
	for len(got) < want {
		select {
		case rr := <-c.ResumeCh():
			if rr != nil && strings.HasPrefix(rr.RunId, "run-") {
				got[rr.RunId] = true
			}
		case <-deadline:
			missing := make([]string, 0, want)
			for i := 1; i <= want; i++ {
				id := fmt.Sprintf("run-%02d", i)
				if !got[id] {
					missing = append(missing, id)
				}
			}
			t.Fatalf("control commands dropped under saturation: delivered %d of %d distinct resumes, missing %v; log:\n%s",
				len(got), want, missing, logBuf.String())
		}
	}

	if logs := logBuf.String(); strings.Contains(logs, "dropping resume_run control message") {
		t.Fatalf("resume command was dropped under saturation despite backpressure; log:\n%s", logs)
	}
}

// TestCRI62RunCancelSaturationNoSilentDrop covers the other forwarding arm
// named in CRI-62: 33+ distinct run.cancel commands sent while the consumer
// does not drain must all be delivered under backpressure, not discarded.
func TestCRI62RunCancelSaturationNoSilentDrop(t *testing.T) {
	f := newFakeServer()
	c, logBuf := startCapturingCtlClient(t, f)

	for i := 0; i < cap(c.runCancelCh); i++ {
		c.runCancelCh <- fmt.Sprintf("filler-%02d", i)
	}

	want := cap(c.runCancelCh) + 1
	for i := 1; i <= want; i++ {
		f.controls <- &pb.ControlMessage{Command: &pb.ControlMessage_RunCancel{RunCancel: &pb.RunCancel{
			RunId:  fmt.Sprintf("run-%02d", i),
			Reason: "saturation",
		}}}
	}
	f.controls <- &pb.ControlMessage{Command: &pb.ControlMessage_WorkflowAssignment{WorkflowAssignment: &pb.WorkflowAssignment{
		RunId: "barrier",
	}}}

	select {
	case <-c.AssignmentCh():
		t.Fatalf("CRI-62 defect reproduced: control loop dispatched past %d saturated run.cancel commands (they were dropped); log:\n%s",
			want, logBuf.String())
	case <-time.After(2 * time.Second):
	}

	got := make(map[string]bool, want)
	deadline := time.After(5 * time.Second)
	for len(got) < want {
		select {
		case runID := <-c.RunCancelCh():
			if strings.HasPrefix(runID, "run-") {
				got[runID] = true
			}
		case <-deadline:
			missing := make([]string, 0, want)
			for i := 1; i <= want; i++ {
				id := fmt.Sprintf("run-%02d", i)
				if !got[id] {
					missing = append(missing, id)
				}
			}
			t.Fatalf("run.cancel commands dropped under saturation: delivered %d of %d distinct cancels, missing %v; log:\n%s",
				len(got), want, missing, logBuf.String())
		}
	}

	if logs := logBuf.String(); strings.Contains(logs, "dropping run.cancel control message") {
		t.Fatalf("run.cancel command was dropped under saturation despite backpressure; log:\n%s", logs)
	}
}

// TestCRI62ShutdownRaceDropHasEvidence asserts the one remaining drop path:
// when the client is closed while the control loop is blocked delivering a
// command (consumer gone), the command is dropped explicitly and the warning
// carries structured evidence (drop_reason) instead of being silent.
func TestCRI62ShutdownRaceDropHasEvidence(t *testing.T) {
	f := newFakeServer()
	c, logBuf := startCapturingCtlClient(t, f)

	for i := 0; i < cap(c.resumeCh); i++ {
		c.resumeCh <- &pb.ResumeRun{RunId: fmt.Sprintf("filler-%02d", i)}
	}

	f.controls <- &pb.ControlMessage{Command: &pb.ControlMessage_ResumeRun{ResumeRun: &pb.ResumeRun{
		RunId:  "run-shutdown",
		Signal: "resume",
	}}}
	// Barrier: proves the control loop has NOT dispatched past the blocked
	// command. If it arrives, the command was dropped without backpressure.
	f.controls <- &pb.ControlMessage{Command: &pb.ControlMessage_WorkflowAssignment{WorkflowAssignment: &pb.WorkflowAssignment{
		RunId: "barrier",
	}}}

	select {
	case <-c.AssignmentCh():
		t.Fatalf("control loop dispatched past a command that must have been blocked on the full buffer; log:\n%s", logBuf.String())
	case <-time.After(1 * time.Second):
	}

	// Consumer gone: closing the client must unblock the send as an explicit,
	// evidenced drop rather than a silent one.
	_ = c.Close()

	if !waitForCond(t, 2*time.Second, func() bool {
		return strings.Contains(logBuf.String(), "dropping resume_run control message") &&
			strings.Contains(logBuf.String(), "run_id=run-shutdown") &&
			strings.Contains(logBuf.String(), "drop_reason=shutdown")
	}) {
		t.Fatalf("shutdown-race drop was not logged with structured evidence; log:\n%s", logBuf.String())
	}

	// The dropped command must not have been delivered after all.
	sawShutdown := false
	drain := time.NewTimer(300 * time.Millisecond)
	defer drain.Stop()
drainLoop:
	for {
		select {
		case rr := <-c.ResumeCh():
			if rr != nil && rr.RunId == "run-shutdown" {
				sawShutdown = true
			}
		case <-drain.C:
			break drainLoop
		}
	}
	if sawShutdown {
		t.Fatal("command blocked on a full buffer must not be delivered after Close dropped it")
	}
}
