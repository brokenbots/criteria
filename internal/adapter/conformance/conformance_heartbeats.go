package conformance

// conformance_heartbeats.go — heartbeat stall/crash detection contract tests.

import (
	"context"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapterhost"
)

// defaultHeartbeatStallThreshold is the fast threshold used by the heartbeat
// conformance suite. Tests can override it via Options.HeartbeatStallThreshold.
const defaultHeartbeatStallThreshold = 250 * time.Millisecond

func testHeartbeats(t *testing.T, name string, loader adapterhost.Loader, opts *Options, info *adapterhost.Info) {
	t.Helper()

	// Heartbeat conformance is mandatory for any adapter that implements
	// LogStreamStarter. It is no longer opt-in via opts.Heartbeats.
	//
	// In-tree fixtures read this variable and heartbeat faster than the
	// production 30 s cadence so the idle-survival test can use a short
	// threshold without waiting 90 s.
	t.Setenv("CRITERIA_TEST_HEARTBEAT_INTERVAL_MS", "50")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	plug, err := loader.Resolve(ctx, name)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	defer plug.Kill()

	if _, ok := plug.(adapterhost.LogStreamStarter); !ok {
		t.Skipf("%s: adapter does not implement LogStreamStarter", name)
	}

	threshold := opts.HeartbeatStallThreshold
	if threshold <= 0 {
		threshold = defaultHeartbeatStallThreshold
	}

	// Positive path: open a session through the host SessionManager, leave it
	// idle well past the stall threshold, and then execute a step.
	sm := adapterhost.NewSessionManager(loader)
	sm.HeartbeatStallThreshold = threshold
	defer func() { _ = sm.Shutdown(ctx) }()

	sessionID := openIdleSession(t, name, sm, ctx, info, opts)
	// Idle past the threshold. A correct host+adapter pair keeps the session
	// alive with log-stream heartbeats.
	time.Sleep(threshold * 3)
	res, execErr := sm.Execute(ctx, sessionID, baseStep(name, info.Name, cloneConfig(opts.StepConfig)), &recordingSink{})
	if execErr != nil {
		t.Fatalf("%s: idle session failed after %s: %v", name, threshold, execErr)
	}
	assertValidOutcome(t, res.Outcome, opts)

	// Contract enforcement: probe a fresh Log stream and count heartbeat events.
	hbEvents := countHeartbeatProbeEvents(t, name, loader, ctx, opts, threshold)
	if hbEvents == 0 {
		t.Fatalf("%s: log stream emitted no heartbeat events while idle; heartbeats are mandatory for log-streaming adapters", name)
	}
}

func openIdleSession(t *testing.T, name string, sm *adapterhost.SessionManager, ctx context.Context, info *adapterhost.Info, opts *Options) string {
	t.Helper()
	sessionID := newSessionID(name + "-idle")
	if err := sm.Open(ctx, sessionID, info.Name, "fail", cloneConfig(opts.OpenConfig), cloneConfig(opts.Secrets)); err != nil {
		t.Fatalf("%s: open session via session manager: %v", name, err)
	}
	t.Cleanup(func() { _ = sm.Close(ctx, sessionID) })
	return sessionID
}

func countHeartbeatProbeEvents(t *testing.T, name string, loader adapterhost.Loader, ctx context.Context, opts *Options, threshold time.Duration) int {
	t.Helper()
	hbCtx, hbCancel := context.WithTimeout(ctx, threshold*4)
	defer hbCancel()

	probePlug, err := loader.Resolve(hbCtx, name)
	if err != nil {
		t.Fatalf("%s: resolve probe adapter: %v", name, err)
	}
	defer probePlug.Kill()

	probeSessionID := newSessionID(name + "-hb-probe")
	if err := probePlug.OpenSession(hbCtx, probeSessionID, cloneConfig(opts.OpenConfig), cloneConfig(opts.Secrets)); err != nil {
		t.Fatalf("%s: open probe session: %v", name, err)
	}
	defer func() { _ = probePlug.CloseSession(ctx, probeSessionID) }()

	hbSink := &recordingSink{}
	cancelLog, done, err := probePlug.(adapterhost.LogStreamStarter).StartLogStream(hbCtx, probeSessionID, hbSink)
	if err != nil {
		t.Fatalf("%s: start probe log stream: %v", name, err)
	}

	// Wait long enough for at least one heartbeat tick.
	time.Sleep(threshold * 2)
	cancelLog()
	<-done
	return hbSink.heartbeatEventCount()
}
