package engine

// Regression tests for CRI-137: a runner-pod restart mid-run used to rotate a
// fresh scope instance + accept token, so surviving adapter pods holding the
// pre-restart token could never re-handshake and the run wedged on the shim
// accept loop forever. The engine now persists the current scope instance per
// adapter and reuses it on re-entry; when the record is missing (first
// restart after this code deploys) or corrupt, it scans the scope's token
// directory for the previous runner's rotated tokens and reuses the newest
// unclaimed instance. A fresh rotation remains the fallback when nothing
// reusable survives, and deliberate teardown releases both record and token.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter/environment/remote"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// newPerScopeTestHarness wires a fresh SessionManager, fake shim and lifecycle
// state against a shared dataDir, mirroring what a restarted runner sees (the
// data dir persists across restarts; in-memory state does not).
func newPerScopeTestHarness(t *testing.T, g *workflow.FSMGraph, dataDir string) (*eventTrackingSink, *fakeRemoteShim, *remoteLifecycleContext, Deps) {
	t.Helper()

	sessions := adapterhost.NewSessionManager(&fakeLoader{})
	sessions.SetGraph(g)
	shim := newFakeRemoteShim(&fakeRemoteHandle{})
	sessions.SetRemoteShim(shim)

	lifecycle := newScopeLifecycleState(dataDir)
	lifecycle.setRunID("run-123")
	sink := &eventTrackingSink{}
	rlc := &remoteLifecycleContext{scopeLifecycle: lifecycle}
	deps := Deps{Sessions: sessions, Sink: sink}
	return sink, shim, rlc, deps
}

func TestInitScopeAdapters_PerScope_ReusesScopeInstanceAfterRestart(t *testing.T) {
	ctx := context.Background()
	g := perScopeRemoteGraph(t)
	dataDir := t.TempDir()

	// First engine instance: initial scope entry rotates a fresh token.
	sink1, _, rlc1, deps1 := newPerScopeTestHarness(t, g, dataDir)
	if _, err := initScopeAdapters(ctx, g, deps1, nil, dataDir, "", nil, rlc1); err != nil {
		t.Fatalf("first initScopeAdapters: %v", err)
	}
	first, ok := sink1.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("first init emitted no provision_wanted event")
	}

	// The persisted record must be restricted like the token material.
	if info, err := os.Stat(filepath.Join(dataDir, "remote-tokens", "current")); err != nil {
		t.Fatalf("stat record dir: %v", err)
	} else if info.Mode().Perm() != 0o700 {
		t.Errorf("record dir permissions = %o, want 0o700", info.Mode().Perm())
	}
	if info, err := os.Stat(filepath.Join(dataDir, "remote-tokens", "current", "noop.default.json")); err != nil {
		t.Fatalf("stat record file: %v", err)
	} else if info.Mode().Perm() != 0o600 {
		t.Errorf("record file permissions = %o, want 0o600", info.Mode().Perm())
	}

	// Simulated runner restart: brand-new in-memory state, same dataDir, but
	// this time with a REAL shim over loopback so the re-handshake is
	// observable end to end instead of simulated by a fake.
	beforeDirs := map[string]struct{}{}
	if matches, err := filepath.Glob(filepath.Join(os.TempDir(), "criteria-remote-*")); err == nil {
		for _, m := range matches {
			beforeDirs[m] = struct{}{}
		}
	}
	realShim, err := remote.NewShim(&remote.Config{ListenAddress: "127.0.0.1:0", PerScopeSessions: true}, nil)
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

	sessions2 := adapterhost.NewSessionManager(&fakeLoader{})
	sessions2.SetGraph(g)
	sessions2.SetRemoteShim(realShim)
	sink2 := &eventTrackingSink{}
	lifecycle2 := newScopeLifecycleState(dataDir)
	lifecycle2.setRunID("run-123")
	rlc2 := &remoteLifecycleContext{scopeLifecycle: lifecycle2}
	deps2 := Deps{Sessions: sessions2, Sink: sink2}

	// Surviving adapter pod: dials the restarted runner's shim with its
	// PRE-restart scope key and token on a retry loop, exactly as the adapter
	// runner does while the runner is restarting. The handshake can only
	// succeed once the restarted engine re-registers the surviving scope.
	tokenBytes, err := os.ReadFile(first.TokenRef)
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	preRestartScopeKey := first.ScopeName + "/" + first.ScopeInstanceID
	hs := &cri137Handshake{
		Name:    "noop",
		Version: "1.0.0",
		Digest:  "sha256:abcd1234",
		Token:   string(tokenBytes),
		Scope:   preRestartScopeKey,
	}
	stopDial := make(chan struct{})
	var infoCalls atomic.Int64
	go dialCri137AdapterLoop(realShim.ListenAddr(), hs, stopDial, &infoCalls)
	defer close(stopDial)

	if _, err := initScopeAdapters(ctx, g, deps2, nil, dataDir, "", nil, rlc2); err != nil {
		t.Fatalf("post-restart initScopeAdapters: %v", err)
	}
	// Verify drives a real Info RPC through the accepted handshake, so a
	// non-zero count proves the surviving pod's pre-restart scope key + token
	// were re-verified by the restarted shim end to end. Without instance
	// reuse the shim would reject every dial with the pre-restart key and the
	// count would stay zero.
	if infoCalls.Load() == 0 {
		t.Fatalf("surviving pod's pre-restart scope key %q never completed a verified handshake on the restarted shim; the re-handshake was not observable", preRestartScopeKey)
	}

	second, ok := sink2.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("post-restart init emitted no provision_wanted event")
	}
	if second.ScopeInstanceID != first.ScopeInstanceID {
		t.Fatalf("post-restart ScopeInstanceID = %q, want reused %q", second.ScopeInstanceID, first.ScopeInstanceID)
	}
	if second.TokenRef != first.TokenRef {
		t.Fatalf("post-restart TokenRef = %q, want reused %q", second.TokenRef, first.TokenRef)
	}
	if got := string(tokenBytes); got == "" {
		t.Fatal("token file must not be empty")
	}
}

