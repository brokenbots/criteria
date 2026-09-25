package remote

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"

	criteriav1 "github.com/brokenbots/criteria/sdk/pb/criteria/v1"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// fakePeerService is the HandlerType placeholder for the hand-rolled
// PeerService descriptor. PeerService has no generated grpc-go server stubs
// (the bindings are connect-style only), so the fake registers the service
// with a placeholder interface exactly as the host's hand-rolled client
// expects.
type fakePeerService interface{}

// fakePeer is an in-process adapter peer. It dials the shim, presents the
// role="peer" handshake, and serves both the adapter v2 AdapterService and
// the supervision PeerService on the phone-home connection: on the peer-dial
// model the peer is the gRPC server on the connection the host dialed.
type fakePeer struct {
	name  string
	scope string
	token string

	// journal is the supervision journal; test code appends events and the
	// Supervise handler replays entries with seq > since_event_seq. Because
	// replay then closes the stream, the host reconnects with its updated
	// cursor — the same at-least-once reconnect model as a real peer.
	journalMu sync.Mutex
	journal   []*criteriav1.SupervisionEvent
	nextSeq   uint64

	mu            sync.Mutex
	opened        []string
	paused        bool
	snapshot      []byte
	schemaVersion uint32
	restored      []byte

	v2.UnimplementedAdapterServiceServer

	srv      *grpc.Server
	conn     net.Conn
	addr     net.Addr
	serveErr chan error
}

func newFakePeer(name, scope string) *fakePeer {
	return &fakePeer{
		name:     name,
		scope:    scope,
		serveErr: make(chan error, 1),
		nextSeq:  1,
	}
}

// connect dials the shim address, presents the peer handshake, and starts
// serving both gRPC services on the accepted connection.
func (f *fakePeer) connect(t *testing.T, addr string) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("fake peer dial: %v", err)
	}
	hs := &handshakeMessage{
		Name:    f.name,
		Version: "1.0.0",
		Digest:  "sha256:abcd1234",
		Token:   f.token,
		Scope:   f.scope,
		Role:    "peer",
		Peer:    &PeerClientIdentity{CriteriaVersion: "test"},
	}
	data, err := json.Marshal(hs)
	if err != nil {
		conn.Close()
		t.Fatalf("marshal handshake: %v", err)
	}
	if _, err := conn.Write(append(data, '\n')); err != nil {
		conn.Close()
		t.Fatalf("write handshake: %v", err)
	}
	f.serve(conn)
}

func (f *fakePeer) serve(conn net.Conn) {
	f.conn = conn
	f.addr = conn.LocalAddr()
	srv := grpc.NewServer()
	f.srv = srv
	v2.RegisterAdapterServiceServer(srv, f)
	srv.RegisterService(&grpc.ServiceDesc{
		ServiceName: "criteria.v1.PeerService",
		HandlerType: (*fakePeerService)(nil),
		Methods: []grpc.MethodDesc{{
			MethodName: "Control",
			Handler:    f.controlHandler,
		}},
		Streams: []grpc.StreamDesc{{
			StreamName:    "Supervise",
			ServerStreams: true,
			Handler:       f.superviseHandler,
		}},
	}, f)
	go func() {
		f.serveErr <- srv.Serve(&peerConnListener{conn: conn, addr: conn.LocalAddr()})
	}()
}

// drop closes the phone-home connection and stops serving, which tears down
// the host-side peer session the way a crashed peer would.
func (f *fakePeer) drop() {
	f.mu.Lock()
	srv := f.srv
	f.mu.Unlock()
	if srv != nil {
		srv.Stop()
	}
}

func (f *fakePeer) appendEvent(ev *criteriav1.SupervisionEvent) {
	f.journalMu.Lock()
	defer f.journalMu.Unlock()
	ev.EventSeq = f.nextSeq
	f.nextSeq++
	ev.AdapterType = f.name
	ev.Scope = f.scope
	f.journal = append(f.journal, ev)
}

// duplicateEvent re-emits an existing journal entry verbatim: same
// event_seq, delivered again on a later replay (at-least-once delivery).
func (f *fakePeer) duplicateEvent(seq uint64) {
	f.journalMu.Lock()
	defer f.journalMu.Unlock()
	for _, ev := range f.journal {
		if ev.GetEventSeq() == seq {
			f.journal = append(f.journal, ev)
			return
		}
	}
}

