package conformance

// conformance_inspect.go — Inspect contract tests.

import (
	"context"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapterhost"
)

func testInspect(t *testing.T, name string, loader adapterhost.Loader, opts *Options, info *adapterhost.Info) {
	t.Helper()

	if !hasFeature(opts, "inspect") {
		t.Skipf("%s: inspect not in supported_features", name)
	}

	// 30 s matches the StartTimeout in the loader.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	plug, err := loader.Resolve(ctx, name)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	defer plug.Kill()

	sessionID := newSessionID("inspect")
	if err := plug.OpenSession(ctx, sessionID, cloneConfig(opts.OpenConfig), cloneConfig(opts.Secrets)); err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer func() {
		_ = plug.CloseSession(context.Background(), sessionID)
	}()

	// Inspect before execute.
	resp, err := plug.Inspect(ctx, sessionID)
	if err != nil {
		t.Fatalf("inspect before execute: %v", err)
	}
	if resp == nil {
		t.Fatal("inspect returned nil response")
	}

	// Execute.
	step := baseStep(name, info.Name, opts.StepConfig)
	sink := &recordingSink{}
	_, execErr := executeNoPanic(t, adapterSessionTarget{handle: plug, sessionID: sessionID, name: info.Name}, ctx, step, sink)
	if execErr != nil {
		t.Fatalf("execute: %v", execErr)
	}

	// Inspect after execute.
	resp2, err := plug.Inspect(ctx, sessionID)
	if err != nil {
		t.Fatalf("inspect after execute: %v", err)
	}
	if resp2 == nil {
		t.Fatal("inspect returned nil response after execute")
	}
}
