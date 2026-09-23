package engine

// cri304_repro_test.go — CRI-304 crash-resume convergence gaps:
//
// (a) A resumed run whose init fails before the run loop (criteria-version
// gate, variable seeding, shim bind, adapter provisioning of the resumed
// step) must publish run.failed so the run record reaches a terminal status
// instead of staying "running" forever (the CRI-302 re-run left castle run
// 64fa8bd9 status=running after an engine-side resume failure).
//
// (b) A fresh replay that follows a checkpoint-consuming resume must adopt
// the prior invocation's surviving per-scope adapter instances instead of
// rotating fresh tokens that the still-running prior pods can never
// re-handshake with, which wedged the CRI-302 re-run for the shim's full 5m
// verify budget. Adoption re-handshakes those pods to the new run's shim.

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

// TestRun_InitFailurePublishesRunFailed_CRI304 covers the item-(a) contract
// on the fresh-run path: when shim startup fails (here: a fixed port occupied
// by a foreign process), the engine publishes run.failed through the sink
// before returning, so the run record reaches a terminal status even though
// no step ever ran.
func TestRun_InitFailurePublishesRunFailed_CRI304(t *testing.T) {
	foreign, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("foreign listener: %v", err)
	}
	defer foreign.Close()
	declared := foreign.Addr().String()

	g := &workflow.FSMGraph{
		PinSet: &lockfile.Lockfile{
			Adapters: []lockfile.LockedAdapter{
				{Type: "noop", ResolvedDigest: "sha256:abcd1234"},
			},
		},
		Environments: map[string]*workflow.EnvironmentNode{
			"remote.primary": cri293Env(t, "primary", declared),
		},
	}
	sink := &fakeSink{}
	eng := New(g, nil, sink, WithLocalShimIsolation())

	runErr := eng.Run(context.Background())
	if runErr == nil {
		t.Fatal("Run on an occupied fixed port: want bind error, got nil")
	}
	if !strings.Contains(runErr.Error(), "bind: address already in use") {
		t.Errorf("Run error = %q, want bind failure naming the occupied address", runErr.Error())
	}
	if sink.failure == "" {
		t.Fatal("Run failed without publishing run.failed; the run record would stay running forever (CRI-302 re-run regression)")
	}
	if !strings.Contains(sink.failure, "bind: address already in use") {
		t.Errorf("run.failed reason = %q, want the underlying bind failure", sink.failure)
	}
}

// TestRunFrom_InitFailurePublishesRunFailed_CRI304 covers the item-(a)
// contract on the resumed path: RunFrom publishes run.failed naming the
// interrupted step when its init fails before the run loop.
func TestRunFrom_InitFailurePublishesRunFailed_CRI304(t *testing.T) {
	foreign, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("foreign listener: %v", err)
	}
	defer foreign.Close()
	declared := foreign.Addr().String()

	g := &workflow.FSMGraph{
		PinSet: &lockfile.Lockfile{
			Adapters: []lockfile.LockedAdapter{
				{Type: "noop", ResolvedDigest: "sha256:abcd1234"},
			},
		},
		Environments: map[string]*workflow.EnvironmentNode{
			"remote.primary": cri293Env(t, "primary", declared),
		},
	}
	sink := &fakeSink{}
	eng := New(g, nil, sink, WithLocalShimIsolation())

	runErr := eng.RunFrom(context.Background(), "work", 1)
	if runErr == nil {
		t.Fatal("RunFrom on an occupied fixed port: want bind error, got nil")
	}
	if sink.failure == "" {
		t.Fatal("RunFrom failed without publishing run.failed; a resumed run would stay non-terminal forever (CRI-302 re-run regression)")
	}
}

