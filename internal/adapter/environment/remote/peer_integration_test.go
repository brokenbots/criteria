package remote

// peer_integration_test.go — T-08 named scenarios driven end to end with the
// REAL noop adapter binary on both ends: the peer runtime spawns the child
// exactly like production (go-plugin subprocess, scrubbed env) and dials a
// real TCP shim, while the host side drives the real peerHandle /
// SessionManager surface. The fake-peer unit tests in peer_session_test.go /
// peer_crash_test.go stay the fast feedback loop; this file is the
// integration layer that catches wiring divergences the fakes cannot see.
//
// Scenarios (KB-18 T-08):
//  1. crash-fidelity: SIGKILL the real child mid-Execute → the peer journals
//     ProcessExited{signal:9} + CrashClassified{process_terminated}; the host
//     consumes the journal wire facts verbatim (no transport-string matching).
//  2. lifecycle-parity: every v2 RPC the noop adapter implements behaves
//     identically through the peer transport and a local go-plugin handle.
//  3. scope-parity: per_scope_sessions with a scope-unaware v2 adapter
//     (fakePeer) → isolated sessions keyed by scope; stale-token rejection
//     after rotation (CRI-115).
//  4. idle-keepalive (CRI-276): an idle session survives idleness with no
//     transport death; the next Execute succeeds on the same child.
//  5. reconnect: bouncing the host listener reconnects the peer with the
//     child kept alive and buffered supervision events replayed exactly once.
//  6. stale-peer-budget (CRI-137): a restarted peer holding a pre-rotation
//     scope is diagnosed in the "stale pod" class; a fresh peer is adopted.
//  7. teardown-window (CRI-287): a follow-on sibling close after an engine
//     step-timeout is routed as timeout, not crash — and the same real crash
//     outside the window classifies verbatim.
//  8. security: non-loopback listeners are refused, oversize identity frames
//     are rejected on the wire, and rejected tokens never leak the expected
//     token (constant-time compare is pinned by the shim unit suite).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	criteriav1 "github.com/brokenbots/criteria/sdk/pb/criteria/v1"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/peer"
	"github.com/brokenbots/criteria/workflow"
)

// buildNoopIntegrationBinary builds the noop conformance adapter and returns
// its path. The peer runtime and the local-parity handle both spawn this
// binary, so both transports exercise the identical adapter implementation.
func buildNoopIntegrationBinary(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve caller path")
	}
	moduleRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", ".."))
	binary := filepath.Join(t.TempDir(), "criteria-adapter-noop")
	cmd := exec.Command("go", "build", "-o", binary, "./internal/adapter/conformance/testdata/noop")
	cmd.Dir = moduleRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build noop adapter: %v\n%s", err, string(out))
	}
	return binary
}