// waitForCri137ShimTeardown blocks until the shim's per-session UDS socket
// directories (os.MkdirTemp("", "criteria-remote-*")) created during this
// test are gone, so the async session teardown has fully settled before the
// engine package's goroutine-leak detector runs. Dirs already present before
// the test started (e.g. from a previous crashed run) are excluded via the
// baseline so they cannot stall this wait.
func waitForCri137ShimTeardown(t *testing.T, before map[string]struct{}) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		matches, err := filepath.Glob(filepath.Join(os.TempDir(), "criteria-remote-*"))
		if err != nil {
			return
		}
		fresh := 0
		for _, m := range matches {
			if _, stale := before[m]; !stale {
				fresh++
			}
		}
		if fresh == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Logf("%d shim socket dirs still present after teardown", fresh)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// cri137Handshake mirrors the shim's pre-gRPC identity frame. The field set
// and JSON keys must stay byte-compatible with the shim's wire format; this
// local copy pins the contract from the outside.
type cri137Handshake struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Digest  string `json:"digest"`
	Token   string `json:"token"`
	Scope   string `json:"scope,omitempty"`
}

// dialCri137AdapterLoop repeatedly dials the shim with the surviving pod's
// handshake frame until stop is closed or a handshake is accepted, mimicking
// the adapter reconnect loop while the runner restarts.
func dialCri137AdapterLoop(addr string, hs *cri137Handshake, stop <-chan struct{}, infoCalls *atomic.Int64) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		if dialCri137Adapter(addr, hs, infoCalls) {
			return
		}
		select {
		case <-stop:
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// dialCri137Adapter connects to the shim, sends the handshake frame, and
// serves a minimal gRPC adapter on the connection — the same shape a real
// adapter runner presents after its phone-home handshake. It reports whether
// the shim accepted the handshake, observed as bridge traffic arriving on the
// adapter connection (the shim's reattach client speaks HTTP/2 toward the
// adapter): a rejected handshake closes the connection with no traffic.
func dialCri137Adapter(addr string, hs *cri137Handshake, infoCalls *atomic.Int64) bool {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return false
	}
	hsBytes, err := json.Marshal(hs)
	if err != nil {
		_ = conn.Close()
		return false
	}
	if _, err := conn.Write(append(hsBytes, '\n')); err != nil {
		_ = conn.Close()
		return false
	}
	observed := &countingConn{Conn: conn}
	grpcServer := grpc.NewServer()
	v2.RegisterAdapterServiceServer(grpcServer, &cri137AdapterServer{name: hs.Name, version: hs.Version, infoCalls: infoCalls})
	go func() { _ = grpcServer.Serve(&cri137SingleConnListener{conn: observed}) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if observed.received.Load() {
			return true
		}
		if observed.closed.Load() {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// countingConn records the first byte the gRPC server reads (the shim's
// post-handshake bridge traffic) and the first read error (the shim closing a
// rejected connection), so the dialer can tell accepted handshakes from
// rejected ones without competing with the gRPC server for bytes.
type countingConn struct {
	net.Conn
	received atomic.Bool
	closed   atomic.Bool
}

func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.received.Store(true)
	}
	if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
		c.closed.Store(true)
	}
	return n, err
}

