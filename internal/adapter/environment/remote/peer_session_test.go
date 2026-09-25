package remote

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
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
	permRecv      []*v2.PermissionEvent

	// behavior overrides for uncovered-path tests; nil/zero keeps defaults.
	logErr                 error // Log returns this error instead of streaming a chunk
	permUnimplemented      bool  // Permissions returns Unimplemented (old peer build)
	superviseUnimplemented bool  // PeerService.Supervise returns Unimplemented
	rejectKillChild        bool  // Control(kill_child) responds Accepted=false

	v2.UnimplementedAdapterServiceServer

	srv      *grpc.Server
	conn     net.Conn
	addr     net.Addr
	serveErr chan error
}

func newFakePeer(scope string) *fakePeer {
	return &fakePeer{
		name:     "noop",
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
		return f.control(ctx, req.(*criteriav1.ControlRequest)), nil
	}
	if interceptor == nil {
		return handler(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/criteria.v1.PeerService/Control"}
	return interceptor(ctx, in, info, handler)
}

func (f *fakePeer) control(_ context.Context, req *criteriav1.ControlRequest) *criteriav1.ControlResponse {
	if req.GetKillChild() != nil {
		if f.rejectKillChild {
			return &criteriav1.ControlResponse{Accepted: false, Detail: "kill rejected by test peer"}
		}
		// A kill request on the peer model ends the adapter child: record the
		// exit in the journal so the host learns it through Supervise.
		f.appendEvent(&criteriav1.SupervisionEvent{
			Kind: &criteriav1.SupervisionEvent_Exited{Exited: &criteriav1.ProcessExited{ExitCode: 143}},
		})
		return &criteriav1.ControlResponse{Accepted: true}
	}
	return &criteriav1.ControlResponse{Accepted: false, Detail: "unsupported control"}
}

func (f *fakePeer) superviseHandler(srv interface{}, stream grpc.ServerStream) error {
	req := new(criteriav1.SupervisionRequest)
	if err := stream.RecvMsg(req); err != nil {
		return err
	}
	if f.superviseUnimplemented {
		return status.Error(codes.Unimplemented, "peer predates PeerService supervision")
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

// Log streams one log chunk for the requested session, proving the host's
// StartLogStream plumbing receives streamed LogEvents over the peer conn.
func (f *fakePeer) Log(req *v2.LogRequest, stream grpc.ServerStreamingServer[v2.LogEvent]) error {
	if f.logErr != nil {
		return f.logErr
	}
	return stream.Send(&v2.LogEvent{
		SessionId: req.GetSessionId(),
		Line:      []byte("hello from peer"),
	})
}

// Permissions echoes a PermissionDecision ACK per received PermissionEvent,
// exercising the host's bidi permission plumbing end to end.
func (f *fakePeer) Permissions(stream grpc.BidiStreamingServer[v2.PermissionEvent, v2.PermissionDecision]) error {
	if f.permUnimplemented {
		return status.Error(codes.Unimplemented, "peer predates the Permissions stream")
	}
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		f.mu.Lock()
		f.permRecv = append(f.permRecv, ev)
		f.mu.Unlock()
		if err := stream.Send(&v2.PermissionDecision{RequestId: ev.GetRequest().GetRequestId(), Decision: "allow"}); err != nil {
			return err
		}
	}
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

func startPeerFixture(t *testing.T, cfg *Config) (provider *peerSessionProvider, addr string) {
	t.Helper()
	shim, addr := startTestShim(t, cfg)
	provider = NewPeerSessionProvider(shim, cfg != nil && cfg.PerScopeSessions)
	shim.SetPeerAcceptor(provider)
	return provider, addr
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

func waitFor(t *testing.T, what string, check func() bool) {
	t.Helper()
	const timeout = 5 * time.Second
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
	provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})
	fp := newFakePeer("")
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
	provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})
	fp := newFakePeer("")
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
	provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})
	stalePeer := newFakePeer("")
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
	waitFor(t, "registry eviction of stale peer", func() bool {
		_, ok := peerHandleFrom(provider, "noop", "")
		return !ok
	})

	freshPeer := newFakePeer("")
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
	provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0", PerScopeSessions: true})
	logs := captureLogs(t)

	provider.RegisterScope("alpha/s1", "tok-1")
	registeredPeer := newFakePeer("alpha/s1")
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
	unregisteredPeer := newFakePeer("alpha/s1")
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
	badTokenPeer := newFakePeer("alpha/s2")
	badTokenPeer.token = "wrong"
	badTokenPeer.connect(t, addr)
	if !waitForLog(t, logs, "accept_token verification failed", 10*time.Second) {
		t.Fatalf("bad-token dial not rejected")
	}
	_ = badTokenPeer
}

