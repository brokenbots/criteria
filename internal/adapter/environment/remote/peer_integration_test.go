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
	"bytes"
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

// buildConformanceBinary builds a conformance fixture adapter binary and
// returns its path. The peer runtime and the local-parity handle both spawn
// this binary, so both transports exercise the identical adapter
// implementation.
func buildConformanceBinary(t *testing.T, pkg string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve caller path")
	}
	moduleRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", ".."))
	base := filepath.Base(pkg)
	binary := filepath.Join(t.TempDir(), "criteria-adapter-"+base)
	cmd := exec.Command("go", "build", "-o", binary, pkg)
	cmd.Dir = moduleRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", base, err, string(out))
	}
	return binary
}

// buildNoopIntegrationBinary builds the noop conformance adapter and returns
// its path.
func buildNoopIntegrationBinary(t *testing.T) string {
	return buildConformanceBinary(t, "./internal/adapter/conformance/testdata/noop")
}

// buildStatefulIntegrationBinary builds the stateful conformance adapter
// (KB-213 multi-adapter scenarios: a second adapter child next to noop).
func buildStatefulIntegrationBinary(t *testing.T) string {
	return buildConformanceBinary(t, "./internal/adapter/conformance/testdata/stateful")
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
	// scopesDir turns on the runner's scope-token dial shape (KB-213): the
	// peer scans the dir every heartbeat and opens one conn per (scope,
	// adapter) token. The manifest still names the hosted children that
	// every scanned conn serves.
	scopesDir string
	// logger overrides the discard logger for log-assertion tests.
	logger *slog.Logger
	// manifest switches the peer to multi-adapter mode (KB-213): ONE conn
	// hosting every spec as a supervised child. binary/digest stay unset.
	manifest []peer.AdapterSpec
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
	if len(opts.manifest) > 0 {
		pcfg.AdapterName = ""
		pcfg.AdapterBinary = ""
		pcfg.Digest = ""
		pcfg.Adapters = opts.manifest
	}
	if opts.scopesDir != "" {
		pcfg.ScopesDir = opts.scopesDir
	}
	if err := pcfg.Resolve(); err != nil {
		t.Fatalf("peer Config.Resolve: %v", err)
	}
	log := opts.logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
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
	return fx.waitForAdapterHandle(t, "noop", scope)
}

// waitForAdapterHandle is the adapter-parametrized form of waitForHandle
// (KB-213: one env hosts several adapter children behind one conn).
func (fx *integrationFixture) waitForAdapterHandle(t *testing.T, adapterType, scope string) *peerHandle {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	handle, err := fx.provider.WaitForHandle(ctx, adapterType, scope)
	if err != nil {
		t.Fatalf("WaitForHandle(%s, %q): %v", adapterType, scope, err)
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
		}, &peerEventCollector{}, nil)
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
				res, err := h.Execute(context.Background(), "s1", &workflow.StepNode{Name: "develop"}, &peerEventCollector{}, nil)
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
				}, &peerEventCollector{}, nil)
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
	res, err := aware.Execute(awareCtx, "aware-s1", &workflow.StepNode{Name: "develop"}, &peerEventCollector{}, nil)
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
	res, err := ph.Execute(ctx, "s1", &workflow.StepNode{Name: "develop"}, &peerEventCollector{}, nil)
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

	res, err = ph.Execute(ctx, "s1", &workflow.StepNode{Name: "develop"}, &peerEventCollector{}, nil)
	if err != nil || res.Outcome != "success" {
		t.Fatalf("post-idle Execute = (%v, %v), want success on the same session", res, err)
	}
}

// --- scenario 5: reconnect after host listener bounce ---