// cri137AdapterServer implements a minimal v2.AdapterServiceServer with the
// capability surface the host expects from a real adapter. infoCalls counts
// Info RPCs the host drove through the shim's bridge, giving the engine test
// a direct observable for a completed, verified re-handshake.
type cri137AdapterServer struct {
	v2.UnimplementedAdapterServiceServer
	name      string
	version   string
	infoCalls *atomic.Int64
}

func (f *cri137AdapterServer) Info(ctx context.Context, req *v2.InfoRequest) (*v2.InfoResponse, error) {
	if f.infoCalls != nil {
		f.infoCalls.Add(1)
	}
	return &v2.InfoResponse{Name: f.name, Version: f.version, Capabilities: []string{"execute"}}, nil
}

func (f *cri137AdapterServer) OpenSession(ctx context.Context, req *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	return &v2.OpenSessionResponse{}, nil
}

// cri137SingleConnListener serves exactly one connection (the shim's
// post-handshake gRPC bridge) and then EOFs like a closed listener.
type cri137SingleConnListener struct {
	conn net.Conn
	mu   sync.Mutex
	done bool
}

func (l *cri137SingleConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done {
		return nil, io.EOF
	}
	l.done = true
	return l.conn, nil
}

func (l *cri137SingleConnListener) Close() error   { return nil }
func (l *cri137SingleConnListener) Addr() net.Addr { return l.conn.LocalAddr() }

func TestInitScopeAdapters_PerScope_FreshRotationAfterRelease(t *testing.T) {
	ctx := context.Background()
	g := perScopeRemoteGraph(t)
	dataDir := t.TempDir()

	sink1, _, rlc1, deps1 := newPerScopeTestHarness(t, g, dataDir)
	order, err := initScopeAdapters(ctx, g, deps1, nil, dataDir, "", nil, rlc1)
	if err != nil {
		t.Fatalf("first initScopeAdapters: %v", err)
	}
	first, ok := sink1.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("first init emitted no provision_wanted event")
	}

	// Deliberate teardown (pause, body exit) releases the scope: the live
	// record is dropped and a tombstone claim keeps the released instance out
	// of token-file scan reuse, while the token file itself is kept for
	// post-run forensics.
	tearDownScopeAdapters(ctx, order, deps1, rlc1)
	if _, err := os.Stat(filepath.Join(dataDir, "remote-tokens", "current", "noop.default.json")); !os.IsNotExist(err) {
		t.Fatalf("live record still present after release: %v", err)
	}
	rawClaim, err := os.ReadFile(filepath.Join(dataDir, "remote-tokens", "current", "released-"+first.ScopeInstanceID+".json"))
	if err != nil {
		t.Fatalf("read released claim: %v", err)
	}
	var claim currentScopeInstanceRecord
	if err := json.Unmarshal(rawClaim, &claim); err != nil {
		t.Fatalf("released claim is not valid JSON: %v", err)
	}
	if claim.ScopeInstanceID != first.ScopeInstanceID || claim.AdapterType != "noop" {
		t.Errorf("released claim = %+v, want tombstone for instance %q", claim, first.ScopeInstanceID)
	}
	if _, err := os.Stat(first.TokenRef); err != nil {
		t.Fatalf("released token file must be kept for forensics: %v", err)
	}

	sink2, _, rlc2, deps2 := newPerScopeTestHarness(t, g, dataDir)
	if _, err := initScopeAdapters(ctx, g, deps2, nil, dataDir, "", nil, rlc2); err != nil {
		t.Fatalf("post-release initScopeAdapters: %v", err)
	}
	second, ok := sink2.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("post-release init emitted no provision_wanted event")
	}
	if second.ScopeInstanceID == first.ScopeInstanceID {
		t.Fatalf("post-release ScopeInstanceID = %q, want a fresh rotation after release", second.ScopeInstanceID)
	}
	if second.TokenRef == first.TokenRef {
		t.Fatalf("post-release TokenRef = %q, want a fresh token file after release", second.TokenRef)
	}
}