func TestPeerProcessExitedAfterJournalEvent(t *testing.T) {
	provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})

	// Pre-populate the journal with a ProcessExited event plus an exact
	// duplicate before the host connects: the first replay delivers both,
	// and the host must apply the exit exactly once.
	fp := newFakePeer("")
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

	waitFor(t, "ProcessExited after journal event", reporter.ProcessExited)
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
	provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})
	fp := newFakePeer("")
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
	provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})
	fp := newFakePeer("")
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
	waitFor(t, "heartbeat liveness timestamp", func() bool {
		return !ps.lastHeartbeatAt().IsZero()
	})
}

func TestPeerStreamFlushedMarksLogDrain(t *testing.T) {
	provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})
	fp := newFakePeer("")
	fp.connect(t, addr)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := provider.WaitForHandle(ctx, "noop", ""); err != nil {
		t.Fatalf("WaitForHandle: %v", err)
	}
	ps := mustPeerSession(t, provider, "noop", "")
	if ps.logDrained() {
		t.Fatal("log drain marked before any StreamFlushed event")
	}

	// StreamFlushed on the log channel means the peer drained the log
	// backlog into its journal (the host-side drain marker T-07 reads).
	fp.appendEvent(&criteriav1.SupervisionEvent{
		Kind: &criteriav1.SupervisionEvent_Flushed{Flushed: &criteriav1.StreamFlushed{Channel: "log", UpToSeq: 3}},
	})
	waitFor(t, "log drain marked after StreamFlushed", ps.logDrained)
	ps.mu.Lock()
	upTo := ps.logFlushed["log"]
	ps.mu.Unlock()
	if upTo != 3 {
		t.Fatalf("log drain watermark = %d, want 3", upTo)
	}
}

func TestPeerKillReportsExitThroughJournal(t *testing.T) {
	provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})
	fp := newFakePeer("")
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
	waitFor(t, "ProcessExited after Kill", reporter.ProcessExited)
}

func TestPeerCloseHandleAndStop(t *testing.T) {
	provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})
	fp := newFakePeer("")
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
	waitFor(t, "registry eviction after CloseHandle", func() bool {
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

// --- wait/wake path tests (production ordering: the host blocks on
// WaitForHandle BEFORE the peer dials — engine/lifecycle.go WaitForHandle) ---

// providerWaiterCount reports how many waiters are registered for a key.
func providerWaiterCount(p *peerSessionProvider, typ, scope string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.waiters[p.key(typ, scope)])
}

// waitForWaiterRegistered blocks until a test's pre-dial wait has registered
// on the provider, so the later dial deterministically exercises the wake arm.
func waitForWaiterRegistered(t *testing.T, p *peerSessionProvider, typ, scope string) {
	t.Helper()
	waitFor(t, "waiter registration", func() bool {
		return providerWaiterCount(p, typ, scope) > 0
	})
}

func TestPeerWaitForHandleWakesOnDial(t *testing.T) {
	provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})
	fp := newFakePeer("")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Production ordering: the host waits first, the peer dials later. The
	// dial must wake the pending waiter and remove it from the waiters map.
	type waitOutcome struct {
		handle adapterhost.Handle
		err    error
	}
	done := make(chan waitOutcome, 1)
	go func() {
		handle, err := provider.WaitForHandle(ctx, "noop", "")
		done <- waitOutcome{handle: handle, err: err}
	}()
	waitForWaiterRegistered(t, provider, "noop", "")

	fp.connect(t, addr)

	select {
	case out := <-done:
		if out.err != nil {
			t.Fatalf("WaitForHandle after dial: %v", out.err)
		}
		ph, ok := out.handle.(*peerHandle)
		if !ok {
			t.Fatalf("handle type %T, want *peerHandle", out.handle)
		}
		if registryHandle, ok := peerHandleFrom(provider, "noop", ""); !ok || registryHandle != ph {
			t.Fatalf("woken handle is not the registered peer handle")
		}
		waitFor(t, "waiter cleanup after wake", func() bool {
			return providerWaiterCount(provider, "noop", "") == 0
		})
	case <-time.After(10 * time.Second):
		t.Fatal("pre-dial WaitForHandle never woke on dial")
	}
}

