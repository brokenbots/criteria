package servertrans

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brokenbots/criteria/events"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// TestNewRunPublisher_NotRegistered verifies that a client without credentials
// returns the "not registered" error from NewRunPublisher. It also exercises
// AssignmentCh() so the zero-coverage accessor is covered.
func TestNewRunPublisher_NotRegistered(t *testing.T) {
	requireNoGoroutineLeak(t)

	c, err := NewClient("http://localhost:9999", newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if c.AssignmentCh() == nil {
		t.Fatal("AssignmentCh() returned nil")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err = c.NewRunPublisher(ctx, "run-1")
	if err == nil {
		t.Fatal("expected error from NewRunPublisher before Register")
	}
	if err.Error() != "not registered" {
		t.Fatalf("NewRunPublisher error: got %q want \"not registered\"", err.Error())
	}
}

// TestNewRunPublisher_HappyPath verifies that NewRunPublisher creates and starts
// a publisher for the requested run id and that Publish delivers events.
func TestNewRunPublisher_HappyPath(t *testing.T) {
	f := newFakeServer()
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
		t.Fatalf("NewRunPublisher: %v", err)
	}
	defer p.Close()

	if p.RunID() != "run-1" {
		t.Fatalf("RunID: got %q want run-1", p.RunID())
	}
	if !p.IsStarted() {
		t.Fatal("publisher should be started")
	}

	env := events.NewEnvelope("run-1", &pb.StepEntered{Step: "s1", Adapter: "shell", Attempt: 1})
	p.Publish(ctx, env)

	if !waitForCond(t, 2*time.Second, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.events["run-1"]) == 1
	}) {
		t.Fatal("event never persisted")
	}
	if p.lastAckedSeq.Load() != 1 {
		t.Fatalf("expected lastAckedSeq=1, got %d", p.lastAckedSeq.Load())
	}
}

// TestRunPublisher_Publish_NilEnvelope verifies Publish is a no-op for nil.
func TestRunPublisher_Publish_NilEnvelope(t *testing.T) {
	f := newFakeServer()
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

	p.Publish(ctx, nil)

	time.Sleep(50 * time.Millisecond)
	f.mu.Lock()
	count := len(f.events["run-1"])
	f.mu.Unlock()
	if count != 0 {
		t.Fatalf("expected no events, got %d", count)
	}
}

// TestRunPublisher_Publish_ContextCancel verifies the ctx.Done branch in
// Publish when the send buffer is full and the context is cancelled.
func TestRunPublisher_Publish_ContextCancel(t *testing.T) {
	c, err := NewClient("http://localhost:9999", newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	p := newRunPublisher(c, "run-1", 1)

	// Use an envelope with no timestamp and no schema version to exercise the
	// default-filling branches in Publish.
	p.Publish(context.Background(), &pb.Envelope{RunId: "run-1", Payload: &pb.Envelope_StepEntered{StepEntered: &pb.StepEntered{Step: "s1", Adapter: "shell", Attempt: 1}}})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.Publish(ctx, events.NewEnvelope("run-1", &pb.StepEntered{Step: "s2", Adapter: "shell", Attempt: 1}))

	if len(p.sendCh) != 1 {
		t.Fatalf("expected sendCh to still contain 1 event, got %d", len(p.sendCh))
	}
}

// TestRunPublisher_Publish_AfterClose verifies the closed-channel branch in
// Publish after the publisher has been closed.
func TestRunPublisher_Publish_AfterClose(t *testing.T) {
	c, err := NewClient("http://localhost:9999", newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	p := newRunPublisher(c, "run-1", 1)
	// Fill the send buffer before closing so the closed-channel case is the
	// only ready select branch for the subsequent Publish.
	p.Publish(context.Background(), events.NewEnvelope("run-1", &pb.StepEntered{Step: "s0", Adapter: "shell", Attempt: 1}))
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Publish(ctx, events.NewEnvelope("run-1", &pb.StepEntered{Step: "s1", Adapter: "shell", Attempt: 1}))

	if len(p.sendCh) != 1 {
		t.Fatalf("expected sendCh to still contain the pre-close event, got %d", len(p.sendCh))
	}
}

// TestRunPublisher_Start_Idempotent verifies that calling Start more than once
// does not open additional streams.
func TestRunPublisher_Start_Idempotent(t *testing.T) {
	f := newFakeServer()
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

	// Publish an event to force the lazy bidi stream to open.
	p.Publish(ctx, events.NewEnvelope("run-1", &pb.StepEntered{Step: "s1", Adapter: "shell", Attempt: 1}))
	if !waitForCond(t, 2*time.Second, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.streamOpenTimes) == 1
	}) {
		t.Fatalf("expected one stream open, got %d", len(f.streamOpenTimes))
	}

	p.Start(ctx) // second Start should be a no-op

	p.Publish(ctx, events.NewEnvelope("run-1", &pb.StepEntered{Step: "s2", Adapter: "shell", Attempt: 1}))
	if !waitForCond(t, 2*time.Second, func() bool { return p.lastAckedSeq.Load() == 2 }) {
		t.Fatalf("expected second ack, got %d", p.lastAckedSeq.Load())
	}

	f.mu.Lock()
	opens := len(f.streamOpenTimes)
	f.mu.Unlock()
	if opens != 1 {
		t.Fatalf("expected still exactly one stream open after idempotent Start, got %d", opens)
	}
}