// TestInitScopeAdapters_PerScope_AdoptsPriorRunScopeInstance_CRI304 covers
// the item-(b) contract: a fresh replay (own data dir, nothing reusable in
// it) adopts the surviving per-scope instance of a prior invocation's data
// dir — same scope instance ID, token self-healed into the replay's own dir,
// shim registered with the adopted token so the prior pods can re-handshake
// immediately, and the prior instance claimed so no later replay can
// double-adopt it.
func TestInitScopeAdapters_PerScope_AdoptsPriorRunScopeInstance_CRI304(t *testing.T) {
	ctx := context.Background()
	g := perScopeRemoteGraph(t)
	priorDir := t.TempDir()
	ownDir := t.TempDir()

	// Prior invocation: rotates a fresh instance and persists record + token.
	priorSink, _, priorRLC, priorDeps := newPerScopeTestHarness(t, g, priorDir)
	if _, err := initScopeAdapters(ctx, g, priorDeps, nil, priorDir, "", nil, priorRLC); err != nil {
		t.Fatalf("prior initScopeAdapters: %v", err)
	}
	first, ok := priorSink.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("prior init emitted no provision_wanted event")
	}
	priorToken, err := os.ReadFile(first.TokenRef)
	if err != nil {
		t.Fatalf("read prior token: %v", err)
	}

	// Fresh replay: brand-new data dir, no checkpoint consumed, but the prior
	// invocation's surviving per-scope pods are still up and adoptable.
	ownSink, ownShim, ownRLC, ownDeps := newPerScopeTestHarness(t, g, ownDir)
	ownRLC.scopeLifecycle.adoptableRunDirs = []string{priorDir}
	if _, err := initScopeAdapters(ctx, g, ownDeps, nil, ownDir, "", nil, ownRLC); err != nil {
		t.Fatalf("replay initScopeAdapters: %v", err)
	}
	adopted, ok := ownSink.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("replay init emitted no provision_wanted event")
	}
	if adopted.ScopeInstanceID != first.ScopeInstanceID {
		t.Fatalf("replay ScopeInstanceID = %q, want adopted %q from the prior invocation", adopted.ScopeInstanceID, first.ScopeInstanceID)
	}
	if got := string(priorToken); got == "" {
		t.Fatal("prior token must not be empty")
	}
	if got := ownShim.registered[adopted.ScopeName+"/"+adopted.ScopeInstanceID]; got != string(priorToken) {
		t.Errorf("shim registered token %q, want adopted prior token %q", got, string(priorToken))
	}

	// The adopted token must live in the replay's OWN data dir so the run's
	// own teardown bookkeeping covers it.
	if !strings.HasPrefix(adopted.TokenRef, ownDir) {
		t.Errorf("adopted TokenRef = %q, want a path under the replay's own data dir %q", adopted.TokenRef, ownDir)
	}
	healedToken, err := os.ReadFile(adopted.TokenRef)
	if err != nil {
		t.Fatalf("read self-healed token: %v", err)
	}
	if string(healedToken) != string(priorToken) {
		t.Errorf("self-healed token = %q, want the adopted prior token %q", string(healedToken), string(priorToken))
	}
	ownRec, err := readCurrentScopeInstance(ownDir, adopted.ScopeName, "noop.default")
	if err != nil {
		t.Fatalf("read replay's current scope record: %v", err)
	}
	if ownRec.ScopeInstanceID != first.ScopeInstanceID || ownRec.AdapterType != "noop" {
		t.Errorf("replay's current scope record = %+v, want instance %q type noop", ownRec, first.ScopeInstanceID)
	}

	// The prior instance must be claimed in the prior dir: the record is
	// released and a tombstone keeps the token out of every later scan.
	if _, err := os.Stat(filepath.Join(priorDir, "remote-tokens", "current", "released-"+first.ScopeInstanceID+".json")); err != nil {
		t.Fatalf("prior adoption claim tombstone missing: %v", err)
	}
	if _, err := readCurrentScopeInstance(priorDir, first.ScopeName, "noop.default"); err == nil {
		t.Error("prior live record still present after adoption; a later replay could re-adopt a bound instance")
	}
}