// TestPeerIntegrationReconnectAfterHostBounce bounces the host listener
// (stop + re-bind on the same address) while the child stays alive,
// SIGKILLs the child during the disconnect window, and asserts the peer
// reconnects to the rebound listener, replays the buffered ProcessExited +
// CrashClassified, and never respawned the child (single ProcessSpawned for
// the fixture lifetime). The backoff+jitter
// timing assertions live in internal/peer/serve_test.go where the sleep and
// rand seams are reachable.
func TestPeerIntegrationReconnectAfterHostBounce(t *testing.T) {
	fx := startIntegrationFixture(t, integrationOpts{})

	ph1 := fx.waitForHandle(t, "")
	waitFor(t, "spawn journal visible", func() bool {
		_, err := findSpawnedNoFatal(fx.journalEvents())
		return err == nil
	})

	// Bounce: stop the current shim and start a fresh shim bound to the
	// SAME address. The peer re-dials the configured host on every round,
	// so the rebound shim takes over the pre-bound host:port without ever
	// mutating cfg.Host after the peer started (the peer goroutine reads
	// it — a post-start write would be a data race).
	bounceAddr := fx.addr
	stopCtx, cancelStop := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelStop()
	if err := fx.shim.Stop(stopCtx); err != nil {
		t.Fatalf("old shim Stop: %v", err)
	}
	// The host's death takes its accepted conns with it; Shim.Stop closes
	// only the listener, so drop the provider's sessions to close the peer
	// conns and trigger the peer's observe-EOF-and-reconnect path.
	fx.dropPeerSessions("host listener bounced")

	newShimCfg := &Config{ListenAddress: bounceAddr, Insecure: true, AcceptToken: fx.token}
	newShim, err := NewShim(newShimCfg, fx.shimVerifier())
	if err != nil {
		t.Fatalf("NewShim (restarted): %v", err)
	}
	startCtx, cancelStart := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelStart()
	if err := newShim.Start(startCtx); err != nil {
		t.Fatalf("restarted shim Start on %s: %v", bounceAddr, err)
	}
	newShim.SetPeerAcceptor(fx.provider)
	fx.mu.Lock()
	fx.shims = append(fx.shims, newShim)
	fx.shim = newShim
	fx.addr = newShim.listener.Addr().String()
	fx.mu.Unlock()

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
		waitForWaiterRegistered(t, fx.provider, "noop", "run_x/inst1")

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
		waitForWaiterRegistered(t, fx.provider, "noop", "run_y/inst1")

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
			}, &peerEventCollector{}, nil)
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
		_, err := sm.Execute(context.Background(), "noop.develop", &workflow.StepNode{Name: "develop"}, coll, nil)
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
		_, err := sm.Execute(context.Background(), "noop.develop", &workflow.StepNode{Name: "develop"}, coll, nil)
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

// --- scenario 9: N>=2 adapter children of one env behind one conn (KB-213) ---

// multiAdapterFixture boots the KB-213 substrate: a shim whose verifier pins
// BOTH conformance binaries, a provider carrying the environment's declared
// adapter set, and manifest-mode peers: one phone-home conn advertises the
// child set, the host routes sessions per adapterType.
type multiAdapterFixture struct {
	fx          *integrationFixture
	noopBin     string
	stateBin    string
	noopDigest  string
	stateDigest string
}