func (f *fakePeer) controlHandler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(criteriav1.ControlRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return f.control(ctx, req.(*criteriav1.ControlRequest))
	}
	if interceptor == nil {
		return handler(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/criteria.v1.PeerService/Control"}
	return interceptor(ctx, in, info, handler)
}

func (f *fakePeer) control(ctx context.Context, req *criteriav1.ControlRequest) (*criteriav1.ControlResponse, error) {
	if req.GetKillChild() != nil {
		// A kill request on the peer model ends the adapter child: record the
		// exit in the journal so the host learns it through Supervise.
		f.appendEvent(&criteriav1.SupervisionEvent{
			Kind: &criteriav1.SupervisionEvent_Exited{Exited: &criteriav1.ProcessExited{ExitCode: 143}},
		})
		return &criteriav1.ControlResponse{Accepted: true}, nil
	}
	return &criteriav1.ControlResponse{Accepted: false, Detail: "unsupported control"}, nil
}

func (f *fakePeer) superviseHandler(srv interface{}, stream grpc.ServerStream) error {
	req := new(criteriav1.SupervisionRequest)
	if err := stream.RecvMsg(req); err != nil {
		return err
	}
	f.journalMu.Lock()
	events := append([]*criteriav1.SupervisionEvent(nil), f.journal...)
	f.journalMu.Unlock()
	for _, ev := range events {
		if ev.GetEventSeq() <= req.GetSinceEventSeq() {
			continue
		}
		if err := stream.SendMsg(ev); err != nil {
			return err
		}
	}
	return nil
}

// --- adapter v2 service ---

func (f *fakePeer) Info(ctx context.Context, req *v2.InfoRequest) (*v2.InfoResponse, error) {
	return &v2.InfoResponse{Name: f.name, Version: "1.0.0", Capabilities: []string{"pause", "snapshot"}}, nil
}

func (f *fakePeer) OpenSession(ctx context.Context, req *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened = append(f.opened, req.GetSessionId())
	return &v2.OpenSessionResponse{}, nil
}

func (f *fakePeer) CloseSession(ctx context.Context, req *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
	return &v2.CloseSessionResponse{}, nil
}

func (f *fakePeer) Pause(ctx context.Context, req *v2.PauseRequest) (*v2.PauseResponse, error) {
	f.mu.Lock()
	f.paused = true
	f.mu.Unlock()
	return &v2.PauseResponse{}, nil
}

func (f *fakePeer) Resume(ctx context.Context, req *v2.ResumeRequest) (*v2.ResumeResponse, error) {
	f.mu.Lock()
	f.paused = false
	f.mu.Unlock()
	return &v2.ResumeResponse{}, nil
}

func (f *fakePeer) Inspect(ctx context.Context, req *v2.InspectRequest) (*v2.InspectResponse, error) {
	f.mu.Lock()
	paused := f.paused
	f.mu.Unlock()
	// Pause state is carried as a freeform InspectField, mirroring how a
	// real adapter reports pause state through the v2 contract.
	val, err := structpb.NewValue(paused)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &v2.InspectResponse{Fields: []*v2.InspectField{{Key: "paused", Value: val}}}, nil
}

func (f *fakePeer) Snapshot(ctx context.Context, req *v2.SnapshotRequest) (*v2.SnapshotResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &v2.SnapshotResponse{State: append([]byte(nil), f.snapshot...), SchemaVersion: f.schemaVersion}, nil
}

func (f *fakePeer) Restore(ctx context.Context, req *v2.RestoreRequest) (*v2.RestoreResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restored = append([]byte(nil), req.GetState()...)
	f.schemaVersion = req.GetSchemaVersion()
	return &v2.RestoreResponse{}, nil
}

// Execute streams a single terminal result event, proving the host's shared
// ExecuteViaClient plumbing consumes the server-streamed events over the
// peer connection.
func (f *fakePeer) Execute(req *v2.ExecuteRequest, stream grpc.ServerStreamingServer[v2.ExecuteEvent]) error {
	return stream.Send(&v2.ExecuteEvent{
		Event: &v2.ExecuteEvent_Result{Result: &v2.ExecuteResult{Outcome: "success"}},
	})
}

// --- test helpers ---

// peerConnListener serves exactly one already-accepted connection.
type peerConnListener struct {
	conn net.Conn
	addr net.Addr
	done chan struct{}
}

func (l *peerConnListener) Accept() (net.Conn, error) {
	if l.conn == nil {
		return nil, net.ErrClosed
	}
	conn := l.conn
	l.conn = nil
	if l.done == nil {
		l.done = make(chan struct{})
		close(l.done)
	}
	return conn, nil
}

func (l *peerConnListener) Close() error {
	if l.done == nil {
		l.done = make(chan struct{})
		close(l.done)
	}
	return nil
}

func (l *peerConnListener) Addr() net.Addr { return l.addr }

func startPeerFixture(t *testing.T, cfg *Config) (shim *Shim, provider *peerSessionProvider, addr string) {
	t.Helper()
	shim, addr = startTestShim(t, cfg)
	provider = NewPeerSessionProvider(shim, cfg != nil && cfg.PerScopeSessions)
	shim.SetPeerAcceptor(provider)
	return shim, provider, addr
}

// peerHandleFrom returns the live handle the provider registry holds.
func peerHandleFrom(p *peerSessionProvider, typ, scope string) (*peerHandle, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ps, ok := p.peers[p.key(typ, scope)]
	if !ok {
		return nil, false
	}
	return ps.handle, true
}

// mustPeerSession returns the live peerSession for a registry key.
func mustPeerSession(t *testing.T, p *peerSessionProvider, typ, scope string) *peerSession {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	ps, ok := p.peers[p.key(typ, scope)]
	if !ok {
		t.Fatalf("no peer session for %q/%q", typ, scope)
	}
	return ps
}

func waitFor(t *testing.T, what string, timeout time.Duration, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s not observed within %s", what, timeout)
}

func assertInspectPaused(t *testing.T, resp *v2.InspectResponse, want bool) {
	t.Helper()
	for _, field := range resp.GetFields() {
		if field.GetKey() != "paused" {
			continue
		}
		if got := field.GetValue().GetBoolValue(); got != want {
			t.Fatalf("inspect field paused = %v, want %v", got, want)
		}
		return
	}
	t.Fatalf("inspect response missing paused field")
}

// --- tests ---

func TestPeerWaitForHandleReturnsLiveHandle(t *testing.T) {
	_, provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})
	fp := newFakePeer("noop", "")
	fp.connect(t, addr)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	handle, err := provider.WaitForHandle(ctx, "noop", "")
	if err != nil {
		t.Fatalf("WaitForHandle: %v", err)
	}
	ph, ok := handle.(*peerHandle)
	if !ok {
		t.Fatalf("handle type %T, want *peerHandle", handle)
	}
	registryHandle, ok := peerHandleFrom(provider, "noop", "")
	if !ok || registryHandle != ph {
		t.Fatalf("WaitForHandle returned a handle outside the provider registry")
	}

	info, err := handle.Info(ctx)
	if err != nil {
		t.Fatalf("Info over peer handle: %v", err)
	}
	if info.Name != "noop" || info.Version != "1.0.0" {
		t.Fatalf("Info = %q/%q, want noop/1.0.0", info.Name, info.Version)
	}
}

