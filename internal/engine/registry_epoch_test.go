package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	engineruntime "github.com/brokenbots/criteria/internal/engine/runtime"
)

func mustMarker(t *testing.T, dir string) registryEpochRecord {
	t.Helper()
	rec, ok := readScopeRegistryEpoch(dir)
	if !ok {
		t.Fatalf("no registry epoch marker found under %s", dir)
	}
	return rec
}

// TestScopeRegistryEpoch_MarkerLifecycle pins the marker lifecycle: every
// invocation that initializes scope sessions opens a new registry epoch,
// completing the invocation seals the current epoch with its terminal
// outcome, and a later invocation resumes the epoch count with a fresh open
// state. Sealing is idempotent.
func TestScopeRegistryEpoch_MarkerLifecycle(t *testing.T) {
	dir := t.TempDir()

	if _, ok := readScopeRegistryEpoch(dir); ok {
		t.Fatal("directory without a marker must not report one")
	}
	if PriorRunRegistrySealed(dir) {
		t.Fatal("directory without a marker must never be sealed")
	}

	epoch, err := bumpScopeRegistryEpoch(dir)
	if err != nil {
		t.Fatalf("first bump: %v", err)
	}
	if epoch != 1 {
		t.Fatalf("first bump epoch = %d, want 1", epoch)
	}
	rec := mustMarker(t, dir)
	if rec.RegistryEpoch != 1 || rec.State != RegistryStateOpen {
		t.Errorf("marker after first bump = %+v, want epoch 1 open", rec)
	}
	if PriorRunRegistrySealed(dir) {
		t.Error("open marker must not be reported sealed")
	}

	epoch, err = bumpScopeRegistryEpoch(dir)
	if err != nil {
		t.Fatalf("second bump: %v", err)
	}
	if epoch != 2 {
		t.Fatalf("second bump epoch = %d, want 2", epoch)
	}

	// The invocation completes at the awaiting_human gate (the KB-227
	// incident shape): seal with the named terminal state and success bit.
	if err := sealScopeRegistryEpoch(dir, AwaitingHumanTerminalState, false); err != nil {
		t.Fatalf("seal: %v", err)
	}
	rec = mustMarker(t, dir)
	if rec.State != RegistryStateSealed || rec.RegistryEpoch != 2 {
		t.Errorf("marker after seal = %+v, want sealed with epoch preserved (2)", rec)
	}
	if rec.FinalState != AwaitingHumanTerminalState || rec.Success == nil || *rec.Success {
		t.Errorf("sealed marker final_state/success = %q/%v, want awaiting_human/false", rec.FinalState, rec.Success)
	}
	if !PriorRunRegistrySealed(dir) {
		t.Error("sealed marker must be reported sealed")
	}

	// Resealing is a no-op: the terminal outcome of record must survive.
	if err := sealScopeRegistryEpoch(dir, "other", true); err != nil {
		t.Fatalf("re-seal: %v", err)
	}
	if rec = mustMarker(t, dir); rec.FinalState != AwaitingHumanTerminalState || rec.Success == nil || *rec.Success {
		t.Errorf("re-seal mutated the sealed marker: %+v", rec)
	}

	// A later invocation reopens the registry and continues the epoch count.
	epoch, err = bumpScopeRegistryEpoch(dir)
	if err != nil {
		t.Fatalf("post-seal bump: %v", err)
	}
	if epoch != 3 {
		t.Fatalf("post-seal bump epoch = %d, want 3", epoch)
	}
	if rec = mustMarker(t, dir); rec.State != RegistryStateOpen || rec.FinalState != "" || rec.Success != nil {
		t.Errorf("marker after reopening = %+v, want open with no terminal outcome", rec)
	}
	if PriorRunRegistrySealed(dir) {
		t.Error("reopened marker must not be reported sealed")
	}

	// Writes are atomic: no staging file may remain.
	entries, err := os.ReadDir(filepath.Join(dir, "remote-tokens"))
	if err != nil {
		t.Fatalf("read remote-tokens dir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("staging file %q left behind by marker write", e.Name())
		}
	}
}

