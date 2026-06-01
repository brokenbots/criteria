package conformance

// conformance_pause_resume.go — pause/resume contract tests.

import (
	"context"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapterhost"
)

func testPauseResume(t *testing.T, name string, loader adapterhost.Loader, opts *Options, info *adapterhost.Info) {
	t.Helper()

	if !hasFeature(opts, "pause") || !hasFeature(opts, "resume") {
		t.Skipf("%s: pause/resume not in supported_features", name)
	}

	// 30 s matches the StartTimeout in the loader.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	plug, err := loader.Resolve(ctx, name)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	defer plug.Kill()

	sessionID := newSessionID("pause")
	if err := plug.OpenSession(ctx, sessionID, cloneConfig(opts.OpenConfig), nil); err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer func() {
		_ = plug.CloseSession(context.Background(), sessionID)
	}()

	step := baseStep(name, info.Name, opts.StepConfig)
	sink := &recordingSink{}

	// Execute once before pause.
	_, execErr := executeNoPanic(t, adapterSessionTarget{handle: plug, sessionID: sessionID, name: info.Name}, ctx, step, sink)
	if execErr != nil {
		t.Fatalf("pre-pause execute: %v", execErr)
	}

	if err := plug.Pause(ctx, sessionID); err != nil {
		t.Fatalf("pause: %v", err)
	}

	// While paused, a second execute should fail or stall.
	pauseCtx, pauseCancel := context.WithTimeout(ctx, 200*time.Millisecond)
	_, pauseErr := executeNoPanic(t, adapterSessionTarget{handle: plug, sessionID: sessionID, name: info.Name}, pauseCtx, step, sink)
	pauseCancel()
	if pauseErr == nil {
		t.Fatal("expected execute while paused to fail or stall, got nil")
	}

	if err := plug.Resume(ctx, sessionID); err != nil {
		t.Fatalf("resume: %v", err)
	}

	// After resume, execute should succeed again.
	_, execErr = executeNoPanic(t, adapterSessionTarget{handle: plug, sessionID: sessionID, name: info.Name}, ctx, step, sink)
	if execErr != nil {
		t.Fatalf("post-resume execute: %v", execErr)
	}
}
