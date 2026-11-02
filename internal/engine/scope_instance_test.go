package engine

// scope_instance_test.go — regression and lifecycle tests for remote scope instances:
//
// 1. CRI-137: Scope instance reuse and token scanning recovery across runner restarts.
//    A runner-pod restart mid-run reuses the persisted scope instance per adapter;
//    when the record is missing or corrupt, it scans the scope's token directory
//    for the previous runner's rotated tokens.
//
// 2. CRI-145: Parent/child scope sharing for subworkflows.
//    A subworkflow body that re-declares the same adapter as its parent scope skips
//    duplicate initialization via the SessionOpen guard, emitting exactly one
//    provision_wanted and one paired released event.
//
// 3. CRI-236: Wire token delivery and transition-window verification.
//    The per-scope adapter accept token rides the wire inside the provision/accept
//    handshake; token_ref stays as a transition-window compatibility surface.
//    Replay after restart re-handshakes the same token.
//
// 4. CRI-304: Crash-resume convergence and surviving instance adoption.
//    (a) Run and RunFrom publish run.failed if init fails before the run loop.
//    (b) A fresh replay adopts surviving per-scope adapter instances from prior
//        run directories instead of rotating fresh unmatchable tokens.

import (
	"bytes"
	"context"
	"encoding/hex"
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
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/testutil"
	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

// -----------------------------------------------------------------------------
// Test Scaffolding & Harness
// -----------------------------------------------------------------------------

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

type cri137Handshake struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Digest  string `json:"digest"`
	Token   string `json:"token"`
	Scope   string `json:"scope,omitempty"`
}

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

func canDialLoopbackTCP() bool {
	return testutil.CanDialLoopbackTCP()
}

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

// Token assertions (CRI-236)
func requireWireTokenShape(t *testing.T, ev *AdapterLifecycleEvent) {
	t.Helper()
	if ev.Token == "" {
		t.Fatalf("provision_wanted for %q/%q carries no wire token", ev.ScopeName, ev.ScopeInstanceID)
	}
	raw, err := hex.DecodeString(ev.Token)
	if err != nil {
		t.Fatalf("wire token %q is not hex: %v", ev.Token, err)
	}
	if len(raw) != 32 {
		t.Fatalf("wire token decodes to %d bytes, want 32", len(raw))
	}
}

func requireShimRegisteredWireToken(t *testing.T, shim *fakeRemoteShim, ev *AdapterLifecycleEvent) {
	t.Helper()
	requireWireTokenShape(t, ev)
	scopeKey := ev.ScopeName + "/" + ev.ScopeInstanceID
	if registered := shim.registeredToken(scopeKey); registered != ev.Token {
		t.Fatalf("shim registered token %q for scope %q, want wire token %q", registered, scopeKey, ev.Token)
	}
}

func requireTokenFileAgreement(t *testing.T, ev *AdapterLifecycleEvent) {
	t.Helper()
	fileBytes, err := os.ReadFile(ev.TokenRef)
	if err != nil {
		t.Fatalf("read token file %s: %v", ev.TokenRef, err)
	}
	if string(fileBytes) != ev.Token {
		t.Fatalf("token file %s holds %q, want the wire token %q", ev.TokenRef, string(fileBytes), ev.Token)
	}
}

// -----------------------------------------------------------------------------
// CRI-137: Scope Instance Persistence & Recovery
// -----------------------------------------------------------------------------