func TestPeerExecuteStreamsResult(t *testing.T) {
	_, provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})
	fp := newFakePeer("noop", "")
	fp.connect(t, addr)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	handle, err := provider.WaitForHandle(ctx, "noop", "")
	if err != nil {
		t.Fatalf("WaitForHandle: %v", err)
	}

	// Execute consumes the server-streamed events via the shared
	// ExecuteViaClient plumbing (the exact path rpcHandle.Execute uses).
	res, err := handle.Execute(ctx, "s1", &workflow.StepNode{Name: "probe"}, &recordingEventSink{})
	if err != nil {
		t.Fatalf("Execute over peer handle: %v", err)
	}
	if res.Outcome != "success" {
		t.Fatalf("Execute outcome = %q, want success", res.Outcome)
	}
}

// recordingEventSink satisfies adapter.EventSink without recording; the
// fake peer only streams a terminal result.
type recordingEventSink struct {
	adapter.EventSink
}

func (s *recordingEventSink) Log(stream string, chunk []byte) {}
func (s *recordingEventSink) Adapter(kind string, data any)   {}

func TestPeerWaitForFreshHandleExcludesStale(t *testing.T) {
	_, provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})
	stalePeer := newFakePeer("noop", "")
	stalePeer.connect(t, addr)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stale, err := provider.WaitForHandle(ctx, "noop", "")
	if err != nil {
		t.Fatalf("WaitForHandle: %v", err)
	}

	// Crash the peer: the phone-home conn dies and the supervision consumer
	// must evict the stale handle from the registry.
	stalePeer.drop()
	waitFor(t, "registry eviction of stale peer", 5*time.Second, func() bool {
		_, ok := peerHandleFrom(provider, "noop", "")
		return !ok
	})

	freshPeer := newFakePeer("noop", "")
	freshPeer.connect(t, addr)
	fresh, err := provider.WaitForFreshHandle(ctx, "noop", "", stale)
	if err != nil {
		t.Fatalf("WaitForFreshHandle: %v", err)
	}
	if fresh == stale {
		t.Fatalf("WaitForFreshHandle returned the stale handle")
	}
	info, err := fresh.Info(ctx)
	if err != nil {
		t.Fatalf("fresh handle Info: %v", err)
	}
	if info.Name != "noop" {
		t.Fatalf("fresh handle Info name = %q", info.Name)
	}
}

