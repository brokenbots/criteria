package engine

// remote_session_lifecycle_test.go — regression and lifecycle tests for remote adapter sessions:
//
// 1. CRI-271: Session crash repair and reopen semantics.
//    A live adapter session dying mid-turn (shim Canceled / plugin stdio EOF after
//    long LLM streaming turns) must not silently tear down the run's bookkeeping.
//    The engine re-opens a crashed session before a follow-on step executes on it.
//
// 2. CRI-276: Idle survival and keepalives.
//    A remote adapter session that goes idle while other sessions work must survive
//    the idle period without transport errors. Keepalive pings keep middleboxes
//    from dropping the connection.
//
// 3. CRI-287: Step timeout teardown cascades and crash classification.
//    An engine-initiated step-timeout cancellation (CRI-275 ceiling) tears down
//    sibling phone-home transports. Follow-on steps within the teardown window
//    route through declared outcomes rather than being misclassified as an
//    unprompted session crash.
//
// 4. KB-257: Detached session re-entry and lifecycle bookkeeping.
//    Re-entering a per-scope remote session within a single run reuses the detached
//    session without re-provisioning, releasing once on run-final teardown.

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	criteriav2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapter/environment/remote"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// -----------------------------------------------------------------------------
// CRI-271: Session Crash Repair & Reopening
// -----------------------------------------------------------------------------

const cri271CrashErr = "rpc error: code = Canceled desc = grpc: the client connection is closing"

const cri271Workflow = `
workflow {
  name = "cri271"
  version = "0.1"
  initial_state = "develop"
  target_state  = "awaiting_human"
}
step "develop" {
  target = adapter.fake
  outcome "success" { next = state.awaiting_human }
  outcome "failure" { next = step.comment_handler_failed }
}
step "comment_handler_failed" {
  target = adapter.fake
  outcome "success" { next = step.set_review_state }
  outcome "error" { next = step.set_review_state }
}
step "set_review_state" {
  target = adapter.fake
  outcome "success" { next = state.awaiting_human }
  outcome "error" { next = state.awaiting_human }
}
state "awaiting_human" {
  terminal = true
  success  = true
}`

type cri271Adapter struct {
	*fakeAdapter
	crashStep string

	mu       sync.Mutex
	opens    []string
	executes []string
	live     bool
}

func newCri271Adapter(crashStep string) *cri271Adapter {
	return &cri271Adapter{fakeAdapter: &fakeAdapter{name: "fake"}, crashStep: crashStep}
}

func (a *cri271Adapter) OpenSession(_ context.Context, name string, _, _ map[string]string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.opens = append(a.opens, name)
	a.live = true
	return nil
}

func (a *cri271Adapter) Execute(_ context.Context, name string, step *workflow.StepNode, _ adapter.EventSink, rejection *criteriav2.ExecutionRejection) (adapter.Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	stepName := ""
	if step != nil {
		stepName = step.Name
	}
	a.executes = append(a.executes, stepName)
	if !a.live {
		return adapter.Result{}, errors.New(cri271CrashErr)
	}
	if stepName == a.crashStep {
		a.live = false
		return adapter.Result{}, errors.New(cri271CrashErr)
	}
	return adapter.Result{Outcome: "success"}, nil
}

func (a *cri271Adapter) callLog() (opens, executes []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.opens...), append([]string(nil), a.executes...)
}

type cri271Sink struct {
	*fakeSink

	mu     sync.Mutex
	events []cri271Event
}

type cri271Event struct {
	kind string
	data map[string]any
}

func (s *cri271Sink) StepEventSink(string) adapter.EventSink {
	return &cri271EventRecorder{parent: s}
}

type cri271EventRecorder struct {
	parent *cri271Sink
}

func (r *cri271EventRecorder) Log(string, []byte) {}
func (r *cri271EventRecorder) Adapter(kind string, data any) {
	payload, _ := data.(map[string]any)
	r.parent.mu.Lock()
	defer r.parent.mu.Unlock()
	r.parent.events = append(r.parent.events, cri271Event{kind: kind, data: payload})
}

func (s *cri271Sink) recorded(kind string) (map[string]any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, evt := range s.events {
		if evt.kind == kind {
			return evt.data, true
		}
	}
	return nil, false
}

func cri271NewLoader(p adapterhost.Handle) *fakeLoader {
	return &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake":         p,
		"fake.default": p,
	}}
}