// TestScopeRegistryEpoch_CorruptMarkerStaysAdoptable pins the backward
// compatibility rule: a directory with a corrupt marker behaves like one
// without a marker (legacy or pre-marker invocation) and stays adoptable.
func TestScopeRegistryEpoch_CorruptMarkerStaysAdoptable(t *testing.T) {
	dir := t.TempDir()
	path := registryEpochPath(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir remote-tokens: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write corrupt marker: %v", err)
	}

	if _, ok := readScopeRegistryEpoch(dir); ok {
		t.Error("corrupt marker must not be reported as readable")
	}
	if PriorRunRegistrySealed(dir) {
		t.Error("corrupt marker must not be reported sealed")
	}
	// Bumping recovers by seeding a fresh epoch over the corrupt record.
	epoch, err := bumpScopeRegistryEpoch(dir)
	if err != nil {
		t.Fatalf("bump over corrupt marker: %v", err)
	}
	if epoch != 1 {
		t.Errorf("bump over corrupt marker epoch = %d, want 1", epoch)
	}
}

// TestEngine_MarksRegistryOpenOnInitAndSealsOnTerminalCompletion_KB227 wires
// both engine call sites end to end: initializing the run opens a registry
// epoch in the run's data dir, and a run completing at a named terminal
// state seals it with that state and success bit — so a successor invocation
// refuses to adopt the completed invocation's rotated scope tokens.
func TestEngine_MarksRegistryOpenOnInitAndSealsOnTerminalCompletion_KB227(t *testing.T) {
	ctx := context.Background()
	g := compile(t, `
workflow {
  name = "kb227-seal"
  version = "0.1"
  initial_state = "done"
  target_state  = "done"
}

state "done" {
  terminal = true
  success  = true
}`)

	dir := t.TempDir()
	sink := &fakeSink{}
	eng := New(g, &fakeLoader{}, sink, WithDataDir(dir), WithRunID("kb-227"))
	if err := eng.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if termState, completed := sink.terminalState(); !completed || termState != "done" {
		t.Fatalf("run did not complete at done: terminal=%q failure=%q", termState, sink.failure)
	}

	// initAdapters bumped the epoch; the terminal completion sealed it.
	rec := mustMarker(t, dir)
	if rec.State != RegistryStateSealed {
		t.Errorf("marker state after terminal completion = %q, want sealed", rec.State)
	}
	if rec.RegistryEpoch != 1 {
		t.Errorf("marker epoch after terminal completion = %d, want 1", rec.RegistryEpoch)
	}
	if rec.FinalState != "done" || rec.Success == nil || !*rec.Success {
		t.Errorf("sealed marker = %+v, want final_state done success true", rec)
	}
	if !PriorRunRegistrySealed(dir) {
		t.Error("completed invocation's data dir must be reported sealed")
	}
}

