package servertrans

// kb53_tail_test.go — regression test for KB-53: the terminal run event
// (castle's runs-row transition source) must survive a SubmitEvents stream
// outage at the moment the run completes. The incident lost the
// RunCompleted/step-outcome tail because the CLI gave the publisher only a
// few seconds while the stream was failing; with the transport-level
// reconnect+replay this test pins that pending tail events are persisted on
// the first successful reconnect and acked within the terminal drain window.

import (
	"context"
	"testing"
	"time"

	"github.com/brokenbots/criteria/events"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

func TestRunPublisher_KB53_TailDeliveredAfterInitialStreamOutage(t *testing.T) {
	f := newFakeServer()
	// The first SubmitEvents stream open fails before any envelope is read —
	// the incident's shape (stream unavailable when the run completed).
	f.submitEventsFailOnAttempt = 1
	url := startFakeServer(t, f)

	c, err := NewClient(url, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer c.Close()

	if err := c.Register(ctx, "n", "h", "v"); err != nil {
		t.Fatal(err)
	}

	p, err := c.NewRunPublisher(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	// The run finished while the stream was down: the terminal envelope is
	// published before the publisher's loop has any healthy stream.
	completed := events.NewEnvelope("run-1", &pb.RunCompleted{FinalState: "awaiting_human", Success: true})
	p.Publish(ctx, completed)
	p.Start(ctx)

	// The terminal drain window must hold until the reconnect attempt
	// delivers and acks the tail — not drop it.
	drainCtx, drainCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer drainCancel()
	p.Drain(drainCtx)

	if got := p.lastAckedSeq.Load(); got != 1 {
		t.Fatalf("lastAckedSeq=%d; want 1 (terminal event must be acked after the outage)", got)
	}
	f.mu.Lock()
	persisted := append([]*pb.Envelope(nil), f.events["run-1"]...)
	f.mu.Unlock()
	if len(persisted) != 1 {
		t.Fatalf("server persisted %d events; want 1", len(persisted))
	}
	if rc := persisted[0].GetRunCompleted(); rc == nil {
		t.Fatalf("persisted payload = %v; want RunCompleted", persisted[0].Payload)
	} else if rc.FinalState != "awaiting_human" || !rc.Success {
		t.Errorf("RunCompleted = %+v; want awaiting_human/success", rc)
	}
}