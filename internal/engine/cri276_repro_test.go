package engine

// Regression tests for CRI-276: a remote adapter session that goes idle while
// other sessions work must survive the idle period and complete its next step
// without a transport error. The production chain is: Verify binds a temporary
// handle (killed by design once verification completes, so the pod re-dials —
// CRI-115/CRI-271), the first Sessions.Execute binds the verified session and
// stores sess.handle, and every later step reuses that stored handle directly.
// The failure mechanism: nothing kept the phone-home connection warm, so an
// idle-closing middlebox (NAT/conntrack) drops it, the shim's bridge EOFs, the
// reattach client transport dies, and the next step on the stored handle fails
// with "gRPC client transport closed (adapter or shim closed the connection)".
//
// The pod-side gRPC server (criteria-adapter-remote-runner serve_remote) is
// configured to ping idle phone-home connections; with that keepalive policy
// the connection carries traffic every ~1s (grpc-go's server ping floor) and
// survives the middlebox indefinitely, so the stored handle stays usable and
// the second step succeeds — remote adapters behave as local ones, which never
// die from idleness.

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapter/environment/remote"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// idleProxy forwards TCP between a local listener and a target, closing both
// directions whenever either side sends nothing for idleWindow. Local copy of
// the adapterhost test's middlebox: test helpers are not importable across
// packages.
type idleProxy struct {
	ln         net.Listener
	target     string
	idleWindow time.Duration

	mu    chan struct{} // guards conns+done below
	conns map[net.Conn]struct{}
	done  bool
}

func newIdleProxy(t *testing.T, target string, idleWindow time.Duration) *idleProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	p := &idleProxy{ln: ln, target: target, idleWindow: idleWindow, mu: make(chan struct{}, 1), conns: map[net.Conn]struct{}{}}
	go p.acceptLoop()
	t.Cleanup(p.Close)
	return p
}

func (p *idleProxy) addr() string { return p.ln.Addr().String() }

func (p *idleProxy) acceptLoop() {
	for {
		downstream, err := p.ln.Accept()
		if err != nil {
			return
		}
		upstream, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = downstream.Close()
			continue
		}
		go func() {
			go p.pump(upstream, downstream)
			p.pump(downstream, upstream)
		}()
	}
}

func (p *idleProxy) pump(dst, src net.Conn) {
	p.mu <- struct{}{}
	if p.done {
		<-p.mu
		_ = dst.Close()
		_ = src.Close()
		return
	}
	p.conns[dst] = struct{}{}
	p.conns[src] = struct{}{}
	<-p.mu

	defer func() { _ = dst.Close(); _ = src.Close() }()
	buf := make([]byte, 32*1024)
	for {
		_ = src.SetReadDeadline(time.Now().Add(p.idleWindow))
		n, err := src.Read(buf)
		if err != nil {
			return
		}
		if _, err := dst.Write(buf[:n]); err != nil {
			return
		}
	}
}

func (p *idleProxy) Close() {
	p.mu <- struct{}{}
	defer func() { <-p.mu }()
	if p.done {
		return
	}
	p.done = true
	_ = p.ln.Close()
	for c := range p.conns {
		_ = c.Close()
	}
}

// cri276PodServer is the minimal v2.AdapterServiceServer the adapter pod runs
// over its phone-home connection: enough surface for Verify (Info) and the
// engine's step path (OpenSession + Execute). It counts the RPCs it served so
// tests can prove traffic actually crossed the bridge.
type cri276PodServer struct {
	v2.UnimplementedAdapterServiceServer
	name, version string
	infoCalls     atomic.Int64
	execCalls     atomic.Int64
}

func (s *cri276PodServer) Info(context.Context, *v2.InfoRequest) (*v2.InfoResponse, error) {
	s.infoCalls.Add(1)
	return &v2.InfoResponse{Name: s.name, Version: s.version, Capabilities: []string{"execute"}}, nil
}

func (s *cri276PodServer) OpenSession(context.Context, *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	return &v2.OpenSessionResponse{}, nil
}

func (s *cri276PodServer) Execute(req *v2.ExecuteRequest, stream v2.AdapterService_ExecuteServer) error {
	s.execCalls.Add(1)
	ev, err := v2.NewExecuteResultEvent("success", map[string]any{"session": req.GetSessionId(), "step": req.GetStepName()})
	if err != nil {
		return err
	}
	return stream.Send(ev)
}