func TestInitScopeAdapters_PerScope_CorruptRecordRecoversInstanceByTokenScan(t *testing.T) {
	ctx := context.Background()
	g := perScopeRemoteGraph(t)
	dataDir := t.TempDir()

	sink1, _, rlc1, deps1 := newPerScopeTestHarness(t, g, dataDir)
	if _, err := initScopeAdapters(ctx, g, deps1, nil, dataDir, "", nil, rlc1); err != nil {
		t.Fatalf("first initScopeAdapters: %v", err)
	}
	first, ok := sink1.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("first init emitted no provision_wanted event")
	}

	recordPath := filepath.Join(dataDir, "remote-tokens", "current", "noop.default.json")
	if err := os.WriteFile(recordPath, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("corrupt record: %v", err)
	}

	// Restart with a corrupt record but an intact rotated token file: the
	// surviving pod still holds that token, so the engine must recover the
	// same instance via the token-file scan instead of rotating a fresh one.
	sink2, _, rlc2, deps2 := newPerScopeTestHarness(t, g, dataDir)
	if _, err := initScopeAdapters(ctx, g, deps2, nil, dataDir, "", nil, rlc2); err != nil {
		t.Fatalf("post-corruption initScopeAdapters: %v", err)
	}
	second, ok := sink2.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("post-corruption init emitted no provision_wanted event")
	}
	if second.ScopeInstanceID != first.ScopeInstanceID {
		t.Fatalf("post-corruption ScopeInstanceID = %q, want scan-reused %q", second.ScopeInstanceID, first.ScopeInstanceID)
	}
	if second.TokenRef != first.TokenRef {
		t.Fatalf("post-corruption TokenRef = %q, want reused %q", second.TokenRef, first.TokenRef)
	}
	// The scan path must self-heal the record for the next restart.
	raw, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("read healed record: %v", err)
	}
	var rec currentScopeInstanceRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("healed record is not valid JSON: %v", err)
	}
	if rec.ScopeInstanceID != first.ScopeInstanceID || rec.AdapterType != "noop" {
		t.Errorf("healed record = %+v, want instance %q for type noop", rec, first.ScopeInstanceID)
	}
}