func TestRemoteSession_FunctionalCrashReopensSessionForBookkeeping(t *testing.T) {
	g := compile(t, cri271Workflow)
	p := newCri271Adapter("develop")
	sink := &cri271Sink{fakeSink: &fakeSink{}}
	if err := NewTestEngine(g, cri271NewLoader(p), sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "awaiting_human" || !sink.terminalOK {
		t.Errorf("terminal state %q success=%v; want awaiting_human/true", sink.terminal, sink.terminalOK)
	}

	opens, executes := p.callLog()
	wantOpens := []string{"fake.default", "fake.default"}
	if len(opens) != len(wantOpens) {
		t.Fatalf("OpenSession calls: %v; want %v", opens, wantOpens)
	}
	for i, want := range wantOpens {
		if opens[i] != want {
			t.Errorf("OpenSession calls[%d] = %q; want %q", i, opens[i], want)
		}
	}
	wantExecutes := []string{"develop", "comment_handler_failed", "set_review_state"}
	if len(executes) != len(wantExecutes) {
		t.Fatalf("Execute calls: %v; want %v", executes, wantExecutes)
	}
	for i, want := range wantExecutes {
		if executes[i] != want {
			t.Errorf("Execute calls[%d] = %q; want %q", i, executes[i], want)
		}
	}

	data, ok := sink.recorded("session.crash")
	if !ok {
		t.Fatal("expected a session.crash event")
	}
	if data["session"] != "fake.default" {
		t.Errorf("session.crash session=%v; want fake.default", data["session"])
	}
	if reason, _ := data["crash_reason"].(string); reason == "" {
		t.Error("expected a named crash_reason on the session.crash event")
	} else if reason != adapterhost.CrashReasonTransportClosed {
		t.Errorf("crash_reason=%q; want the named shim-connection-drop cause", reason)
	}
	if idle, ok := data["idle_since_last_event"].(string); !ok || idle == "" {
		t.Errorf("expected idle_since_last_event on the session.crash event, got %v", data["idle_since_last_event"])
	}
}

func TestRemoteSession_ReopenDisabledKeepsPreCRI271Behavior(t *testing.T) {
	t.Setenv("CRITERIA_SESSION_CRASH_REOPEN", "0")
	g := compile(t, cri271Workflow)
	p := newCri271Adapter("develop")
	sink := &fakeSink{}
	if err := NewTestEngine(g, cri271NewLoader(p), sink).Run(context.Background()); err == nil {
		t.Fatal("expected the run to fail with re-open disabled")
	}
	if sink.failure == "" {
		t.Error("expected OnRunFailed with re-open disabled")
	} else if !strings.Contains(sink.failure, "comment_handler_failed") || !strings.Contains(sink.failure, cri271CrashErr) {
		t.Errorf("OnRunFailed %q; want the comment step's exhaustion failure with the crash signature", sink.failure)
	}
	opens, executes := p.callLog()
	if len(opens) != 1 {
		t.Errorf("OpenSession calls: %v; want exactly the initial open", opens)
	}
	wantExecutes := []string{"develop", "comment_handler_failed"}
	if len(executes) != len(wantExecutes) {
		t.Fatalf("Execute calls: %v; want %v", executes, wantExecutes)
	}
}

// -----------------------------------------------------------------------------
// CRI-276: Idle Survival & Keepalives
// -----------------------------------------------------------------------------

type idleProxy struct {
	ln         net.Listener
	target     string
	idleWindow time.Duration

	mu    chan struct{}
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

type cri276PodServer struct {
	criteriav2.UnimplementedAdapterServiceServer
	name, version string
	infoCalls     atomic.Int64
	execCalls     atomic.Int64
}

func (s *cri276PodServer) Info(context.Context, *criteriav2.InfoRequest) (*criteriav2.InfoResponse, error) {
	s.infoCalls.Add(1)
	return &criteriav2.InfoResponse{Name: s.name, Version: s.version, Capabilities: []string{"execute"}}, nil
}

func (s *cri276PodServer) OpenSession(context.Context, *criteriav2.OpenSessionRequest) (*criteriav2.OpenSessionResponse, error) {
	return &criteriav2.OpenSessionResponse{}, nil
}

func (s *cri276PodServer) Execute(req *criteriav2.ExecuteRequest, stream criteriav2.AdapterService_ExecuteServer) error {
	s.execCalls.Add(1)
	ev, err := criteriav2.NewExecuteResultEvent("success", map[string]any{"session": req.GetSessionId(), "step": req.GetStepName()})
	if err != nil {
		return err
	}
	return stream.Send(ev)
}

func podHomeLoop(proxyAddr string, hs *cri137Handshake, stop <-chan struct{}, done chan<- struct{}, accepted *atomic.Int64, server criteriav2.AdapterServiceServer, serverOpts []grpc.ServerOption) {
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
		criteriav2.RegisterAdapterServiceServer(grpcServer, server)
		serveDone := make(chan struct{})
		go func() {
			defer close(serveDone)
			_ = grpcServer.Serve(&cri137SingleConnListener{conn: observed})
		}()

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

func podHomeSleep(stop <-chan struct{}, d time.Duration) bool {
	select {
	case <-stop:
		return false
	case <-time.After(d):
		return true
	}
}

type cri276AdapterEvents struct{}

func (cri276AdapterEvents) Log(string, []byte)  {}
func (cri276AdapterEvents) Adapter(string, any) {}

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

	stopDial := make(chan struct{})
	podDone := make(chan struct{})
	pod := &cri276PodServer{name: "noop.default", version: "1.0.0"}
	accepted := &atomic.Int64{}
	go podHomeLoop(proxy.addr(), hs, stopDial, podDone, accepted, pod, podServerOpts)
	// Registration order = teardown order (t.Cleanup runs LIFO): stop the
	// dial loop first, shut the sessions down second. Shutting down while
	// the pod is still dialing leaves a mid-handshake dial racing the
	// provider stop (the provider now rejects-and-closes those, so this
	// ordering just keeps teardown quiet and deterministic).
	t.Cleanup(func() {
		_ = sessions.Shutdown(context.Background())
	})
	t.Cleanup(func() {
		close(stopDial)
		<-podDone
	})

	if err := <-initDone; err != nil {
		t.Fatalf("initScopeAdapters: %v", err)
	}

	acceptedDeadline := time.Now().Add(5 * time.Second)
	for c := accepted.Load(); c < 1; c = accepted.Load() {
		if time.Now().After(acceptedDeadline) {
			t.Fatalf("pod completed %d handshakes, want 1 (the verify handshake; the engine must not kill the phone-home bridge, KB-232)", c)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return &idleChain{sessions: sessions, pod: pod, accepted: accepted}
}

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

	res1, err1 := chain.sessions.Execute(ctx, "noop.default", step, events, nil)
	before := chain.acceptCount()

	time.Sleep(3500 * time.Millisecond)

	res2, err2 := chain.sessions.Execute(ctx, "noop.default", step, events, nil)
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

func TestRemoteSession_IdleSurvivesWithKeepalives(t *testing.T) {
	if !canDialLoopbackTCP() {
		t.Skip("skipping test: loopback TCP dialing is restricted in current environment")
	}
	if testing.Short() {
		t.Skip("skipping slow idle middlebox test in -short mode")
	}
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

func TestRemoteSession_IdleDiesWithoutKeepalives(t *testing.T) {
	if !canDialLoopbackTCP() {
		t.Skip("skipping test: loopback TCP dialing is restricted in current environment")
	}
	if testing.Short() {
		t.Skip("skipping slow idle middlebox test in -short mode")
	}
	c := runIdleExecCycle(t, nil)

	if c.firstErr != nil {
		t.Fatalf("first step failed before the idle period: %v", c.firstErr)
	}
	if c.firstResult.Outcome != "success" {
		t.Fatalf("first step outcome = %q, want success", c.firstResult.Outcome)
	}
	if c.secondErr != nil {
		t.Fatalf("second step failed after the phone-home connection died; the KB-246 re-entry should have retried on the fresh handle: %v", c.secondErr)
	}
	if c.secondResult.Outcome != "success" {
		t.Fatalf("second step outcome = %q, want success after the KB-246 re-entry", c.secondResult.Outcome)
	}
	if c.chain.pod.execCalls.Load() != 2 {
		t.Fatalf("pod served %d Execute calls, want 2", c.chain.pod.execCalls.Load())
	}
	if c.acceptedAfter <= c.acceptedBefore {
		t.Fatalf("pod handshakes went from %d to %d: the reconnect loop did not re-handshake after the teardown", c.acceptedBefore, c.acceptedAfter)
	}
}

// -----------------------------------------------------------------------------
// CRI-287: Step-Timeout Teardown Cascades vs Crash Classification
// -----------------------------------------------------------------------------

const cri287Workflow = `
workflow {
  name = "cri287"
  version = "0.1"
  initial_state = "boot"
  target_state  = "done"
}
adapter "copilot" "default" {}
adapter "shell" "default" {}
step "boot" {
  target = adapter.shell.default
  outcome "success" { next = step.develop }
  outcome "failure" { next = step.develop }
}
step "develop" {
  target = adapter.copilot.default
  timeout = "150ms"
  outcome "success" { next = step.push_checkpoint }
  outcome "failure" { next = step.comment_handler_failed }
}
step "comment_handler_failed" {
  target = adapter.shell.default
  outcome "success" { next = step.push_checkpoint }
  outcome "failure" { next = step.push_checkpoint }
}
step "push_checkpoint" {
  target = adapter.shell.default
  outcome "success" { next = step.develop }
  outcome "failure" { next = step.develop }
  outcome "done"    { next = state.done }
}
state "done" {
  terminal = true
  success  = true
}`

type cri287Shared struct {
	mu           sync.Mutex
	up           bool
	developTurns int
}

func (s *cri287Shared) setUp(up bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.up = up
}

func (s *cri287Shared) isUp() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.up
}

func (s *cri287Shared) completedTurn() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.developTurns++
}

func (s *cri287Shared) turns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.developTurns
}

type cri287Copilot struct {
	shared *cri287Shared

	mu    sync.Mutex
	opens []string
	turns int
}

func (a *cri287Copilot) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: "copilot", Version: "test"}, nil
}