// podHomeLoop mimics criteria-adapter-remote-runner's phone-home behavior: it
// dials through proxyAddr with the handshake frame, serves the adapter server
// on the accepted connection, and re-dials whenever the connection dies.
// accepted counts completed handshakes; a count above one proves the
// phone-home connection was torn down mid-test.
func podHomeLoop(proxyAddr string, hs *cri137Handshake, stop <-chan struct{}, done chan<- struct{}, accepted *atomic.Int64, server v2.AdapterServiceServer, serverOpts []grpc.ServerOption) {
	defer close(done)
	for {
		select {
		case <-stop:
			return
		default:
		}
		conn, err := net.Dial("tcp", proxyAddr)
		if err != nil {
			if !podHomeSleep(stop, 20*time.Millisecond) {
				return
			}
			continue
		}
		hsBytes, err := json.Marshal(hs)
		if err != nil {
			_ = conn.Close()
			return
		}
		if _, err := conn.Write(append(hsBytes, '\n')); err != nil {
			_ = conn.Close()
			if !podHomeSleep(stop, 20*time.Millisecond) {
				return
			}
			continue
		}

		observed := &countingConn{Conn: conn}
		grpcServer := grpc.NewServer(serverOpts...)
		v2.RegisterAdapterServiceServer(grpcServer, server)
		serveDone := make(chan struct{})
		go func() {
			defer close(serveDone)
			_ = grpcServer.Serve(&cri137SingleConnListener{conn: observed})
		}()

		// Wait for bridge traffic (handshake accepted) or connection death
		// (rejected handshake), bounded like a real reconnect loop.
		handshakeAccepted := false
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if observed.received.Load() {
				handshakeAccepted = true
				break
			}
			if observed.closed.Load() {
				break
			}
			if !podHomeSleep(stop, 5*time.Millisecond) {
				grpcServer.Stop()
				_ = conn.Close()
				return
			}
		}
		if handshakeAccepted {
			accepted.Add(1)
			// Serve until the phone-home connection dies (bridge teardown) or
			// the test ends. A closed conn is what sends the loop back around.
			for {
				if observed.closed.Load() {
					break
				}
				if !podHomeSleep(stop, 10*time.Millisecond) {
					grpcServer.Stop()
					_ = conn.Close()
					return
				}
			}
		}
		grpcServer.Stop()
		_ = conn.Close()
		<-serveDone
	}
}

// podHomeSleep waits for d or until stop closes; it reports whether the wait
// completed normally.
func podHomeSleep(stop <-chan struct{}, d time.Duration) bool {
	select {
	case <-stop:
		return false
	case <-time.After(d):
		return true
	}
}

// cri276AdapterEvents is a minimal adapter.EventSink for the step path; the
// lifecycle assertions use the separate engine sink.
type cri276AdapterEvents struct{}

func (cri276AdapterEvents) Log(string, []byte)  {}
func (cri276AdapterEvents) Adapter(string, any) {}

// idleChain is a fully wired idle-survival chain: a real shim, a per-scope
// remote adapter session verified through an idle-closing middlebox, and an
// adapter pod phone-homing through that middlebox. By the time runIdleChain
// returns, verification completed, the temporary verify handle was killed (by
// design), and the pod's re-dial settled — the state the engine is in before
// its first step binds the session.
type idleChain struct {
	sessions *adapterhost.SessionManager
	pod      *cri276PodServer
	accepted *atomic.Int64
}

func (c *idleChain) acceptCount() int64 { return c.accepted.Load() }

