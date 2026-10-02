package engine

// Regression tests for KB-70: a per-scope remote-adapter init wait must not
// fail with the CRI-137 dead-Job verdict while the adapter pod is still
// Pending. A burst of per-scope pod creations (parallel runs) pushes pod
// start past the fixed handshake budget, and a pod that never scheduled
// cannot handshake — the live incident failed init with "no identity
// handshake observed at all ... adapter Job may be complete or dead" while
// the pod had never started. The shim's two-budget wait (handshake budget
// armed only on pod-started evidence, separate scheduling budget while
// Pending) keeps init alive across the burst latency; these tests drive the
// real shim through initScopeAdapters, the same path a run uses, with a
// pod-state probe standing in for the k8s observations.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapter/environment/remote"
	"github.com/brokenbots/criteria/internal/adapterhost"
)

// kb70PodProbe scripts the adapter pod's phase from construction: "Pending"
// until pendingFor elapses, "Running" afterwards. In production the pod-state
// probe is wired by the host environment; here it simulates the burst
// scheduling latency that keeps a pod Pending past the handshake budget.
type kb70PodProbe struct {
	start      time.Time
	pendingFor time.Duration
}

func newKB70PodProbe(t *testing.T, pendingFor time.Duration) *kb70PodProbe {
	t.Helper()
	return &kb70PodProbe{start: time.Now(), pendingFor: pendingFor}
}

func (p *kb70PodProbe) PodState(string, string) (remote.PodState, bool) {
	if time.Since(p.start) < p.pendingFor {
		return remote.PodState{Phase: "Pending"}, true
	}
	return remote.PodState{Phase: "Running"}, true
}

// kb70InitChain is a per-scope remote-adapter init underway against a real
// shim: initScopeAdapters is already running in a goroutine, the scope token
// is persisted and provision_wanted is emitted, and the Verify wait is pending
// on the adapter's phone-home handshake.
type kb70InitChain struct {
	initDone <-chan error
	sink     *eventTrackingSink
	sessions *adapterhost.SessionManager
	shim     *remote.Shim
	addr     string
	scopeKey string
	hs       *cri137Handshake
}

// startKB70InitChain wires the init path (mirroring the CRI-276 harness) with
// KB-70 budgets: the pod stays Pending for pendingFor from now, then reports
// Running; handshakeBudget bounds the wait once the pod started,
// schedulingBudget bounds it while Pending. Cleanup shuts the manager down
// LIFO before the shim so no handle outlives the test (goleak).
func startKB70InitChain(t *testing.T, probe remote.PodStateProbe, handshakeBudget, schedulingBudget time.Duration) *kb70InitChain {
	t.Helper()
	ctx := context.Background()
	g := perScopeRemoteGraph(t)
	dataDir := t.TempDir()

	// Temp-dir hygiene mirrors the CRI-137/CRI-276 harnesses.
	beforeDirs := map[string]struct{}{}
	if matches, err := filepath.Glob(filepath.Join(os.TempDir(), "criteria-remote-*")); err == nil {
		for _, m := range matches {
			beforeDirs[m] = struct{}{}
		}
	}

	realShim, err := remote.NewShim(&remote.Config{
		ListenAddress:           "127.0.0.1:0",
		PerScopeSessions:        true,
		SessionHandshakeBudget:  handshakeBudget,
		SessionSchedulingBudget: schedulingBudget,
	}, nil)
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
	realShim.SetPodStateProbe(probe)

	sessions := adapterhost.NewSessionManager(&fakeLoader{})
	sessions.SetGraph(g)
	sessions.SetRemoteShim(realShim)
	sink := &eventTrackingSink{}
	lifecycle := newScopeLifecycleState(dataDir)
	lifecycle.setRunID("run-123")
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

	// Shut the manager down first (LIFO): it closes bound sessions through
	// the still-live shim, so no handle outlives the test (goleak).
	t.Cleanup(func() {
		_ = sessions.Shutdown(context.Background())
	})

	return &kb70InitChain{initDone: initDone, sink: sink, sessions: sessions, shim: realShim, addr: realShim.ListenAddr(), scopeKey: scopeKey, hs: hs}
}