func TestPeerScopeTokenChecks(t *testing.T) {
	_, provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0", PerScopeSessions: true})
	logs := captureLogs(t)

	provider.RegisterScope("alpha/s1", "tok-1")
	registeredPeer := newFakePeer("noop", "alpha/s1")
	registeredPeer.token = "tok-1"
	registeredPeer.connect(t, addr)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := provider.WaitForHandle(ctx, "noop", "alpha/s1"); err != nil {
		t.Fatalf("WaitForHandle with registered scope token: %v", err)
	}

	// Unregistering the scope invalidates the token: a reconnect with the
	// same token must now be rejected by the handshake.
	provider.UnregisterScope("alpha/s1")
	unregisteredPeer := newFakePeer("noop", "alpha/s1")
	unregisteredPeer.token = "tok-1"
	unregisteredPeer.connect(t, addr)
	// The rejection is logged as slog.Warn("remote shim accept failed",
	// "error", ...) so quote characters are escaped in the text handler.
	if !waitForLog(t, logs, `is not registered`, 10*time.Second) {
		t.Fatalf("unregistered scope dial not rejected")
	}
	_ = unregisteredPeer

	// A wrong token for a registered scope is rejected as well.
	provider.RegisterScope("alpha/s2", "tok-2")
	badTokenPeer := newFakePeer("noop", "alpha/s2")
	badTokenPeer.token = "wrong"
	badTokenPeer.connect(t, addr)
	if !waitForLog(t, logs, "accept_token verification failed", 10*time.Second) {
		t.Fatalf("bad-token dial not rejected")
	}
	_ = badTokenPeer
}

func TestPeerProcessExitedAfterJournalEvent(t *testing.T) {
	_, provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})

	// Pre-populate the journal with a ProcessExited event plus an exact
	// duplicate before the host connects: the first replay delivers both,
	// and the host must apply the exit exactly once.
	fp := newFakePeer("noop", "")
	fp.appendEvent(&criteriav1.SupervisionEvent{
		Kind: &criteriav1.SupervisionEvent_Exited{Exited: &criteriav1.ProcessExited{ExitCode: 1, IdleMs: 5}},
	})
	fp.duplicateEvent(1)
	fp.connect(t, addr)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	handle, err := provider.WaitForHandle(ctx, "noop", "")
	if err != nil {
		t.Fatalf("WaitForHandle: %v", err)
	}
	reporter := handle.(adapterhost.ProcessExitReporter)
	if reporter.ProcessExited() {
		t.Fatalf("ProcessExited true before journal event observed")
	}

	waitFor(t, "ProcessExited after journal event", 5*time.Second, reporter.ProcessExited)
	ps := mustPeerSession(t, provider, "noop", "")
	ps.mu.Lock()
	reason, detail, lastSeq := ps.exitReason, ps.exitDetail, ps.lastSeq
	ps.mu.Unlock()
	if reason != "process_exited" {
		t.Fatalf("exit reason = %q, want process_exited", reason)
	}
	if detail != "exit_code=1 signal=0 idle_ms=5" {
		t.Fatalf("exit detail = %q, want exit_code=1 signal=0 idle_ms=5", detail)
	}
	if lastSeq != 1 {
		t.Fatalf("last replay seq = %d, want 1 (duplicate event_seq must not advance)", lastSeq)
	}

	// The adapterhost-level probe the crash classifier uses must agree.
	if !adapterhost.ProcessExited(handle) {
		t.Fatalf("adapterhost.ProcessExited(handle) = false after journal exit")
	}
}