func (a *cri287Copilot) OpenSession(_ context.Context, name string, _, _ map[string]string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.opens = append(a.opens, name)
	a.shared.setUp(true)
	return nil
}

func (a *cri287Copilot) Execute(ctx context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink, rejection *criteriav2.ExecutionRejection) (adapter.Result, error) {
	a.mu.Lock()
	a.turns++
	turn := a.turns
	a.mu.Unlock()
	if turn == 1 {
		<-ctx.Done()
		a.shared.setUp(false)
		return adapter.Result{}, ctx.Err()
	}
	a.shared.setUp(true)
	a.shared.completedTurn()
	return adapter.Result{Outcome: "success"}, nil
}

func (a *cri287Copilot) CloseSession(context.Context, string) error { return nil }
func (a *cri287Copilot) Kill()                                      {}
func (a *cri287Copilot) Pause(context.Context, string) error        { return nil }
func (a *cri287Copilot) Resume(context.Context, string) error       { return nil }
func (a *cri287Copilot) Inspect(context.Context, string) (*criteriav2.InspectResponse, error) {
	return &criteriav2.InspectResponse{}, nil
}
func (a *cri287Copilot) Snapshot(context.Context, string) (*criteriav2.SnapshotResponse, error) {
	return &criteriav2.SnapshotResponse{}, nil
}
func (a *cri287Copilot) Restore(context.Context, string, []byte, uint32) error { return nil }