func waitForKB70InitError(t *testing.T, initDone <-chan error) error {
	t.Helper()
	select {
	case err := <-initDone:
		if err == nil {
			t.Fatal("scoped init returned no error, want a terminal init failure")
		}
		return err
	case <-time.After(8 * time.Second):
		t.Fatal("scoped init never failed although the pod never started")
		return nil
	}
}

func TestInitScopeAdapters_PodPendingPastHandshakeBudget_InitCompletesOnceRunning(t *testing.T) {
	// The refire scenario (KB-70): a burst of per-scope pod creations keeps
	// the adapter pod Pending past the handshake budget before it goes
	// Running and the adapter phone-homes. Init must survive the scheduling
	// latency — the handshake budget is frozen while the pod is Pending —
	// and complete once the pod starts.
	chain := startKB70InitChain(t, newKB70PodProbe(t, 300*time.Millisecond), 150*time.Millisecond, 30*time.Second)

	// Under the old fixed handshake budget the verify wait expired at
	// +150ms (pod still Pending) and init failed before the adapter could
	// dial. The scheduling grace keeps it alive; the phone-home lands
	// shortly after the pod goes Running and init completes.
	time.Sleep(310 * time.Millisecond)
	stopDial := make(chan struct{})
	dialDone := make(chan struct{})
	go func() {
		defer close(dialDone)
		dialCri137AdapterLoop(chain.addr, chain.hs, stopDial, nil)
	}()
	t.Cleanup(func() {
		close(stopDial)
		<-dialDone
	})

	select {
	case err := <-chain.initDone:
		if err != nil {
			t.Fatalf("init failed although the pod went Running and dialed: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("init never completed after the pod went Running and dialed")
	}
}

func TestInitScopeAdapters_PodPendingAtSchedulingExpiry_FailsWithPodPhase(t *testing.T) {
	// A pod still Pending when the scheduling budget expires must fail init
	// with the pod's actual phase: a pod that never ran had no Job to die,
	// so the verdict must not claim the adapter Job is complete or dead.
	chain := startKB70InitChain(t, newKB70PodProbe(t, 10*time.Minute), 30*time.Second, 500*time.Millisecond)

	err := waitForKB70InitError(t, chain.initDone)
	msg := err.Error()
	if !strings.Contains(msg, "waiting for the adapter pod to start") {
		t.Errorf("error must come from the scheduling budget, got: %v", err)
	}
	if !strings.Contains(msg, `last observed pod phase: "Pending"`) {
		t.Errorf("error must cite the pending pod phase, got: %v", err)
	}
	if strings.Contains(msg, "adapter Job may be complete or dead") {
		t.Errorf("dead-Job verdict is wrong for a pod that never started, got: %v", err)
	}
	initFailed := false
	for _, s := range chain.sink.lifecycleStatuses {
		if strings.HasSuffix(s, ":init_failed") {
			initFailed = true
		}
	}
	if !initFailed {
		t.Errorf("expected init_failed lifecycle event, got %v", chain.sink.lifecycleStatuses)
	}
}

func TestInitScopeAdapters_PodRunningNeverDials_FailsAtHandshakeBudget(t *testing.T) {
	// A pod observed Running that never presents an identity frame is
	// genuinely a dead or completed Job: init must fail at the handshake
	// budget with the CRI-137 verdict, not linger in the scheduling grace.
	chain := startKB70InitChain(t, newKB70PodProbe(t, 0), 300*time.Millisecond, 30*time.Second)

	err := waitForKB70InitError(t, chain.initDone)
	msg := err.Error()
	if !strings.Contains(msg, "without a successful identity handshake") {
		t.Errorf("error must be bounded by the handshake budget, got: %v", err)
	}
	if !strings.Contains(msg, "adapter Job may be complete or dead") || !strings.Contains(msg, "observed Running but never dialed") {
		t.Errorf("dead-Job diagnosis must be kept for a started pod, got: %v", err)
	}
	if strings.Contains(msg, "waiting for the adapter pod to start") {
		t.Errorf("handshake-budget failure must not be reported as scheduling, got: %v", err)
	}
	if strings.Contains(msg, "adapter pod phase is") {
		t.Errorf("no terminal pod phase was observed, got: %v", err)
	}
}