// startMultiAdapterFixture builds both real binaries, pins each adapter's
// digest with its own accepted value, wires the declared-adapter set onto the
// provider, and boots one manifest-mode peer hosting both children.
func startMultiAdapterFixture(t *testing.T, opts integrationOpts) *multiAdapterFixture {
	t.Helper()
	noopBin := buildNoopIntegrationBinary(t)
	stateBin := buildStatefulIntegrationBinary(t)
	noopDigest := noopBinaryDigest(t, noopBin)
	stateDigest := noopBinaryDigest(t, stateBin)
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
	shim, err := NewShim(shimCfg, &multiDigestVerifier{allowed: map[string]map[string]bool{
		"noop":     {noopDigest: true},
		"stateful": {stateDigest: true},
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
	provider.SetDeclaredAdapters([]string{"noop", "stateful"})
	shim.SetPeerAcceptor(provider)
	ma := &multiAdapterFixture{
		fx: &integrationFixture{
			shim:     shim,
			shims:    []*Shim{shim},
			provider: provider,
			addr:     shim.listener.Addr().String(),
			token:    token,
			binary:   noopBin,
			digest:   noopDigest,
		},
		noopBin:     noopBin,
		stateBin:    stateBin,
		noopDigest:  noopDigest,
		stateDigest: stateDigest,
	}
	t.Cleanup(ma.fx.Close)
	if !opts.noPeer {
		ma.bootManifestPeer(t, opts.scope, token)
	}
	return ma
}

// manifestSpecs builds the two-adapters-one-env manifest the peer boots from.
func (ma *multiAdapterFixture) manifestSpecs() []peer.AdapterSpec {
	return []peer.AdapterSpec{
		{Name: "noop", Binary: ma.noopBin, Digest: ma.noopDigest},
		{Name: "stateful", Binary: ma.stateBin, Digest: ma.stateDigest},
	}
}

// bootManifestPeer starts an additional manifest-mode peer against the
// fixture shim.
func (ma *multiAdapterFixture) bootManifestPeer(t *testing.T, scope, token string) *peer.Config {
	t.Helper()
	pcfg, _ := ma.fx.startPeer(t, &peerOpts{scope: scope, token: token, manifest: ma.manifestSpecs()})
	return pcfg
}

// TestPeerIntegrationTwoAdaptersOneEnvOneConn verifies the KB-213 substrate
// end to end with real children: one manifest peer hosts noop + stateful
// behind a single phone-home conn, both host-side handles are adopted from
// that one conn, and session dispatch per adapterType lands on the right
// child (Info echoes each adapter's own name).
func TestPeerIntegrationTwoAdaptersOneEnvOneConn(t *testing.T) {
	ma := startMultiAdapterFixture(t, integrationOpts{})

	phNoop := ma.fx.waitForHandle(t, "")
	phStateful := ma.fx.waitForAdapterHandle(t, "stateful", "")

	infoNoop, err := phNoop.Info(context.Background())
	if err != nil {
		t.Fatalf("noop Info: %v", err)
	}
	if infoNoop.Name != "noop" {
		t.Fatalf("noop handle routed to child %q", infoNoop.Name)
	}
	infoState, err := phStateful.Info(context.Background())
	if err != nil {
		t.Fatalf("stateful Info: %v", err)
	}
	if infoState.Name != "stateful" {
		t.Fatalf("stateful handle routed to child %q", infoState.Name)
	}

	// Both children accept sessions over the same conn.
	if err := phNoop.OpenSession(context.Background(), "s-noop", nil, nil); err != nil {
		t.Fatalf("noop OpenSession: %v", err)
	}
	if err := phStateful.OpenSession(context.Background(), "s-stateful", nil, nil); err != nil {
		t.Fatalf("stateful OpenSession: %v", err)
	}
}

// TestPeerIntegrationMissingManifestAdapterFailClosed verifies the host's
// fail-closed acceptance with real peers: when a peer's manifest does not
// cover the environment's declared adapter set, its phone-home conn is
// rejected on every dial (no session is ever adopted) until a fully-covered
// manifest dials.
func TestPeerIntegrationMissingManifestAdapterFailClosed(t *testing.T) {
	ma := startMultiAdapterFixture(t, integrationOpts{noPeer: true})
	fx := ma.fx

	// The under-covered peer: manifest hosts only noop.
	fx.startPeer(t, &peerOpts{manifest: []peer.AdapterSpec{
		{Name: "noop", Binary: ma.noopBin, Digest: ma.noopDigest},
	}})

	// Several re-dial cycles at the 50-200ms backoff must leave the
	// registry empty: every dial is rejected before adoption.
	time.Sleep(1200 * time.Millisecond)
	fx.mu.Lock()
	adopted := len(fx.provider.peers)
	fx.mu.Unlock()
	if adopted != 0 {
		t.Fatalf("under-covered peer adopted while missing declared adapters (%d sessions)", adopted)
	}

	// Recovery: a manifest peer covering the declared set is adopted for
	// both adapters.
	ma.bootManifestPeer(t, "", fx.token)
	phStateful := ma.fx.waitForAdapterHandle(t, "stateful", "")
	if info, err := phStateful.Info(context.Background()); err != nil || info.Name != "stateful" {
		t.Fatalf("recovered stateful dispatch: info=%v err=%v", info, err)
	}
}

// TestPeerIntegrationMultiAdapterPerScopeRotation verifies the scope-SET
// model (CRI-137/304 sweep classes) with the multi-adapter manifest: a
// per-scope manifest peer is adopted for both adapters, a restarted peer
// holding a stale scope instance is diagnosed, and the fresh scoped dial is
// adopted for the full child set.
func TestPeerIntegrationMultiAdapterPerScopeRotation(t *testing.T) {
	t.Run("manifest peer adopted per scope for both adapters", func(t *testing.T) {
		ma := startMultiAdapterFixture(t, integrationOpts{perScope: true, scope: "run_z/inst1"})
		phNoop := ma.fx.waitForAdapterHandle(t, "noop", "run_z/inst1")
		phStateful := ma.fx.waitForAdapterHandle(t, "stateful", "run_z/inst1")
		if phNoop == nil || phStateful == nil {
			t.Fatal("nil handle")
		}
	})

	t.Run("stale scope diagnosed, fresh scope adopted", func(t *testing.T) {
		ma := startMultiAdapterFixture(t, integrationOpts{perScope: true, scope: "run_w/inst1", noPeer: true})
		fx := ma.fx
		fx.shim.verifyFailureBudget = 600 * time.Millisecond

		waitCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		waitErr := make(chan error, 1)
		go func() {
			_, err := fx.provider.WaitForFreshHandle(waitCtx, "stateful", "run_w/inst1", nil)
			waitErr <- err
		}()
		waitForWaiterRegistered(t, fx.provider, "stateful", "run_w/inst1")

		// A manifest peer whose scope was never re-registered after the
		// runner restart is the CRI-137 stale-pod class.
		fx.startPeer(t, &peerOpts{scope: "run_w/old-inst", token: fx.token, manifest: ma.manifestSpecs()})

		select {
		case err := <-waitErr:
			if err == nil {
				t.Fatal("WaitForFreshHandle succeeded, want the stale-pod diagnosis")
			}
			if !strings.Contains(err.Error(), "is not registered") {
				t.Errorf("diagnosis %q does not name the unregistered scope", err.Error())
			}
		case <-time.After(10 * time.Second):
			t.Fatal("stale scope not diagnosed within budget")
		}

		// The fresh scoped dial is adopted for the full child set.
		fx.startPeer(t, &peerOpts{scope: "run_w/inst1", token: fx.token, manifest: ma.manifestSpecs()})
		ctx, cancelAdopt := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancelAdopt()
		for _, adapterType := range []string{"stateful", "noop"} {
			if _, err := fx.provider.WaitForFreshHandle(ctx, adapterType, "run_w/inst1", nil); err != nil {
				t.Fatalf("adopted %s handle: %v", adapterType, err)
			}
		}
	})
}

// syncedBuffer is a concurrency-safe slog sink: the peer server logs from
// its own goroutines, so log assertions race without the mutex.
type syncedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// scopesInstA/B are fixed, UUID-valid instance directories the scopes-dir
// tests write token files into (the scanner ignores non-UUID instance dirs).
const (
	scopesInstA = "8a2bb7d7-6a4b-4ab1-9c8a-2f5d3a4b1c01"
	scopesInstB = "4b5d6d29-3ac2-4e11-a5d8-1f0e2b3c4d99"
)

// writeScopesTokens lays out one instance's token files in the runner's
// remote-tokens shape: "<dir>/<scopeLabel>/<instance>/<adapter>.token",
// with an empty scope label meaning the run's root scope. Returns the scope
// string the scanner produces for the instance.
func writeScopesTokens(t *testing.T, dir, scopeLabel, instance, token string, adapters ...string) string {
	t.Helper()
	instDir := filepath.Join(dir, scopeLabel, instance)
	if err := os.MkdirAll(instDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", instDir, err)
	}
	for _, adapter := range adapters {
		if err := os.WriteFile(filepath.Join(instDir, adapter+".token"), []byte(token+"\n"), 0o600); err != nil {
			t.Fatalf("write %s token: %v", adapter, err)
		}
	}
	return scopeLabel + "/" + instance
}

// dropRegistryConn closes the live conn of a registry session to force
// conn-death + peer redial; used to check whether the scope-set supervisor
// would (re-)adopt a scope.
func dropRegistryConn(t *testing.T, p *peerSessionProvider, adapterType, scope string) {
	t.Helper()
	p.mu.Lock()
	ps, ok := p.peers[p.key(adapterType, scope)]
	p.mu.Unlock()
	if !ok {
		t.Fatalf("no registry session for (%s, %s) to drop", adapterType, scope)
	}
	ps.conn.Close()
}

// TestPeerIntegrationScopesDirDispatchPerAdapterType drives the real peer
// through the runner's scope-token dir (CRITERIA_REMOTE_SCOPES_DIR) with
// both adapter tokens present from boot: one phone-home conn per (scope,
// adapter) token, both handles adopted, and session dispatch per
// adapterType lands on the child whose identity matches — Info echoes each
// child's own name (KB-213 scope-set integration).
func TestPeerIntegrationScopesDirDispatchPerAdapterType(t *testing.T) {
	t.Setenv("CRITERIA_HEARTBEAT_INTERVAL", "200ms")
	scopesDir := t.TempDir()
	scope := writeScopesTokens(t, scopesDir, "run_d", scopesInstA, "tok-scoped", "noop", "stateful")
	ma := startMultiAdapterFixture(t, integrationOpts{perScope: true, scope: scope, token: "tok-scoped", noPeer: true})
	ma.fx.startPeer(t, &peerOpts{
		scopesDir: scopesDir,
		scope:     scope,
		token:     "tok-scoped",
		manifest:  ma.manifestSpecs(),
	})

	phNoop := ma.fx.waitForAdapterHandle(t, "noop", scope)
	phStateful := ma.fx.waitForAdapterHandle(t, "stateful", scope)
	if phNoop == nil || phStateful == nil {
		t.Fatal("nil handle")
	}
	if info, err := phNoop.Info(context.Background()); err != nil || info.Name != "noop" {
		t.Fatalf("noop dispatch: info=%+v err=%v", info, err)
	}
	if info, err := phStateful.Info(context.Background()); err != nil || info.Name != "stateful" {
		t.Fatalf("stateful dispatch: info=%+v err=%v", info, err)
	}
	if err := phNoop.OpenSession(context.Background(), "s-d-noop", nil, nil); err != nil {
		t.Fatalf("noop OpenSession: %v", err)
	}
	if err := phStateful.OpenSession(context.Background(), "s-d-stateful", nil, nil); err != nil {
		t.Fatalf("stateful OpenSession: %v", err)
	}
}

// TestPeerIntegrationScopesDirRoutedWakeDispatchesOnTargetChild is the
// regression test for the finding the reviewer blocked on: with
// CRITERIA_REMOTE_SCOPES_DIR and only the noop token present, the host's
// pending stateful wait is drained by the reconnecting noop conn (the
// scoped dial of that scope). The re-accepted conn must honor the host's
// x-criteria-adapter route header and execute the stateful session on the
// STATEFUL child — no session may run on a non-target child.
func TestPeerIntegrationScopesDirRoutedWakeDispatchesOnTargetChild(t *testing.T) {
	t.Setenv("CRITERIA_HEARTBEAT_INTERVAL", "200ms")
	scopesDir := t.TempDir()
	scope := writeScopesTokens(t, scopesDir, "run_w2", scopesInstA, "tok-wake", "noop")
	ma := startMultiAdapterFixture(t, integrationOpts{perScope: true, scope: scope, token: "tok-wake", noPeer: true})

	// Register the stateful wait BEFORE any conn is accepted: with only the
	// noop token scanned, nothing dials stateful. The noop conn's accept
	// then drains the pending wait via collectRoutedWaiters — the exact
	// drain that mis-dispatched sessions (ignoring the x-criteria-adapter
	// route header) before per-scope conns served the full hosted set.
	waitCtx, cancelWait := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelWait()
	waitDone := make(chan adapterhost.Handle, 1)
	waitFailed := make(chan error, 1)
	go func() {
		handle, err := ma.fx.provider.WaitForHandle(waitCtx, "stateful", scope)
		if err != nil {
			waitFailed <- err
			return
		}
		waitDone <- handle
	}()
	waitForWaiterRegistered(t, ma.fx.provider, "stateful", scope)

	// The noop conn dials now: its accept must adopt a routed stateful
	// handle over its conn, carried into the stateful child by the connMux.
	ma.fx.startPeer(t, &peerOpts{
		scopesDir: scopesDir,
		scope:     scope,
		token:     "tok-wake",
		manifest:  ma.manifestSpecs(),
	})

	var routed adapterhost.Handle
	select {
	case routed = <-waitDone:
	case err := <-waitFailed:
		t.Fatalf("stateful wait after the noop conn accepted: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("pending stateful wait was never drained from the noop conn accept")
	}
	phRouted, ok := routed.(*peerHandle)
	if !ok {
		t.Fatalf("routed handle type %T", routed)
	}
	if info, err := phRouted.Info(context.Background()); err != nil || info.Name != "stateful" {
		t.Fatalf("routed wait executed on child %q (err=%v): session dispatch crossed adapters", info.Name, err)
	}
	if err := phRouted.OpenSession(context.Background(), "s-wake-stateful", nil, nil); err != nil {
		t.Fatalf("stateful OpenSession over the routed conn: %v", err)
	}

	// The dial child is unaffected: its own handle dispatches on the noop
	// child.
	phNoop := ma.fx.waitForAdapterHandle(t, "noop", scope)
	if info, err := phNoop.Info(context.Background()); err != nil || info.Name != "noop" {
		t.Fatalf("noop dispatch: info=%+v err=%v", info, err)
	}
}

// TestPeerIntegrationScopesDirRefusesUnhostedAdapterLoudly verifies that a
// token naming an adapter the peer does not host is refused loudly (logged
// with the adapter and scope) instead of being silently served, while the
// healthy conns are unaffected.
func TestPeerIntegrationScopesDirRefusesUnhostedAdapterLoudly(t *testing.T) {
	t.Setenv("CRITERIA_HEARTBEAT_INTERVAL", "200ms")
	scopesDir := t.TempDir()
	scope := writeScopesTokens(t, scopesDir, "run_g", scopesInstA, "tok-ghost", "noop", "ghost")
	capture := &syncedBuffer{}
	ma := startMultiAdapterFixture(t, integrationOpts{perScope: true, scope: scope, token: "tok-ghost", noPeer: true})
	ma.fx.startPeer(t, &peerOpts{
		scopesDir: scopesDir,
		scope:     scope,
		token:     "tok-ghost",
		manifest:  ma.manifestSpecs(),
		logger:    slog.New(slog.NewTextHandler(capture, nil)),
	})

	phNoop := ma.fx.waitForAdapterHandle(t, "noop", scope)
	if info, err := phNoop.Info(context.Background()); err != nil || info.Name != "noop" {
		t.Fatalf("noop dispatch: info=%+v err=%v", info, err)
	}

	waitFor(t, "ghost refusal logged", func() bool {
		text := capture.String()
		return strings.Contains(text, "does not host") && strings.Contains(text, "adapter=ghost")
	})

	// The ghost adapter stays unadopted across several re-scan + re-dial
	// cycles: exactly one session (the healthy noop conn) is in the
	// registry, never a ghost session.
	time.Sleep(700 * time.Millisecond)
	ma.fx.provider.mu.Lock()
	adopted := len(ma.fx.provider.peers)
	ma.fx.provider.mu.Unlock()
	if adopted != 1 {
		t.Fatalf("registry sessions: want the single noop conn, got %d", adopted)
	}
}

// TestPeerIntegrationScopesDirRotationConvergence verifies the scope-SET
// registration + rotation classes (CRI-137/304): a newly written instance's
// tokens are adopted while the stale instance stays live, and after the
// stale instance dirs are removed and its conns dropped the supervisor
// never re-adopts the stale scopes.
func TestPeerIntegrationScopesDirRotationConvergence(t *testing.T) {
	t.Setenv("CRITERIA_HEARTBEAT_INTERVAL", "200ms")
	scopesDir := t.TempDir()
	scopeOld := writeScopesTokens(t, scopesDir, "run_r", scopesInstA, "tok-old", "noop", "stateful")
	ma := startMultiAdapterFixture(t, integrationOpts{perScope: true, scope: scopeOld, token: "tok-old", noPeer: true})
	ma.fx.startPeer(t, &peerOpts{
		scopesDir: scopesDir,
		scope:     scopeOld,
		token:     "tok-old",
		manifest:  ma.manifestSpecs(),
	})

	phNoopOld := ma.fx.waitForAdapterHandle(t, "noop", scopeOld)
	if info, err := phNoopOld.Info(context.Background()); err != nil || info.Name != "noop" {
		t.Fatalf("stale instance noop dispatch: info=%+v err=%v", info, err)
	}
	ma.fx.waitForAdapterHandle(t, "stateful", scopeOld)

	// Runner restart: the fresh instance's tokens appear, the stale
	// instance's dirs vanish. The supervisor adopts the fresh tokens while
	// the stale-instance conns keep serving until they die.
	scopeNew := writeScopesTokens(t, scopesDir, "run_r", scopesInstB, "tok-new", "noop", "stateful")
	ma.fx.shim.RegisterScope(scopeNew, "tok-new")
	if err := os.RemoveAll(filepath.Join(scopesDir, "run_r", scopesInstA)); err != nil {
		t.Fatalf("remove stale instance dir: %v", err)
	}

	phNoopNew := ma.fx.waitForAdapterHandle(t, "noop", scopeNew)
	if info, err := phNoopNew.Info(context.Background()); err != nil || info.Name != "noop" {
		t.Fatalf("fresh instance noop dispatch: info=%+v err=%v", info, err)
	}
	phStatefulNew := ma.fx.waitForAdapterHandle(t, "stateful", scopeNew)
	if info, err := phStatefulNew.Info(context.Background()); err != nil || info.Name != "stateful" {
		t.Fatalf("fresh instance stateful dispatch: info=%+v err=%v", info, err)
	}

	// Once the stale instance conns die, the stale tokens (removed from the
	// dir) must NOT be re-adopted: dropping each stale conn must leave the
	// registry without their keys across several re-scan + re-dial windows.
	dropRegistryConn(t, ma.fx.provider, "noop", scopeOld)
	dropRegistryConn(t, ma.fx.provider, "stateful", scopeOld)
	time.Sleep(1200 * time.Millisecond)
	ma.fx.provider.mu.Lock()
	staleNoop := ma.fx.provider.peers[ma.fx.provider.key("noop", scopeOld)]
	staleStateful := ma.fx.provider.peers[ma.fx.provider.key("stateful", scopeOld)]
	ma.fx.provider.mu.Unlock()
	if staleNoop != nil {
		t.Fatalf("stale scope %q re-adopted for noop after its token vanished", scopeOld)
	}
	if staleStateful != nil {
		t.Fatalf("stale scope %q re-adopted for stateful after its token vanished", scopeOld)
	}

	// The fresh instance keeps serving.
	if info, err := phNoopNew.Info(context.Background()); err != nil || info.Name != "noop" {
		t.Fatalf("fresh instance noop dispatch after stale teardown: info=%+v err=%v", info, err)
	}
}