func (a *cri287Copilot) openLog() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.opens...)
}

type cri287Shell struct {
	shared *cri287Shared

	mu    sync.Mutex
	opens []string
}

func (a *cri287Shell) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: "shell", Version: "test"}, nil
}

func (a *cri287Shell) OpenSession(_ context.Context, name string, _, _ map[string]string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.opens = append(a.opens, name)
	a.shared.setUp(true)
	return nil
}

func (a *cri287Shell) Execute(_ context.Context, _ string, step *workflow.StepNode, _ adapter.EventSink, rejection *criteriav2.ExecutionRejection) (adapter.Result, error) {
	if !a.shared.isUp() {
		return adapter.Result{}, errors.New(cri271CrashErr)
	}
	if step != nil && step.Name == "push_checkpoint" && a.shared.turns() >= 2 {
		return adapter.Result{Outcome: "done"}, nil
	}
	return adapter.Result{Outcome: "success"}, nil
}

func (a *cri287Shell) CloseSession(context.Context, string) error { return nil }
func (a *cri287Shell) Kill()                                      {}
func (a *cri287Shell) Pause(context.Context, string) error        { return nil }
func (a *cri287Shell) Resume(context.Context, string) error       { return nil }
func (a *cri287Shell) Inspect(context.Context, string) (*criteriav2.InspectResponse, error) {
	return &criteriav2.InspectResponse{}, nil
}
func (a *cri287Shell) Snapshot(context.Context, string) (*criteriav2.SnapshotResponse, error) {
	return &criteriav2.SnapshotResponse{}, nil
}
func (a *cri287Shell) Restore(context.Context, string, []byte, uint32) error { return nil }

func (a *cri287Shell) openLog() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.opens...)
}

type cri287Sink struct {
	*fakeSink

	engine *Engine

	mu              sync.Mutex
	outcomes        []cri287Outcome
	events          []cri271Event
	windowAtRunFail bool
	sawRunFail      bool
}