// TestInitScopeAdapters_PerScope_ScanReusesTokensWithoutRecord covers the
// first-restart-after-deploy transition: the previous runner wrote only
// rotated token files (records did not exist yet), so the engine must scan
// the scope's token directory, register every surviving token, and reuse the
// instance instead of rotating a fresh one.
func TestInitScopeAdapters_PerScope_ScanReusesTokensWithoutRecord(t *testing.T) {
	ctx := context.Background()
	g := perScopeRemoteGraph(t)
	dataDir := t.TempDir()

	// Hand-craft the pre-upgrade on-disk state: one rotated token, no record.
	scopeInstanceID := uuid.NewString()
	legacyToken := "legacy-surviving-token"
	tokenPath, err := writeRotatedToken(dataDir, "", scopeInstanceID, "noop", legacyToken)
	if err != nil {
		t.Fatalf("write legacy token: %v", err)
	}
	scopeKey := "/" + scopeInstanceID

	sink, shim, rlc, deps := newPerScopeTestHarness(t, g, dataDir)
	if _, err := initScopeAdapters(ctx, g, deps, nil, dataDir, "", nil, rlc); err != nil {
		t.Fatalf("initScopeAdapters: %v", err)
	}
	event, ok := sink.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("init emitted no provision_wanted event")
	}
	if event.ScopeInstanceID != scopeInstanceID {
		t.Fatalf("ScopeInstanceID = %q, want scanned %q", event.ScopeInstanceID, scopeInstanceID)
	}
	if event.TokenRef != tokenPath {
		t.Fatalf("TokenRef = %q, want reused %q", event.TokenRef, tokenPath)
	}
	if got := shim.registeredToken(scopeKey); got != legacyToken {
		t.Errorf("shim registered token %q, want the surviving token %q", got, legacyToken)
	}
	// The scanned instance must be healed into a current record.
	rec, recErr := readCurrentScopeInstance(dataDir, "", "noop.default")
	if recErr != nil {
		t.Fatalf("record not written after scan reuse: %v", recErr)
	}
	if rec.ScopeInstanceID != scopeInstanceID || rec.AdapterType != "noop" {
		t.Errorf("healed record = %+v, want instance %q for type noop", rec, scopeInstanceID)
	}
}

func TestInitScopeAdapters_PerScope_MissingTokenFileFallsBackToFreshRotation(t *testing.T) {
	ctx := context.Background()
	g := perScopeRemoteGraph(t)
	dataDir := t.TempDir()

	sink1, _, rlc1, deps1 := newPerScopeTestHarness(t, g, dataDir)
	if _, err := initScopeAdapters(ctx, g, deps1, nil, dataDir, "", nil, rlc1); err != nil {
		t.Fatalf("first initScopeAdapters: %v", err)
	}
	first, ok := sink1.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("first init emitted no provision_wanted event")
	}
	if err := os.Remove(first.TokenRef); err != nil {
		t.Fatalf("remove token file: %v", err)
	}

	sink2, _, rlc2, deps2 := newPerScopeTestHarness(t, g, dataDir)
	if _, err := initScopeAdapters(ctx, g, deps2, nil, dataDir, "", nil, rlc2); err != nil {
		t.Fatalf("post-removal initScopeAdapters: %v", err)
	}
	second, ok := sink2.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("post-removal init emitted no provision_wanted event")
	}
	if second.ScopeInstanceID == first.ScopeInstanceID {
		t.Fatalf("post-removal ScopeInstanceID = %q, want a fresh rotation", second.ScopeInstanceID)
	}
}

// TestInitScopeAdapters_VerifyFailureSurfacesAcceptTokenError covers the
// engine-side half of the CRI-137 fail-fast: when the shim gives up waiting
// (stale pods can never verify), Verify fails and the engine must surface the
// accept_token error through init_failed and return it so the run fails
// terminally instead of retrying forever.
func TestInitScopeAdapters_VerifyFailureSurfacesAcceptTokenError(t *testing.T) {
	ctx := context.Background()
	g := perScopeRemoteGraph(t)
	dataDir := t.TempDir()

	sessions := adapterhost.NewSessionManager(&fakeLoader{})
	sessions.SetGraph(g)
	shim := &failingRemoteShim{fakeRemoteShim: newFakeRemoteShim(&fakeRemoteHandle{}), waitErr: errors.New(`remote adapter "noop" session wait for scope "" exceeded 5m0s without a successful identity handshake: last identity rejection for scope "": accept_token verification failed for scope ""; stale adapter pod holding a pre-rotation accept token? (CRI-137)`)}
	sessions.SetRemoteShim(shim)

	lifecycle := newScopeLifecycleState(dataDir)
	lifecycle.setRunID("run-123")
	sink := &eventTrackingSink{}
	rlc := &remoteLifecycleContext{scopeLifecycle: lifecycle}
	deps := Deps{Sessions: sessions, Sink: sink}

	_, err := initScopeAdapters(ctx, g, deps, nil, dataDir, "", nil, rlc)
	if err == nil {
		t.Fatal("expected initScopeAdapters to fail when the shim wait fails")
	}
	if !strings.Contains(err.Error(), "accept_token verification failed for scope") {
		t.Errorf("error must surface the accept_token failure, got: %v", err)
	}
	initFailed := false
	for _, s := range sink.lifecycleStatuses {
		if strings.HasSuffix(s, ":init_failed") {
			initFailed = true
		}
	}
	if !initFailed {
		t.Errorf("expected init_failed lifecycle event, got %v", sink.lifecycleStatuses)
	}
}

