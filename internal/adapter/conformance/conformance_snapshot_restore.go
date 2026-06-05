package conformance

// conformance_snapshot_restore.go — snapshot/restore contract tests.

import (
	"context"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapterhost"
)

func executeAndAssert(t *testing.T, name string, plug adapterhost.Handle, sessionID string, ctx context.Context, cfg map[string]string, info *adapterhost.Info, phase string) {
	t.Helper()
	step := baseStep(name, info.Name, cfg)
	sink := &recordingSink{}
	_, execErr := executeNoPanic(t, adapterSessionTarget{handle: plug, sessionID: sessionID, name: info.Name}, ctx, step, sink)
	if execErr != nil {
		t.Fatalf("%s execute: %v", phase, execErr)
	}
}

func testSnapshotRestore(t *testing.T, name string, loader adapterhost.Loader, opts *Options, info *adapterhost.Info) {
	t.Helper()
	if !hasFeature(opts, "snapshot") || !hasFeature(opts, "restore") {
		t.Skipf("%s: snapshot/restore not in supported_features", name)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	plug, sessionID := resolveAndOpen(t, ctx, loader, name, opts.OpenConfig, opts.Secrets)
	defer plug.Kill()
	defer func() { _ = plug.CloseSession(context.Background(), sessionID) }()

	executeAndAssert(t, name, plug, sessionID, ctx, opts.StepConfig, info, "pre-snapshot")

	snap, err := plug.Snapshot(ctx, sessionID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	plug2, sessionID2 := resolveAndOpen(t, ctx, loader, name, opts.OpenConfig, opts.Secrets)
	defer plug2.Kill()
	defer func() { _ = plug2.CloseSession(context.Background(), sessionID2) }()

	if err := plug2.Restore(ctx, sessionID2, snap.GetState(), snap.GetSchemaVersion()); err != nil {
		t.Fatalf("restore: %v", err)
	}
	executeAndAssert(t, name, plug2, sessionID2, ctx, opts.StepConfig, info, "post-restore")
}