type cri287Outcome struct {
	step       string
	outcome    string
	err        string
	windowOpen bool
}

func (s *cri287Sink) teardownWindowOpen() bool {
	s.mu.Lock()
	engine := s.engine
	s.mu.Unlock()
	if engine == nil {
		return false
	}
	engine.mu.RLock()
	sessions := engine.liveSessions
	engine.mu.RUnlock()
	if sessions == nil {
		return false
	}
	return sessions.StepTimeoutTeardownWindowOpen()
}

func (s *cri287Sink) OnStepOutcome(step, outcome string, _ time.Duration, err error, comment string) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	open := s.teardownWindowOpen()
	s.mu.Lock()
	s.outcomes = append(s.outcomes, cri287Outcome{step: step, outcome: outcome, err: msg, windowOpen: open})
	s.mu.Unlock()
}

func (s *cri287Sink) OnRunFailed(reason, step string) {
	open := s.teardownWindowOpen()
	s.mu.Lock()
	s.windowAtRunFail = open
	s.sawRunFail = true
	s.mu.Unlock()
	s.fakeSink.OnRunFailed(reason, step)
}

func (s *cri287Sink) StepEventSink(string) adapter.EventSink {
	return &cri287EventRecorder{parent: s}
}

type cri287EventRecorder struct {
	parent *cri287Sink
}

func (r *cri287EventRecorder) Log(string, []byte) {}
func (r *cri287EventRecorder) Adapter(kind string, data any) {
	payload, _ := data.(map[string]any)
	r.parent.mu.Lock()
	defer r.parent.mu.Unlock()
	r.parent.events = append(r.parent.events, cri271Event{kind: kind, data: payload})
}

func (s *cri287Sink) recorded(kind string) (map[string]any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, evt := range s.events {
		if evt.kind == kind {
			return evt.data, true
		}
	}
	return nil, false
}

func (s *cri287Sink) eventCount(kind string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, evt := range s.events {
		if evt.kind == kind {
			n++
		}
	}
	return n
}

func TestRemoteSession_StepTimeoutTeardownReentersCheckpointLoop(t *testing.T) {
	g := compile(t, cri287Workflow)
	shared := &cri287Shared{up: true}
	copilot := &cri287Copilot{shared: shared}
	shell := &cri287Shell{shared: shared}
	sink := &cri287Sink{fakeSink: &fakeSink{}}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"copilot":         copilot,
		"copilot.default": copilot,
		"shell":           shell,
		"shell.default":   shell,
	}}
	e := NewTestEngine(g, loader, sink)
	sink.engine = e
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Fatalf("terminal state %q success=%v; want done/true", sink.terminal, sink.terminalOK)
	}
	if sink.failure != "" {
		t.Errorf("OnRunFailed: %q; want none (the checkpoint loop, not a run failure)", sink.failure)
	}

	var developFailure *cri287Outcome
	for i := range sink.outcomes {
		o := &sink.outcomes[i]
		if o.step == "develop" && o.outcome == "failure" {
			developFailure = o
			break
		}
	}
	if developFailure == nil {
		t.Fatalf("develop outcomes: %+v; want a declared failure outcome", sink.outcomes)
	}
	if !strings.Contains(developFailure.err, "context deadline exceeded") {
		t.Errorf("develop failure error = %q; want the CRI-275 context deadline signature", developFailure.err)
	}

	var commentOutcome *cri287Outcome
	for i := range sink.outcomes {
		o := &sink.outcomes[i]
		if o.step == "comment_handler_failed" && o.outcome == "failure" {
			commentOutcome = o
			break
		}
	}
	if commentOutcome == nil {
		t.Fatalf("comment_handler_failed outcomes: %+v; want a declared failure outcome", sink.outcomes)
	}
	if !commentOutcome.windowOpen {
		t.Errorf("comment_handler_failed outcome observed windowOpen=false; want the step-timeout teardown window open")
	}

	if n := sink.eventCount("session.crash"); n != 0 {
		t.Errorf("session.crash events: %d; want 0 for a step-timeout teardown cascade", n)
	}

	if opens := shell.openLog(); len(opens) != 1 {
		t.Errorf("shell OpenSession calls: %v; want exactly the initial open (no respawn)", opens)
	}
	if opens := copilot.openLog(); len(opens) != 1 {
		t.Errorf("copilot OpenSession calls: %v; want exactly the initial open", opens)
	}

	steps := sink.stepsRun
	if len(steps) < 5 {
		t.Fatalf("steps run: %v; want the checkpoint loop between develop turns", steps)
	}
	if steps[0] != "boot" {
		t.Errorf("steps[0] = %q; want the bootstrap step that opens the shell session", steps[0])
	}
	if steps[1] != "develop" {
		t.Errorf("steps[1] = %q; want the timed-out develop turn", steps[1])
	}
	if steps[2] != "comment_handler_failed" {
		t.Errorf("steps[2] = %q; want the failure-route bookkeeping step", steps[2])
	}
	if steps[3] != "push_checkpoint" {
		t.Errorf("steps[3] = %q; want the checkpoint step before develop resumes", steps[3])
	}
	if steps[4] != "develop" {
		t.Errorf("steps[4] = %q; want develop re-entered from the checkpoint loop", steps[4])
	}
}