// TestEngine_FailureSealsRegistryButCanceledKeepsItOpen_KB227 pins the
// handleEvalError seal semantics: a run failure with a live context is a
// terminal run record (its pod fleet is released), so the registry is sealed;
// a canceled context is a stop — the run stays resumable and the registry
// stays open so its tokens remain adoption-recoverable.
func TestEngine_FailureSealsRegistryButCanceledKeepsItOpen_KB227(t *testing.T) {
	g := compile(t, `
workflow {
  name = "kb227-fail"
  version = "0.1"
  initial_state = "done"
  target_state  = "done"
}

state "done" {
  terminal = true
  success  = true
}`)

	t.Run("failure seals", func(t *testing.T) {
		dir := t.TempDir()
		sink := &fakeSink{}
		eng := New(g, &fakeLoader{}, sink, WithDataDir(dir), WithRunID("kb-227-fail"))
		eng.markScopeRegistryOpen()
		if _, ok := readScopeRegistryEpoch(dir); !ok {
			t.Fatal("open marker missing after markScopeRegistryOpen")
		}

		st := &RunState{Current: "done"}
		if err := eng.handleEvalError(context.Background(), st, errors.New("boom"), sink); err == nil {
			t.Fatal("handleEvalError must propagate the failure")
		}
		if sink.failure == "" {
			t.Error("OnRunFailed not observed")
		}

		rec := mustMarker(t, dir)
		if rec.State != RegistryStateSealed {
			t.Errorf("marker state after run failure = %q, want sealed", rec.State)
		}
		if rec.FinalState != "" || rec.Success == nil || *rec.Success {
			t.Errorf("sealed marker after run failure = %+v, want no final state and success false", rec)
		}
	})

	t.Run("canceled context keeps registry open", func(t *testing.T) {
		dir := t.TempDir()
		sink := &fakeSink{}
		eng := New(g, &fakeLoader{}, sink, WithDataDir(dir), WithRunID("kb-227-stop"))
		eng.markScopeRegistryOpen()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		st := &RunState{Current: "done"}
		if err := eng.handleEvalError(ctx, st, errors.New("boom"), sink); err == nil {
			t.Fatal("handleEvalError must propagate the failure")
		}

		rec := mustMarker(t, dir)
		if rec.State != RegistryStateOpen {
			t.Errorf("marker state after canceled run = %q, want open (run stays resumable)", rec.State)
		}
		if PriorRunRegistrySealed(dir) {
			t.Error("stopped run's registry must stay adoptable")
		}
	})

	t.Run("paused keeps registry open", func(t *testing.T) {
		dir := t.TempDir()
		sink := &fakeSink{}
		eng := New(g, &fakeLoader{}, sink, WithDataDir(dir), WithRunID("kb-227-pause"))
		eng.markScopeRegistryOpen()

		st := &RunState{Current: "wait-somewhere"}
		if err := eng.handleEvalError(context.Background(), st, engineruntime.ErrPaused, sink); err != nil {
			t.Fatalf("handleEvalError(paused): %v", err)
		}

		rec := mustMarker(t, dir)
		if rec.State != RegistryStateOpen {
			t.Errorf("marker state after paused run = %q, want open", rec.State)
		}
	})
}

// TestInitScopeAdapters_PerScope_AdoptionRefusesSealedPriorRegistry_KB227 is
// the regression test for the incident: process 2 crash-resumed by ADOPTING
// process 1's rotated scope tokens after process 1 completed at
// awaiting_human. The fresh shim registry had no rows for those scopes, so
// every recreated pod dialed with stale claims and looped on scope-not-
// registered until the verify budget expired, restarting the cycle. Now the
// completed invocation seals its registry, and adopting a sealed prior
// directory must rotate a fresh scope instance instead — with a fresh token
// the shim actually serves — without failing the run.
func TestInitScopeAdapters_PerScope_AdoptionRefusesSealedPriorRegistry_KB227(t *testing.T) {
	ctx := context.Background()
	g := perScopeRemoteGraph(t)
	priorDir := t.TempDir()
	ownDir := t.TempDir()

	// Prior invocation: opens its registry epoch, rotates an instance, and
	// completes at the awaiting_human gate, which seals the registry.
	if _, err := bumpScopeRegistryEpoch(priorDir); err != nil {
		t.Fatalf("prior bump: %v", err)
	}
	priorSink, _, priorRLC, priorDeps := newPerScopeTestHarness(t, g, priorDir)
	if _, err := initScopeAdapters(ctx, g, priorDeps, nil, priorDir, "", nil, priorRLC); err != nil {
		t.Fatalf("prior initScopeAdapters: %v", err)
	}
	prior, ok := priorSink.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("prior init emitted no provision_wanted event")
	}
	priorTokenRaw, err := os.ReadFile(prior.TokenRef)
	if err != nil {
		t.Fatalf("read prior token: %v", err)
	}
	priorToken := string(priorTokenRaw)
	if err := sealScopeRegistryEpoch(priorDir, AwaitingHumanTerminalState, false); err != nil {
		t.Fatalf("seal prior registry: %v", err)
	}
	if !PriorRunRegistrySealed(priorDir) {
		t.Fatal("prior dir must be sealed for this test to exercise the gate")
	}

	// Successor invocation (crash-resumed process 2): fresh data dir and
	// shim registry, but the prior dir is adoptable on paper.
	if _, err := bumpScopeRegistryEpoch(ownDir); err != nil {
		t.Fatalf("own bump: %v", err)
	}
	ownSink, ownShim, ownRLC, ownDeps := newPerScopeTestHarness(t, g, ownDir)
	ownRLC.scopeLifecycle.adoptableRunDirs = []string{priorDir}
	order, err := initScopeAdapters(ctx, g, ownDeps, nil, ownDir, "", nil, ownRLC)
	if err != nil {
		t.Fatalf("successor initScopeAdapters must proceed, got error: %v", err)
	}
	if len(order) != 1 || order[0] != "noop.default" {
		t.Fatalf("expected order [noop.default], got %v", order)
	}

	fresh, ok := ownSink.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("successor init emitted no provision_wanted event")
	}
	if fresh.ScopeInstanceID == prior.ScopeInstanceID {
		t.Errorf("successor adopted the sealed prior instance %q; must rotate a fresh scope instance", fresh.ScopeInstanceID)
	}
	if got := ownShim.registered[fresh.ScopeName+"/"+fresh.ScopeInstanceID]; got == priorToken {
		t.Error("successor shim registered the stale prior token; fresh rotation must serve a fresh token")
	}
	for key, tok := range ownShim.registered {
		if tok == priorToken {
			t.Errorf("sealed prior token re-registered under %q; the fresh registry must have no rows for it", key)
		}
	}

	// Refusal happens before any claim: the prior dir's live record and
	// token must survive untouched for diagnosis and later manual cleanup.
	if _, err := readCurrentScopeInstance(priorDir, prior.ScopeName, "noop.default"); err != nil {
		t.Errorf("sealed prior dir's live record was consumed by the refusal: %v", err)
	}
	if _, err := os.Stat(prior.TokenRef); err != nil {
		t.Errorf("sealed prior dir's token file was consumed by the refusal: %v", err)
	}
}