// TestRunPublisher_Drain_EmptyAndClosed verifies Drain returns immediately when
// there is nothing pending and returns via the closed channel when closed with
// outstanding pending events.
func TestRunPublisher_Drain_EmptyAndClosed(t *testing.T) {
	c, err := NewClient("http://localhost:9999", newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Empty publisher: Drain returns immediately.
	p := newRunPublisher(c, "run-1", 1)
	done := make(chan struct{})
	go func() { p.Drain(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Drain did not return immediately on empty publisher")
	}

	// Closed with pending: Drain returns via the closed branch.
	env := events.NewEnvelope("run-1", &pb.StepEntered{Step: "s1", Adapter: "shell", Attempt: 1})
	p.appendPending(env)
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if !p.isClosed() {
		t.Fatal("isClosed should be true after Close")
	}

	done2 := make(chan struct{})
	go func() { p.Drain(context.Background()); close(done2) }()
	select {
	case <-done2:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Drain did not return after Close")
	}
}

// TestRunPublisher_ClearPendingEmptyCorrelationID directly exercises the early
// return in clearPending when the correlation id is empty.
func TestRunPublisher_ClearPendingEmptyCorrelationID(t *testing.T) {
	c, err := NewClient("http://localhost:9999", newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	p := newRunPublisher(c, "run-1", 1)
	env := events.NewEnvelope("run-1", &pb.StepEntered{Step: "s1", Adapter: "shell", Attempt: 1})
	env.CorrelationId = "cid-1"
	p.appendPending(env)

	p.clearPending("")
	if len(p.snapshotPending()) != 1 {
		t.Fatal("clearPending(\"\") should not remove pending events")
	}
}

// TestRunPublisher_Reconnect_RecoversPending verifies that a stream failure
// after the first ack is recovered by reconnecting and resending pending
// events exactly once.
func TestRunPublisher_Reconnect_RecoversPending(t *testing.T) {
	f := newFakeServer()
	f.failAfterAcks = 1
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

	p.Publish(ctx, events.NewEnvelope("run-1", &pb.StepEntered{Step: "s1", Adapter: "shell", Attempt: 1}))
	p.Publish(ctx, events.NewEnvelope("run-1", &pb.StepEntered{Step: "s2", Adapter: "shell", Attempt: 1}))

	if !waitForCond(t, 5*time.Second, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.events["run-1"]) == 2 && p.lastAckedSeq.Load() == 2
	}) {
		f.mu.Lock()
		got := len(f.events["run-1"])
		f.mu.Unlock()
		t.Fatalf("expected 2 persisted events with lastAckedSeq=2, got %d events, lastAckedSeq=%d", got, p.lastAckedSeq.Load())
	}

	f.mu.Lock()
	evts := append([]*pb.Envelope(nil), f.events["run-1"]...)
	f.mu.Unlock()
	wantSteps := []string{"s1", "s2"}
	if len(evts) != len(wantSteps) {
		t.Fatalf("expected %d events, got %d", len(wantSteps), len(evts))
	}
	for i, want := range wantSteps {
		if se := evts[i].GetStepEntered(); se == nil || se.Step != want {
			t.Errorf("event[%d]: want StepEntered{Step:%q}, got %v", i, want, evts[i])
		}
	}
}

// TestRunPublisher_Close_Idempotent verifies that concurrent Close calls do not
// panic and the publisher reports closed.
func TestRunPublisher_Close_Idempotent(t *testing.T) {
	c, err := NewClient("http://localhost:9999", newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	p := newRunPublisher(c, "run-1", 1)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = p.Close()
		}()
	}
	wg.Wait()

	if !p.isClosed() {
		t.Fatal("publisher should be closed")
	}
}