func TestInitScopeAdapters_PerScope_ReusesScopeInstanceAfterRestart(t *testing.T) {
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

	sink2, shim2, rlc2, deps2 := newPerScopeTestHarness(t, g, dataDir)

	tokenBytes, err := os.ReadFile(first.TokenRef)
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	preRestartScopeKey := first.ScopeName + "/" + first.ScopeInstanceID

	if _, err := initScopeAdapters(ctx, g, deps2, nil, dataDir, "", nil, rlc2); err != nil {
		t.Fatalf("post-restart initScopeAdapters: %v", err)
	}

	registeredToken, ok := shim2.registered[preRestartScopeKey]
	if !ok {
		t.Fatalf("surviving pod's pre-restart scope key %q was not re-registered with the shim on restart", preRestartScopeKey)
	}
	if registeredToken != string(tokenBytes) {
		t.Fatalf("registered token = %q, want reused %q", registeredToken, string(tokenBytes))
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
	if claim.ScopeInstanceID != first.ScopeInstanceID || claim.AdapterKey != "noop.default" {
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
	raw, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("read healed record: %v", err)
	}
	var rec currentScopeInstanceRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("healed record is not valid JSON: %v", err)
	}
	if rec.ScopeInstanceID != first.ScopeInstanceID || rec.AdapterKey != "noop.default" {
		t.Errorf("healed record = %+v, want instance %q for adapter noop.default", rec, first.ScopeInstanceID)
	}
}