// failingRemoteShim behaves like fakeRemoteShim but fails the initial
// WaitForHandle, as the real shim does when the CRI-137 failure bound trips.
type failingRemoteShim struct {
	*fakeRemoteShim
	waitErr error
}

func (f *failingRemoteShim) WaitForHandle(_ context.Context, adapterType, scope string) (adapterhost.Handle, error) {
	f.record(fmt.Sprintf("WaitForHandle:%s:%s", adapterType, scope))
	return nil, f.waitErr
}

func (f *failingRemoteShim) WaitForFreshHandle(_ context.Context, adapterType, scope string, _ adapterhost.Handle) (adapterhost.Handle, error) {
	f.record(fmt.Sprintf("WaitForFreshHandle:%s:%s", adapterType, scope))
	return nil, f.waitErr
}

// --- unit tests for the path builders and the token-file scan ---

func TestRotatedTokenPath_RejectsTraversalLabels(t *testing.T) {
	dataDir := t.TempDir()
	for _, scope := range []string{"../escape", "a/b", "sub\\dir", "a..b"} {
		_, err := rotatedTokenPath(dataDir, scope, "00000000-0000-0000-0000-000000000000", "noop")
		if err == nil {
			t.Errorf("scope %q accepted as a token path component", scope)
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("scope %q:", scope)) {
			t.Errorf("error for scope %q must name the offending label, got: %v", scope, err)
		}
	}
	_, err := rotatedTokenPath(dataDir, "", "../../escape", "noop")
	if err == nil {
		t.Fatal("traversal scope instance ID accepted")
	}
	if !strings.Contains(err.Error(), `scope instance "../../escape":`) {
		t.Errorf("error must name the offending scope instance label, got: %v", err)
	}
	if _, err := rotatedTokenPath(dataDir, "", "00000000-0000-0000-0000-000000000000", "../type"); err == nil {
		t.Error("traversal adapter type accepted")
	}
}

func TestCurrentScopeInstancePath_RejectsTraversalLabels(t *testing.T) {
	dataDir := t.TempDir()
	for _, instance := range []string{"../escape", "a/b"} {
		_, err := currentScopeInstancePath(dataDir, "", instance)
		if err == nil {
			t.Errorf("adapter instance %q accepted as a record path component", instance)
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("adapter instance %q:", instance)) {
			t.Errorf("error for adapter instance %q must name the offending label, got: %v", instance, err)
		}
	}
}