func runIdleChain(t *testing.T, podServerOpts []grpc.ServerOption) *idleChain {
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

	sessions := adapterhost.NewSessionManager(&fakeLoader{})
	sessions.SetGraph(g)
	sessions.SetRemoteShim(realShim)
	sink := &eventTrackingSink{}
	lifecycle := newScopeLifecycleState(dataDir)
	lifecycle.setRunID("run-123")
	rlc := &remoteLifecycleContext{scopeLifecycle: lifecycle}
	deps := Deps{Sessions: sessions, Sink: sink}

	proxy := newIdleProxy(t, realShim.ListenAddr(), 1500*time.Millisecond)

	// Verify blocks on the shim until the pod handshakes; the token is
	// persisted and provision_wanted is emitted before that block (CRI-115),
	// so init runs in a goroutine and the test polls for the event.
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
	go podHomeLoop(proxy.addr(), hs, stopDial, podDone, accepted, pod, podServerOpts)
	t.Cleanup(func() {
		close(stopDial)
		<-podDone
	})
	// Shut the manager down first (LIFO): it closes bound sessions through the
	// still-live shim, so no handle outlives the test (goleak).
	t.Cleanup(func() {
		_ = sessions.Shutdown(context.Background())
	})

	if err := <-initDone; err != nil {
		t.Fatalf("initScopeAdapters: %v", err)
	}

	// Verify's temporary handle is killed once verification completes (by
	// design), which tears the first phone-home connection down and makes the
	// pod re-dial. Wait for the re-dial to settle so the session the engine is
	// about to bind resolves to the stable, post-verify connection.
	acceptedDeadline := time.Now().Add(5 * time.Second)
	for c := accepted.Load(); c < 2; c = accepted.Load() {
		if time.Now().After(acceptedDeadline) {
			t.Fatalf("pod completed %d handshakes, want 2 after verify (verify handshake + re-dial)", c)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return &idleChain{sessions: sessions, pod: pod, accepted: accepted}
}

// idleExecCycle drives the engine's real step path across an idle window:
// execute a step (binding the verified session and storing sess.handle),
// sit fully idle, then execute a second step against the same session —
// exactly what the engine does between two steps of a long run.
type idleExecCycle struct {
	chain          *idleChain
	firstResult    adapter.Result
	firstErr       error
	secondResult   adapter.Result
	secondErr      error
	acceptedBefore int64
	acceptedAfter  int64
}

func runIdleExecCycle(t *testing.T, podServerOpts []grpc.ServerOption) idleExecCycle {
	t.Helper()
	chain := runIdleChain(t, podServerOpts)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	events := cri276AdapterEvents{}
	step := &workflow.StepNode{Name: "idle-step"}

	res1, err1 := chain.sessions.Execute(ctx, "noop.default", step, events)
	before := chain.acceptCount()

	// Fully idle: no RPCs on any hop. Only pod-side keepalive pings (if any)
	// generate traffic across the middlebox.
	time.Sleep(3500 * time.Millisecond)

	res2, err2 := chain.sessions.Execute(ctx, "noop.default", step, events)
	return idleExecCycle{
		chain:          chain,
		firstResult:    res1,
		firstErr:       err1,
		secondResult:   res2,
		secondErr:      err2,
		acceptedBefore: before,
		acceptedAfter:  chain.acceptCount(),
	}
}

// TestRemoteSessionIdleSurvivesWithKeepalives: with the pod-side keepalive
// policy (compressed to grpc-go's 1s server ping floor), a phone-home
// connection that sits fully idle for longer than the middlebox's idle window
// survives, and the second step on the stored session handle completes
// without transport error — no pod re-dial, remote behaves as local.
func TestRemoteSessionIdleSurvivesWithKeepalives(t *testing.T) {
	opts := adapterhost.KeepaliveServerOptionsFor(
		keepalive.ServerParameters{Time: time.Second, Timeout: 500 * time.Millisecond},
		keepalive.EnforcementPolicy{MinTime: 50 * time.Millisecond, PermitWithoutStream: true},
	)
	c := runIdleExecCycle(t, opts)

	if c.firstErr != nil {
		t.Fatalf("first step failed (session bind): %v", c.firstErr)
	}
	if c.firstResult.Outcome != "success" {
		t.Fatalf("first step outcome = %q, want success", c.firstResult.Outcome)
	}
	if c.secondErr != nil {
		t.Fatalf("second step failed after idle: the adapter session did not survive idleness: %v", c.secondErr)
	}
	if c.secondResult.Outcome != "success" {
		t.Fatalf("second step outcome = %q, want success", c.secondResult.Outcome)
	}
	if c.acceptedAfter != c.acceptedBefore {
		t.Fatalf("pod completed %d handshakes after the first step, want %d: the phone-home connection was closed during the idle period", c.acceptedAfter, c.acceptedBefore)
	}
	if got := c.chain.pod.execCalls.Load(); got != 2 {
		t.Fatalf("pod served %d Execute calls, want 2", got)
	}
}

// TestRemoteSessionIdleDiesWithoutKeepalives is the negative control: with a
// default pod-side server (no keepalive pings), the middlebox kills the idle
// phone-home connection. The first step succeeds (the bind happens before the
// idle), but the second step on the stored handle fails exactly as it did in
// production ("gRPC client transport closed"), while the pod's reconnect loop
// re-handshakes — the CRI-271 reopen machinery restores serviceability on a
// fresh handle, but the step that reached for the old one already failed.
func TestRemoteSessionIdleDiesWithoutKeepalives(t *testing.T) {
	c := runIdleExecCycle(t, nil)

	if c.firstErr != nil {
		t.Fatalf("first step failed before the idle period: %v", c.firstErr)
	}
	if c.firstResult.Outcome != "success" {
		t.Fatalf("first step outcome = %q, want success", c.firstResult.Outcome)
	}
	// The second step must fail: this is the production failure signature.
	// The exact grpc error text varies by failure point, so pin the behavior,
	// not the wording.
	if c.secondErr == nil {
		t.Fatal("second step succeeded after the phone-home connection died; expected a transport error")
	}
	if c.acceptedAfter <= c.acceptedBefore {
		t.Fatalf("pod handshakes went from %d to %d: the reconnect loop did not re-handshake after the teardown", c.acceptedBefore, c.acceptedAfter)
	}
}
