package servertrans

import (
	"context"
	"testing"
	"time"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// CRI-259 M12.2 regression tests (ADR-0006 R1). Before the fix, an
// AgentPrompt control message fell through the controlLoop dispatch and was
// silently dropped: TestCRI259AgentPromptDeliveredToPromptCh received nothing
// within 2 s. After the fix it must arrive on AgentPromptCh with matching
// run_id/step/prompt, while resume delivery keeps working (bounding test).

// startCtlClient registers and attaches the Control stream against the fake
// server, returning the client. Callers own cleanup via t.Cleanup.
func startCtlClient(t *testing.T, f *fakeServer) (*Client, context.Context) {
	t.Helper()
	url := startFakeServer(t, f)
	c, err := NewClient(url, newTestLogger())
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
	return c, ctx
}

// TestCRI259AgentPromptDeliveredToPromptCh asserts that an AgentPrompt sent
// on the Control stream is observable on AgentPromptCh within 2 s (R1).
func TestCRI259AgentPromptDeliveredToPromptCh(t *testing.T) {
	f := newFakeServer()
	c, _ := startCtlClient(t, f)

	f.controls <- &pb.ControlMessage{Command: &pb.ControlMessage_AgentPrompt{AgentPrompt: &pb.AgentPrompt{
		RunId:  "run-cri259",
		Step:   "deploy",
		Prompt: "please re-run with more logging",
	}}}

	select {
	case got := <-c.AgentPromptCh():
		if got == nil {
			t.Fatal("received nil agent prompt")
		}
		if got.RunId != "run-cri259" {
			t.Fatalf("RunId: got %q want run-cri259", got.RunId)
		}
		if got.Step != "deploy" {
			t.Fatalf("Step: got %q want deploy", got.Step)
		}
		if got.Prompt != "please re-run with more logging" {
			t.Fatalf("Prompt: got %q", got.Prompt)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("agent prompt not delivered to AgentPromptCh within 2s (silent drop)")
	}
}

// TestCRI259AgentPromptBoundingResumeStillWorks asserts that resume delivery
// on the same Control stream keeps working after the AgentPrompt dispatch was
// added — neither arm starves the other.
func TestCRI259AgentPromptBoundingResumeStillWorks(t *testing.T) {
	f := newFakeServer()
	c, _ := startCtlClient(t, f)

	f.controls <- &pb.ControlMessage{Command: &pb.ControlMessage_AgentPrompt{AgentPrompt: &pb.AgentPrompt{
		RunId:  "run-cri259",
		Step:   "deploy",
		Prompt: "hold on",
	}}}
	f.controls <- &pb.ControlMessage{Command: &pb.ControlMessage_ResumeRun{ResumeRun: &pb.ResumeRun{
		RunId: "run-cri259",
	}}}

	deadline := time.After(2 * time.Second)
	gotPrompt, gotResume := false, false
	for !gotPrompt || !gotResume {
		select {
		case ap := <-c.AgentPromptCh():
			if ap != nil && ap.RunId == "run-cri259" {
				gotPrompt = true
			}
		case rr := <-c.ResumeCh():
			if rr != nil && rr.RunId == "run-cri259" {
				gotResume = true
			}
		case <-deadline:
			t.Fatalf("bounding: gotPrompt=%v gotResume=%v (both must deliver)", gotPrompt, gotResume)
		}
	}
}

// TestCRI259AgentPromptEmptyRunIdStillObservable asserts that an AgentPrompt
// without a run_id is still dispatched (the routing layer records the typed
// failure) rather than dropped silently in the transport (R1).
func TestCRI259AgentPromptEmptyRunIdStillObservable(t *testing.T) {
	f := newFakeServer()
	c, _ := startCtlClient(t, f)

	f.controls <- &pb.ControlMessage{Command: &pb.ControlMessage_AgentPrompt{AgentPrompt: &pb.AgentPrompt{
		Step:   "deploy",
		Prompt: "orphan",
	}}}

	select {
	case <-c.AgentPromptCh():
	case <-time.After(2 * time.Second):
		t.Fatal("agent prompt with empty run_id was dropped unobserved")
	}
}

// TestCRI259ControlMessageUnsetCommandDoesNotKillStream asserts that an
// unrecognized/unset ControlMessage arm is logged (no silent fall-through)
// and leaves the stream healthy for subsequent messages (R1).
func TestCRI259ControlMessageUnsetCommandDoesNotKillStream(t *testing.T) {
	f := newFakeServer()
	c, _ := startCtlClient(t, f)

	f.controls <- &pb.ControlMessage{}

	f.controls <- &pb.ControlMessage{Command: &pb.ControlMessage_ResumeRun{ResumeRun: &pb.ResumeRun{
		RunId: "run-after-unset",
	}}}

	select {
	case rr := <-c.ResumeCh():
		if rr == nil || rr.RunId != "run-after-unset" {
			t.Fatalf("unexpected resume after unset command: %+v", rr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("control loop did not survive an unset command message")
	}
}