func TestPeerWaitForFreshHandleCancelWhileWaiting(t *testing.T) {
	provider, _ := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	waitCtx, waitCancel := context.WithCancel(ctx)

	type waitOutcome struct {
		handle adapterhost.Handle
		err    error
	}
	done := make(chan waitOutcome, 1)
	go func() {
		handle, err := provider.WaitForFreshHandle(waitCtx, "noop", "", nil)
		done <- waitOutcome{handle: handle, err: err}
	}()
	waitForWaiterRegistered(t, provider, "noop", "")
	waitCancel()

	select {
	case out := <-done:
		if !errors.Is(out.err, context.Canceled) {
			t.Fatalf("cancelled wait err = %v, want context.Canceled", out.err)
		}
		if out.handle != nil {
			t.Fatalf("cancelled wait returned handle %v, want nil", out.handle)
		}
		waitFor(t, "waiter cleanup after cancel", func() bool {
			return providerWaiterCount(provider, "noop", "") == 0
		})
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled WaitForFreshHandle never returned")
	}
}

func TestPeerWaitForFreshHandleBudgetExpiresWithoutHandshake(t *testing.T) {
	provider, _ := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})
	provider.shim.verifyFailureBudget = 120 * time.Millisecond
	defer func() { provider.shim.verifyFailureBudget = DefaultVerifyFailureBudget }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := provider.WaitForFreshHandle(ctx, "noop", "wait-scope", nil)
	if err == nil {
		t.Fatal("budget-expired wait returned a handle, want an error")
	}
	// The CRI-137 diagnosis class must surface through the shim's
	// waitTimeoutError, keyed by the waited adapter type + scope.
	msg := err.Error()
	if !strings.Contains(msg, "noop") || !strings.Contains(msg, "wait-scope") {
		t.Fatalf("budget error not keyed by adapter+scope: %q", msg)
	}
	if !strings.Contains(msg, "CRI-137") || !strings.Contains(msg, "no identity handshake observed") {
		t.Fatalf("budget error missing the no-handshake diagnosis: %q", msg)
	}
}