// TestInitScopeAdapters_PerScope_AdoptionSkipsClaimedPriorInstance_CRI304
// pins the exclusion rule: a prior instance that a prior run already
// released (or that another adapter claims) must never be re-adopted — the
// replay rotates a fresh instance exactly as it would with nothing
// adoptable.
func TestInitScopeAdapters_PerScope_AdoptionSkipsClaimedPriorInstance_CRI304(t *testing.T) {
	ctx := context.Background()
	g := perScopeRemoteGraph(t)
	priorDir := t.TempDir()
	ownDir := t.TempDir()

	// Seed a prior dir with a rotated token whose instance is already
	// claimed: a release tombstone in the prior dir's current records.
	scopeInstanceID := uuid.NewString()
	if _, err := writeRotatedToken(priorDir, "", scopeInstanceID, "noop", "prior-token"); err != nil {
		t.Fatalf("seed prior token: %v", err)
	}
	if err := writeCurrentScopeInstance(priorDir, "", "released-"+scopeInstanceID, currentScopeInstanceRecord{
		ScopeInstanceID: scopeInstanceID,
		AdapterType:     "noop",
	}); err != nil {
		t.Fatalf("seed prior claim: %v", err)
	}

	ownSink, ownShim, ownRLC, ownDeps := newPerScopeTestHarness(t, g, ownDir)
	ownRLC.scopeLifecycle.adoptableRunDirs = []string{priorDir}
	if _, err := initScopeAdapters(ctx, g, ownDeps, nil, ownDir, "", nil, ownRLC); err != nil {
		t.Fatalf("replay initScopeAdapters: %v", err)
	}
	fresh, ok := ownSink.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("replay init emitted no provision_wanted event")
	}
	if fresh.ScopeInstanceID == scopeInstanceID {
		t.Fatal("replay adopted a claimed prior instance; claimed tokens must never be re-adopted")
	}
	for scope, tok := range ownShim.registered {
		if tok == "prior-token" {
			t.Errorf("replay registered the claimed prior token under %q; claimed tokens must never be re-adopted", scope)
		}
	}
	if !strings.HasPrefix(fresh.TokenRef, ownDir) {
		t.Errorf("fresh TokenRef = %q, want a path under the replay's own data dir %q", fresh.TokenRef, ownDir)
	}
}

// TestInitScopeAdapters_PerScope_AdoptionTeardownReleasesOwnCopy_CRI304
// verifies the adopted instance is released through the replay's own record
// and tombstoned in its own dir at teardown, so a run that terminates while
// holding an adopted instance cannot leave it re-adoptable while its pods
// are still up.
func TestInitScopeAdapters_PerScope_AdoptionTeardownReleasesOwnCopy_CRI304(t *testing.T) {
	ctx := context.Background()
	g := perScopeRemoteGraph(t)
	priorDir := t.TempDir()
	ownDir := t.TempDir()

	priorSink, _, priorRLC, priorDeps := newPerScopeTestHarness(t, g, priorDir)
	if _, err := initScopeAdapters(ctx, g, priorDeps, nil, priorDir, "", nil, priorRLC); err != nil {
		t.Fatalf("prior initScopeAdapters: %v", err)
	}
	first, ok := priorSink.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("prior init emitted no provision_wanted event")
	}

	ownSink, _, ownRLC, ownDeps := newPerScopeTestHarness(t, g, ownDir)
	ownRLC.scopeLifecycle.adoptableRunDirs = []string{priorDir}
	ownOrder, err := initScopeAdapters(ctx, g, ownDeps, nil, ownDir, "", nil, ownRLC)
	if err != nil {
		t.Fatalf("replay initScopeAdapters: %v", err)
	}
	if _, ok := ownSink.firstStatus("provision_wanted"); !ok {
		t.Fatal("replay init emitted no provision_wanted event")
	}

	// Replay teardown: LIFO release through the replay's own record.
	tearDownScopeAdapters(ctx, ownOrder, ownDeps, ownRLC)
	replayEvent, ok := ownSink.firstStatus("released")
	if !ok {
		t.Fatal("replay teardown emitted no released event")
	}
	if replayEvent.ScopeInstanceID != first.ScopeInstanceID {
		t.Errorf("released ScopeInstanceID = %q, want adopted %q", replayEvent.ScopeInstanceID, first.ScopeInstanceID)
	}
	if _, err := os.Stat(filepath.Join(ownDir, "remote-tokens", "current", "released-"+first.ScopeInstanceID+".json")); err != nil {
		t.Fatalf("replay's release tombstone missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ownDir, "remote-tokens", "current", "noop.default.json")); err == nil {
		t.Error("replay's live record survived teardown; the adopted instance must be released in the owning run")
	}
}