// TestClientPublish_NilEnvelope verifies Client.Publish is a no-op for nil.
func TestClientPublish_NilEnvelope(t *testing.T) {
	c, err := NewClient("http://localhost:9999", newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Publish(ctx, nil)
}

// TestClientPublish_NoDefaultPublisher verifies Client.Publish drops an event
// with no run id when no default publisher exists.
func TestClientPublish_NoDefaultPublisher(t *testing.T) {
	c, err := NewClient("http://localhost:9999", newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Publish(ctx, &pb.Envelope{}) // RunId is empty, no default publisher
}

// TestClientPublish_RunIDMismatch verifies Client.Publish drops an envelope
// whose run id differs from the default publisher's run id.
func TestClientPublish_RunIDMismatch(t *testing.T) {
	f := newFakeServer()
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
	if err := c.StartPublishStream(ctx, "run-1"); err != nil {
		t.Fatalf("StartPublishStream: %v", err)
	}

	c.Publish(ctx, events.NewEnvelope("run-1", &pb.StepEntered{Step: "s1", Adapter: "shell", Attempt: 1}))
	if !waitForCond(t, 2*time.Second, func() bool { return c.defaultPublisher.lastAckedSeq.Load() == 1 }) {
		t.Fatalf("first ack not observed")
	}

	c.Publish(ctx, events.NewEnvelope("run-2", &pb.StepEntered{Step: "s2", Adapter: "shell", Attempt: 1}))

	time.Sleep(100 * time.Millisecond)
	f.mu.Lock()
	count := len(f.events["run-1"])
	f.mu.Unlock()
	if count != 1 {
		t.Fatalf("expected 1 event for run-1, got %d", count)
	}
	if c.defaultPublisher.lastAckedSeq.Load() != 1 {
		t.Fatalf("expected lastAckedSeq to stay 1, got %d", c.defaultPublisher.lastAckedSeq.Load())
	}
}

// TestClientDrain_NilDefaultPublisher verifies Client.Drain returns immediately
// when there is no default publisher.
func TestClientDrain_NilDefaultPublisher(t *testing.T) {
	requireNoGoroutineLeak(t)

	c, err := NewClient("http://localhost:9999", newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	done := make(chan struct{})
	go func() { c.Drain(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Drain did not return with nil default publisher")
	}
}

// TestClientStartControl_AlreadyStarted verifies that StartControl is
// idempotent: a second call returns nil without opening another control stream.
func TestClientStartControl_AlreadyStarted(t *testing.T) {
	f := newFakeServer()
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
	if err := c.StartControl(ctx); err != nil {
		t.Fatalf("first StartControl: %v", err)
	}
	select {
	case <-f.ctlAttached:
	case <-time.After(2 * time.Second):
		t.Fatal("control never attached")
	}

	if err := c.StartControl(ctx); err != nil {
		t.Fatalf("second StartControl: %v", err)
	}
	if !c.controlStarted.Load() {
		t.Fatal("controlStarted should be true")
	}
	if f.controlAttempts.Load() != 1 {
		t.Fatalf("expected one Control attempt, got %d", f.controlAttempts.Load())
	}
}

// TestClientStartStreams_ControlError verifies that StartStreams returns an
// error when StartControl fails, covering the StartStreams error path.
func TestClientStartStreams_ControlError(t *testing.T) {
	f := newFakeServer()
	f.controlFailOnAttempt = 1
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

	err = c.StartStreams(ctx, "run-1")
	if err == nil {
		t.Fatal("expected error from StartStreams when control fails")
	}
	if !strings.Contains(err.Error(), "control stream") || !strings.Contains(err.Error(), "forced control stream failure") {
		t.Fatalf("expected control stream error, got %v", err)
	}
}

// TestRunPublisher_ContextCancelDuringStream verifies that cancelling the
// publisher context causes sendLoop to return context.Canceled and the
// publishLoop to exit on the context.Canceled branch.
func TestRunPublisher_ContextCancelDuringStream(t *testing.T) {
	f := newFakeServer()
	url := startFakeServer(t, f)

	c, err := NewClient(url, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer c.Close()

	if err := c.Register(ctx, "n", "h", "v"); err != nil {
		t.Fatal(err)
	}
	p, err := c.NewRunPublisher(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	p.Publish(ctx, events.NewEnvelope("run-1", &pb.StepEntered{Step: "s1", Adapter: "shell", Attempt: 1}))
	if !waitForCond(t, 2*time.Second, func() bool { return p.lastAckedSeq.Load() == 1 }) {
		t.Fatalf("expected ack, lastAckedSeq=%d", p.lastAckedSeq.Load())
	}

	cancel()
	// After cancel, publishLoop should stop and no further events reach the
	// server even if Publish is called with a fresh context.
	p.Publish(context.Background(), events.NewEnvelope("run-1", &pb.StepEntered{Step: "s2", Adapter: "shell", Attempt: 1}))
	time.Sleep(200 * time.Millisecond)

	f.mu.Lock()
	got := len(f.events["run-1"])
	f.mu.Unlock()
	if got != 1 {
		t.Fatalf("expected exactly one server event, got %d", got)
	}
}

// TestRunPublisher_CloseDuringStream verifies that closing the publisher stops
// sendLoop via the closed channel and causes publishLoop to exit after
// runSubmitEvents returns.
func TestRunPublisher_CloseDuringStream(t *testing.T) {
	f := newFakeServer()
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

	p.Publish(ctx, events.NewEnvelope("run-1", &pb.StepEntered{Step: "s1", Adapter: "shell", Attempt: 1}))
	if !waitForCond(t, 2*time.Second, func() bool { return p.lastAckedSeq.Load() == 1 }) {
		t.Fatalf("expected ack, lastAckedSeq=%d", p.lastAckedSeq.Load())
	}

	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if !p.isClosed() {
		t.Fatal("publisher should report closed")
	}
}

// TestRunPublisher_PendingSendError verifies that an error while replaying
// pending events on reconnect is returned from runSubmitEvents.
func TestRunPublisher_PendingSendError(t *testing.T) {
	f := newFakeServer()
	// Persist the first event but drop the ack so it stays pending.
	f.dropAcksBeforeSend = 1
	// The next stream open fails before reading, so the pending replay Send
	// returns an error.
	f.submitEventsFailOnAttempt = 2
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

	p.Publish(ctx, events.NewEnvelope("run-1", &pb.StepEntered{Step: "s1", Adapter: "shell", Attempt: 1}))

	// Wait for the reconnect attempt to occur and fail.
	if !waitForCond(t, 3*time.Second, func() bool {
		return f.submitEventsAttempts.Load() >= 2
	}) {
		t.Fatalf("expected at least 2 SubmitEvents attempts, got %d", f.submitEventsAttempts.Load())
	}
}

// TestRunPublisher_SendChannelClosed verifies that sendLoop exits gracefully
// when sendCh is closed.
func TestRunPublisher_SendChannelClosed(t *testing.T) {
	c, err := NewClient("http://localhost:9999", newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	p := newRunPublisher(c, "run-1", 1)
	p.Start(context.Background())
	close(p.sendCh)
	if !waitForCond(t, 2*time.Second, func() bool { return len(p.sendCh) == 0 }) {
		t.Fatal("sendCh was not closed")
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestClientDrain_WithDefaultPublisher verifies Client.Drain forwards to the
// default publisher when one exists.
func TestClientDrain_WithDefaultPublisher(t *testing.T) {
	f := newFakeServer()
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
	if err := c.StartPublishStream(ctx, "run-1"); err != nil {
		t.Fatalf("StartPublishStream: %v", err)
	}

	c.Publish(ctx, events.NewEnvelope("run-1", &pb.StepEntered{Step: "s1", Adapter: "shell", Attempt: 1}))
	if !waitForCond(t, 2*time.Second, func() bool {
		return c.defaultPublisher != nil && c.defaultPublisher.lastAckedSeq.Load() == 1
	}) {
		t.Fatalf("default publisher did not ack, lastAckedSeq=%d", c.defaultPublisher.lastAckedSeq.Load())
	}

	done := make(chan struct{})
	go func() {
		c.Drain(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Drain did not return with default publisher")
	}
}

// TestRunPublisher_ContextCancel_ExitsLoops verifies that cancelling the
// publisher context exits the send and publish loops cleanly and covers the
// context.Canceled fast-path in publishLoop.
func TestRunPublisher_ContextCancel_ExitsLoops(t *testing.T) {
	f := newFakeServer()
	url := startFakeServer(t, f)

	c, err := NewClient(url, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer c.Close()

	if err := c.Register(ctx, "n", "h", "v"); err != nil {
		t.Fatal(err)
	}

	p, err := c.NewRunPublisher(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	// Publish one event so the stream opens and sendLoop is blocked waiting
	// for the next envelope.
	p.Publish(ctx, events.NewEnvelope("run-1", &pb.StepEntered{Step: "s1", Adapter: "shell", Attempt: 1}))
	if !waitForCond(t, 2*time.Second, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.streamOpenTimes) >= 1
	}) {
		t.Fatal("stream never opened")
	}

	cancel()

	// If the loop did not exit, publishLoop would reconnect within ~500ms
	// and open a second stream.  Wait long enough to detect a reconnect and
	// assert it never happened.
	time.Sleep(800 * time.Millisecond)
	f.mu.Lock()
	opens := len(f.streamOpenTimes)
	f.mu.Unlock()
	if opens != 1 {
		t.Fatalf("publisher loop did not exit after context cancel; opened %d streams", opens)
	}
}