func TestPeerWaitForFreshHandleBudgetSurfacesRejectionDiagnosis(t *testing.T) {
	// Per-scope mode: the accept token is scope-registered, so a stale pod
	// presenting a pre-rotation token is rejected by the shim itself.
	provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0", PerScopeSessions: true})
	provider.RegisterScope("diag-scope", "good-token")
	provider.shim.verifyFailureBudget = 250 * time.Millisecond
	defer func() { provider.shim.verifyFailureBudget = DefaultVerifyFailureBudget }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	type waitOutcome struct {
		handle adapterhost.Handle
		err    error
	}
	done := make(chan waitOutcome, 1)
	go func() {
		handle, err := provider.WaitForFreshHandle(ctx, "noop", "diag-scope", nil)
		done <- waitOutcome{handle: handle, err: err}
	}()
	waitForWaiterRegistered(t, provider, "noop", "diag-scope")

	// A stale adapter pod presents a pre-rotation accept token: the shim
	// rejects the dial and attributes the failure to the pending waiters, so
	// the budget expiry carries that diagnosis instead of the generic one.
	conn := dialRawHandshake(t, addr, &handshakeMessage{
		Name:    "noop",
		Version: "1.0.0",
		Digest:  "sha256:abcd1234",
		Token:   "stale-token",
		Scope:   "diag-scope",
		Role:    "peer",
		Peer:    &PeerClientIdentity{CriteriaVersion: "test"},
	})
	_ = conn.Close()

	select {
	case out := <-done:
		if out.err == nil {
			t.Fatal("budget-expired wait after rejected dial returned a handle")
		}
		msg := out.err.Error()
		if !strings.Contains(msg, "last identity rejection") || !strings.Contains(msg, "CRI-137") {
			t.Fatalf("budget error missing the rejection diagnosis: %q", msg)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("budget-expired wait never returned")
	}
}

func TestPeerWaitForFreshHandleFallsBackToDefaultBudget(t *testing.T) {
	provider, _ := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})
	// A non-positive budget falls back to DefaultVerifyFailureBudget; the
	// wait is bounded by the caller's context instead of the wall clock.
	provider.shim.verifyFailureBudget = 0

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := provider.WaitForFreshHandle(ctx, "noop", "", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait err = %v, want context.DeadlineExceeded", err)
	}
}

func TestPeerWaitForFreshHandleWakesOnLegacyHandshake(t *testing.T) {
	provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Mixed fleet: a role-absent reattach dial stores the session on the
	// shim's legacy registry and must wake the provider's legacy waiter.
	type waitOutcome struct {
		handle adapterhost.Handle
		err    error
	}
	done := make(chan waitOutcome, 1)
	go func() {
		handle, err := provider.WaitForFreshHandle(ctx, "noop", "", nil)
		done <- waitOutcome{handle: handle, err: err}
	}()
	waitForWaiterRegistered(t, provider, "noop", "")

	if err := dialFakeAdapter(addr, &handshakeMessage{Name: "noop", Version: "1.0.0", Digest: "sha256:abcd1234"}, nil); err != nil {
		t.Fatalf("legacy dial: %v", err)
	}

	var legacy adapterhost.Handle
	select {
	case out := <-done:
		if out.err != nil {
			t.Fatalf("legacy wake: %v", out.err)
		}
		if out.handle == nil {
			t.Fatal("legacy wake returned a nil handle")
		}
		legacy = out.handle
		// The legacy handle comes from the shim's registry, not the provider's.
		if _, ok := peerHandleFrom(provider, "noop", ""); ok {
			t.Fatalf("legacy dial was misregistered as a peer session")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("legacy handshake never woke the provider wait")
	}
	info, err := legacy.Info(ctx)
	if err != nil {
		t.Fatalf("Info over legacy handle: %v", err)
	}
	if info.Name != "noop" {
		t.Fatalf("legacy Info = %q, want noop", info.Name)
	}
}

func TestPeerWaitForFreshHandleSkipsStaleLegacySession(t *testing.T) {
	provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})

	if err := dialFakeAdapter(addr, &handshakeMessage{Name: "noop", Version: "1.0.0", Digest: "sha256:abcd1234"}, nil); err != nil {
		t.Fatalf("legacy dial: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	legacy, err := provider.WaitForFreshHandle(ctx, "noop", "", nil)
	if err != nil {
		t.Fatalf("legacy wait: %v", err)
	}

	// The same legacy handle as `stale` is not fresh: the wait must not
	// return it; the caller's context bounds the wait instead.
	waitCtx, waitCancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer waitCancel()
	if h, err := provider.WaitForFreshHandle(waitCtx, "noop", "", legacy); h != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stale legacy returned h=%v err=%v, want deadline exceeded", h, err)
	}
}