type cri287CopilotDying struct {
	shared *cri287Shared

	mu    sync.Mutex
	opens []string
	turns int
}

func (a *cri287CopilotDying) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: "copilot", Version: "test"}, nil
}

func (a *cri287CopilotDying) OpenSession(_ context.Context, name string, _, _ map[string]string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.opens = append(a.opens, name)
	a.shared.setUp(true)
	return nil
}

func (a *cri287CopilotDying) Execute(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink, rejection *criteriav2.ExecutionRejection) (adapter.Result, error) {
	a.mu.Lock()
	a.turns++
	turn := a.turns
	a.mu.Unlock()
	if turn == 1 {
		a.shared.setUp(false)
		return adapter.Result{}, errors.New(cri271CrashErr)
	}
	a.shared.setUp(true)
	a.shared.completedTurn()
	return adapter.Result{Outcome: "success"}, nil
}

func (a *cri287CopilotDying) CloseSession(context.Context, string) error { return nil }
func (a *cri287CopilotDying) Kill()                                      {}
func (a *cri287CopilotDying) Pause(context.Context, string) error        { return nil }
func (a *cri287CopilotDying) Resume(context.Context, string) error       { return nil }
func (a *cri287CopilotDying) Inspect(context.Context, string) (*criteriav2.InspectResponse, error) {
	return &criteriav2.InspectResponse{}, nil
}
func (a *cri287CopilotDying) Snapshot(context.Context, string) (*criteriav2.SnapshotResponse, error) {
	return &criteriav2.SnapshotResponse{}, nil
}
func (a *cri287CopilotDying) Restore(context.Context, string, []byte, uint32) error { return nil }

func TestRemoteSession_GenuineAdapterDeathStillCrashClassified(t *testing.T) {
	g := compile(t, strings.Replace(cri287Workflow, "  timeout = \"150ms\"\n", "", 1))
	shared := &cri287Shared{up: true}
	copilot := &cri287CopilotDying{shared: shared}
	shell := &cri287Shell{shared: shared}
	sink := &cri287Sink{fakeSink: &fakeSink{}}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"copilot":         copilot,
		"copilot.default": copilot,
		"shell":           shell,
		"shell.default":   shell,
	}}
	if err := NewTestEngine(g, loader, sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Fatalf("terminal state %q success=%v; want done/true", sink.terminal, sink.terminalOK)
	}

	data, ok := sink.recorded("session.crash")
	if !ok {
		t.Fatal("expected a session.crash event for a genuine adapter death")
	}
	if data["session"] != "copilot.default" {
		t.Errorf("session.crash session=%v; want copilot.default", data["session"])
	}
	if reason, _ := data["crash_reason"].(string); reason != adapterhost.CrashReasonTransportClosed {
		t.Errorf("crash_reason=%q; want the classified transport-close cause", reason)
	}
}

func TestRemoteSession_ParentDeadlineExpiryDoesNotOpenTeardownWindow(t *testing.T) {
	g := compile(t, strings.Replace(cri287Workflow, "  timeout = \"150ms\"\n", "", 1))
	shared := &cri287Shared{up: true}
	copilot := &cri287Copilot{shared: shared}
	shell := &cri287Shell{shared: shared}
	sink := &cri287Sink{fakeSink: &fakeSink{}}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"copilot":         copilot,
		"copilot.default": copilot,
		"shell":           shell,
		"shell.default":   shell,
	}}
	e := NewTestEngine(g, loader, sink)
	sink.engine = e

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := e.Run(ctx); err == nil {
		t.Fatal("run: expected the parent-deadline cancellation to fail the run")
	}

	if !sink.sawRunFail {
		t.Fatal("expected OnRunFailed for the parent-deadline cancellation")
	}
	if sink.windowAtRunFail {
		t.Error("teardown window open at run failure; a parent-context deadline must not open the CRI-287 window")
	}
	for _, o := range sink.outcomes {
		if o.windowOpen {
			t.Errorf("outcome %s/%s observed windowOpen=true; no step timeout was installed, so the window must never open", o.step, o.outcome)
		}
	}
	if n := sink.eventCount("session.crash"); n != 0 {
		t.Errorf("session.crash events: %d; want 0 — a run-context deadline expiry is not an adapter crash", n)
	}
}