// TestInitScopeAdapters_PerScope_AdoptionContinuesWithOpenPriorRegistry_KB227
// pins the non-regression half: a prior invocation that did NOT complete
// (open registry epoch — a crash or stop) stays adoptable exactly as in
// CRI-304, marker or no marker.
func TestInitScopeAdapters_PerScope_AdoptionContinuesWithOpenPriorRegistry_KB227(t *testing.T) {
	ctx := context.Background()
	g := perScopeRemoteGraph(t)
	priorDir := t.TempDir()
	ownDir := t.TempDir()

	// Prior invocation crashed before completing: registry epoch is open.
	if _, err := bumpScopeRegistryEpoch(priorDir); err != nil {
		t.Fatalf("prior bump: %v", err)
	}
	priorSink, _, priorRLC, priorDeps := newPerScopeTestHarness(t, g, priorDir)
	if _, err := initScopeAdapters(ctx, g, priorDeps, nil, priorDir, "", nil, priorRLC); err != nil {
		t.Fatalf("prior initScopeAdapters: %v", err)
	}
	prior, ok := priorSink.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("prior init emitted no provision_wanted event")
	}
	if PriorRunRegistrySealed(priorDir) {
		t.Fatal("open prior dir must not be reported sealed")
	}

	// The successor invocation still adopts the surviving instance.
	if _, err := bumpScopeRegistryEpoch(ownDir); err != nil {
		t.Fatalf("own bump: %v", err)
	}
	ownSink, ownShim, ownRLC, ownDeps := newPerScopeTestHarness(t, g, ownDir)
	ownRLC.scopeLifecycle.adoptableRunDirs = []string{priorDir}
	if _, err := initScopeAdapters(ctx, g, ownDeps, nil, ownDir, "", nil, ownRLC); err != nil {
		t.Fatalf("successor initScopeAdapters: %v", err)
	}
	adopted, ok := ownSink.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("successor init emitted no provision_wanted event")
	}
	if adopted.ScopeInstanceID != prior.ScopeInstanceID {
		t.Fatalf("successor ScopeInstanceID = %q, want adopted %q from the crashed prior invocation", adopted.ScopeInstanceID, prior.ScopeInstanceID)
	}
	priorToken, err := os.ReadFile(prior.TokenRef)
	if err != nil {
		t.Fatalf("read prior token: %v", err)
	}
	if got := ownShim.registered[adopted.ScopeName+"/"+adopted.ScopeInstanceID]; got != string(priorToken) {
		t.Errorf("shim registered token %q, want the adopted prior token %q", got, string(priorToken))
	}
}