func TestPeerStopWakesPendingWaiters(t *testing.T) {
	provider, _ := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	type waitOutcome struct {
		handle adapterhost.Handle
		err    error
	}
	done := make(chan waitOutcome, 1)
	go func() {
		handle, err := provider.WaitForHandle(ctx, "noop", "")
		done <- waitOutcome{handle: handle, err: err}
	}()
	waitForWaiterRegistered(t, provider, "noop", "")

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopCancel()
	if err := provider.Stop(stopCtx); err != nil {
		t.Fatalf("provider Stop: %v", err)
	}

	select {
	case out := <-done:
		if out.err == nil || !strings.Contains(out.err.Error(), "remote shim stopped") {
			t.Fatalf("pending wait after Stop err = %v, want remote shim stopped", out.err)
		}
		if out.handle != nil {
			t.Fatalf("pending wait after Stop returned handle %v, want nil", out.handle)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Stop never woke the pending waiter")
	}
}

func TestPeerWaitAfterStopRejected(t *testing.T) {
	provider, _ := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopCancel()
	if err := provider.Stop(stopCtx); err != nil {
		t.Fatalf("provider Stop: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := provider.WaitForHandle(ctx, "noop", ""); err == nil || !strings.Contains(err.Error(), "remote shim stopped") {
		t.Fatalf("post-stop wait err = %v, want remote shim stopped", err)
	}
}

func TestPeerListenAddrDelegatesToShim(t *testing.T) {
	provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})
	if got := provider.ListenAddr(); got != addr || got == "" {
		t.Fatalf("ListenAddr = %q, want shim addr %q", got, addr)
	}
}

func TestPeerCloseHandleFallsBackToLegacyShim(t *testing.T) {
	provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})

	if err := dialFakeAdapter(addr, &handshakeMessage{Name: "noop", Version: "1.0.0", Digest: "sha256:abcd1234"}, nil); err != nil {
		t.Fatalf("legacy dial: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := provider.WaitForHandle(ctx, "noop", ""); err != nil {
		t.Fatalf("WaitForHandle over legacy session: %v", err)
	}

	// No peer session for the key: the provider delegates the teardown to
	// the legacy shim, which cancels the bridge and drops the session.
	if err := provider.CloseHandle(ctx, "noop", ""); err != nil {
		t.Fatalf("CloseHandle fallback: %v", err)
	}
	provider.shim.mu.Lock()
	_, stillThere := provider.shim.sessions[provider.key("noop", "")]
	provider.shim.mu.Unlock()
	if stillThere {
		t.Fatal("legacy shim session survived CloseHandle")
	}

	// A close for an unknown key is a no-op, not an error.
	if err := provider.CloseHandle(ctx, "noop", "never-dialed"); err != nil {
		t.Fatalf("CloseHandle for unknown key: %v", err)
	}
}

// --- stream starter tests (LogStreamStarter + PermissionStreamer contract) ---

// testLogSink records the LogEvents a started log stream delivers.
type testLogSink struct {
	mu     sync.Mutex
	events []*v2.LogEvent
}

func (s *testLogSink) Emit(ev *v2.LogEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
	return nil
}

func (s *testLogSink) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

func TestPeerStartLogStreamDeliversChunks(t *testing.T) {
	provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})
	fp := newFakePeer("")
	fp.connect(t, addr)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	handle, err := provider.WaitForHandle(ctx, "noop", "")
	if err != nil {
		t.Fatalf("WaitForHandle: %v", err)
	}

	sink := &testLogSink{}
	// The host consumes the stream starter by runtime type assertion
	// (adapterhost sessions.go LogStreamStarter); assert through it.
	starter, ok := handle.(adapterhost.LogStreamStarter)
	if !ok {
		t.Fatalf("handle %T does not implement LogStreamStarter", handle)
	}
	startCancel, done, err := starter.StartLogStream(ctx, "ls-1", sink)
	if err != nil {
		t.Fatalf("StartLogStream: %v", err)
	}
	if startCancel == nil || done == nil {
		t.Fatal("StartLogStream returned nil cancel/done")
	}

	// The streamed chunk reaches the sink over the peer connection.
	waitFor(t, "log chunk delivery", func() bool { return sink.len() > 0 })
	sink.mu.Lock()
	ev := sink.events[0]
	sink.mu.Unlock()
	if ev.GetSessionId() != "ls-1" || string(ev.GetLine()) != "hello from peer" {
		t.Fatalf("unexpected log event: session=%q line=%q", ev.GetSessionId(), string(ev.GetLine()))
	}

	// The fake ends the stream cleanly, so done reports the benign close.
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("done after clean stream close = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("log stream done never fired")
	}
	startCancel() // safe after the stream ended
}

