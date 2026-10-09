// KB-153/KB-232 engine-level contract tests. KB-153's original premise — the
// engine's verify phase verifying the adapter through a throwaway handshake
// handle killed synchronously before the bind phase waits — was the remote
// environment's dial protocol until KB-232: on Gate-3 the bind phase's
// WaitForHandle raced the shim's asynchronous registry cleanup, resolved the
// dead bridge, and the step hard-failed with "rpc error: code = Canceled
// desc = grpc: the client connection is closing" after 1 attempts.
//
// KB-232 changed the remote (phone-home) dial protocol: a shim session
// handle IS the adapter's live phone-home bridge (PhoneHomeBridgeHandle), so
// the verify phase must NOT kill it — the bridge resolved by verify is
// reused by the bind phase without any forced re-dial, and the shim refuses
// (rather than displaces) live-session re-dials. The KB-153 kill-path
// registry invariant (a torn-down session never serves a fresh wait) remains
// pinned at the shim level (kb153_verify_kill_registry_test.go) and holds for
// engine-ordered kills (close/teardown).
//
// These tests run the real chain — real shim, real SessionManager bind path,
// pod phone-homing — in the exact post-init window that used to race:
//
//  1. the bind immediately after init must reuse the verify bridge (exactly
//     one handshake accepted, no forced re-dial, step succeeds), and
//  2. a genuinely dead adapter (no bridge, no re-dials) must fail the bind
//     at the session handshake budget with the CRI-137 diagnosis — never the
//     torn-down transport error.
//
// The shim level pins the refusal semantics themselves
// (kb232_live_redial_refusal_test.go).
package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapter/environment/remote"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

type kb153BindChain struct {
	sessions *adapterhost.SessionManager
	shim     *remote.Shim
	pod      *cri276PodServer
	accepted *atomic.Int64
	scopeKey string
}

// runKB153BindChain wires the full remote chain and returns it right at init
// return — the exact window that used to race the verify-kill against the
// bind phase's registry wait. With the KB-232 protocol the verify phase
// resolved the phone-home bridge and left it live, so the returning chain
// holds one established bridge that the bind phase must reuse as-is.
// budget > 0 overrides the shim's session-wait budgets (zero selects the
// defaults); keepRedialing=false stops the pod's phone-home loop right at
// init return — the pod stops serving and closes its connection, so the
// chain holds a bridge that is about to die with no respawn (the genuinely
// dead adapter; the test closes it deterministically via the engine's own
// CloseHandle path).
func runKB153BindChain(t *testing.T, budget time.Duration, keepRedialing bool) *kb153BindChain {
	t.Helper()
	ctx := context.Background()
	g := perScopeRemoteGraph(t)
	dataDir := t.TempDir()

	beforeDirs := map[string]struct{}{}
	if matches, err := filepath.Glob(filepath.Join(os.TempDir(), "criteria-remote-*")); err == nil {
		for _, m := range matches {
			beforeDirs[m] = struct{}{}
		}
	}

	cfg := &remote.Config{ListenAddress: "127.0.0.1:0", PerScopeSessions: true}
	if budget > 0 {
		cfg.SessionHandshakeBudget = budget
		cfg.SessionSchedulingBudget = budget
	}
	realShim, err := remote.NewShim(cfg, nil)
	if err != nil {
		t.Fatalf("NewShim: %v", err)
	}
	shimCtx, cancelShim := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancelShim()
		_ = realShim.Stop(context.Background())
		waitForCri137ShimTeardown(t, beforeDirs)
	})
	if err := realShim.Start(shimCtx); err != nil {
		t.Fatalf("start real shim: %v", err)
	}

	sessions := adapterhost.NewSessionManager(&fakeLoader{})
	sessions.SetGraph(g)
	sessions.SetRemoteShim(realShim)
	sink := &eventTrackingSink{}
	lifecycle := newScopeLifecycleState(dataDir)
	lifecycle.setRunID("run-kb153")
	rlc := &remoteLifecycleContext{scopeLifecycle: lifecycle}
	deps := Deps{Sessions: sessions, Sink: sink}

	initDone := make(chan error, 1)
	go func() {
		_, err := initScopeAdapters(ctx, g, deps, nil, dataDir, "", nil, rlc)
		initDone <- err
	}()

	var first AdapterLifecycleEvent
	eventDeadline := time.Now().Add(3 * time.Second)
	for {
		ev, ok := sink.firstStatus("provision_wanted")
		if ok {
			first = ev
			break
		}
		if time.Now().After(eventDeadline) {
			t.Fatal("init emitted no provision_wanted event")
		}
		time.Sleep(5 * time.Millisecond)
	}

	tokenBytes, err := os.ReadFile(first.TokenRef)
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	scopeKey := first.ScopeName + "/" + first.ScopeInstanceID
	hs := &cri137Handshake{
		Name:    "noop",
		Version: "1.0.0",
		Digest:  "sha256:abcd1234",
		Token:   string(tokenBytes),
		Scope:   scopeKey,
	}

	stopDial := make(chan struct{})
	podDone := make(chan struct{})
	pod := &cri276PodServer{name: "noop", version: "1.0.0"}
	accepted := &atomic.Int64{}
	stopDialOnce := &sync.Once{}
	stopDialing := func() { stopDialOnce.Do(func() { close(stopDial) }) }
	go podHomeLoop(realShim.ListenAddr(), hs, stopDial, podDone, accepted, pod, nil)
	t.Cleanup(func() {
		stopDialing()
		<-podDone
	})
	// Shut the manager down first (LIFO): it closes bound sessions through
	// the still-live shim, so no handle outlives the test (goleak).
	t.Cleanup(func() {
		_ = sessions.Shutdown(context.Background())
	})

	if err := <-initDone; err != nil {
		t.Fatalf("initScopeAdapters: %v", err)
	}
	if !keepRedialing {
		// Genuinely dead adapter: no bridge survives init. The pod stops
		// redialing (its loop closes the established connection) and the
		// test closes the engine-side session deterministically through the
		// engine's own close path, so the bind below cannot resolve any
		// bridge and must fail at the handshake budget.
		stopDialing()
	}

	// Intentionally NO settle wait for the bind: the bind phase runs in the
	// same window as the verify phase that just resolved the phone-home
	// bridge — the window that the KB-153 kill-path race (and, before that,
	// the verify kill itself) made flaky.
	return &kb153BindChain{
		sessions: sessions,
		shim:     realShim,
		pod:      pod,
		accepted: accepted,
		scopeKey: first.ScopeName + "/" + first.ScopeInstanceID,
	}
}