const cri287ProcessExitWorkflow = `
workflow {
  name = "cri287_process_exit"
  version = "0.1"
  initial_state = "boot"
  target_state  = "done"
}
adapter "copilot" "default" {}
adapter "shell" "default" {}
step "boot" {
  target = adapter.shell.default
  outcome "success" { next = step.develop }
  outcome "failure" { next = step.develop }
}
step "develop" {
  target = adapter.copilot.default
  timeout = "150ms"
  outcome "success" { next = step.verify_death }
  outcome "failure" { next = step.verify_death }
}
step "verify_death" {
  target = adapter.copilot.default
  outcome "success" { next = step.push_checkpoint }
  outcome "failure" { next = step.push_checkpoint }
}
step "push_checkpoint" {
  target = adapter.shell.default
  outcome "success" { next = step.develop }
  outcome "failure" { next = step.develop }
  outcome "done"    { next = state.done }
}
state "done" {
  terminal = true
  success  = true
}`

type cri287ProcessExitCopilot struct {
	shared *cri287Shared

	exited atomic.Bool

	mu    sync.Mutex
	opens []string
	turns int
}

func (a *cri287ProcessExitCopilot) ProcessExited() bool { return a.exited.Load() }

func (a *cri287ProcessExitCopilot) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: "copilot", Version: "test"}, nil
}

func (a *cri287ProcessExitCopilot) OpenSession(_ context.Context, name string, _, _ map[string]string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.opens = append(a.opens, name)
	a.shared.setUp(true)
	return nil
}

func (a *cri287ProcessExitCopilot) Execute(ctx context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink, rejection *criteriav2.ExecutionRejection) (adapter.Result, error) {
	a.mu.Lock()
	a.turns++
	turn := a.turns
	a.mu.Unlock()
	switch turn {
	case 1:
		<-ctx.Done()
		a.exited.Store(true)
		a.shared.setUp(false)
		return adapter.Result{}, ctx.Err()
	case 2:
		return adapter.Result{}, errors.New(cri271CrashErr)
	}
	a.shared.setUp(true)
	a.shared.completedTurn()
	return adapter.Result{Outcome: "success"}, nil
}

func (a *cri287ProcessExitCopilot) CloseSession(context.Context, string) error { return nil }
func (a *cri287ProcessExitCopilot) Kill()                                      {}
func (a *cri287ProcessExitCopilot) Pause(context.Context, string) error        { return nil }
func (a *cri287ProcessExitCopilot) Resume(context.Context, string) error       { return nil }
func (a *cri287ProcessExitCopilot) Inspect(context.Context, string) (*criteriav2.InspectResponse, error) {
	return &criteriav2.InspectResponse{}, nil
}
func (a *cri287ProcessExitCopilot) Snapshot(context.Context, string) (*criteriav2.SnapshotResponse, error) {
	return &criteriav2.SnapshotResponse{}, nil
}
func (a *cri287ProcessExitCopilot) Restore(context.Context, string, []byte, uint32) error {
	return nil
}

