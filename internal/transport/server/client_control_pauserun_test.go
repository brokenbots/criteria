package servertrans

import (
	"testing"
	"time"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// CRI-254 regression tests. Before this change, a PauseRun control message
// fell through the controlLoop dispatch: no pause_run arm existed in
// criteria's ControlMessage and an orchestrator-issued pause was silently
// ignored by the agent. After the fix the message must arrive on PauseRunCh,
// a pause without a run_id must be observed (not silently discarded), and
// pause/backpressure must be held like the other control arms (CRI-62).

// TestCRI254PauseRunDeliveredToPauseRunCh asserts that a PauseRun sent on the
// Control stream is observable on PauseRunCh (R1).
func TestCRI254PauseRunDeliveredToPauseRunCh(t *testing.T) {
	f := newFakeServer()
	c := startCtlClient(t, f)

	f.controls <- &pb.ControlMessage{Command: &pb.ControlMessage_PauseRun{PauseRun: &pb.PauseRun{
		RunId:  "run-cri254",
		Reason: "hold the pipeline",
	}}}

	select {
	case got := <-c.PauseRunCh():
		if got == nil {
			t.Fatal("received nil pause run")
		}
		if got.RunId != "run-cri254" {
			t.Fatalf("RunId: got %q want run-cri254", got.RunId)
		}
		if got.Reason != "hold the pipeline" {
			t.Fatalf("Reason: got %q", got.Reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pause run not delivered to PauseRunCh within 2s (silent drop)")
	}
}

// TestCRI254PauseRunBoundingResumeStillWorks asserts that resume delivery on
// the same Control stream keeps working after the PauseRun dispatch was added
// — neither arm starves the other.
func TestCRI254PauseRunBoundingResumeStillWorks(t *testing.T) {
	f := newFakeServer()
	c := startCtlClient(t, f)

	f.controls <- &pb.ControlMessage{Command: &pb.ControlMessage_PauseRun{PauseRun: &pb.PauseRun{
		RunId: "run-cri254",
	}}}
	f.controls <- &pb.ControlMessage{Command: &pb.ControlMessage_ResumeRun{ResumeRun: &pb.ResumeRun{
		RunId: "run-cri254",
	}}}

	deadline := time.After(2 * time.Second)
	gotPause, gotResume := false, false
	for !gotPause || !gotResume {
		select {
		case pr := <-c.PauseRunCh():
			if pr != nil && pr.RunId == "run-cri254" {
				gotPause = true
			}
		case rr := <-c.ResumeCh():
			if rr != nil && rr.RunId == "run-cri254" {
				gotResume = true
			}
		case <-deadline:
			t.Fatalf("bounding: gotPause=%v gotResume=%v (both must deliver)", gotPause, gotResume)
		}
	}
}

// TestCRI254PauseRunWithoutRunIdNotDelivered asserts that a PauseRun without
// a run_id is not forwarded as an addressable command (the run_id guard
// mirrors run.cancel/resume): it must be observed by the transport log only,
// never routed to a run.
func TestCRI254PauseRunWithoutRunIdNotDelivered(t *testing.T) {
	f := newFakeServer()
	c := startCtlClient(t, f)

	f.controls <- &pb.ControlMessage{Command: &pb.ControlMessage_PauseRun{PauseRun: &pb.PauseRun{
		Reason: "no run id",
	}}}

	select {
	case got := <-c.PauseRunCh():
		t.Fatalf("pause_run without run_id must not be forwarded, got %+v", got)
	case <-time.After(500 * time.Millisecond):
	}
}

// TestCRI254PauseRunBackpressureHoldsMessage verifies that a pause arriving
// while the pause channel is saturated is held by backpressure and delivered
// once the consumer drains, instead of being silently discarded (CRI-62).
func TestCRI254PauseRunBackpressureHoldsMessage(t *testing.T) {
	f := newFakeServer()
	c := startCtlClient(t, f)

	f.controls <- &pb.ControlMessage{Command: &pb.ControlMessage_PauseRun{PauseRun: &pb.PauseRun{
		RunId: "run-cri254",
	}}}
	select {
	case got := <-c.PauseRunCh():
		if got == nil || got.RunId != "run-cri254" {
			t.Fatalf("unexpected pause delivery: %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pause run not delivered within 2s")
	}

	for i := 0; i < 32; i++ {
		c.pauseRunCh <- &pb.PauseRun{RunId: "filler"}
	}
	f.controls <- &pb.ControlMessage{Command: &pb.ControlMessage_PauseRun{PauseRun: &pb.PauseRun{
		RunId: "run-saturated",
	}}}

	// Allow controlLoop to receive the message and block on the full buffer so
	// the hold (rather than drop) is exercised deterministically.
	time.Sleep(200 * time.Millisecond)

	delivered := 0
	sawReal := false
	drain := time.NewTimer(2 * time.Second)
	defer drain.Stop()
drainLoop:
	for {
		select {
		case got := <-c.PauseRunCh():
			delivered++
			if got.RunId == "run-saturated" {
				sawReal = true
			}
		case <-drain.C:
			break drainLoop
		}
	}
	if delivered != 33 {
		t.Fatalf("expected 33 delivered pause messages (32 fillers + saturated), got %d", delivered)
	}
	if !sawReal {
		t.Fatal("expected the saturated pause message to be delivered after the consumer drained")
	}
}