func TestBindAfterInit_ReusesLivePhoneHomeBridge(t *testing.T) {
	chain := runKB153BindChain(t, 0, true)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	events := cri276AdapterEvents{}
	step := &workflow.StepNode{Name: "kb232-reuse"}

	res, err := chain.sessions.Execute(ctx, "noop.default", step, events, nil)
	if err != nil {
		if strings.Contains(err.Error(), "connection is closing") {
			t.Fatalf("bind resolved a dead bridge and failed on the torn-down transport (KB-153 race): %v", err)
		}
		t.Fatalf("first step after init failed: %v", err)
	}
	if res.Outcome != "success" {
		t.Fatalf("first step outcome = %q, want success", res.Outcome)
	}

	// counts handshakes and its server counts Execute calls asynchronously
	// (the accept poll runs after the bridge traffic arrives), so settle
	// briefly before asserting both counters.
	settleDeadline := time.Now().Add(5 * time.Second)
	for {
		if chain.pod.execCalls.Load() >= 1 && chain.accepted.Load() >= 1 {
			break
		}
		if time.Now().After(settleDeadline) {
			t.Fatalf("pod served %d Execute calls and counted %d accepted handshakes within 5s of a successful first step; want the step to cross the verify bridge", chain.pod.execCalls.Load(), chain.accepted.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := chain.accepted.Load(); got != 1 {
		t.Fatalf("pod counted %d accepted handshakes after a step that reused the verify bridge; want exactly 1 (verify dial only — the engine must not kill the phone-home bridge, KB-232)", got)
	}
}

// TestBindDeadAdapter_FailsAtHandshakeBudget is the engine-level dead-adapter
// half of the acceptance pair: an adapter with no established bridge and no
// re-dialing pod must fail the step at the session handshake budget with the
// CRI-137 diagnosis — never with a torn-down transport error.
func TestBindDeadAdapter_FailsAtHandshakeBudget(t *testing.T) {
	const budget = 400 * time.Millisecond
	chain := runKB153BindChain(t, budget, false)

	// The pod's phone-home loop is stopped (it closed its bridge
	// connection); close the engine-side session deterministically through
	// the engine's own close path so the bind below starts from an empty
	// registry rather than racing the dying bridge (the KB-153 registry
	// invariant guarantees the torn-down session is deregistered once
	// CloseHandle returns).
	if err := chain.shim.CloseHandle(context.Background(), "noop", chain.scopeKey); err != nil {
		t.Fatalf("CloseHandle: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	events := cri276AdapterEvents{}
	step := &workflow.StepNode{Name: "kb232-dead-bind"}

	start := time.Now()
	_, err := chain.sessions.Execute(ctx, "noop.default", step, events, nil)
	if err == nil {
		t.Fatalf("step against a dead adapter succeeded; want handshake-budget failure")
	}
	msg := err.Error()
	if strings.Contains(msg, "connection is closing") {
		t.Fatalf("dead adapter surfaced the torn-down transport signature instead of the budget diagnosis: %v", err)
	}
	if !strings.Contains(msg, "identity handshake") {
		t.Fatalf("dead adapter failure = %q, want CRI-137 diagnosis mentioning identity handshake", msg)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("dead adapter failure took %s; the 400ms handshake budget should bound it", elapsed)
	}
}