func TestInitScopeAdapters_PerScope_ScanReusesTokensWithoutRecord(t *testing.T) {
	ctx := context.Background()
	g := perScopeRemoteGraph(t)
	dataDir := t.TempDir()

	scopeInstanceID := uuid.NewString()
	legacyToken := "legacy-surviving-token"
	tokenPath, err := writeRotatedToken(dataDir, "", scopeInstanceID, "noop.default", legacyToken)
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
	rec, recErr := readCurrentScopeInstance(dataDir, "", "noop.default")
	if recErr != nil {
		t.Fatalf("record not written after scan reuse: %v", recErr)
	}
	if rec.ScopeInstanceID != scopeInstanceID || rec.AdapterKey != "noop.default" {
		t.Errorf("healed record = %+v, want instance %q for adapter noop.default", rec, scopeInstanceID)
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
	if _, err := writeRotatedToken(dataDir, "", "../escape", "noop", "tok"); err == nil {
		t.Fatal("writeRotatedToken accepted a traversal scope instance ID")
	}
	if _, err := writeRotatedToken(dataDir, "../escape", "00000000-0000-0000-0000-000000000000", "noop", "tok"); err == nil {
		t.Fatal("writeRotatedToken accepted a traversal scope name")
	}

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
	instA := "11111111-2222-3333-4444-555555555555"
	instB := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	if _, err := writeRotatedToken(dataDir, "", instA, "mcp.default", "token-a"); err != nil {
		t.Fatalf("write token-a: %v", err)
	}
	if _, err := writeRotatedToken(dataDir, "", instB, "noop.default", "token-b"); err != nil {
		t.Fatalf("write token-b: %v", err)
	}
	if err := writeCurrentScopeInstance(dataDir, "", "mcp.default", currentScopeInstanceRecord{
		ScopeInstanceID: instA,
		AdapterKey:      "mcp.default",
	}); err != nil {
		t.Fatalf("write claiming record: %v", err)
	}

	found, err := scanScopeTokenFiles(dataDir, "", "noop.default")
	if err != nil {
		t.Fatalf("scanScopeTokenFiles: %v", err)
	}
	kept := filterUnclaimedScopeInstances(dataDir, "", found)
	if len(kept) != 1 || kept[0].instanceID != instB {
		t.Fatalf("filter kept %+v, want only the unclaimed instance %q", kept, instB)
	}
}

// -----------------------------------------------------------------------------
// CRI-145: Parent/Child Subworkflow Scope Sharing
// -----------------------------------------------------------------------------

type cri145Pod struct {
	executions *atomic.Int64
}

func (p *cri145Pod) serveLoop(sink *eventTrackingSink, stop <-chan struct{}) {
	var hs *cri137Handshake
	var addr string
	for {
		select {
		case <-stop:
			return
		case <-time.After(10 * time.Millisecond):
		}
		ev, ok := sink.firstStatus("provision_wanted")
		if !ok {
			continue
		}
		token, err := os.ReadFile(ev.TokenRef)
		if err != nil {
			continue
		}
		addr = ev.ShimListenAddress
		hs = &cri137Handshake{
			Name:    strings.TrimSuffix(filepath.Base(ev.TokenRef), ".token"),
			Version: "1.0.0",
			Digest:  pinDigest,
			Token:   string(token),
			Scope:   ev.ScopeName + "/" + ev.ScopeInstanceID,
		}
		break
	}
	for {
		select {
		case <-stop:
			return
		default:
		}
		if !p.dialOnce(addr, hs, stop) {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}
}

func (p *cri145Pod) dialOnce(addr string, hs *cri137Handshake, stop <-chan struct{}) bool {
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
	srv := grpc.NewServer()
	v2.RegisterAdapterServiceServer(srv, &cri145FakeAdapterServer{executions: p.executions})
	go func() { _ = srv.Serve(&cri137SingleConnListener{conn: observed}) }()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if observed.closed.Load() {
			break
		}
		select {
		case <-stop:
			srv.Stop()
			return false
		case <-time.After(200 * time.Microsecond):
		}
	}
	srv.Stop()
	return true
}

type cri145FakeAdapterServer struct {
	v2.UnimplementedAdapterServiceServer
	executions *atomic.Int64
}

func (f *cri145FakeAdapterServer) Info(ctx context.Context, req *v2.InfoRequest) (*v2.InfoResponse, error) {
	return &v2.InfoResponse{Name: "shell", Version: "1.0.0", Capabilities: []string{"execute"}}, nil
}

func (f *cri145FakeAdapterServer) OpenSession(ctx context.Context, req *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	return &v2.OpenSessionResponse{}, nil
}

func (f *cri145FakeAdapterServer) CloseSession(ctx context.Context, req *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
	return &v2.CloseSessionResponse{}, nil
}

func (f *cri145FakeAdapterServer) Execute(req *v2.ExecuteRequest, stream grpc.ServerStreamingServer[v2.ExecuteEvent]) error {
	if f.executions != nil {
		f.executions.Add(1)
	}
	return stream.Send(&v2.ExecuteEvent{Event: &v2.ExecuteEvent_Result{Result: &v2.ExecuteResult{Outcome: "success"}}})
}

func cri145LifecycleStats(sink *eventTrackingSink) (provisions, releases []AdapterLifecycleEvent) {
	for i := range sink.provisionEvents {
		ev := &sink.provisionEvents[i]
		switch ev.Status {
		case "provision_wanted":
			provisions = append(provisions, *ev)
		case "released":
			releases = append(releases, *ev)
		}
	}
	return provisions, releases
}

func cri145AssertPairedLifecycle(t *testing.T, provisions, releases []AdapterLifecycleEvent, maxConcurrent int) {
	t.Helper()
	outstanding := map[string]int{}
	concurrent := 0
	for i := range provisions {
		p := &provisions[i]
		key := p.ScopeName + "/" + p.ScopeInstanceID
		outstanding[key]++
		concurrent++
		if concurrent > maxConcurrent {
			t.Errorf("concurrent per-scope instances peaked at %d, want <= %d", concurrent, maxConcurrent)
		}
	}
	for i := range releases {
		r := &releases[i]
		key := r.ScopeName + "/" + r.ScopeInstanceID
		if outstanding[key] <= 0 {
			t.Errorf("orphan release for %s (no outstanding provision)", key)
			continue
		}
		outstanding[key]--
		concurrent--
	}
	for key, count := range outstanding {
		if count != 0 {
			t.Errorf("%d unreleased provisions for %s", count, key)
		}
	}
}

func cri145UnitRootHCL() string {
	return `workflow {
  name          = "cri145-unit"
  version       = "0.1"
  initial_state = "pre"
  target_state  = "done"
}

environment "remote" "prod" {
  listen_address     = "127.0.0.1:0"
  per_scope_sessions = true
}

adapter "shell" "default" {
  environment = remote.prod
}

subworkflow "child" {
  source = "./child"
}

step "pre" {
  target = adapter.shell.default
  outcome "success" { next = state.done }
  outcome "error" { next = state.done }
}

state "done" {
  terminal = true
  success  = true
}
`
}

func cri145RootHCL() string {
	return `workflow {
  name          = "cri145-parent"
  version       = "0.1"
  initial_state = "warmup"
  target_state  = "done"

  policy {
    max_step_retries = 50
  }
}

environment "remote" "prod" {
  listen_address     = "127.0.0.1:0"
  per_scope_sessions = true
}

environment "shell" "local" {}

adapter "shell" "default" {
  environment = remote.prod
}

adapter "noop" "default" {
  environment = shell.local
}

subworkflow "child" {
  source = "./child"
}

step "warmup" {
  target = adapter.noop.default
  outcome "success" { next = step.pre }
  outcome "error" { next = step.pre }
}

step "pre" {
  target = adapter.shell.default
  outcome "success" { next = step.run_child }
  outcome "error" { next = step.run_child }
}

step "run_child" {
  for_each = ["alpha", "beta", "gamma"]
  target   = subworkflow.child
  outcome "all_succeeded" { next = step.post }
  outcome "any_failed"    { next = state.failed }
}

step "post" {
  target = adapter.shell.default
  outcome "success" { next = state.done }
  outcome "error" { next = state.done }
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

func cri145ChildHCL() string {
	return `workflow {
  name          = "cri145-child"
  version       = "0.1"
  initial_state = "engage"
  target_state  = "complete"
}

environment "remote" "prod" {
  listen_address     = "127.0.0.1:0"
  per_scope_sessions = true
}

adapter "shell" "default" {
  environment = remote.prod
}

step "engage" {
  target = adapter.shell.default
  outcome "success" { next = state.complete }
  outcome "error" { next = state.complete }
}

state "complete" {
  terminal = true
  success  = true
}
`
}

func cri145Lockfile() *lockfile.Lockfile {
	return &lockfile.Lockfile{
		SchemaVersion: 1,
		Adapters: []lockfile.LockedAdapter{{
			Type:               "shell",
			Name:               "default",
			Reference:          "ghcr.io/brokenbots/criteria-adapter-shell",
			ResolvedDigest:     pinDigest,
			SourceURL:          "https://github.com/brokenbots/criteria",
			SDKProtocolVersion: 2,
			Platforms:          []string{"linux/amd64"},
		}},
	}
}

func TestScopeInstance_SubworkflowRedeclaredAdapterLifecycleStream(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "main.chcl"), cri145RootHCL())
	writeFile(t, filepath.Join(root, "child", "main.chcl"), cri145ChildHCL())
	writeLockfile(t, root, cri145Lockfile())
	g := compileWorkflowDir(t, root)

	if !canDialLoopbackTCP() {
		t.Skip("skipping test: loopback TCP dialing is restricted in current environment")
	}

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
		WithRunID("cri145"))

	stopDial := make(chan struct{})
	executions := &atomic.Int64{}
	pod := &cri145Pod{executions: executions}
	go pod.serveLoop(sink, stopDial)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(func() {
		close(stopDial)
		cancel()
		waitForCri137ShimTeardown(t, beforeDirs)
	})

	runErr := make(chan error, 1)
	go func() { runErr <- e.Run(ctx) }()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(35 * time.Second):
		t.Fatal("run did not finish in time")
	}

	if got := sink.terminal; got != "done" {
		t.Fatalf("terminal state = %q (ok=%v, failure=%q), want done", got, sink.terminalOK, sink.failure)
	}
	if !sink.terminalOK {
		t.Fatalf("run did not succeed (failure=%q)", sink.failure)
	}

	provisions, releases := cri145LifecycleStats(sink)
	if len(provisions) != 1 {
		t.Fatalf("lifecycle stream has %d provision_wanted events (%+v), want exactly 1 for the parent scope", len(provisions), provisions)
	}
	provision := provisions[0]
	if provision.ScopeName != "" {
		t.Fatalf("provision_wanted scope = %q, want the root scope \"\"", provision.ScopeName)
	}
	if len(releases) != 1 {
		t.Fatalf("lifecycle stream has %d released events (%+v), want exactly 1", len(releases), releases)
	}
	release := releases[0]
	if release.ScopeInstanceID != provision.ScopeInstanceID {
		t.Fatalf("released.ScopeInstanceID = %q, want the provisioned %q", release.ScopeInstanceID, provision.ScopeInstanceID)
	}
	if release.TokenRef != provision.TokenRef {
		t.Fatalf("released.TokenRef = %q, want the provisioned %q", release.TokenRef, provision.TokenRef)
	}
	if release.ScopeName != provision.ScopeName {
		t.Fatalf("released.ScopeName = %q, want the provisioned %q", release.ScopeName, provision.ScopeName)
	}
	cri145AssertPairedLifecycle(t, provisions, releases, 2)

	if got := executions.Load(); got < 5 || got > 55 {
		t.Errorf("adapter Execute calls = %d, want 5..55 (pre up to 51 attempts, 3 child iterations, post)", got)
	}
}

func TestScopeInstance_GuardSkipsChildRedeclaredAdapter(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "main.chcl"), cri145UnitRootHCL())
	writeFile(t, filepath.Join(root, "child", "main.chcl"), cri145ChildHCL())
	g := compileWorkflowDir(t, root)
	childBody := g.Subworkflows["child"].Body
	if childBody == nil {
		t.Fatal("compiled parent graph has no child subworkflow body")
	}
	dataDir := t.TempDir()

	sink, shim, rlc, deps := newPerScopeTestHarness(t, g, dataDir)
	parentOrder, err := initScopeAdapters(ctx, g, deps, nil, dataDir, "", nil, rlc)
	if err != nil {
		t.Fatalf("parent initScopeAdapters: %v", err)
	}
	if _, ok := sink.firstStatus("provision_wanted"); !ok {
		t.Fatal("parent init emitted no provision_wanted event")
	}

	childOrder, err := initScopeAdapters(ctx, childBody, deps, nil, dataDir, "engage", nil, rlc)
	if err != nil {
		t.Fatalf("child body initScopeAdapters: %v", err)
	}
	provisions, _ := cri145LifecycleStats(sink)
	if len(provisions) != 1 {
		t.Fatalf("after child init the stream has %d provision_wanted events (%+v), want exactly 1 (guard skipped the re-declared adapter)", len(provisions), provisions)
	}
	for scope, token := range shim.registered {
		if strings.HasPrefix(scope, "engage/") {
			t.Fatalf("child scope registered scope %q (token %q) on the shim; the guard did not skip the child init", scope, token)
		}
	}
	if _, err := os.Stat(filepath.Join(dataDir, "remote-tokens", "engage")); !os.IsNotExist(err) {
		t.Fatalf("child scope wrote token material under remote-tokens/engage: %v", err)
	}

	tearDownScopeAdapters(ctx, childOrder, deps, rlc)
	_, releases := cri145LifecycleStats(sink)
	if len(releases) != 0 {
		t.Fatalf("child teardown emitted %d released events, want 0", len(releases))
	}
	tearDownScopeAdapters(ctx, parentOrder, deps, rlc)
	provisions, releases = cri145LifecycleStats(sink)
	if len(provisions) != 1 {
		t.Fatalf("stream has %d provision_wanted events, want 1", len(provisions))
	}
	if len(releases) != 1 {
		t.Fatalf("stream has %d released events, want 1", len(releases))
	}
	if releases[0].ScopeInstanceID != provisions[0].ScopeInstanceID || releases[0].TokenRef != provisions[0].TokenRef {
		t.Fatalf("released %+v does not pair with provisioned %+v", releases[0], provisions[0])
	}
}

// -----------------------------------------------------------------------------
// CRI-236: Wire Token Invariants
// -----------------------------------------------------------------------------

func TestInitScopeAdapters_PerScope_ProvisionCarriesWireToken(t *testing.T) {
	ctx := context.Background()
	g := perScopeRemoteGraph(t)
	dataDir := t.TempDir()

	sink, shim, rlc, deps := newPerScopeTestHarness(t, g, dataDir)
	if _, err := initScopeAdapters(ctx, g, deps, nil, dataDir, "", nil, rlc); err != nil {
		t.Fatalf("initScopeAdapters: %v", err)
	}

	ev, ok := sink.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("init emitted no provision_wanted event")
	}
	requireShimRegisteredWireToken(t, shim, &ev)
	requireTokenFileAgreement(t, &ev)
}

func TestInitScopeAdapters_PerScope_ReplayAfterRestartRehandshakesWireToken(t *testing.T) {
	ctx := context.Background()
	g := perScopeRemoteGraph(t)
	dataDir := t.TempDir()

	sink1, shim1, rlc1, deps1 := newPerScopeTestHarness(t, g, dataDir)
	if _, err := initScopeAdapters(ctx, g, deps1, nil, dataDir, "", nil, rlc1); err != nil {
		t.Fatalf("first initScopeAdapters: %v", err)
	}
	first, ok := sink1.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("first init emitted no provision_wanted event")
	}
	requireShimRegisteredWireToken(t, shim1, &first)
	wireToken := first.Token
	if wireToken == "" {
		t.Fatal("pre-restart provision event carries no wire token")
	}

	sink2, shim2, rlc2, deps2 := newPerScopeTestHarness(t, g, dataDir)
	if _, err := initScopeAdapters(ctx, g, deps2, nil, dataDir, "", nil, rlc2); err != nil {
		t.Fatalf("post-restart initScopeAdapters: %v", err)
	}
	second, ok := sink2.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("post-restart init emitted no provision_wanted event")
	}
	if second.Token != wireToken {
		t.Fatalf("post-restart wire token = %q, want the same pre-restart token %q (replay desync)", second.Token, wireToken)
	}
	if second.ScopeInstanceID != first.ScopeInstanceID {
		t.Fatalf("post-restart ScopeInstanceID = %q, want reused %q", second.ScopeInstanceID, first.ScopeInstanceID)
	}
	requireShimRegisteredWireToken(t, shim2, &second)
	requireTokenFileAgreement(t, &second)
}

func TestInitScopeAdapters_PerScope_ScanReuseCarriesWireToken(t *testing.T) {
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
	wireToken := first.Token
	if wireToken == "" {
		t.Fatal("pre-restart provision event carries no wire token")
	}

	if err := os.RemoveAll(filepath.Join(dataDir, "remote-tokens", "current")); err != nil {
		t.Fatalf("remove current record dir: %v", err)
	}

	sink2, shim2, rlc2, deps2 := newPerScopeTestHarness(t, g, dataDir)
	if _, err := initScopeAdapters(ctx, g, deps2, nil, dataDir, "", nil, rlc2); err != nil {
		t.Fatalf("post-restart initScopeAdapters: %v", err)
	}
	second, ok := sink2.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("post-restart init emitted no provision_wanted event")
	}
	if second.ScopeInstanceID != first.ScopeInstanceID {
		t.Fatalf("post-restart ScopeInstanceID = %q, want scan-reused %q", second.ScopeInstanceID, first.ScopeInstanceID)
	}
	if second.Token != wireToken {
		t.Fatalf("scan-reuse wire token = %q, want %q", second.Token, wireToken)
	}
	requireShimRegisteredWireToken(t, shim2, &second)
	requireTokenFileAgreement(t, &second)
}

func TestTearDownScopeAdapters_PerScope_ReleasedEventCarriesNoToken(t *testing.T) {
	ctx := context.Background()
	g := perScopeRemoteGraph(t)
	dataDir := t.TempDir()

	sink, _, rlc, deps := newPerScopeTestHarness(t, g, dataDir)
	order, err := initScopeAdapters(ctx, g, deps, nil, dataDir, "", nil, rlc)
	if err != nil {
		t.Fatalf("initScopeAdapters: %v", err)
	}
	tearDownScopeAdapters(ctx, order, deps, rlc)

	var released *AdapterLifecycleEvent
	for i := range sink.provisionEvents {
		if sink.provisionEvents[i].Status == "released" {
			released = &sink.provisionEvents[i]
			break
		}
	}
	if released == nil {
		t.Fatal("teardown emitted no released event")
	}
	if released.Token != "" {
		t.Fatalf("released event carries wire token %q, want empty (provision-time only)", released.Token)
	}
	if released.TokenRef == "" {
		t.Fatal("released event must keep its TokenRef (forensic trail / transition window)")
	}
}

// -----------------------------------------------------------------------------
// CRI-304: Crash-Resume & Instance Adoption
// -----------------------------------------------------------------------------

func TestScopeInstance_InitFailurePublishesRunFailed(t *testing.T) {
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

func TestScopeInstance_RunFrom_InitFailurePublishesRunFailed(t *testing.T) {
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

func TestInitScopeAdapters_PerScope_AdoptsPriorRunScopeInstance(t *testing.T) {
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
	priorToken, err := os.ReadFile(first.TokenRef)
	if err != nil {
		t.Fatalf("read prior token: %v", err)
	}

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

	if !strings.HasPrefix(adopted.TokenRef, ownDir) {
		t.Errorf("adopted TokenRef = %q, want a path under the replay's own data dir %q", adopted.TokenRef, ownDir)
	}
	healedToken, err := os.ReadFile(adopted.TokenRef)
	if err != nil {
		t.Fatalf("read self-healed token: %v", err)
	}
	if !bytes.Equal(healedToken, priorToken) {
		t.Errorf("self-healed token = %q, want the adopted prior token %q", string(healedToken), string(priorToken))
	}
	ownRec, err := readCurrentScopeInstance(ownDir, adopted.ScopeName, "noop.default")
	if err != nil {
		t.Fatalf("read replay's current scope record: %v", err)
	}
	if ownRec.ScopeInstanceID != first.ScopeInstanceID || ownRec.AdapterKey != "noop.default" {
		t.Errorf("replay's current scope record = %+v, want instance %q adapter noop.default", ownRec, first.ScopeInstanceID)
	}

	if _, err := os.Stat(filepath.Join(priorDir, "remote-tokens", "current", "released-"+first.ScopeInstanceID+".json")); err != nil {
		t.Fatalf("prior adoption claim tombstone missing: %v", err)
	}
	if _, err := readCurrentScopeInstance(priorDir, first.ScopeName, "noop.default"); err == nil {
		t.Error("prior live record still present after adoption; a later replay could re-adopt a bound instance")
	}
}

func TestInitScopeAdapters_PerScope_AdoptionSkipsClaimedPriorInstance(t *testing.T) {
	ctx := context.Background()
	g := perScopeRemoteGraph(t)
	priorDir := t.TempDir()
	ownDir := t.TempDir()

	scopeInstanceID := uuid.NewString()
	if _, err := writeRotatedToken(priorDir, "", scopeInstanceID, "noop", "prior-token"); err != nil {
		t.Fatalf("seed prior token: %v", err)
	}
	if err := writeCurrentScopeInstance(priorDir, "", "released-"+scopeInstanceID, currentScopeInstanceRecord{
		ScopeInstanceID: scopeInstanceID,
		AdapterKey:      "noop.default",
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

func TestInitScopeAdapters_PerScope_AdoptionTeardownReleasesOwnCopy(t *testing.T) {
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
