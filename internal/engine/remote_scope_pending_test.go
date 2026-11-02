// Remote scope pending and scheduling test suite.
// Unifies:
//   - KB-70 (burst scheduling budget vs handshake budget: cri70_scheduling_repro_test.go)
//   - KB-153 (verify-to-bind phone-home bridge reuse: kb153_verify_kill_bind_test.go)
//   - KB-232 (two-phase scope registration before token visibility: kb232_scope_registration_test.go)
//   - KB-232 (scope session pending event bridge: kb232_scope_session_pending_test.go)
//   - KB-232 (slow pod acceptance: kb232_slow_pod_acceptance_test.go)
package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapter/environment/remote"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

// -----------------------------------------------------------------------------
// KB-70: Pod Scheduling vs Handshake Budgets
// -----------------------------------------------------------------------------

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
		Name:    "noop.default",
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
	if !canDialLoopbackTCP() {
		t.Skip("skipping test: loopback TCP dialing is restricted in current environment")
	}
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

// -----------------------------------------------------------------------------
// KB-153: Verify-to-Bind Phone-Home Bridge Reuse
// -----------------------------------------------------------------------------

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
		Name:    "noop.default",
		Version: "1.0.0",
		Digest:  "sha256:abcd1234",
		Token:   string(tokenBytes),
		Scope:   scopeKey,
	}

	pod := &cri276PodServer{}
	accepted := &atomic.Int64{}
	stopDial := make(chan struct{})
	podDone := make(chan struct{})
	var stopOnce atomic.Bool
	stopDialing := func() {
		if stopOnce.CompareAndSwap(false, true) {
			close(stopDial)
		}
	}
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
	if !canDialLoopbackTCP() {
		t.Skip("skipping test: loopback TCP dialing is restricted in current environment")
	}
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

	// Settle briefly before asserting both counters.
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
	if !canDialLoopbackTCP() {
		t.Skip("skipping test: loopback TCP dialing is restricted in current environment")
	}
	const budget = 400 * time.Millisecond
	chain := runKB153BindChain(t, budget, false)

	// The pod's phone-home loop is stopped (it closed its bridge
	// connection); close the engine-side session deterministically through
	// the engine's own close path so the bind below starts from an empty
	// registry rather than racing the dying bridge (the KB-153 registry
	// invariant guarantees the torn-down session is deregistered once
	// CloseHandle returns).
	if err := chain.shim.CloseHandle(context.Background(), "noop.default", chain.scopeKey); err != nil {
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

// -----------------------------------------------------------------------------
// KB-232: Scope Registration Order and Two-Phase Initialization
// -----------------------------------------------------------------------------

// kb232PerScopeTwoAdapterGraph compiles a workflow whose two remote adapters
// are bound to one per_scope_sessions environment, mirroring the
// sub-workflow shape that burst-provisions a pod fleet.
func kb232PerScopeTwoAdapterGraph(t *testing.T) *workflow.FSMGraph {
	t.Helper()
	return compile(t, `
workflow {
  name = "kb232-two-adapter"
  version = "0.1"
  initial_state = "start"
  target_state  = "done"
}

environment "remote" "prod" {
  listen_address     = "127.0.0.1:0"
  per_scope_sessions = true
}

adapter "shell" "intake" {
  environment = remote.prod
}

adapter "copilot" "planner" {
  environment = remote.prod
}

step "start" {
  target = adapter.shell.intake
  outcome "success" { next = step.done }
  outcome "error" { next = step.done }
}

state "done" {
  terminal = true
  success  = true
}`)
}

// kb232TokenPaths globs every rotated token file under the run data dir.
// Tokens live at remote-tokens/<scope>/<instance>/<type>.token, with the
// root workflow's empty scope name collapsing out (filepath.Join): the root
// shape is remote-tokens/<instance>/<type>.token.
func kb232TokenPaths(dataDir string) []string {
	patterns := []string{
		filepath.Join(dataDir, "remote-tokens", "*", "*.token"),
		filepath.Join(dataDir, "remote-tokens", "*", "*", "*.token"),
	}
	var out []string
	for _, pattern := range patterns {
		matches, _ := filepath.Glob(pattern)
		out = append(out, matches...)
	}
	return out
}

// TestRotateFreshRemoteScope_RegistersScopeBeforeTokenFileVisible pins the
// KB-232 ordering for contract item 1: when the shim's scope registry learns
// a freshly rotated scope, its accept token file must not be visible on disk
// yet.
func TestRotateFreshRemoteScope_RegistersScopeBeforeTokenFileVisible(t *testing.T) {
	g := perScopeRemoteGraph(t)
	dataDir := t.TempDir()

	sessions := adapterhost.NewSessionManager(&fakeLoader{})
	sessions.SetGraph(g)
	shim := newFakeRemoteShim(&fakeRemoteHandle{})
	shim.onRegister = func(scope, token string) {
		if visible := kb232TokenPaths(dataDir); len(visible) != 0 {
			t.Errorf("token file(s) already visible at shim registration time: %v", visible)
		}
	}
	sessions.SetRemoteShim(shim)

	lifecycle := newScopeLifecycleState(dataDir)
	lifecycle.setRunID("run-kb232-order")
	sink := &eventTrackingSink{}
	rlc := &remoteLifecycleContext{scopeLifecycle: lifecycle}
	deps := Deps{Sessions: sessions, Sink: sink}

	adapter := g.Adapters[g.AdapterOrder[0]]
	scopeKey, err := maybeRotateRemoteScope(deps, rlc, g, adapter, g.AdapterOrder[0], "")
	if err != nil {
		t.Fatalf("maybeRotateRemoteScope: %v", err)
	}
	if scopeKey == "" {
		t.Fatal("expected a per-scope verify scope key")
	}
	registeredToken := shim.registeredToken(scopeKey)
	if registeredToken == "" {
		t.Fatal("shim must hold the rotated scope token when rotation returns")
	}

	matches := kb232TokenPaths(dataDir)
	if len(matches) != 1 {
		t.Fatalf("token file must exist after rotation (matches=%v)", matches)
	}
	tokenBytes, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	if string(tokenBytes) != registeredToken {
		t.Errorf("persisted token %q, want the shim-registered token %q", string(tokenBytes), registeredToken)
	}
}

// TestRotateFreshRemoteScope_PersistFailure_UnregistersScope pins the
// rollback side of the reordered rotation: when persisting the rotated token
// or the current-instance record fails after the shim registration, the scope
// must be unregistered again and no provision_wanted may be emitted.
func TestRotateFreshRemoteScope_PersistFailure_UnregistersScope(t *testing.T) {
	g := perScopeRemoteGraph(t)
	dataDir := t.TempDir()

	sessions := adapterhost.NewSessionManager(&fakeLoader{})
	sessions.SetGraph(g)
	shim := newFakeRemoteShim(&fakeRemoteHandle{})
	sessions.SetRemoteShim(shim)

	lifecycle := newScopeLifecycleState(dataDir)
	lifecycle.setRunID("run-kb232-rollback")
	sink := &eventTrackingSink{}
	rlc := &remoteLifecycleContext{scopeLifecycle: lifecycle}
	deps := Deps{Sessions: sessions, Sink: sink}

	adapter := g.Adapters[g.AdapterOrder[0]]

	// Block the current-instance record write: a file where the record
	// directory must be created makes the atomic record write fail.
	scopeDir := filepath.Join(dataDir, "remote-tokens")
	if err := os.MkdirAll(scopeDir, 0o700); err != nil {
		t.Fatalf("seed scope dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(scopeDir, "current"), []byte("blocker"), 0o600); err != nil {
		t.Fatalf("seed record blocker: %v", err)
	}

	_, err := maybeRotateRemoteScope(deps, rlc, g, adapter, g.AdapterOrder[0], "")
	if err == nil {
		t.Fatal("expected rotation to fail on the blocked record write")
	}
	sink.mu.Lock()
	statuses := append([]string(nil), sink.lifecycleStatuses...)
	sink.mu.Unlock()
	hasInitFailed := false
	for _, s := range statuses {
		if strings.HasSuffix(s, ":init_failed") {
			hasInitFailed = true
		}
	}
	if !hasInitFailed {
		t.Errorf("expected an init_failed lifecycle status, got %v", statuses)
	}
	if sink.hasStatus("provision_wanted") {
		t.Errorf("provision_wanted must not be emitted when rotation fails, got %v", sink.provisionEvents)
	}
	if got := len(shim.unregistered); got != 1 {
		t.Fatalf("expected the failed rotation's scope to be unregistered once, got %d (%v)", got, shim.unregistered)
	}
	if shim.unregistered[0] == "" {
		t.Errorf("unregistered scope key must not be empty")
	}
}

// TestInitScopeAdapters_TwoPhase_RegistersAllScopesBeforeAnyVerify pins the
// KB-232 two-phase split: every adapter's per-scope registration happens in
// phase A (prepare + rotate + register) and the first blocking handshake
// (phase B Verify → WaitForHandle) starts only after all scopes are
// registered with the shim.
func TestInitScopeAdapters_TwoPhase_RegistersAllScopesBeforeAnyVerify(t *testing.T) {
	ctx := context.Background()
	g := kb232PerScopeTwoAdapterGraph(t)
	dataDir := t.TempDir()

	sessions := adapterhost.NewSessionManager(&fakeLoader{})
	sessions.SetGraph(g)
	shim := newFakeRemoteShim(&fakeRemoteHandle{})
	sessions.SetRemoteShim(shim)

	lifecycle := newScopeLifecycleState(dataDir)
	lifecycle.setRunID("run-kb232-two-phase")
	sink := &eventTrackingSink{}
	rlc := &remoteLifecycleContext{scopeLifecycle: lifecycle}
	deps := Deps{Sessions: sessions, Sink: sink}

	order, err := initScopeAdapters(ctx, g, deps, nil, dataDir, "run_handler", nil, rlc)
	if err != nil {
		t.Fatalf("initScopeAdapters: %v", err)
	}
	if len(order) != 2 {
		t.Fatalf("expected 2 adapters provisioned, got %v", order)
	}

	shim.mu.Lock()
	calls := append([]string(nil), shim.calls...)
	shim.mu.Unlock()

	lastRegister, firstWait := -1, -1
	registrations, waits := 0, 0
	for i, call := range calls {
		switch {
		case strings.HasPrefix(call, "RegisterScope:"):
			registrations++
			lastRegister = i
		case strings.HasPrefix(call, "WaitForHandle:"):
			waits++
			if firstWait == -1 {
				firstWait = i
			}
		}
	}
	if registrations != 2 {
		t.Fatalf("expected 2 scope registrations, got %d in %v", registrations, calls)
	}
	if waits == 0 {
		t.Fatalf("expected WaitForHandle calls for both adapters, calls=%v", calls)
	}
	if lastRegister > firstWait {
		t.Fatalf("scope registrations must complete before the first verify handshake: first WaitForHandle at %d, last RegisterScope at %d in %v", firstWait, lastRegister, calls)
	}

	provisionedCount := 0
	for _, ev := range sink.provisionEvents {
		if ev.Status == "provision_wanted" {
			provisionedCount++
		}
	}
	if provisionedCount != 2 {
		t.Errorf("expected 2 provision_wanted events, got %d (%v)", provisionedCount, sink.provisionEvents)
	}
}

// -----------------------------------------------------------------------------
// KB-232: Scope Session Pending Event Bridge
// -----------------------------------------------------------------------------

// kb232SessionWaitPending is the run identity the pending-event bridge must
// stamp: the runSink fallback path carries the engine's own run identity
// because the shim starts before Run wires the run-scoped sink.
func kb232SessionWaitPending() *remote.ScopeSessionPending {
	return &remote.ScopeSessionPending{
		AdapterType: "shell",
		Scope:       "run_handler/9c1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8",
		Window:      30 * time.Second,
		Waited:      15 * time.Second,
		Dials:       4,
		Rejections:  3,
	}
}

// TestScopeSessionEventBridge_EmitsSessionPendingEvent pins the engine-side
// translation (KB-232 item 3): a shim scope_session_pending signal becomes a
// structured AdapterLifecycleEvent with Status scope_session_pending, the
// scope split into scope_name / scope_instance_id, the environment name, and
// the dialed/rejected counts.
func TestScopeSessionEventBridge_EmitsSessionPendingEvent(t *testing.T) {
	tracker := &eventTrackingSink{}
	e := &Engine{runID: "kb232-bridge-run", sink: tracker}
	bridge := &scopeSessionEventBridge{engine: e, environmentName: "prod"}

	bridge.OnScopeSessionPending(kb232SessionWaitPending())

	evt, ok := tracker.firstStatus(ScopeSessionPendingStatus)
	if !ok {
		t.Fatal("bridge emitted no scope_session_pending event")
	}
	if evt.Status != "scope_session_pending" {
		t.Errorf("status: got %q want scope_session_pending", evt.Status)
	}
	if evt.RunID != "kb232-bridge-run" {
		t.Errorf("run_id: got %q want the engine run id", evt.RunID)
	}
	if evt.AdapterType != "shell" {
		t.Errorf("adapter_type: got %q want shell", evt.AdapterType)
	}
	if evt.ScopeName != "run_handler" {
		t.Errorf("scope_name: got %q want run_handler", evt.ScopeName)
	}
	if evt.ScopeInstanceID != "9c1a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8" {
		t.Errorf("scope_instance_id: got %q want the scope's instance id", evt.ScopeInstanceID)
	}
	if evt.EnvironmentName != "prod" {
		t.Errorf("environment_name: got %q want prod", evt.EnvironmentName)
	}
	if evt.SessionWaitSeconds != 15 {
		t.Errorf("session_wait_seconds: got %d want 15", evt.SessionWaitSeconds)
	}
	if evt.SessionDials != 4 {
		t.Errorf("session_dials: got %d want 4", evt.SessionDials)
	}
	if evt.SessionRejections != 3 {
		t.Errorf("session_rejections: got %d want 3", evt.SessionRejections)
	}
}

// TestScopeSessionEventBridge_RootScopeShapeFallback pins the empty-scope
// decode: the root workflow's "/<instance>" full scope key falls back to an
// empty scope_name with the instance id carried whole.
func TestScopeSessionEventBridge_RootScopeShapeFallback(t *testing.T) {
	tracker := &eventTrackingSink{}
	e := &Engine{runID: "kb232-root-run", sink: tracker}
	bridge := &scopeSessionEventBridge{engine: e, environmentName: "prod"}

	bridge.OnScopeSessionPending(&remote.ScopeSessionPending{
		AdapterType: "copilot",
		Scope:       "/0e1d2c3b-4a59-6877-8695-94938271605f",
		Window:      time.Minute,
		Waited:      31 * time.Second,
	})

	evt, ok := tracker.firstStatus(ScopeSessionPendingStatus)
	if !ok {
		t.Fatal("bridge emitted no scope_session_pending event")
	}
	if evt.ScopeName != "" {
		t.Errorf("scope_name: got %q want empty for the root scope shape", evt.ScopeName)
	}
	if evt.ScopeInstanceID != "0e1d2c3b-4a59-6877-8695-94938271605f" {
		t.Errorf("scope_instance_id: got %q want the instance id", evt.ScopeInstanceID)
	}
	if evt.AdapterType != "copilot" {
		t.Errorf("adapter_type: got %q want copilot", evt.AdapterType)
	}
}

// TestScopeSessionEventBridge_PrefersRunSink pins the sink precedence: when
// Run has wired the run-scoped sink the pending event rides it, not the
// engine-level fallback.
func TestScopeSessionEventBridge_PrefersRunSink(t *testing.T) {
	tracker := &eventTrackingSink{}
	e := &Engine{runID: "kb232-run-sink-run", sink: &fakeSink{}}
	runTracker := &eventTrackingSink{}
	e.runSink = runTracker
	bridge := &scopeSessionEventBridge{engine: e, environmentName: "prod"}

	bridge.OnScopeSessionPending(kb232SessionWaitPending())

	if _, ok := runTracker.firstStatus(ScopeSessionPendingStatus); !ok {
		t.Fatal("pending event missing from the run sink")
	}
	if tracker.hasStatus(ScopeSessionPendingStatus) {
		t.Error("pending event must not also land on the engine fallback sink")
	}
}

// TestEngine_ScopeSessionPendingEventWiredThroughShim proves the full wiring
// (KB-232 item 3): an engine-started per-scope shim, whose env names the event
// bridge at startRemoteShimForEnv time, delivers the half-window signal to the
// engine sink as a scope_session_pending lifecycle event while a session wait
// that never sees its adapter dials.
func TestEngine_ScopeSessionPendingEventWiredThroughShim(t *testing.T) {
	g := kb232PerScopeTwoAdapterGraph(t)
	dataDir := t.TempDir()
	eng := New(g, nil, &fakeSink{}, WithDataDir(dataDir))
	eng.runID = "kb232-wired-run"
	tracker := &eventTrackingSink{}
	eng.sink = tracker
	sessions := eng.newCRI293Sessions(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := eng.maybeStartRemoteShim(ctx, sessions); err != nil {
		t.Fatalf("maybeStartRemoteShim: %v", err)
	}
	t.Cleanup(func() {
		_ = sessions.Shutdown(context.WithoutCancel(ctx))
	})

	wrapped, ok := sessions.RemoteShimForEnv("remote.prod").(interface{ WrappedShim() *remote.Shim })
	if !ok {
		t.Fatal("per-scope session shim is not the remote shim this engine wires the event bridge into")
	}

	const scope = "kb-handler/7b6a5c4d-3e2f-4150-9283-a4b5c6d7e8f9"
	waitCtx, waitCancel := context.WithTimeout(context.WithoutCancel(ctx), 300*time.Millisecond)
	defer waitCancel()
	waitErr := make(chan error, 1)
	go func() {
		_, err := wrapped.WrappedShim().WaitForFreshHandle(waitCtx, "shell", scope, nil)
		waitErr <- err
	}()

	// The half-window signal fires on the waiting goroutine's wake timer;
	// poll the tracker until the translation lands or the wait fails.
	deadline := time.Now().Add(5 * time.Second)
	var evt AdapterLifecycleEvent
	var found bool
	for {
		evt, found = tracker.firstStatus(ScopeSessionPendingStatus)
		if found {
			break
		}
		select {
		case err := <-waitErr:
			t.Fatalf("session wait failed before the pending event arrived: %v", err)
		case now := <-time.After(5 * time.Millisecond):
			if now.After(deadline) {
				t.Fatal("engine never received the scope_session_pending event")
			}
		}
	}

	if evt.RunID != "kb232-wired-run" {
		t.Errorf("run_id: got %q want the engine run id (fallback path)", evt.RunID)
	}
	if evt.AdapterType != "shell" {
		t.Errorf("adapter_type: got %q want shell (from the awaited key)", evt.AdapterType)
	}
	if evt.ScopeName != "kb-handler" {
		t.Errorf("scope_name: got %q want kb-handler", evt.ScopeName)
	}
	if evt.ScopeInstanceID != "7b6a5c4d-3e2f-4150-9283-a4b5c6d7e8f9" {
		t.Errorf("scope_instance_id: got %q want the scope's instance id", evt.ScopeInstanceID)
	}
	if evt.EnvironmentName != "prod" {
		t.Errorf("environment_name: got %q want prod (the env that started the shim)", evt.EnvironmentName)
	}
	if evt.SessionDials != 0 || evt.SessionRejections != 0 {
		t.Errorf("counts: got dialed=%d rejected=%d want zeros (no adapter dialed)", evt.SessionDials, evt.SessionRejections)
	}
}

// TestEngine_ScopeSessionPendingStatusConstValue pins the status string the
// orchestrator keys its operator log on.
func TestEngine_ScopeSessionPendingStatusConstValue(t *testing.T) {
	if ScopeSessionPendingStatus != "scope_session_pending" {
		t.Fatalf("ScopeSessionPendingStatus = %q, want \"scope_session_pending\"", ScopeSessionPendingStatus)
	}
}

// -----------------------------------------------------------------------------
// KB-232: Slow Pod Acceptance Test
// -----------------------------------------------------------------------------

// kb232SlowPodAcceptanceHCL returns the parent workflow: a local warmup step,
// then the subworkflow step whose body owns the per-scope remote work.
func kb232SlowPodAcceptanceHCL() string {
	return `workflow {
  name          = "kb232-acceptance"
  version       = "0.1"
  initial_state = "warmup"
  target_state  = "done"

  # Widen the retry budget past the known verify→bind stale-handle window
  # (production shares this window; the pod redials on the same timescale).
  policy {
    max_step_retries = 50
  }
}

environment "remote" "prod" {
  listen_address     = "127.0.0.1:0"
  per_scope_sessions = true
}

environment "shell" "local" {}

adapter "noop" "default" {
  environment = shell.local
}

subworkflow "child" {
  source = "./child"
}

step "warmup" {
  target = adapter.noop.default
  outcome "success" { next = step.engage }
  outcome "error" { next = step.engage }
}

step "engage" {
  target = subworkflow.child
  outcome "success" { next = state.done }
  outcome "failure" { next = state.failed }
}

state "done" {
  terminal = true
  success  = true
}

state "failed" {
  terminal = true
  success  = false
}
`
}

// kb232SlowPodChildHCL returns the subworkflow body: per_scope_sessions on the
// shared remote environment with its own adapter key.
func kb232SlowPodChildHCL() string {
	return `workflow {
  name          = "kb232-child"
  version       = "0.1"
  initial_state = "setup_worktree"
  target_state  = "done"
}

environment "remote" "prod" {
  listen_address     = "127.0.0.1:0"
  per_scope_sessions = true
}

adapter "shell" "worktree" {
  environment = remote.prod
}

step "setup_worktree" {
  target = adapter.shell.worktree
  outcome "success" { next = state.done }
  outcome "failure" { next = state.failed }
}

state "done" {
  terminal = true
  success  = true
}

state "failed" {
  terminal = true
  success  = false
}
`
}

// kb232AcceptanceLockfile pins both shell adapter keys with the digest the
// engine's shim digest verifier accepts.
func kb232AcceptanceLockfile() *lockfile.Lockfile {
	return &lockfile.Lockfile{
		SchemaVersion: 1,
		Adapters: []lockfile.LockedAdapter{
			{
				Type:               "shell",
				Name:               "worktree",
				Reference:          "ghcr.io/brokenbots/criteria-adapter-shell",
				ResolvedDigest:     pinDigest,
				SourceURL:          "https://github.com/brokenbots/criteria",
				SDKProtocolVersion: 2,
				Platforms:          []string{"linux/amd64"},
			},
			{
				Type:               "shell",
				Name:               "default",
				Reference:          "ghcr.io/brokenbots/criteria-adapter-shell",
				ResolvedDigest:     pinDigest,
				SourceURL:          "https://github.com/brokenbots/criteria",
				SDKProtocolVersion: 2,
				Platforms:          []string{"linux/amd64"},
			},
		},
	}
}

// TestKB232_SubworkflowPerScopeSessionEstablishesWithSlowPodStart is the
// KB-232 acceptance test: a parent run reaches its sub-workflow, whose
// per-scope adapter session is satisfied by a pod that takes five seconds to
// come up and start dialing.
func TestKB232_SubworkflowPerScopeSessionEstablishesWithSlowPodStart(t *testing.T) {
	if !canDialLoopbackTCP() {
		t.Skip("skipping test: loopback TCP dialing is restricted in current environment")
	}
	if testing.Short() {
		t.Skip("skipping slow acceptance test in -short mode")
	}
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "main.chcl"), kb232SlowPodAcceptanceHCL())
	writeFile(t, filepath.Join(root, "child", "main.chcl"), kb232SlowPodChildHCL())
	writeLockfile(t, root, kb232AcceptanceLockfile())
	g := compileWorkflowDir(t, root)

	beforeDirs := map[string]struct{}{}
	if matches, err := filepath.Glob(filepath.Join(os.TempDir(), "criteria-remote-*")); err == nil {
		for _, m := range matches {
			beforeDirs[m] = struct{}{}
		}
	}

	sink := &eventTrackingSink{}
	e := NewTestEngine(g, &fakeLoader{adapters: map[string]adapterhost.Handle{
		"noop": &fakeAdapter{name: "noop", outcome: "success"},
	}}, sink,
		WithWorkflowDir(g.WorkflowDir),
		WithDataDir(root),
		WithRunID("kb232-acceptance"))

	executions := &atomic.Int64{}
	pod := &cri145Pod{executions: executions}
	stopPod := make(chan struct{})
	go kb232DelayedPodServeLoop(sink, 5*time.Second, pod, stopPod)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(func() {
		close(stopPod)
		cancel()
		waitForCri137ShimTeardown(t, beforeDirs)
	})

	started := time.Now()
	runErr := make(chan error, 1)
	go func() { runErr <- e.Run(ctx) }()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(70 * time.Second):
		t.Fatalf("run did not finish in time; terminal=%q", sink.terminal)
	}
	if got := sink.terminal; got != "done" {
		t.Fatalf("terminal state = %q (ok=%v, failure=%q), want done", got, sink.terminalOK, sink.failure)
	}
	if !sink.terminalOK {
		t.Fatalf("run did not succeed (failure=%q)", sink.failure)
	}
	if elapsed := time.Since(started); elapsed > 30*time.Second {
		t.Errorf("run took %s for a 5s-pod-delay workflow; a session deadline burn would look like this", elapsed)
	}

	if got := executions.Load(); got < 1 {
		t.Fatalf("adapter Execute calls = %d, want at least 1 (the child step ran over the session)", got)
	}
	provisions, _ := cri145LifecycleStats(sink)
	var childProvision *AdapterLifecycleEvent
	for i := range provisions {
		if provisions[i].ScopeName != "" {
			childProvision = &provisions[i]
			break
		}
	}
	if childProvision == nil {
		t.Fatalf("no per-scope provision for the child's session (%d provisions: %+v)", len(provisions), provisions)
	}
	if childProvision.ScopeName != "engage" {
		t.Errorf("child provision scope_name = %q, want the subworkflow step name \"engage\"", childProvision.ScopeName)
	}
	if childProvision.RunID != "kb232-acceptance" {
		t.Errorf("child provision run_id = %q, want kb232-acceptance", childProvision.RunID)
	}

	if tracker := sink; tracker.hasStatus(ScopeSessionPendingStatus) {
		t.Error("scope_session_pending emitted for a run whose session established well inside the window")
	}
}

// kb232DelayedPodServeLoop parks the pod for startDelay, then hands over
// to the standard redial loop.
func kb232DelayedPodServeLoop(sink *eventTrackingSink, startDelay time.Duration, pod *cri145Pod, stop <-chan struct{}) {
	select {
	case <-time.After(startDelay):
	case <-stop:
		return
	}
	pod.serveLoop(sink, stop)
}