func TestPeerStartLogStreamReportsUnexpectedClose(t *testing.T) {
	provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})
	fp := newFakePeer("")
	fp.mu.Lock()
	fp.logErr = status.Error(codes.Internal, "peer disk exploded")
	fp.mu.Unlock()
	fp.connect(t, addr)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	handle, err := provider.WaitForHandle(ctx, "noop", "")
	if err != nil {
		t.Fatalf("WaitForHandle: %v", err)
	}

	sink := &testLogSink{}
	starter, ok := handle.(adapterhost.LogStreamStarter)
	if !ok {
		t.Fatalf("handle %T does not implement LogStreamStarter", handle)
	}
	startCancel, done, err := starter.StartLogStream(ctx, "ls-2", sink)
	if err != nil {
		t.Fatalf("StartLogStream: %v", err)
	}

	// The unexpected error surfaces on done for the heartbeat contract to
	// observe; cancel remains usable and idempotent.
	select {
	case got := <-done:
		if got == nil || status.Code(got) != codes.Internal {
			t.Fatalf("done = %v, want the peer's Internal error", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("log stream done never fired")
	}
	startCancel()
	startCancel()
}

func TestIsExpectedStreamClose(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		extra []codes.Code
		want  bool
	}{
		{"nil", nil, nil, true},
		{"eof", io.EOF, nil, true},
		{"ctx canceled", context.Canceled, nil, true},
		{"grpc canceled", status.Error(codes.Canceled, "bye"), nil, true},
		{"extra code", status.Error(codes.Unavailable, "conn reset"), []codes.Code{codes.Unavailable}, true},
		{"internal", status.Error(codes.Internal, "boom"), nil, false},
		{"unimplemented without extra", status.Error(codes.Unimplemented, "old build"), nil, false},
	}
	for _, tc := range cases {
		if got := isExpectedStreamClose(tc.err, tc.extra...); got != tc.want {
			t.Fatalf("%s: isExpectedStreamClose = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestPeerStartPermissionStreamRoundTrip(t *testing.T) {
	provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})
	fp := newFakePeer("")
	fp.connect(t, addr)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	handle, err := provider.WaitForHandle(ctx, "noop", "")
	if err != nil {
		t.Fatalf("WaitForHandle: %v", err)
	}
	ph := handle.(*peerHandle)

	requests := make(chan *v2.PermissionEvent, 4)
	streamCancel, err := ph.StartPermissionStream(ctx, "ps-1", requests)
	if err != nil {
		t.Fatalf("StartPermissionStream: %v", err)
	}

	// A permission request round-trips: the peer receives it and its
	// decision ACK flows back on the same bidi stream.
	requests <- &v2.PermissionEvent{
		Event: &v2.PermissionEvent_Request{Request: &v2.PermissionRequest{RequestId: "r1", Tool: "shell"}},
	}
	close(requests)
	waitFor(t, "permission request received by peer", func() bool {
		fp.mu.Lock()
		defer fp.mu.Unlock()
		return len(fp.permRecv) == 1 && fp.permRecv[0].GetRequest().GetRequestId() == "r1"
	})

	// Cancel marks the session inactive so Execute's fallback stream
	// resumes (the permActive contract shared with rpcHandle).
	streamCancel()
	waitFor(t, "permission stream deregistered", func() bool {
		ph.permMu.Lock()
		defer ph.permMu.Unlock()
		return len(ph.permActive) == 0
	})
}

func TestPeerStartPermissionStreamUnimplementedDrainsRequests(t *testing.T) {
	provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})
	fp := newFakePeer("")
	fp.mu.Lock()
	fp.permUnimplemented = true
	fp.mu.Unlock()
	fp.connect(t, addr)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	handle, err := provider.WaitForHandle(ctx, "noop", "")
	if err != nil {
		t.Fatalf("WaitForHandle: %v", err)
	}
	ph := handle.(*peerHandle)

	// A peer build predating the Permissions stream: the starter still
	// succeeds, drains the request channel so Evaluate never blocks, and
	// deregisters the session.
	requests := make(chan *v2.PermissionEvent)
	streamCancel, err := ph.StartPermissionStream(ctx, "ps-2", requests)
	if err != nil {
		t.Fatalf("StartPermissionStream: %v", err)
	}
	streamCancel()
	close(requests)
	waitFor(t, "unimplemented permission stream deregistered", func() bool {
		ph.permMu.Lock()
		defer ph.permMu.Unlock()
		return len(ph.permActive) == 0
	})
}