func TestWriteRotatedToken_UsesCheckedPathBuilder(t *testing.T) {
	dataDir := t.TempDir()

	// The writer must enforce the same label rules as the readers, so a bad
	// label cannot drift past validation on the write side.
	if _, err := writeRotatedToken(dataDir, "", "../escape", "noop", "tok"); err == nil {
		t.Fatal("writeRotatedToken accepted a traversal scope instance ID")
	}
	if _, err := writeRotatedToken(dataDir, "../escape", "00000000-0000-0000-0000-000000000000", "noop", "tok"); err == nil {
		t.Fatal("writeRotatedToken accepted a traversal scope name")
	}

	// A valid write must land exactly where rotatedTokenPath derives it, so
	// readers and the writer can never disagree about the layout.
	instanceID := "11111111-2222-3333-4444-555555555555"
	path, err := writeRotatedToken(dataDir, "", instanceID, "noop", "tok")
	if err != nil {
		t.Fatalf("writeRotatedToken: %v", err)
	}
	want, err := rotatedTokenPath(dataDir, "", instanceID, "noop")
	if err != nil {
		t.Fatalf("rotatedTokenPath: %v", err)
	}
	if path != want {
		t.Errorf("written path %q does not match the derived path %q", path, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat written token: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("token file permissions = %o, want 0o600", info.Mode().Perm())
	}
}

func TestScanScopeTokenFiles_IgnoresNonInstanceEntries(t *testing.T) {
	dataDir := t.TempDir()

	// Two legitimate instance dirs with tokens, plus entries the scan must
	// ignore: the record dir, a non-UUID dir, a stray file, and an instance
	// dir whose token belongs to another adapter type.
	instA := "11111111-2222-3333-4444-555555555555"
	instB := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	if _, err := writeRotatedToken(dataDir, "", instA, "noop", "token-a"); err != nil {
		t.Fatalf("write token-a: %v", err)
	}
	if _, err := writeRotatedToken(dataDir, "", instB, "noop", "token-b"); err != nil {
		t.Fatalf("write token-b: %v", err)
	}
	scopeDir := filepath.Join(dataDir, "remote-tokens", "")
	if err := os.MkdirAll(filepath.Join(scopeDir, "current"), 0o700); err != nil {
		t.Fatalf("mkdir current: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(scopeDir, "not-a-uuid"), 0o700); err != nil {
		t.Fatalf("mkdir non-uuid: %v", err)
	}
	if err := os.WriteFile(filepath.Join(scopeDir, "stray.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write stray: %v", err)
	}
	if _, err := writeRotatedToken(dataDir, "", instA, "mcp", "mcp-token"); err != nil {
		t.Fatalf("write mcp token: %v", err)
	}

	found, err := scanScopeTokenFiles(dataDir, "", "noop")
	if err != nil {
		t.Fatalf("scanScopeTokenFiles: %v", err)
	}
	if len(found) != 2 {
		t.Fatalf("scan found %d candidates, want 2: %+v", len(found), found)
	}
	gotIDs := map[string]string{}
	for _, cand := range found {
		gotIDs[cand.instanceID] = cand.token
	}
	if gotIDs[instA] != "token-a" || gotIDs[instB] != "token-b" {
		t.Errorf("scan result = %v, want tokens for both instances", gotIDs)
	}

	// A missing token directory is an empty result, not an error.
	empty, err := scanScopeTokenFiles(dataDir, "other-scope", "noop")
	if err != nil {
		t.Fatalf("scan of missing scope dir: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("scan of missing scope dir returned %d candidates, want 0", len(empty))
	}
}

func TestFilterUnclaimedScopeInstances_ExcludesOtherAdaptersInstances(t *testing.T) {
	dataDir := t.TempDir()

	// instA is claimed by another adapter's healthy record; instB is free.
	instA := "11111111-2222-3333-4444-555555555555"
	instB := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	if _, err := writeRotatedToken(dataDir, "", instA, "noop", "token-a"); err != nil {
		t.Fatalf("write token-a: %v", err)
	}
	if _, err := writeRotatedToken(dataDir, "", instB, "noop", "token-b"); err != nil {
		t.Fatalf("write token-b: %v", err)
	}
	if err := writeCurrentScopeInstance(dataDir, "", "mcp.default", currentScopeInstanceRecord{
		ScopeInstanceID: instA,
		AdapterType:     "mcp",
	}); err != nil {
		t.Fatalf("write claiming record: %v", err)
	}

	found, err := scanScopeTokenFiles(dataDir, "", "noop")
	if err != nil {
		t.Fatalf("scanScopeTokenFiles: %v", err)
	}
	kept := filterUnclaimedScopeInstances(dataDir, "", found)
	if len(kept) != 1 || kept[0].instanceID != instB {
		t.Fatalf("filter kept %+v, want only the unclaimed instance %q", kept, instB)
	}
}
