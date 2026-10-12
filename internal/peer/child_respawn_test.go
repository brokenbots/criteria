package peer

import (
	"bytes"
	"context"
	"syscall"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapterhost"
	criteriav2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
)

// TestPeerRuntime_EnschildRespawnsExitedChild pins the serve-on-demand
// respawn (KB-253 fleet canary root cause): a manifest child that exits
// (copilot idle-exit after its serving session closes) must resolve to a
// live client again on the next serve-path lookup, respawned from its
// preserved boot spec (same binary, same digest pin, no re-resolution).
// Fail-closed counterpart: a kill_child-commanded child stays dead.
func TestPeerRuntime_EnsureChildRespawnsExitedChild(t *testing.T) {
	if testing.Short() {
		t.Skip("respawn test spawns a real adapter subprocess")
	}
	bin := buildNoopAdapter(t)

	cfg, err := LoadConfig(getenvFrom(map[string]string{EnvRemoteHost: "127.0.0.1:7999"}))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.AdapterName = "noop"
	cfg.AdapterBinary = bin
	cfg.JournalLimit = 8

	var logs bytes.Buffer
	rt := NewRuntime(&cfg, captureLogger(&logs))
	rt.exitPoll = 20 * time.Millisecond
	rt.shutdownGrace = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Boot(ctx); err != nil {
		t.Fatalf("boot: %v", err)
	}

	rt.mu.Lock()
	killedPid, _ := adapterhost.ProcessPID(rt.children["noop"].handle)
	rt.mu.Unlock()
	if killedPid <= 0 {
		t.Fatalf("boot child pid = %d, want a real pid", killedPid)
	}
	// Kill the child out from under the runtime exactly once, like an
	// idle-exit: process gone, no killRequested flag, spec preserved.
	if err := syscall.Kill(int(killedPid), syscall.SIGKILL); err != nil {
		t.Fatalf("kill child: %v", err)
	}

	// Wait for the exit watcher to observe the exit (journal gets the crash
	// fact) AND for the handle to report exited.
	waitForExit := func() bool {
		for i := 0; i < 100; i++ {
			rt.mu.Lock()
			exited := rt.children["noop"] != nil && adapterhost.ProcessExited(rt.children["noop"].handle)
			rt.mu.Unlock()
			if exited {
				return true
			}
			time.Sleep(10 * time.Millisecond)
		}
		return false
	}
	if !waitForExit() {
		t.Fatal("child exit never observed")
	}

	client, ok := rt.ensureChild(ctx, "noop")
	if !ok || client == nil {
		t.Fatalf("ensureChild after exit did not respawn (ok=%v)", ok)
	}
	info, err := client.Info(ctx, &criteriav2.InfoRequest{})
	if err != nil {
		t.Fatalf("respawned child Info: %v", err)
	}
	if info.GetVersion() == "" {
		t.Fatalf("respawned child Info missing version: %+v", info)
	}

	rt.mu.Lock()
	c := rt.children["noop"]
	newPid, _ := adapterhost.ProcessPID(c.handle)
	killRequested := c.killRequested
	exited := adapterhost.ProcessExited(c.handle)
	rt.mu.Unlock()
	if newPid == killedPid {
		t.Fatalf("respawned child has the old pid %d: no respawn actually happened", newPid)
	}
	if exited || killRequested {
		t.Fatalf("respawned child state wrong: exited=%v killRequested=%v", exited, killRequested)
	}

	// Journal carries the new spawn fact attributed to the same definition.
	events := rt.Journal().Replay(0)
	spawnFacts := 0
	for _, ev := range events {
		if ev.GetSpawned() != nil {
			spawnFacts++
		}
	}
	if spawnFacts < 2 {
		t.Fatalf("journal spawn facts after respawn = %d, want >= 2", spawnFacts)
	}

	// killRequested honors a host-commanded kill: mark and ensure no respawn.
	rt.mu.Lock()
	rt.children["noop"].killRequested = true
	rt.mu.Unlock()
	again, ok := rt.ensureChild(ctx, "noop")
	if ok || again != nil {
		t.Fatal("ensureChild respawned a kill_child-commanded child")
	}

	// Unknown definitions (not in the boot manifest) fail closed.
	if _, ok := rt.ensureChild(ctx, "ghost.definition"); ok {
		t.Fatal("ensureChild respawned a definition outside the boot spec")
	}

	if err := rt.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}