func TestPeerPauseInspectSnapshotRestore(t *testing.T) {
	_, provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})
	fp := newFakePeer("noop", "")
	fp.snapshot = []byte("checkpoint-1")
	fp.schemaVersion = 7
	fp.connect(t, addr)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	handle, err := provider.WaitForHandle(ctx, "noop", "")
	if err != nil {
		t.Fatalf("WaitForHandle: %v", err)
	}
	if err := handle.OpenSession(ctx, "s1", map[string]string{"k": "v"}, nil); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	fp.mu.Lock()
	opened := append([]string(nil), fp.opened...)
	fp.mu.Unlock()
	if len(opened) != 1 || opened[0] != "s1" {
		t.Fatalf("peer opened sessions = %v, want [s1]", opened)
	}

	// Pause then Inspect must report the paused state from the peer.
	if err := handle.Pause(ctx, "s1"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	inspect, err := handle.Inspect(ctx, "s1")
	if err != nil {
		t.Fatalf("Inspect after Pause: %v", err)
	}
	assertInspectPaused(t, inspect, true)

	// Resume clears the state.
	if err := handle.Resume(ctx, "s1"); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	inspect, err = handle.Inspect(ctx, "s1")
	if err != nil {
		t.Fatalf("Inspect after Resume: %v", err)
	}
	assertInspectPaused(t, inspect, false)

	// Snapshot returns the peer's state; Restore pushes state back.
	snap, err := handle.Snapshot(ctx, "s1")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if string(snap.GetState()) != "checkpoint-1" || snap.GetSchemaVersion() != 7 {
		t.Fatalf("Snapshot = %q/%d, want checkpoint-1/7", snap.GetState(), snap.GetSchemaVersion())
	}
	if err := handle.Restore(ctx, "s1", snap.GetState(), snap.GetSchemaVersion()); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	fp.mu.Lock()
	restored := fp.restored
	fp.mu.Unlock()
	if string(restored) != "checkpoint-1" {
		t.Fatalf("peer restored state = %q, want checkpoint-1", restored)
	}
}

func TestPeerSupervisionHeartbeatUpdatesLiveness(t *testing.T) {
	_, provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})
	fp := newFakePeer("noop", "")
	fp.connect(t, addr)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := provider.WaitForHandle(ctx, "noop", ""); err != nil {
		t.Fatalf("WaitForHandle: %v", err)
	}
	ps := mustPeerSession(t, provider, "noop", "")
	if got := ps.lastHeartbeatAt(); !got.IsZero() {
		t.Fatalf("heartbeat timestamp set before any heartbeat: %v", got)
	}

	fp.appendEvent(&criteriav1.SupervisionEvent{
		Kind: &criteriav1.SupervisionEvent_Heartbeat{Heartbeat: &criteriav1.SupervisionHeartbeat{LastEventSeq: 1}},
	})
	waitFor(t, "heartbeat liveness timestamp", 5*time.Second, func() bool {
		return !ps.lastHeartbeatAt().IsZero()
	})
}

func TestPeerKillReportsExitThroughJournal(t *testing.T) {
	_, provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})
	fp := newFakePeer("noop", "")
	fp.connect(t, addr)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	handle, err := provider.WaitForHandle(ctx, "noop", "")
	if err != nil {
		t.Fatalf("WaitForHandle: %v", err)
	}
	reporter := handle.(adapterhost.ProcessExitReporter)
	if reporter.ProcessExited() {
		t.Fatalf("ProcessExited true before Kill")
	}

	// Kill issues Control{kill_child} on the peer; the fake peer records the
	// exit in its journal and the supervision replay flips ProcessExited.
	handle.Kill()
	waitFor(t, "ProcessExited after Kill", 5*time.Second, reporter.ProcessExited)
}

func TestPeerCloseHandleAndStop(t *testing.T) {
	shim, provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})
	_ = shim
	fp := newFakePeer("noop", "")
	fp.connect(t, addr)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := provider.WaitForHandle(ctx, "noop", ""); err != nil {
		t.Fatalf("WaitForHandle: %v", err)
	}

	// CloseHandle dispatches through the provider (the RemoteShim the
	// SessionManager holds): it closes the peer session and releases the
	// transport.
	if err := provider.CloseHandle(ctx, "noop", ""); err != nil {
		t.Fatalf("CloseHandle: %v", err)
	}
	waitFor(t, "registry eviction after CloseHandle", 5*time.Second, func() bool {
		_, ok := peerHandleFrom(provider, "noop", "")
		return !ok
	})

	// Stop must drain cleanly with no remaining peers.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopCancel()
	if err := provider.Stop(stopCtx); err != nil {
		t.Fatalf("provider Stop: %v", err)
	}
}