// --- supervision availability + error-arm tests ---

func TestPeerSupervisionUnimplementedKeepsHandleUsable(t *testing.T) {
	provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})
	fp := newFakePeer("")
	fp.mu.Lock()
	fp.superviseUnimplemented = true
	fp.mu.Unlock()
	fp.connect(t, addr)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	handle, err := provider.WaitForHandle(ctx, "noop", "")
	if err != nil {
		t.Fatalf("WaitForHandle: %v", err)
	}

	// A pre-PeerService peer build: the supervisor records unavailability in
	// the log, ProcessExited stays false (the caller falls back to error
	// heuristics, like a legacy handle), and the v2 service remains usable.
	logs := captureLogs(t)
	if _, ok := handle.(adapterhost.ProcessExitReporter); !ok {
		t.Fatalf("handle %T does not implement ProcessExitReporter", handle)
	}
	if handle.(adapterhost.ProcessExitReporter).ProcessExited() {
		t.Fatal("ProcessExited = true without any journal event")
	}
	if !waitForLog(t, logs, "does not implement supervision", 5*time.Second) {
		t.Fatal("supervision-unavailable warning never recorded")
	}
	if _, err := handle.Info(ctx); err != nil {
		t.Fatalf("Info still usable without supervision: %v", err)
	}
}

func TestPeerInfoAndKillErrorArms(t *testing.T) {
	provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})
	fp := newFakePeer("")
	fp.connect(t, addr)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	handle, err := provider.WaitForHandle(ctx, "noop", "")
	if err != nil {
		t.Fatalf("WaitForHandle: %v", err)
	}
	ph := handle.(*peerHandle)

	// Drop the transport: v2 calls fail with a transport error and the
	// kill control's failure arm is exercised (warn, not panic).
	fp.drop()
	waitFor(t, "transport teardown", func() bool { return ph.ps.cc.GetState() != connectivity.Ready })
	if _, err := ph.Info(ctx); err == nil {
		t.Fatal("Info after drop unexpectedly succeeded")
	}
	ph.Kill()
}

func TestPeerKillChildRejectionIsTolerated(t *testing.T) {
	provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})
	fp := newFakePeer("")
	fp.mu.Lock()
	fp.rejectKillChild = true
	fp.mu.Unlock()
	fp.connect(t, addr)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	handle, err := provider.WaitForHandle(ctx, "noop", "")
	if err != nil {
		t.Fatalf("WaitForHandle: %v", err)
	}

	// A peer that rejects the kill control still yields a nil Kill: the
	// rejection is logged, never fatal.
	handle.Kill()
}

func TestPeerProcessExitedNilGuards(t *testing.T) {
	var nilHandle *peerHandle
	if nilHandle.ProcessExited() {
		t.Fatal("nil handle ProcessExited = true")
	}
}