func TestRemoteSession_ProcessExitDuringTeardownWindowStillCrashClassified(t *testing.T) {
	g := compile(t, cri287ProcessExitWorkflow)
	shared := &cri287Shared{up: true}
	copilot := &cri287ProcessExitCopilot{shared: shared}
	shell := &cri287Shell{shared: shared}
	sink := &cri287Sink{fakeSink: &fakeSink{}}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"copilot":         copilot,
		"copilot.default": copilot,
		"shell":           shell,
		"shell.default":   shell,
	}}
	e := NewTestEngine(g, loader, sink)
	sink.engine = e
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "done" || !sink.terminalOK {
		t.Fatalf("terminal state %q success=%v; want done/true (the checkpoint loop resumes)", sink.terminal, sink.terminalOK)
	}

	var verifyOutcome *cri287Outcome
	for i := range sink.outcomes {
		o := &sink.outcomes[i]
		if o.step == "verify_death" {
			verifyOutcome = o
			break
		}
	}
	if verifyOutcome == nil {
		t.Fatalf("verify_death outcomes: %+v; want the death-verification turn recorded", sink.outcomes)
	}
	if !verifyOutcome.windowOpen {
		t.Errorf("verify_death outcome observed windowOpen=false; want the step-timeout teardown window open")
	}
	if verifyOutcome.outcome != "failure" {
		t.Errorf("verify_death outcome = %q; want the declared failure outcome", verifyOutcome.outcome)
	}

	data, ok := sink.recorded("session.crash")
	if !ok {
		t.Fatal("expected a session.crash event for the verifiably exited adapter")
	}
	if data["session"] != "copilot.default" {
		t.Errorf("session.crash session=%v; want copilot.default", data["session"])
	}
	if reason, _ := data["crash_reason"].(string); reason != adapterhost.CrashReasonProcessExitedEarly {
		t.Errorf("crash_reason=%q; want the ProcessExited classification", reason)
	}
	if n := sink.eventCount("session.crash"); n != 1 {
		t.Errorf("session.crash events: %d; want exactly 1 (the copilot death; shell transport closes stay timeout-routed)", n)
	}
}

// -----------------------------------------------------------------------------
// KB-257: Detached Session Reentry & Lifecycle
// -----------------------------------------------------------------------------

func TestRemoteSession_ReentryReusesDetachedSession(t *testing.T) {
	ctx := context.Background()
	g := perScopeRemoteGraph(t)
	dataDir := t.TempDir()

	sink, shim, rlc, deps := newPerScopeTestHarness(t, g, dataDir)

	order1, err := initScopeAdapters(ctx, g, deps, nil, dataDir, "entry_one", nil, rlc)
	if err != nil {
		t.Fatalf("entry_one init: %v", err)
	}
	first, ok := sink.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("entry_one emitted no provision_wanted")
	}
	scopeOne := first.ScopeInstanceID
	scopeKeyOne := first.ScopeName + "/" + scopeOne

	detached := rlc.DetachBodyScope(order1, deps)
	if len(detached) != 1 {
		t.Fatalf("entry_one detach moved %d records, want 1", len(detached))
	}

	order2, err := initScopeAdapters(ctx, g, deps, nil, dataDir, "entry_two", nil, rlc)
	if err != nil {
		t.Fatalf("entry_two init: %v", err)
	}
	if len(order2) != 0 {
		t.Fatalf("entry_two provisioned %d adapters, want 0 (CRI-145 skip must engage on re-entry)", len(order2))
	}
	second, ok := sink.nthStatus("provision_wanted", 2)
	if ok {
		t.Fatalf("entry_two emitted a SECOND provision_wanted (instance %q) — re-entry must reuse, not rotate", second.ScopeInstanceID)
	}

	if tok := shim.registeredToken(scopeKeyOne); tok == "" {
		t.Errorf("scope %q token unregistered after body exit; the detached session's shim registration must survive (the pod keeps dialing with it)", scopeKeyOne)
	}

	tearDownScopeAdapters(ctx, nil, deps, rlc)
	rlc.detachMu.Lock()
	remaining := rlc.detached
	rlc.detached = nil
	rlc.detachMu.Unlock()
	flushOrder := make([]string, 0, len(remaining))
	for id, rec := range remaining {
		flushOrder = append(flushOrder, id)
		rlc.scopeLifecycle.add(rec)
	}
	if len(flushOrder) > 0 {
		tearDownScopeAdapters(ctx, flushOrder, deps, rlc)
	}

	provisions, detaches, releases := 0, 0, 0
	seen := make([]string, 0, 8)
	events := sink.provisionEventsLog()
	for _, ev := range events {
		seen = append(seen, ev.AdapterName+":"+ev.Status)
		switch ev.Status {
		case "provision_wanted":
			provisions++
		case "detached":
			detaches++
		case "released":
			releases++
		}
	}
	if releases != 1 {
		t.Fatalf("released events = %d, want 1 (run-final teardown only); full event trail: %v", releases, seen)
	}
	if provisions != 1 {
		t.Errorf("provision_wanted events = %d, want 1 (one provision total; re-entry reuses)", provisions)
	}
	if detaches != 1 {
		t.Errorf("detached events = %d, want 1 (body-one exit; body-two never owned the record)", detaches)
	}
	if releases != 1 {
		t.Errorf("released events = %d, want 1 (run-final teardown only)", releases)
	}
}