// noopBinaryDigest reads the binary and returns the digest the shim verifier
// and the peer identity frame must agree on.
func noopBinaryDigest(t *testing.T, binary string) string {
	t.Helper()
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatalf("read noop binary: %v", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// integrationFixture wires a real shim + provider + a real peer runtime
// serving the noop child. Everything it starts is torn down in Cleanup.
type integrationFixture struct {
	shim     *Shim
	shims    []*Shim // every shim started over the fixture lifetime (bounce restarts)
	provider *peerSessionProvider
	addr     string // current shim listen address (mutated by bounce)
	token    string
	binary   string       // noop adapter binary shared by every peer in the fixture
	digest   string       // digest pinned by the fixture's shim verifier
	pcfg     *peer.Config // primary peer's resolved config (nil with noPeer)
	journal  *peer.EventJournal

	mu     sync.Mutex
	peers  []context.CancelFunc
	closed bool
}

// integrationOpts configures startIntegrationFixture.
type integrationOpts struct {
	perScope bool
	scope    string
	token    string
	// noPeer skips the primary peer boot: scenarios that stage their own
	// dials (stale-peer budget) need the registry empty for the scope.
	noPeer bool
}

// startIntegrationFixture builds the noop binary, starts a loopback shim with
// a digest verifier pinned to the real binary, and boots a real peer runtime
// that phones home into it.
func startIntegrationFixture(t *testing.T, opts integrationOpts) *integrationFixture {
	t.Helper()
	binary := buildNoopIntegrationBinary(t)
	digest := noopBinaryDigest(t, binary)
	token := opts.token
	if token == "" {
		token = "integration-accept-token"
	}

	shimCfg := &Config{
		ListenAddress: "127.0.0.1:0",
		Insecure:      true,
		AcceptToken:   token,
	}
	if opts.perScope {
		shimCfg.PerScopeSessions = true
		shimCfg.AcceptToken = ""
	}
	// The verifier accepts both the real noop binary digest (every real peer
	// runtime in this fixture) and the fakePeer's pinned digest (the
	// scope-unaware non-Go-class v2 test adapter presents its own identity).
	shim, err := NewShim(shimCfg, &multiDigestVerifier{allowed: map[string]map[string]bool{
		"noop": {digest: true, "sha256:abcd1234": true},
	}})
	if err != nil {
		t.Fatalf("NewShim: %v", err)
	}
	startCtx, cancelStart := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelStart()
	if err := shim.Start(startCtx); err != nil {
		t.Fatalf("shim Start: %v", err)
	}
	if opts.perScope {
		shim.RegisterScope(opts.scope, token)
	}

	provider := NewPeerSessionProvider(shim, opts.perScope)
	shim.SetPeerAcceptor(provider)

	fx := &integrationFixture{
		shim:     shim,
		shims:    []*Shim{shim},
		provider: provider,
		addr:     shim.listener.Addr().String(),
		token:    token,
		binary:   binary,
		digest:   digest,
	}
	t.Cleanup(fx.Close)

	if opts.noPeer {
		return fx
	}
	fx.pcfg, fx.journal = fx.startPeer(t, &peerOpts{scope: opts.scope, binary: binary, digest: digest})
	return fx
}

func (fx *integrationFixture) Close() {
	fx.mu.Lock()
	if fx.closed {
		fx.mu.Unlock()
		return
	}
	fx.closed = true
	cancels := append([]context.CancelFunc(nil), fx.peers...)
	fx.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range fx.shims {
		_ = s.Stop(ctx)
	}
}

// peerOpts configures an additional (or the primary) peer runtime.
type peerOpts struct {
	scope  string
	token  string
	host   string
	binary string
	digest string
}

// startPeer boots a real peer runtime + phone-home server pointed at the
// current fixture address and registers its cancellation for cleanup.
func (fx *integrationFixture) startPeer(t *testing.T, opts *peerOpts) (*peer.Config, *peer.EventJournal) {
	t.Helper()
	host := opts.host
	if host == "" {
		host = fx.addr
	}
	token := opts.token
	if token == "" {
		token = fx.token
	}
	pcfg := &peer.Config{
		Host:           host,
		Token:          token,
		Scope:          opts.scope,
		AdapterName:    "noop",
		AdapterBinary:  opts.binary,
		Digest:         opts.digest,
		ChildKeepAlive: true,
		BackoffMin:     50 * time.Millisecond,
		BackoffMax:     200 * time.Millisecond,
	}
	if err := pcfg.Resolve(); err != nil {
		t.Fatalf("peer Config.Resolve: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rt := peer.NewRuntime(pcfg, log)
	bootCtx, cancelBoot := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelBoot()
	if err := rt.Boot(bootCtx); err != nil {
		t.Fatalf("peer runtime Boot: %v", err)
	}
	server := peer.NewServer(pcfg, rt, log)
	serveCtx, cancelServe := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- server.Serve(serveCtx) }()
	fx.mu.Lock()
	fx.peers = append(fx.peers, cancelServe)
	fx.mu.Unlock()
	t.Cleanup(func() {
		cancelServe()
		select {
		case err := <-errCh:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Logf("peer server returned: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("peer server did not stop within 15s of cancellation")
		}
	})
	return pcfg, rt.Journal()
}

// waitForHandle blocks until the registry holds a peer handle for the given
// adapter type + scope.
func (fx *integrationFixture) waitForHandle(t *testing.T, scope string) *peerHandle {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	handle, err := fx.provider.WaitForHandle(ctx, "noop", scope)
	if err != nil {
		t.Fatalf("WaitForHandle(noop, %q): %v", scope, err)
	}
	ph, ok := handle.(*peerHandle)
	if !ok {
		t.Fatalf("handle type %T, want *peerHandle", handle)
	}
	return ph
}

// journalEvents returns a snapshot of the primary peer's supervision journal.
func (fx *integrationFixture) journalEvents() []*criteriav1.SupervisionEvent {
	return fx.journal.Replay(0)
}

// firstCrash returns the first collected session.crash event, if any.
func firstCrash(coll *peerEventCollector) (map[string]any, bool) {
	coll.mu.Lock()
	defer coll.mu.Unlock()
	for _, evt := range coll.events {
		if evt.kind == "session.crash" {
			return evt.data, true
		}
	}
	return nil, false
}

// findSpawned returns the journal's ProcessSpawned record (the child PID the
// crash tests kill).
func findSpawned(t *testing.T, events []*criteriav1.SupervisionEvent) *criteriav1.ProcessSpawned {
	t.Helper()
	for _, ev := range events {
		if sp := ev.GetSpawned(); sp != nil {
			return sp
		}
	}
	t.Fatalf("no ProcessSpawned in journal (%d events)", len(events))
	return nil
}

// waitForExitedCrash waits for the journal to carry the SIGKILL record pair:
// ProcessExited{signal 9} followed by CrashClassified{process_terminated}.
func waitForExitedCrash(t *testing.T, fx *integrationFixture) (*criteriav1.ProcessExited, *criteriav1.CrashClassified) {
	t.Helper()
	var exited *criteriav1.ProcessExited
	var crash *criteriav1.CrashClassified
	waitFor(t, "journal Exited{signal 9} + CrashClassified{process_terminated}", func() bool {
		for _, ev := range fx.journalEvents() {
			if e := ev.GetExited(); e != nil && e.GetSignal() == 9 {
				exited = e
			}
			if c := ev.GetCrash(); c != nil && c.GetReason() == adapterhost.CrashReasonProcessTerminated {
				crash = c
			}
		}
		return exited != nil && crash != nil
	})
	return exited, crash
}

// openSessionWithLog opens an adapter session on the raw peer handle and
// starts its Log stream into a collector, mirroring how a real session is
// wired before a step runs. The returned cancel stops everything it started.
func openSessionWithLog(t *testing.T, ph *peerHandle, sessionID string) (*logEventCollector, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	if err := ph.OpenSession(ctx, sessionID, nil, nil); err != nil {
		cancel()
		t.Fatalf("OpenSession %q: %v", sessionID, err)
	}
	coll := &logEventCollector{}
	starter, ok := adapterhost.Handle(ph).(adapterhost.LogStreamStarter)
	if !ok {
		cancel()
		t.Fatalf("peer handle %T does not implement LogStreamStarter", ph)
	}
	logCtx, logCancel := context.WithCancel(ctx)
	cancelLog, _, err := starter.StartLogStream(logCtx, sessionID, coll)
	if err != nil {
		logCancel()
		cancel()
		t.Fatalf("StartLogStream: %v", err)
	}
	return coll, func() {
		cancelLog()
		logCancel()
		cancel()
	}
}

// --- scenario 1: crash-fidelity ---

// TestPeerIntegrationCrashFidelity kills the real noop child mid-Execute and
// asserts the precise classification branch: the peer journals the real wait
// status (signal 9) plus the shared-taxonomy CrashClassified reason, and the
// host consumes those wire facts verbatim — the crash_reason the engine sees
// comes from the journal, never from matching transport error strings.
// Pre-crash log lines must already have reached the host sink.
func TestPeerIntegrationCrashFidelity(t *testing.T) {
	fx := startIntegrationFixture(t, integrationOpts{})
	ph := fx.waitForHandle(t, "")

	coll, stopLog := openSessionWithLog(t, ph, "s1")
	defer stopLog()

	// Long-running Execute with a log line queued up front: the line must be
	// delivered over the peer Log stream before the crash lands.
	execDone := make(chan error, 1)
	go func() {
		_, err := ph.Execute(context.Background(), "s1", &workflow.StepNode{
			Name: "develop",
			Input: map[string]string{
				"delay_ms": "15000",
				"emit_log": "pre-crash log evidence",
			},
		}, &peerEventCollector{})
		execDone <- err
	}()
	waitFor(t, "pre-crash log line at host sink", func() bool {
		return coll.count("pre-crash log evidence") > 0
	})

	spawned := findSpawned(t, fx.journalEvents())
	if spawned.GetPid() <= 1 {
		t.Fatalf("journal ProcessSpawned.Pid = %d, want the real child pid", spawned.GetPid())
	}
	if err := syscall.Kill(int(spawned.GetPid()), syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL child pid %d: %v", spawned.GetPid(), err)
	}

	exited, crash := waitForExitedCrash(t, fx)
	if exited.GetGraceful() {
		t.Error("Exited.Graceful = true, want false for an external SIGKILL")
	}
	if got := exited.GetExitCode(); got != -1 {
		t.Errorf("Exited.ExitCode = %d, want -1 (killed by signal, no exit code)", got)
	}
	if !strings.Contains(crash.GetDetail(), "signal 9") {
		t.Errorf("CrashClassified.Detail = %q, want the real wait status (signal 9)", crash.GetDetail())
	}

	// The host observes the journal wire facts verbatim: ProcessExited flips
	// from the supervision replay and SupervisionCrashReason carries the
	// journal's classification — the precise branch, not a transport string.
	waitFor(t, "host ProcessExited from supervision replay", func() bool {
		return adapterhost.ProcessExited(ph)
	})
	reason, ok := adapterhost.SupervisionCrashReason(ph)
	if !ok || reason != adapterhost.CrashReasonProcessTerminated {
		t.Fatalf("SupervisionCrashReason = (%q, %v), want %q", reason, ok, adapterhost.CrashReasonProcessTerminated)
	}

	// The Execute stream dies with the child; the error must not be nil.
	if err := <-execDone; err == nil {
		t.Fatal("Execute survived the SIGKILL of the adapter child")
	}
}

// --- scenario 2: lifecycle parity (real adapter, both transports) ---

// TestPeerIntegrationLifecycleParity drives every v2 RPC the noop adapter
// implements through both transports backed by the SAME binary: a local
// go-plugin handle (production local path) and the peer transport. The local
// transport's observation is the expectation; any divergence is a peer-path
// bug, not a fixture difference.
func TestPeerIntegrationLifecycleParity(t *testing.T) {
	binary := buildNoopIntegrationBinary(t)
	fx := startIntegrationFixture(t, integrationOpts{})
	peerH := fx.waitForHandle(t, "")

	loader := adapterhost.NewLoaderWithDiscovery(func(string) (string, error) { return binary, nil })
	resolveCtx, cancelResolve := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelResolve()
	localH, err := loader.Resolve(resolveCtx, "noop")
	if err != nil {
		t.Fatalf("local loader.Resolve: %v", err)
	}
	t.Cleanup(func() { _ = loader.Shutdown(context.Background()) })

	rows := []struct {
		name string
		run  func(t *testing.T, h adapterhost.Handle) string
	}{
		{
			name: "Info",
			run: func(t *testing.T, h adapterhost.Handle) string {
				info, err := h.Info(context.Background())
				if err != nil {
					return "err=" + err.Error()
				}
				return fmt.Sprintf("name=%s caps=%d err=<nil>", info.Name, len(info.Capabilities))
			},
		},
		{
			name: "OpenSession",
			run: func(t *testing.T, h adapterhost.Handle) string {
				if err := h.OpenSession(context.Background(), "s1", map[string]string{"k": "v"}, nil); err != nil {
					return "err=" + err.Error()
				}
				return "ok"
			},
		},
		{
			name: "Execute",
			run: func(t *testing.T, h adapterhost.Handle) string {
				res, err := h.Execute(context.Background(), "s1", &workflow.StepNode{Name: "develop"}, &peerEventCollector{})
				if err != nil {
					return "err=" + err.Error()
				}
				return "outcome=" + res.Outcome
			},
		},
		{
			name: "LogDeliversExecuteLine",
			run: func(t *testing.T, h adapterhost.Handle) string {
				if err := h.OpenSession(context.Background(), "s2", nil, nil); err != nil {
					return "open err=" + err.Error()
				}
				coll := &logEventCollector{}
				starter, ok := h.(adapterhost.LogStreamStarter)
				if !ok {
					return "no LogStreamStarter"
				}
				logCtx, logCancel := context.WithCancel(context.Background())
				defer logCancel()
				cancelLog, _, err := starter.StartLogStream(logCtx, "s2", coll)
				if err != nil {
					return "start err=" + err.Error()
				}
				defer cancelLog()
				_, execErr := h.Execute(context.Background(), "s2", &workflow.StepNode{
					Name:  "develop",
					Input: map[string]string{"emit_log": "integration parity log line"},
				}, &peerEventCollector{})
				if execErr != nil {
					return "exec err=" + execErr.Error()
				}
				waitFor(t, "log line over transport", func() bool {
					return coll.count("integration parity log line") > 0
				})
				return "log delivered"
			},
		},
		{
			name: "CloseSession",
			run: func(t *testing.T, h adapterhost.Handle) string {
				if err := h.CloseSession(context.Background(), "s1"); err != nil {
					return "err=" + err.Error()
				}
				return "ok"
			},
		},
	}

	handles := []struct {
		transport string
		h         adapterhost.Handle
	}{
		{transport: "local", h: localH},
		{transport: "peer", h: peerH},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			want := ""
			for _, tr := range handles {
				got := row.run(t, tr.h)
				if want == "" {
					want = got
					continue
				}
				if got != want {
					t.Errorf("%s transport: got %q, want the local transport's %q", tr.transport, got, want)
				}
			}
		})
	}
}

// --- scenario 3: scope parity + stale-token rotation (CRI-115) ---

// TestPeerIntegrationScopeParityIsolatedSessions proves per_scope_sessions
// isolates adapter sessions keyed by scope: the real noop peer and a
// scope-unaware v2 adapter (fakePeer — the non-Go-class v2 adapter shape)
// land in independent sessions per scope, and neither observes the other's
// traffic.
func TestPeerIntegrationScopeParityIsolatedSessions(t *testing.T) {
	fx := startIntegrationFixture(t, integrationOpts{perScope: true, scope: "run_a/inst1", token: "scope-tok-a"})

	// Second scope served by the scope-unaware v2 adapter: the shim's
	// per-scope token gate is adapter-agnostic, so an adapter SDK with no
	// scope concept still lands in its own isolated session.
	fx.provider.RegisterScope("run_b/inst1", "scope-tok-b")
	unaware := newFakePeer("run_b/inst1")
	unaware.token = "scope-tok-b"
	unaware.connect(t, fx.addr)

	aware := fx.waitForHandle(t, "run_a/inst1")
	unawareCtx, cancelUnaware := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelUnaware()
	unawareH, err := fx.provider.WaitForHandle(unawareCtx, "noop", "run_b/inst1")
	if err != nil {
		t.Fatalf("WaitForHandle(run_b): %v", err)
	}

	// Isolation: both sessions open and execute independently.
	awareCtx, cancelAware := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelAware()
	if err := aware.OpenSession(awareCtx, "aware-s1", nil, nil); err != nil {
		t.Fatalf("aware OpenSession: %v", err)
	}
	if err := unawareH.OpenSession(awareCtx, "unaware-s1", nil, nil); err != nil {
		t.Fatalf("unaware OpenSession: %v", err)
	}
	res, err := aware.Execute(awareCtx, "aware-s1", &workflow.StepNode{Name: "develop"}, &peerEventCollector{})
	if err != nil || res.Outcome != "success" {
		t.Fatalf("aware Execute = (%v, %v), want success", res, err)
	}

	// The registry keys are distinct: one scope's session never replaces the
	// other's.
	fx.provider.mu.Lock()
	_, hasA := fx.provider.peers[fx.provider.key("noop", "run_a/inst1")]
	_, hasB := fx.provider.peers[fx.provider.key("noop", "run_b/inst1")]
	fx.provider.mu.Unlock()
	if !hasA || !hasB {
		t.Fatalf("registry isolation broken: run_a=%t run_b=%t", hasA, hasB)
	}
}

// TestPeerIntegrationScopeStaleTokenRejectedAfterRotation pins CRI-115 on
// the peer path: after a scope's token rotates, a reconnect holding the
// pre-rotation token is rejected at identity verification and the provider
// never adopts a new session for it.
func TestPeerIntegrationScopeStaleTokenRejectedAfterRotation(t *testing.T) {
	fx := startIntegrationFixture(t, integrationOpts{perScope: true, scope: "run_c/inst1", token: "tok-v1"})
	binary, digest := fxBinaryAndDigest(fx)

	stale := fx.waitForHandle(t, "run_c/inst1")

	// Rotate the scope token: only tok-v2 dials are accepted from now on.
	fx.provider.RegisterScope("run_c/inst1", "tok-v2")

	// A restarted peer still holding tok-v1 dials forever; every dial is
	// rejected and nothing new is adopted for the scope.
	fx.startPeer(t, &peerOpts{scope: "run_c/inst1", token: "tok-v1", binary: binary, digest: digest})

	// Raw wire proof: a dial presenting the stale token gets its connection
	// closed by the shim (identity verification failure).
	conn, err := net.Dial("tcp", fx.addr)
	if err != nil {
		t.Fatalf("dial shim: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write(peerIdentityFrame(t, "noop", "run_c/inst1", "tok-v1", digest)); err != nil {
		t.Fatalf("write stale handshake: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64)
	if n, readErr := conn.Read(buf); readErr == nil && n > 0 {
		t.Fatalf("stale-token dial was answered with %q, want connection close", buf[:n])
	}

	// The provider never adopts a replacement while the stale token is in
	// circulation: WaitForFreshHandle keeps waiting (bounded by ctx here).
	waitCtx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	if _, waitErr := fx.provider.WaitForFreshHandle(waitCtx, "noop", "run_c/inst1", stale); !errors.Is(waitErr, context.DeadlineExceeded) {
		t.Fatalf("WaitForFreshHandle err = %v, want DeadlineExceeded (no adoption under stale token)", waitErr)
	}

	// The original session is still the registry entry — a rejected dial
	// must not have replaced it.
	fx.provider.mu.Lock()
	ps, ok := fx.provider.peers[fx.provider.key("noop", "run_c/inst1")]
	fx.provider.mu.Unlock()
	if !ok || ps.handle != adapterhost.Handle(stale) {
		t.Fatalf("registry entry changed after stale-token dials (present=%t)", ok)
	}
}

// peerIdentityFrame renders the newline-terminated peer identity frame a
// real peer writes as its first bytes.
func peerIdentityFrame(t *testing.T, name, scope, token, digest string) []byte {
	t.Helper()
	hs := &handshakeMessage{
		Name:    name,
		Version: "1.0.0",
		Digest:  digest,
		Token:   token,
		Scope:   scope,
		Role:    "peer",
		Peer:    &PeerClientIdentity{CriteriaVersion: "test"},
	}
	data, err := json.Marshal(hs)
	if err != nil {
		t.Fatalf("marshal handshake: %v", err)
	}
	return append(data, '\n')
}

// --- scenario 4: idle keepalive (CRI-276) ---

// TestPeerIntegrationIdleSessionSurvives pins the CRI-276 behavior on the
// peer path: a session left fully idle stays connected — no transport death,
// no peer re-dial, no journal growth — and the next Execute succeeds on the
// same child. The compressed-clock transport proof (server keepalive pings
// through an idle-closing middlebox) lives in internal/peer/serve_test.go;
// this integration layer pins the observable contract on the real stack.
func TestPeerIntegrationIdleSessionSurvives(t *testing.T) {
	fx := startIntegrationFixture(t, integrationOpts{})
	ph := fx.waitForHandle(t, "")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := ph.OpenSession(ctx, "s1", nil, nil); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	res, err := ph.Execute(ctx, "s1", &workflow.StepNode{Name: "develop"}, &peerEventCollector{})
	if err != nil || res.Outcome != "success" {
		t.Fatalf("pre-idle Execute = (%v, %v), want success", res, err)
	}

	eventsBefore := len(fx.journalEvents())
	seqBefore := fx.journal.LastSeq()
	ps := mustPeerSession(t, fx.provider)

	// Idle well past any request-scoped deadline: the connection must stay
	// warm (server keepalive pings) rather than idle-close.
	time.Sleep(3 * time.Second)

	fx.provider.mu.Lock()
	sameSession := fx.provider.peers[fx.provider.key("noop", "")] == ps
	fx.provider.mu.Unlock()
	if !sameSession {
		t.Fatal("peer re-dialed during idle: registry session was replaced")
	}
	if got := fx.journal.LastSeq(); got != seqBefore {
		t.Fatalf("journal seq advanced during idle: %d → %d (spurious supervision events)", seqBefore, got)
	}
	if got := len(fx.journalEvents()); got != eventsBefore {
		t.Fatalf("journal changed during idle: %d → %d events", eventsBefore, got)
	}

	res, err = ph.Execute(ctx, "s1", &workflow.StepNode{Name: "develop"}, &peerEventCollector{})
	if err != nil || res.Outcome != "success" {
		t.Fatalf("post-idle Execute = (%v, %v), want success on the same session", res, err)
	}
}

// --- scenario 5: reconnect after host listener bounce ---

// TestPeerIntegrationReconnectAfterHostBounce bounces the host listener
// while the child stays alive, SIGKILLs the child during the disconnect
// window, and asserts the peer reconnects to the new listener, replays the
// buffered ProcessExited + CrashClassified, and never respawned the child
// (single ProcessSpawned for the fixture lifetime). The backoff+jitter
// timing assertions live in internal/peer/serve_test.go where the sleep and
// rand seams are reachable.
func TestPeerIntegrationReconnectAfterHostBounce(t *testing.T) {
	fx := startIntegrationFixture(t, integrationOpts{})

	ph1 := fx.waitForHandle(t, "")
	waitFor(t, "spawn journal visible", func() bool {
		_, err := findSpawnedNoFatal(fx.journalEvents())
		return err == nil
	})

	// Bounce: stop the current shim and start a fresh listener; the peer's
	// reconnect loop must find the new address (each dial re-reads the
	// resolved host).
	stopCtx, cancelStop := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelStop()
	if err := fx.shim.Stop(stopCtx); err != nil {
		t.Fatalf("old shim Stop: %v", err)
	}
	// The host's death takes its accepted conns with it; Shim.Stop closes
	// only the listener, so drop the provider's sessions to close the peer
	// conns and trigger the peer's observe-EOF-and-reconnect path.
	fx.dropPeerSessions("host listener bounced")

	newShimCfg := &Config{ListenAddress: "127.0.0.1:0", Insecure: true, AcceptToken: fx.token}
	newShim, err := NewShim(newShimCfg, fx.shimVerifier())
	if err != nil {
		t.Fatalf("NewShim (restarted): %v", err)
	}
	startCtx, cancelStart := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelStart()
	if err := newShim.Start(startCtx); err != nil {
		t.Fatalf("restarted shim Start: %v", err)
	}
	newShim.SetPeerAcceptor(fx.provider)
	fx.mu.Lock()
	fx.shims = append(fx.shims, newShim)
	fx.shim = newShim
	fx.addr = newShim.listener.Addr().String()
	fx.mu.Unlock()
	fx.pcfg.Host = fx.addr

	// Disconnect window: kill the child while the host is unreachable. The
	// exit + crash facts buffer in the peer journal and replay on reconnect.
	spawned := findSpawned(t, fx.journalEvents())
	if err := syscall.Kill(int(spawned.GetPid()), syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL child during disconnect: %v", err)
	}
	exited, crash := waitForExitedCrash(t, fx)
	if exited.GetGraceful() {
		t.Error("Exited.Graceful = true, want false for SIGKILL")
	}
	if crash.GetReason() != adapterhost.CrashReasonProcessTerminated {
		t.Errorf("CrashClassified.Reason = %q, want %q", crash.GetReason(), adapterhost.CrashReasonProcessTerminated)
	}

	// Reconnect: the fresh handle replaces the stale one in the registry.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	fresh, err := fx.provider.WaitForFreshHandle(ctx, "noop", "", ph1)
	if err != nil {
		t.Fatalf("WaitForFreshHandle after bounce: %v", err)
	}
	ph2, ok := fresh.(*peerHandle)
	if !ok {
		t.Fatalf("fresh handle type %T, want *peerHandle", fresh)
	}
	if ph2 == ph1 {
		t.Fatal("WaitForFreshHandle returned the stale handle")
	}

	// The buffered facts replay onto the fresh handle: the supervision
	// cursor starts at the journal tail for a new session and event_seq
	// dedup guards against double delivery.
	waitFor(t, "replayed ProcessExited on fresh handle", func() bool {
		return adapterhost.ProcessExited(ph2)
	})
	reason, ok := adapterhost.SupervisionCrashReason(ph2)
	if !ok || reason != adapterhost.CrashReasonProcessTerminated {
		t.Fatalf("replayed SupervisionCrashReason = (%q, %v), want %q", reason, ok, adapterhost.CrashReasonProcessTerminated)
	}

	// The child was never respawned: exactly one ProcessSpawned for the
	// fixture lifetime, even across the reconnect.
	spawnCount := 0
	for _, ev := range fx.journalEvents() {
		if ev.GetSpawned() != nil {
			spawnCount++
		}
	}
	if spawnCount != 1 {
		t.Fatalf("ProcessSpawned count = %d, want 1 (child kept alive across reconnect)", spawnCount)
	}
}

// --- scenario 6: stale-peer budget (CRI-137 on the peer path) ---

// TestPeerIntegrationStalePeerBudget proves the CRI-137 semantics on the
// peer path: a restarted peer presenting a pre-rotation scope key is
// rejected (scope not registered), a pending wait that only ever sees those
// rejections is diagnosed in the "stale pod" class when the verify-failure
// budget expires, and a fresh peer presenting the current scope is adopted
// well within DefaultVerifyFailureBudget.
func TestPeerIntegrationStalePeerBudget(t *testing.T) {
	t.Run("stale scope dials diagnose the pending wait", func(t *testing.T) {
		fx := startIntegrationFixture(t, integrationOpts{perScope: true, scope: "run_x/inst1", token: "stale-tok", noPeer: true})
		binary, digest := fxBinaryAndDigest(fx)

		// Compress the wall-clock budget: the shim's authoritative bound for
		// a pending session wait.
		fx.shim.verifyFailureBudget = 600 * time.Millisecond

		waitCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		waitErr := make(chan error, 1)
		go func() {
			_, err := fx.provider.WaitForFreshHandle(waitCtx, "noop", "run_x/inst1", nil)
			waitErr <- err
		}()
		waitForWaiterRegistered(t, fx.provider, "run_x/inst1")

		// The restarted peer holds a pre-rotation scope instance: its token
		// is valid, but its scope key was never re-registered → the stale-pod
		// shape (CRI-137).
		fx.startPeer(t, &peerOpts{scope: "run_x/old-inst", token: "stale-tok", binary: binary, digest: digest})

		select {
		case err := <-waitErr:
			if err == nil {
				t.Fatal("WaitForFreshHandle succeeded, want the stale-pod budget diagnosis")
			}
			msg := err.Error()
			if !strings.Contains(msg, "is not registered") {
				t.Errorf("diagnosis %q does not name the unregistered scope", msg)
			}
			if !strings.Contains(msg, "stale adapter pod holding a pre-rotation accept token") {
				t.Errorf("diagnosis %q is not the stale-pod class (CRI-137)", msg)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("WaitForFreshHandle did not return within the compressed budget")
		}
	})

	t.Run("fresh peer adopted within budget", func(t *testing.T) {
		fx := startIntegrationFixture(t, integrationOpts{perScope: true, scope: "run_y/inst1", token: "fresh-tok", noPeer: true})
		binary, digest := fxBinaryAndDigest(fx)

		waitCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		handleCh := make(chan adapterhost.Handle, 1)
		errCh := make(chan error, 1)
		go func() {
			h, err := fx.provider.WaitForFreshHandle(waitCtx, "noop", "run_y/inst1", nil)
			if err != nil {
				errCh <- err
				return
			}
			handleCh <- h
		}()
		waitForWaiterRegistered(t, fx.provider, "run_y/inst1")

		// A stale dial lands first (rejected, recorded for diagnosis), then
		// the fresh peer with the current scope+token is adopted — well
		// inside DefaultVerifyFailureBudget.
		fx.startPeer(t, &peerOpts{scope: "run_y/old-inst", token: "fresh-tok", binary: binary, digest: digest})
		fx.startPeer(t, &peerOpts{scope: "run_y/inst1", token: "fresh-tok", binary: binary, digest: digest})

		select {
		case err := <-errCh:
			t.Fatalf("WaitForFreshHandle failed: %v", err)
		case h := <-handleCh:
			if h == nil {
				t.Fatal("adopted nil handle")
			}
		case <-time.After(15 * time.Second):
			t.Fatal("fresh peer not adopted within the budget")
		}
	})
}

// --- scenario 7: teardown window (CRI-287 on the peer path, real child) ---

// TestPeerIntegrationTeardownWindowOnPeerPath drives CRI-287 with a real
// noop child: the same SIGKILL'd child is routed as a timeout teardown
// inside the engine step-timeout window and classifies verbatim from the
// journal wire fact outside it.
func TestPeerIntegrationTeardownWindowOnPeerPath(t *testing.T) {
	t.Run("inside the window: sibling close routed as timeout", func(t *testing.T) {
		fx := startIntegrationFixture(t, integrationOpts{})
		ph := fx.waitForHandle(t, "")
		sm := adapterhost.NewSessionManager(&peerLoader{handle: ph})
		sm.StepTimeoutTeardownWindow = time.Minute
		openCtx, cancelOpen := context.WithTimeout(context.Background(), 15*time.Second)
		if err := sm.Open(openCtx, "noop.develop", "noop", "", nil, nil); err != nil {
			cancelOpen()
			t.Fatalf("sm.Open: %v", err)
		}
		cancelOpen()

		// Step in flight when the engine's step ceiling fires: cancel the
		// turn (the step-timeout cancellation), mark the teardown window, and
		// kill the child — the follow-on sibling close must be routed as a
		// teardown consequence, not a crash.
		stepCtx, cancelStep := context.WithCancel(context.Background())
		stepDone := make(chan error, 1)
		go func() {
			_, err := sm.Execute(stepCtx, "noop.develop", &workflow.StepNode{
				Name:  "develop",
				Input: map[string]string{"delay_ms": "15000"},
			}, &peerEventCollector{})
			stepDone <- err
		}()
		sm.MarkEngineStepTimeoutTeardown()
		cancelStep()
		if err := <-stepDone; err == nil {
			t.Fatal("canceled step returned no error")
		}

		spawned := findSpawned(t, fx.journalEvents())
		if err := syscall.Kill(int(spawned.GetPid()), syscall.SIGKILL); err != nil {
			t.Fatalf("SIGKILL child: %v", err)
		}
		waitFor(t, "child death replays to the host", func() bool {
			return adapterhost.ProcessExited(ph)
		})

		coll := &peerEventCollector{}
		_, err := sm.Execute(context.Background(), "noop.develop", &workflow.StepNode{Name: "develop"}, coll)
		if err == nil {
			t.Fatal("expected the follow-on Execute to fail on the dead child")
		}
		var crashErr *adapterhost.SessionCrashError
		if errors.As(err, &crashErr) {
			t.Fatalf("follow-on Execute err = %v, want the raw transport error inside the teardown window", err)
		}
		if _, ok := firstCrash(coll); ok {
			t.Error("session.crash event must not be emitted inside the teardown window")
		}
	})

	t.Run("outside the window: journal wire fact classifies verbatim", func(t *testing.T) {
		fx := startIntegrationFixture(t, integrationOpts{})
		ph := fx.waitForHandle(t, "")
		sm := adapterhost.NewSessionManager(&peerLoader{handle: ph})
		openCtx, cancelOpen := context.WithTimeout(context.Background(), 15*time.Second)
		if err := sm.Open(openCtx, "noop.develop", "noop", "", nil, nil); err != nil {
			cancelOpen()
			t.Fatalf("sm.Open: %v", err)
		}
		cancelOpen()

		// No teardown mark: the same SIGKILL is a genuine crash.
		spawned := findSpawned(t, fx.journalEvents())
		if err := syscall.Kill(int(spawned.GetPid()), syscall.SIGKILL); err != nil {
			t.Fatalf("SIGKILL child: %v", err)
		}
		waitFor(t, "ProcessExited from journal", func() bool {
			return adapterhost.ProcessExited(ph)
		})
		waitFor(t, "CrashClassified from journal", func() bool {
			r, ok := adapterhost.SupervisionCrashReason(ph)
			return ok && r == adapterhost.CrashReasonProcessTerminated
		})

		coll := &peerEventCollector{}
		_, err := sm.Execute(context.Background(), "noop.develop", &workflow.StepNode{Name: "develop"}, coll)
		var crashErr *adapterhost.SessionCrashError
		if !errors.As(err, &crashErr) || crashErr.Session != "noop.develop" {
			t.Fatalf("Execute err = %v, want SessionCrashError for noop.develop", err)
		}
		event, ok := firstCrash(coll)
		if !ok {
			t.Fatal("expected the session.crash event")
		}
		if got := event["crash_reason"]; got != adapterhost.CrashReasonProcessTerminated {
			t.Errorf("crash_reason = %q, want the journal wire fact verbatim %q", got, adapterhost.CrashReasonProcessTerminated)
		}
	})
}

// --- scenario 8: security posture on the peer path ---

// TestPeerIntegrationSecurityPosture pins the shim's security invariants in
// the integration topology: non-loopback listeners are refused, oversize
// identity frames are rejected on the wire, and token rejections never leak
// the expected token value (the constant-time comparison itself is pinned
// by the shim unit suite; here the observable contract is verified).
func TestPeerIntegrationSecurityPosture(t *testing.T) {
	binary := buildNoopIntegrationBinary(t)
	digest := noopBinaryDigest(t, binary)

	t.Run("non-loopback listener refused", func(t *testing.T) {
		// Auth-less non-loopback listeners are refused at construction; mTLS
		// or an accept token are what make a non-loopback listener legal.
		for _, addr := range []string{"0.0.0.0:0", ":0"} {
			_, err := NewShim(&Config{ListenAddress: addr}, &fixedDigestVerifier{allowed: map[string]string{"noop": digest}})
			if err == nil {
				t.Fatalf("NewShim(%q) accepted an unauthenticated non-loopback listener", addr)
			}
			if !strings.Contains(err.Error(), "non-loopback") {
				t.Errorf("refusal for %q is not the non-loopback auth error: %v", addr, err)
			}
		}
	})

	t.Run("oversize identity frame rejected", func(t *testing.T) {
		fx := startIntegrationFixture(t, integrationOpts{})
		conn, err := net.Dial("tcp", fx.addr)
		if err != nil {
			t.Fatalf("dial shim: %v", err)
		}
		defer conn.Close()
		oversize := bytesRepeat('a', handshakeFrameCap+1)
		if _, err := conn.Write(oversize); err != nil {
			t.Fatalf("write oversize frame: %v", err)
		}
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 64)
		if n, readErr := conn.Read(buf); readErr == nil && n > 0 {
			t.Fatalf("oversize frame was answered with %q, want connection close", buf[:n])
		}
	})

	t.Run("token rejection does not leak the expected token", func(t *testing.T) {
		fx := startIntegrationFixture(t, integrationOpts{perScope: true, scope: "run_z/inst1", token: "the-secret-token"})
		conn, err := net.Dial("tcp", fx.addr)
		if err != nil {
			t.Fatalf("dial shim: %v", err)
		}
		defer conn.Close()
		if _, err := conn.Write(peerIdentityFrame(t, "noop", "run_z/inst1", "wrong-token", digest)); err != nil {
			t.Fatalf("write wrong-token handshake: %v", err)
		}
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		// The shim closes the conn; anything it sent back must not name the
		// configured token.
		buf := make([]byte, 4096)
		n, _ := conn.Read(buf)
		if strings.Contains(string(buf[:n]), "the-secret-token") {
			t.Fatal("shim response leaked the expected token value")
		}
	})
}

// --- fixture helpers ---

// fxBinaryAndDigest returns the fixture's adapter binary and digest for
// spawning additional peers against the same shim verifier.
func fxBinaryAndDigest(fx *integrationFixture) (binary, digest string) {
	return fx.binary, fx.digest
}

// shimVerifier builds a fresh digest verifier matching the fixture's binary
// digest (used when the host listener bounces to a new shim).
func (fx *integrationFixture) shimVerifier() DigestVerifier {
	return &multiDigestVerifier{allowed: map[string]map[string]bool{
		"noop": {fx.digest: true, "sha256:abcd1234": true},
	}}
}

// multiDigestVerifier accepts any of a per-adapter set of digests: real
// peers present the built binary's digest while the fakePeer test adapter
// presents its own pinned digest.
type multiDigestVerifier struct {
	allowed map[string]map[string]bool
}

func (v *multiDigestVerifier) Verify(adapterType, digest string) error {
	if set, ok := v.allowed[adapterType]; ok && set[digest] {
		return nil
	}
	return fmt.Errorf("digest verification failed for adapter %q digest %q", adapterType, digest)
}

// dropPeerSessions emulates a real host bounce: when the host process dies
// its accepted conns die with it. Shim.Stop closes only the listener, so the
// test drops the provider's sessions (closing their conns) explicitly — the
// peer-side observe-EOF-and-reconnect behavior under test is unchanged.
func (fx *integrationFixture) dropPeerSessions(reason string) {
	fx.provider.mu.Lock()
	olds := make([]*peerSession, 0, len(fx.provider.peers))
	for key, ps := range fx.provider.peers {
		olds = append(olds, ps)
		delete(fx.provider.peers, key)
	}
	fx.provider.mu.Unlock()
	for _, ps := range olds {
		ps.close(reason)
	}
}

// findSpawnedNoFatal is the non-fatal form of findSpawned for waitFor.
func findSpawnedNoFatal(events []*criteriav1.SupervisionEvent) (*criteriav1.ProcessSpawned, error) {
	for _, ev := range events {
		if sp := ev.GetSpawned(); sp != nil {
			return sp, nil
		}
	}
	return nil, errors.New("no ProcessSpawned event")
}

// bytesRepeat builds a []byte of n copies of b (test-local helper).
func bytesRepeat(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}
