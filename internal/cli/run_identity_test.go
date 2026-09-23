package cli

// Unit tests for the CRI-304 invocation-identity marker and the prior-run
// adoption enumeration. The engine-side adoption mechanism (token
// self-healing, claim tombstones, shim registration) is covered by the
// engine package's cri304_repro_test.go; these tests cover the CLI layer:
// marker persistence, the adoptable-directory filter (own run excluded,
// in-flight checkpoints excluded, fingerprint mismatch excluded, newest
// first), and the engine-option wiring.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	applytest "github.com/brokenbots/criteria/internal/cli/applytest"
	"github.com/brokenbots/criteria/internal/engine"
	servertrans "github.com/brokenbots/criteria/internal/transport/server"
	"github.com/brokenbots/criteria/workflow"
)

func writeInvocationMarker(t *testing.T, runID, fingerprint string, recordedAt time.Time) {
	t.Helper()
	d, err := stateDir()
	if err != nil {
		t.Fatalf("stateDir: %v", err)
	}
	dataDir := filepath.Join(d, "runs", runID)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	raw, err := json.Marshal(invocationIdentityMarker{
		Fingerprint: fingerprint,
		RecordedAt:  recordedAt,
	})
	if err != nil {
		t.Fatalf("marshal marker: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, invocationIdentityMarkerName), raw, 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
}

func TestInvocationIdentityMarker_RoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", home)

	dataDir := filepath.Join(home, "runs", "run-1")
	if err := writeInvocationIdentityMarker(dataDir, "fp-1"); err != nil {
		t.Fatalf("writeInvocationIdentityMarker: %v", err)
	}
	info, err := os.Stat(filepath.Join(dataDir, invocationIdentityMarkerName))
	if err != nil {
		t.Fatalf("marker file missing: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("marker file permissions = %o, want 0o600", info.Mode().Perm())
	}
	marker, err := readInvocationIdentityMarker(dataDir)
	if err != nil {
		t.Fatalf("readInvocationIdentityMarker: %v", err)
	}
	if marker.Fingerprint != "fp-1" {
		t.Errorf("marker fingerprint = %q, want fp-1", marker.Fingerprint)
	}
	if marker.RecordedAt.IsZero() {
		t.Error("marker recorded_at must be persisted")
	}
}

func TestWriteInvocationIdentityMarker_EmptyFingerprintIsNoOp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", home)

	dataDir := filepath.Join(home, "runs", "run-1")
	if err := writeInvocationIdentityMarker(dataDir, ""); err != nil {
		t.Fatalf("writeInvocationIdentityMarker with empty fingerprint: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, invocationIdentityMarkerName)); !os.IsNotExist(err) {
		t.Errorf("marker file must not exist for a fingerprintless invocation, stat err = %v", err)
	}
}

func TestAdoptablePriorRunDirs_FiltersAndOrders(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", home)
	base := time.Now().UTC()

	const fp = "fp-aaa"
	writeInvocationMarker(t, "match-new", fp, base.Add(2*time.Minute))
	writeInvocationMarker(t, "match-old", fp, base)
	writeInvocationMarker(t, "match-own", fp, base.Add(3*time.Minute))
	writeInvocationMarker(t, "match-inflight", fp, base.Add(4*time.Minute))
	writeInvocationMarker(t, "mismatch", "fp-other", base.Add(5*time.Minute))
	// A same-fingerprint directory without a marker (pre-CRI-304 run) and one
	// with a corrupt marker are never adoptable.
	if err := os.MkdirAll(filepath.Join(home, "runs", "no-marker"), 0o700); err != nil {
		t.Fatalf("mkdir no-marker: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(home, "runs", "corrupt-marker"), 0o700); err != nil {
		t.Fatalf("mkdir corrupt-marker: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, "runs", "corrupt-marker", invocationIdentityMarkerName), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write corrupt marker: %v", err)
	}
	// The in-flight run owns a crash-recovery checkpoint and must be excluded.
	if err := os.WriteFile(filepath.Join(home, "runs", "match-inflight.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write in-flight checkpoint: %v", err)
	}

	dirs, err := adoptablePriorRunDirs(fp, "match-own")
	if err != nil {
		t.Fatalf("adoptablePriorRunDirs: %v", err)
	}
	want := []string{
		filepath.Join(home, "runs", "match-new"),
		filepath.Join(home, "runs", "match-old"),
	}
	if len(dirs) != len(want) {
		t.Fatalf("adoptablePriorRunDirs = %v, want %v", dirs, want)
	}
	for i, w := range want {
		if dirs[i] != w {
			t.Errorf("adoptablePriorRunDirs[%d] = %q, want %q (newest marker first)", i, dirs[i], w)
		}
	}
}

func TestAdoptablePriorRunDirs_EmptyFingerprintYieldsNil(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", home)
	writeInvocationMarker(t, "run-1", "fp-1", time.Now().UTC())

	dirs, err := adoptablePriorRunDirs("", "")
	if err != nil {
		t.Fatalf("adoptablePriorRunDirs with empty fingerprint: %v", err)
	}
	if dirs != nil {
		t.Errorf("adoptablePriorRunDirs with empty fingerprint = %v, want nil", dirs)
	}
}

func TestAdoptablePriorRunDirs_MissingRunsDirYieldsNil(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", home)

	dirs, err := adoptablePriorRunDirs("fp-1", "")
	if err != nil {
		t.Fatalf("adoptablePriorRunDirs without runs dir: %v", err)
	}
	if dirs != nil {
		t.Errorf("adoptablePriorRunDirs without runs dir = %v, want nil", dirs)
	}
}

func TestEngineAdoptionOptions_WiresMarkerAndOption(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", home)
	writeInvocationMarker(t, "prior-1", "fp-1", time.Now().UTC())

	dataDir := filepath.Join(home, "runs", "own-1")
	opts := engineAdoptionOptions(dataDir, "fp-1", "own-1")
	if len(opts) != 1 {
		t.Fatalf("engineAdoptionOptions returned %d options, want 1 WithAdoptableRunDirs", len(opts))
	}
	if _, err := os.Stat(filepath.Join(dataDir, invocationIdentityMarkerName)); err != nil {
		t.Fatalf("marker not persisted by engineAdoptionOptions: %v", err)
	}

	eng := engine.New(&workflow.FSMGraph{}, nil, nil, opts...)
	if eng == nil {
		t.Fatal("engine.New rejected the adoption options")
	}
}

func TestEngineAdoptionOptions_NoPriorRunsYieldsNoOptions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", home)

	dataDir := filepath.Join(home, "runs", "own-1")
	opts := engineAdoptionOptions(dataDir, "fp-1", "own-1")
	if len(opts) != 0 {
		t.Fatalf("engineAdoptionOptions with nothing adoptable returned %d options, want 0", len(opts))
	}
	// The marker must still be recorded so a later replay can adopt THIS run.
	if _, err := os.Stat(filepath.Join(dataDir, invocationIdentityMarkerName)); err != nil {
		t.Fatalf("marker not persisted by engineAdoptionOptions: %v", err)
	}
}

// TestExecuteServerRunPerScopeSessionsAdoptsPriorRunToken_CRI304 covers the
// full item-(b) wiring through executeServerRun: a fresh server-mode run
// whose fingerprint matches a prior invocation's marker adopts the prior
// run's surviving per-scope token (self-healed into its own data dir)
// instead of rotating a fresh one the prior pods could never re-handshake.
func TestExecuteServerRunPerScopeSessionsAdoptsPriorRunToken_CRI304(t *testing.T) {
	requireNoGoroutineLeak(t)
	home := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", home)

	fake := applytest.New(t)
	wfPath := writeScopeSessionWorkflow(t, scopeSessionWorkflowHCL)
	opts := applyOptions{workflowPath: wfPath, serverURL: fake.URL()}
	fingerprint := runIdentityFingerprint(opts.workflowPath, opts.serverURL, opts.varFiles, opts.varOverrides)
	if fingerprint == "" {
		t.Fatal("runIdentityFingerprint returned empty")
	}

	// Seed the prior invocation's run data dir: a surviving rotated token
	// with its current record (the shape the engine persists) and the
	// invocation-identity marker matching this replay's fingerprint.
	priorScopeInstanceID := "11111111-2222-3333-4444-555555555555"
	priorToken := "cri304-prior-adopted-token"
	priorDataDir := filepath.Join(home, "runs", "cri304-prior")
	if err := os.MkdirAll(filepath.Join(priorDataDir, "remote-tokens", "current"), 0o700); err != nil {
		t.Fatalf("mkdir prior record dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(priorDataDir, "remote-tokens", "current", "noop.default.json"),
		[]byte(`{"scope_instance_id":"`+priorScopeInstanceID+`","adapter_type":"noop"}`), 0o600); err != nil {
		t.Fatalf("write prior record: %v", err)
	}
	priorTokenPath := filepath.Join(priorDataDir, "remote-tokens", priorScopeInstanceID, "noop.token")
	if err := os.MkdirAll(filepath.Dir(priorTokenPath), 0o700); err != nil {
		t.Fatalf("mkdir prior token dir: %v", err)
	}
	if err := os.WriteFile(priorTokenPath, []byte(priorToken), 0o600); err != nil {
		t.Fatalf("write prior token: %v", err)
	}
	if err := writeInvocationIdentityMarker(priorDataDir, fingerprint); err != nil {
		t.Fatalf("write prior marker: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	log := discardLogger()
	src, graph, loader, err := compileForExecution(ctx, wfPath, log, false, false)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	defer func() { _ = loader.Shutdown(context.WithoutCancel(ctx)) }()

	copts := servertrans.Options{TLSMode: servertrans.TLSDisable}
	client, runID, resumed, _, err := setupServerRun(ctx, log, graph, src, fake.URL(), "cri304-adopt", &copts, cancel, nil, fingerprint)
	if err != nil {
		t.Fatalf("setupServerRun: %v", err)
	}
	if resumed {
		t.Fatal("setupServerRun unexpectedly resumed a matching checkpoint")
	}
	defer client.Close()

	state := newLocalRunState(runID, graph.Name, fake.URL())
	errCh := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		errCh <- executeServerRun(ctx, log, loader, client, state, graph, opts, nil)
		close(done)
	}()

	token := waitForRotatedToken(t, home, done, 8*time.Second)
	time.Sleep(300 * time.Millisecond) // let the engine park on the session bind
	cancel()
	runErr := <-errCh
	if runErr == nil {
		t.Fatal("expected the server-mode per-scope run to block without phone-home")
	}
	requireNotGateError(t, runErr)
	if token == "" {
		t.Fatalf("token files under %s: none rotated before the run ended (%v)", runDataDirPath(t, runID), runErr)
	}

	tokens := tokenFilesUnder(t, home, runID)
	if len(tokens) != 1 {
		t.Fatalf("token files under %s: got %v, want exactly one (the adopted copy)", runDataDirPath(t, runID), tokens)
	}
	healed, err := os.ReadFile(tokens[0])
	if err != nil {
		t.Fatalf("read adopted token: %v", err)
	}
	if string(healed) != priorToken {
		t.Errorf("run token = %q, want adopted prior token %q; the fresh run rotated fresh instead of adopting", string(healed), priorToken)
	}
	if !strings.Contains(tokens[0], priorScopeInstanceID) {
		t.Errorf("token path = %q, want the adopted prior scope instance %q", tokens[0], priorScopeInstanceID)
	}
}
