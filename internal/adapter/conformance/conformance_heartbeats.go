package conformance

// conformance_heartbeats.go — heartbeat stall/crash detection contract tests.

import (
	"context"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapterhost"
)

func testHeartbeats(t *testing.T, name string, loader adapterhost.Loader, opts *Options, info *adapterhost.Info) {
	t.Helper()

	if !opts.Streaming || !opts.Heartbeats {
		t.Skipf("%s: streaming or heartbeats not enabled in options", name)
	}

	// 30 s matches the StartTimeout in the loader.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	plug, err := loader.Resolve(ctx, name)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	defer plug.Kill()

	sessionID := newSessionID("heartbeat")
	if err := plug.OpenSession(ctx, sessionID, cloneConfig(opts.OpenConfig), nil); err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer func() {
		_ = plug.CloseSession(context.Background(), sessionID)
	}()

	// Start the Log stream; the adapter is expected to stall it.
	if lss, ok := plug.(adapterhost.LogStreamStarter); ok {
		logSink := &recordingSink{}
		cancelLog, err := lss.StartLogStream(ctx, sessionID, logSink)
		if err != nil {
			t.Fatalf("start log stream: %v", err)
		}
		defer cancelLog()
	} else {
		t.Skipf("%s: adapter does not implement LogStreamStarter", name)
	}

	// The adapter must be configured to stall the Log stream.
	cfg := cloneConfig(opts.StepConfig)
	cfg["stall_log_stream"] = "true"
	step := baseStep(name, info.Name, cfg)
	sink := &recordingSink{}

	// Use a short timeout because we expect the adapter to stall and the
	// host to detect it.
	execCtx, execCancel := context.WithTimeout(ctx, 2*time.Second)
	_, execErr := executeNoPanic(t, adapterSessionTarget{handle: plug, sessionID: sessionID, name: info.Name}, execCtx, step, sink)
	execCancel()

	// We expect either a heartbeat-timeout error or a normal timeout.
	if execErr == nil {
		t.Fatalf("%s: adapter did not stall or host did not detect heartbeat loss — expected an error", name)
	}
}
