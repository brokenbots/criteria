package peer

// Loopback tests for the phone-home gRPC server: the fixture drives the full
// pipeline (dial → identity frame → serve) over net.Pipe, with the test
// acting as the criteria host (the gRPC client on the held connection), the
// same topology the real shim/peer_session pair exercises end-to-end.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	criteriav1 "github.com/brokenbots/criteria/sdk/pb/criteria/v1"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow/version"
)

// fakePeerChild is an in-memory adapter v2 client backing the phone-home
// bridge; every method records its request so the round-trip tests can
// assert the full 12-RPC surface survives a served connection.
type fakePeerChild struct {
	mu             sync.Mutex
	infoCalled     int
	openSessionIDs []string
	closedSessions []string
	paused         []string
	resumed        []string
	snapshotted    []string
	restored       []string
	inspected      []string
	executeReq     *v2.ExecuteRequest
	logSessions    []string
	permEvents     []*v2.PermissionEvent
	prompt         *adapterhost.PromptRequest
	promptCalled   int
}

func (f *fakePeerChild) Info(ctx context.Context, req *v2.InfoRequest) (*v2.InfoResponse, error) {
	f.mu.Lock()
	f.infoCalled++
	f.mu.Unlock()
	return &v2.InfoResponse{Name: "fakex", Version: "2.0.0"}, nil
}

func (f *fakePeerChild) OpenSession(ctx context.Context, req *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	f.mu.Lock()
	f.openSessionIDs = append(f.openSessionIDs, req.GetSessionId())
	f.mu.Unlock()
	return &v2.OpenSessionResponse{}, nil
}

func (f *fakePeerChild) Execute(ctx context.Context, req *v2.ExecuteRequest, sink adapterhost.ExecuteEventSink) error {
	f.mu.Lock()
	f.executeReq = req
	f.mu.Unlock()
	if err := sink.Emit(&v2.ExecuteEvent{Event: &v2.ExecuteEvent_Tool{Tool: &v2.ToolInvocation{ToolName: "testtool"}}}); err != nil {
		return err
	}
	return sink.Emit(&v2.ExecuteEvent{Event: &v2.ExecuteEvent_Result{Result: &v2.ExecuteResult{Outcome: "success"}}})
}

func (f *fakePeerChild) Log(ctx context.Context, req *v2.LogRequest, sink adapterhost.LogEventSink) error {
	f.mu.Lock()
	f.logSessions = append(f.logSessions, req.GetSessionId())
	f.mu.Unlock()
	return sink.Emit(&v2.LogEvent{SessionId: req.GetSessionId(), Line: []byte("hello")})
}

func (f *fakePeerChild) Permissions(ctx context.Context, requests <-chan *v2.PermissionEvent) error {
	for ev := range requests {
		f.mu.Lock()
		f.permEvents = append(f.permEvents, ev)
		f.mu.Unlock()
	}
	return nil
}

func (f *fakePeerChild) Pause(ctx context.Context, req *v2.PauseRequest) (*v2.PauseResponse, error) {
	f.mu.Lock()
	f.paused = append(f.paused, req.GetSessionId())
	f.mu.Unlock()
	return &v2.PauseResponse{}, nil
}

func (f *fakePeerChild) Resume(ctx context.Context, req *v2.ResumeRequest) (*v2.ResumeResponse, error) {
	f.mu.Lock()
	f.resumed = append(f.resumed, req.GetSessionId())
	f.mu.Unlock()
	return &v2.ResumeResponse{}, nil
}

func (f *fakePeerChild) Snapshot(ctx context.Context, req *v2.SnapshotRequest) (*v2.SnapshotResponse, error) {
	f.mu.Lock()
	f.snapshotted = append(f.snapshotted, req.GetSessionId())
	f.mu.Unlock()
	return &v2.SnapshotResponse{State: []byte("state")}, nil
}

func (f *fakePeerChild) Restore(ctx context.Context, req *v2.RestoreRequest) (*v2.RestoreResponse, error) {
	f.mu.Lock()
	f.restored = append(f.restored, req.GetSessionId())
	f.mu.Unlock()
	return &v2.RestoreResponse{}, nil
}

func (f *fakePeerChild) Inspect(ctx context.Context, req *v2.InspectRequest) (*v2.InspectResponse, error) {
	f.mu.Lock()
	f.inspected = append(f.inspected, req.GetSessionId())
	f.mu.Unlock()
	return &v2.InspectResponse{}, nil
}

func (f *fakePeerChild) CloseSession(ctx context.Context, req *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
	f.mu.Lock()
	f.closedSessions = append(f.closedSessions, req.GetSessionId())
	f.mu.Unlock()
	return &v2.CloseSessionResponse{}, nil
}

func (f *fakePeerChild) Prompt(ctx context.Context, req *adapterhost.PromptRequest) (*adapterhost.PromptResponse, error) {
	f.mu.Lock()
	f.prompt = req
	f.promptCalled++
	f.mu.Unlock()
	return &adapterhost.PromptResponse{Accepted: true, Detail: "delivered"}, nil
}

// peerServeFixture wires a peer Server to a fake adapter child client for
// loopback tests.
type peerServeFixture struct {
	t      *testing.T
	cfg    Config
	rt     *peerRuntime
	server *Server
	child  *fakePeerChild
	ctx    context.Context
	cancel context.CancelFunc
}

func newPeerServeFixture(t *testing.T) *peerServeFixture {
	t.Helper()
	cfg, err := LoadConfig(getenvFrom(map[string]string{EnvRemoteHost: "pipe"}))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.AdapterName = "fakex"
	cfg.AdapterVersion = "9.9.9"
	cfg.Token = "tok"
	cfg.Scope = "sc"
	cfg.Digest = "sha256:aa"
	rt := NewRuntime(&cfg, captureLogger(&bytes.Buffer{}))
	child := &fakePeerChild{}
	server := NewServer(&cfg, rt, captureLogger(&bytes.Buffer{}))
	server.childClient = func() (adapterhost.Client, bool) { return child, true }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &peerServeFixture{
		t:      t,
		cfg:    cfg,
		rt:     rt,
		server: server,
		child:  child,
		ctx:    ctx,
		cancel: cancel,
	}
}

// append journals one event; returns the journaled event (with its seq).
func (f *peerServeFixture) append(kind EventKind) *criteriav1.SupervisionEvent {
	ev, err := f.rt.Journal().Append(kind, f.cfg.AdapterName, f.cfg.Scope, "")
	if err != nil {
		f.t.Fatalf("journal append: %v", err)
	}
	return ev
}

// startConn drives one serveOnce over net.Pipe: the fixture's dialFunc
// (one-shot) hands the server end of the pipe to the server; the returned
// client end is the host side. The first return value is the identity frame
// bytes read by the host before any gRPC traffic; the second is the error
// serveOnce returns when the connection ends.
func (f *peerServeFixture) startConn() (conn net.Conn, frame []byte, serveErr <-chan error) {
	f.t.Helper()
	serverConn, clientConn := net.Pipe()

	var dialed atomic.Bool
	f.server.dialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if !dialed.CompareAndSwap(false, true) {
			return nil, errors.New("fixture dialer: one-shot")
		}
		return serverConn, nil
	}

	frameCh := make(chan []byte, 1)
	go func() {
		defer close(frameCh)
		data, err := readFrameLine(clientConn)
		if err != nil {
			return
		}
		frameCh <- data
	}()

	serveErrCh := make(chan error, 1)
	go func() { serveErrCh <- f.server.serveOnce(f.ctx) }()

	var got []byte
	select {
	case data := <-frameCh:
		got = data
	case err := <-serveErrCh:
		f.t.Fatalf("serveOnce returned before the frame was served: %v", err)
	case <-time.After(5 * time.Second):
		f.t.Fatalf("identity frame not written within 5s")
	}
	return clientConn, got, serveErrCh
}

// readFrameLine reads one newline-terminated identity frame byte-at-a-time
// (no over-read into the gRPC stream that follows) with a bounded deadline.
// The read deadline is cleared before returning so the reset cannot race the
// gRPC transport's reads on the same connection.
func readFrameLine(conn net.Conn) ([]byte, error) {
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return nil, err
	}
	buf := make([]byte, 0, 512)
	one := make([]byte, 1)
	for {
		if _, err := io.ReadFull(conn, one); err != nil {
			return nil, err
		}
		if one[0] == '\n' {
			break
		}
		buf = append(buf, one[0])
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return nil, err
	}
	return buf, nil
}

// hostClient builds the criteria-host-side gRPC client over the held
// connection with a one-shot dialer (the transport cannot be re-dialed; a
// lost connection requires the peer to re-dial).
func (f *peerServeFixture) hostClient(conn net.Conn) *grpc.ClientConn {
	f.t.Helper()
	var dialed atomic.Bool
	cc, err := grpc.NewClient("passthrough:///criteria-peer",
		grpc.WithContextDialer(func(ctx context.Context, target string) (net.Conn, error) {
			if !dialed.CompareAndSwap(false, true) {
				return nil, errors.New("peer test transport: connection already handed out")
			}
			return conn, nil
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		f.t.Fatalf("grpc.NewClient: %v", err)
	}
	f.t.Cleanup(func() { _ = cc.Close() })
	return cc
}

// superviseStream is one open PeerService.Supervise stream with a single
// long-lived receiver goroutine feeding an event channel. A single receiver
// keeps multi-receive flows (replay check, then live events) deterministic:
// an abandoned short-timeout receive must not race a later one for the same
// next message.
type superviseStream struct {
	ss   grpc.ClientStream
	recv chan superviseRecv
}

type superviseRecv struct {
	ev  *criteriav1.SupervisionEvent
	err error
}

// openSupervise opens the PeerService.Supervise stream from since (the
// host's last applied event_seq), half-closes the send side (exactly as the
// host's superviseOnce does), and starts the single receiver goroutine.
func (f *peerServeFixture) openSupervise(cc *grpc.ClientConn, since uint64) *superviseStream {
	f.t.Helper()
	ss, err := cc.NewStream(f.ctx, &grpc.StreamDesc{StreamName: "Supervise", ServerStreams: true}, peerSuperviseMethod)
	if err != nil {
		f.t.Fatalf("open supervise stream: %v", err)
	}
	if err := ss.SendMsg(&criteriav1.SupervisionRequest{SinceEventSeq: since}); err != nil {
		f.t.Fatalf("send supervise request: %v", err)
	}
	if err := ss.CloseSend(); err != nil {
		f.t.Fatalf("close send: %v", err)
	}
	st := &superviseStream{ss: ss, recv: make(chan superviseRecv, 8)}
	go func() {
		for {
			ev := new(criteriav1.SupervisionEvent)
			err := ss.RecvMsg(ev)
			st.recv <- superviseRecv{ev: ev, err: err}
			if err != nil {
				return
			}
		}
	}()
	return st
}

func (st *superviseStream) recvSupervisionEvent(timeout time.Duration) (*criteriav1.SupervisionEvent, error, bool) {
	select {
	case r := <-st.recv:
		if r.err != nil {
			return nil, r.err, true
		}
		return r.ev, nil, true
	case <-time.After(timeout):
		return nil, errors.New("supervise recv timed out"), false
	}
}

func (f *peerServeFixture) mustRecvSupervisionEvent(st *superviseStream, timeout time.Duration) *criteriav1.SupervisionEvent {
	f.t.Helper()
	ev, err, ok := st.recvSupervisionEvent(timeout)
	if !ok {
		f.t.Fatalf("supervise recv timed out after %s", timeout)
	}
	if err != nil {
		f.t.Fatalf("supervise recv: %v", err)
	}
	return ev
}

// TestServer_IdentityFrameGoldenJSON pins the exact identity frame bytes:
// field order matches the documented frame shape, sdk_protocol_version is 2,
// role is "peer", and the peer capabilities block advertises the full v2
// adapter surface plus supervision.
func TestServer_IdentityFrameGoldenJSON(t *testing.T) {
	cfg, err := LoadConfig(getenvFrom(map[string]string{EnvRemoteHost: "pipe"}))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.AdapterName = "fakex"
	cfg.AdapterVersion = "9.9.9"
	cfg.Token = "tok"
	cfg.Scope = "sc"
	cfg.Digest = "sha256:aa"
	rt := NewRuntime(&cfg, captureLogger(&bytes.Buffer{}))
	server := NewServer(&cfg, rt, captureLogger(&bytes.Buffer{}))

	got, err := server.identityFrame()
	if err != nil {
		t.Fatalf("identityFrame: %v", err)
	}
	want := `{"name":"fakex","version":"9.9.9","digest":"sha256:aa","token":"tok","scope":"sc","sdk_protocol_version":2,"role":"peer","peer":{"criteria_version":"` + version.Version + `","capabilities":["adapter.v2.full","supervision.v1"]}}` + "\n"
	if string(got) != want {
		t.Errorf("identity frame =\n%s\nwant\n%s", got, want)
	}
	if len(got) > peerHandshakeFrameCap {
		t.Errorf("identity frame is %d bytes, want <= %d", len(got), peerHandshakeFrameCap)
	}
}

// TestServer_IdentityFrameOverCapRejected keeps the unauthenticated first
// write bounded.
func TestServer_IdentityFrameOverCapRejected(t *testing.T) {
	cfg, err := LoadConfig(getenvFrom(map[string]string{EnvRemoteHost: "pipe"}))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.AdapterName = "fakex"
	cfg.Token = string(make([]byte, peerHandshakeFrameCap))
	rt := NewRuntime(&cfg, captureLogger(&bytes.Buffer{}))
	server := NewServer(&cfg, rt, captureLogger(&bytes.Buffer{}))
	if _, err := server.identityFrame(); err == nil {
		t.Fatal("identityFrame accepted a frame over the 16 KiB cap")
	}
}

// TestServer_ServesAdapterAndPeerServices drives the full pipeline (dial,
// identity frame, serve) and round-trips ALL 12 v2 AdapterService RPCs
// through the held connection against the local adapter client.
func TestServer_ServesAdapterAndPeerServices(t *testing.T) {
	f := newPeerServeFixture(t)
	conn, got, serveErr := f.startConn()
	want, err := f.server.identityFrame()
	if err != nil {
		t.Fatalf("identityFrame: %v", err)
	}
	// readFrameLine strips the terminating newline.
	if !bytes.Equal(got, bytes.TrimSuffix(want, []byte("\n"))) {
		t.Fatalf("identity frame mismatch:\n%s\nwant\n%s", got, want)
	}
	cc := f.hostClient(conn)
	client := adapterhost.NewClientForConn(cc)
	ctx := context.Background()

	t.Run("Info", func(t *testing.T) {
		resp, err := client.Info(ctx, &v2.InfoRequest{})
		if err != nil {
			t.Fatalf("Info: %v", err)
		}
		if resp.GetName() != "fakex" || resp.GetVersion() != "2.0.0" {
			t.Errorf("Info = %q/%q, want fakex/2.0.0", resp.GetName(), resp.GetVersion())
		}
	})
	t.Run("OpenSession", func(t *testing.T) {
		if _, err := client.OpenSession(ctx, &v2.OpenSessionRequest{SessionId: "s-open"}); err != nil {
			t.Fatalf("OpenSession: %v", err)
		}
	})
	t.Run("Execute", func(t *testing.T) {
		var tools, results int
		err := client.Execute(ctx, &v2.ExecuteRequest{SessionId: "s-exec"}, execSinkFn(func(ev *v2.ExecuteEvent) error {
			switch {
			case ev.GetTool() != nil:
				tools++
			case ev.GetResult() != nil:
				results++
			}
			return nil
		}))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if tools != 1 || results != 1 {
			t.Errorf("Execute events = %d tools/%d results, want 1/1", tools, results)
		}
	})
	t.Run("Log", func(t *testing.T) {
		// The bridge holds the host Log stream open after the backend
		// returns (the host drains it for the session lifetime), so the
		// round-trip asserts on the delivered event and accepts the
		// deadline error that ends the held stream.
		logCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()
		var lines [][]byte
		_ = client.Log(logCtx, &v2.LogRequest{SessionId: "s-log"}, logSinkFn(func(ev *v2.LogEvent) error {
			lines = append(lines, ev.GetLine())
			return nil
		}))
		if len(lines) != 1 || string(lines[0]) != "hello" {
			t.Errorf("Log events = %v, want one 'hello' line", lines)
		}
	})
	t.Run("Permissions", func(t *testing.T) {
		requests := make(chan *v2.PermissionEvent, 1)
		requests <- &v2.PermissionEvent{Event: &v2.PermissionEvent_Request{Request: &v2.PermissionRequest{RequestId: "r1"}}}
		close(requests)
		if err := client.Permissions(ctx, requests); err != nil {
			t.Fatalf("Permissions: %v", err)
		}
	})
	t.Run("Pause", func(t *testing.T) {
		if _, err := client.Pause(ctx, &v2.PauseRequest{SessionId: "s-pause"}); err != nil {
			t.Fatalf("Pause: %v", err)
		}
	})
	t.Run("Resume", func(t *testing.T) {
		if _, err := client.Resume(ctx, &v2.ResumeRequest{SessionId: "s-resume"}); err != nil {
			t.Fatalf("Resume: %v", err)
		}
	})
	t.Run("Snapshot", func(t *testing.T) {
		resp, err := client.Snapshot(ctx, &v2.SnapshotRequest{SessionId: "s-snap"})
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		if string(resp.GetState()) != "state" {
			t.Errorf("Snapshot state = %q, want state", resp.GetState())
		}
	})
	t.Run("Restore", func(t *testing.T) {
		if _, err := client.Restore(ctx, &v2.RestoreRequest{SessionId: "s-restore", State: []byte("state")}); err != nil {
			t.Fatalf("Restore: %v", err)
		}
	})
	t.Run("Inspect", func(t *testing.T) {
		if _, err := client.Inspect(ctx, &v2.InspectRequest{SessionId: "s-inspect"}); err != nil {
			t.Fatalf("Inspect: %v", err)
		}
	})
	t.Run("CloseSession", func(t *testing.T) {
		if _, err := client.CloseSession(ctx, &v2.CloseSessionRequest{SessionId: "s-close"}); err != nil {
			t.Fatalf("CloseSession: %v", err)
		}
	})
	t.Run("Prompt", func(t *testing.T) {
		resp, err := client.Prompt(ctx, &adapterhost.PromptRequest{SessionID: "s-prompt", Prompt: "stop"})
		if err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		if !resp.Accepted || resp.Detail != "delivered" {
			t.Errorf("Prompt = %+v, want accepted/delivered", resp)
		}
	})

	f.child.mu.Lock()
	roundTrips := f.child.infoCalled + len(f.child.openSessionIDs) + len(f.child.executeReqSessions()) + len(f.child.logSessions) +
		len(f.child.permEvents) + len(f.child.paused) + len(f.child.resumed) + len(f.child.snapshotted) +
		len(f.child.restored) + len(f.child.inspected) + len(f.child.closedSessions) + f.child.promptCalled
	f.child.mu.Unlock()
	if roundTrips != 12 {
		t.Errorf("child backend saw %d of 12 expected calls: %+v", roundTrips, f.child)
	}
	f.cancel()
	select {
	case <-serveErr:
	case <-time.After(5 * time.Second):
		t.Error("serveOnce did not return after cancel")
	}
}

func (f *fakePeerChild) executeReqSessions() []string {
	if f.executeReq == nil {
		return nil
	}
	return []string{f.executeReq.GetSessionId()}
}

type execSinkFn func(*v2.ExecuteEvent) error

func (fn execSinkFn) Emit(ev *v2.ExecuteEvent) error { return fn(ev) }

type logSinkFn func(*v2.LogEvent) error

func (fn logSinkFn) Emit(ev *v2.LogEvent) error { return fn(ev) }

// TestServer_SuperviseReplayThenLiveThenReconnect verifies the supervision
// stream semantics: an exact one-time replay of the unseen suffix, live
// events as they are journaled, and a reconnect replay that starts strictly
// after the host's since_event_seq (no duplicates, no gaps).
func TestServer_SuperviseReplayThenLiveThenReconnect(t *testing.T) {
	f := newPeerServeFixture(t)
	ev1 := f.append(&criteriav1.SupervisionEvent_Spawned{Spawned: &criteriav1.ProcessSpawned{Binary: "bin", Pid: 1}})
	ev2 := f.append(&criteriav1.SupervisionEvent_Exited{Exited: &criteriav1.ProcessExited{ExitCode: -1}})

	conn, _, _ := f.startConn()
	cc := f.hostClient(conn)

	// The host reconnects with since_event_seq=1: exactly [ev2] is replayed.
	ss := f.openSupervise(cc, ev1.GetEventSeq())
	got1 := f.mustRecvSupervisionEvent(ss, 5*time.Second)
	if got1.GetEventSeq() != ev2.GetEventSeq() {
		t.Fatalf("replay event 1 seq = %d, want %d", got1.GetEventSeq(), ev2.GetEventSeq())
	}
	if got1.GetExited() == nil {
		t.Fatalf("replay event 1 kind = %T, want exited", got1.GetKind())
	}

	// Live journaling streams immediately.
	ev3 := f.append(&criteriav1.SupervisionEvent_Spawned{Spawned: &criteriav1.ProcessSpawned{Binary: "bin2", Pid: 2}})
	got2 := f.mustRecvSupervisionEvent(ss, 5*time.Second)
	if got2.GetEventSeq() != ev3.GetEventSeq() {
		t.Fatalf("live event seq = %d, want %d", got2.GetEventSeq(), ev3.GetEventSeq())
	}
	if got2.GetSpawned() == nil || got2.GetSpawned().GetBinary() != "bin2" {
		t.Fatalf("live event = %+v, want spawned bin2", got2.GetKind())
	}

	// A stream-level reset replays strictly after the host's cursor: exactly
	// once, no duplicates.
	ss2 := f.openSupervise(cc, ev3.GetEventSeq())
	if dup, err, ok := ss2.recvSupervisionEvent(200 * time.Millisecond); ok && err == nil {
		t.Fatalf("reconnect replayed an already-seen event: seq %d %+v", dup.GetEventSeq(), dup.GetKind())
	}
	ev4 := f.append(&criteriav1.SupervisionEvent_Exited{Exited: &criteriav1.ProcessExited{ExitCode: 0}})
	got3 := f.mustRecvSupervisionEvent(ss2, 5*time.Second)
	if got3.GetEventSeq() != ev4.GetEventSeq() {
		t.Fatalf("reconnect live event seq = %d, want %d", got3.GetEventSeq(), ev4.GetEventSeq())
	}
}

// TestServer_SuperviseHeartbeat verifies the idle heartbeat on an open
// Supervise stream when no journal events flow.
func TestServer_SuperviseHeartbeat(t *testing.T) {
	f := newPeerServeFixture(t)
	f.server.heartbeat = 25 * time.Millisecond

	conn, _, _ := f.startConn()
	cc := f.hostClient(conn)

	ss := f.openSupervise(cc, 0)
	got := f.mustRecvSupervisionEvent(ss, 3*time.Second)
	hb := got.GetHeartbeat()
	if hb == nil {
		t.Fatalf("event = %+v, want supervision heartbeat", got.GetKind())
	}
	if hb.GetLastEventSeq() != 0 {
		t.Errorf("heartbeat last_event_seq = %d, want 0 (empty journal)", hb.GetLastEventSeq())
	}
}

// TestServer_SuperviseHeartbeatResumesAfterEvents verifies the heartbeat
// timer restarts after journal activity (no heartbeat while events flow).
func TestServer_SuperviseHeartbeatResumesAfterEvents(t *testing.T) {
	f := newPeerServeFixture(t)
	f.server.heartbeat = 50 * time.Millisecond

	conn, _, _ := f.startConn()
	cc := f.hostClient(conn)

	ss := f.openSupervise(cc, 0)
	ev := f.append(&criteriav1.SupervisionEvent_Spawned{Spawned: &criteriav1.ProcessSpawned{Binary: "bin", Pid: 1}})
	got := f.mustRecvSupervisionEvent(ss, 5*time.Second)
	if got.GetEventSeq() != ev.GetEventSeq() {
		t.Fatalf("first event seq = %d, want %d", got.GetEventSeq(), ev.GetEventSeq())
	}
	// The journal is quiet now: the next message must be a heartbeat, not a
	// spurious replay.
	got2 := f.mustRecvSupervisionEvent(ss, 3*time.Second)
	if got2.GetHeartbeat() == nil {
		t.Fatalf("second event = %+v, want heartbeat", got2.GetKind())
	}
}

// TestServer_ControlKillChild exercises PeerService.Control over the served
// connection: kill_child is acknowledged, the child is killed after the
// grace period, and the watcher journals a peer-initiated (graceful) exit
// without a crash classification.
func TestServer_ControlKillChild(t *testing.T) {
	if testing.Short() {
		t.Skip("control test boots a runtime child")
	}
	f := newPeerServeFixture(t)
	fake := &fakeHandle{}
	f.rt.loader.RegisterBuiltin("fakex", func() adapterhost.Handle { return fake })
	f.rt.exitPoll = 10 * time.Millisecond
	if err := f.rt.Boot(f.ctx); err != nil {
		t.Fatalf("boot: %v", err)
	}

	conn, _, _ := f.startConn()
	cc := f.hostClient(conn)

	out := new(criteriav1.ControlResponse)
	if err := cc.Invoke(f.ctx, peerControlMethod, &criteriav1.ControlRequest{
		AdapterType: f.cfg.AdapterName,
		Scope:       f.cfg.Scope,
		GraceMs:     10,
		Kind:        &criteriav1.ControlRequest_KillChild{KillChild: &criteriav1.KillChild{}},
	}, out); err != nil {
		t.Fatalf("Control: %v", err)
	}
	if !out.GetAccepted() {
		t.Fatalf("Control response = %+v, want accepted", out)
	}

	deadline := time.Now().Add(5 * time.Second)
	for !fake.killed.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !fake.killed.Load() {
		t.Fatal("child was not killed after the control grace period")
	}
	<-f.rt.watchDone // watcher recorded the exit fact

	events := f.rt.Journal().Replay(0)
	if len(events) != 2 {
		t.Fatalf("journal has %d events, want spawned+exited", len(events))
	}
	exited := events[1].GetExited()
	if exited == nil || !exited.GetGraceful() {
		t.Fatalf("event 2 = %+v, want graceful exited (peer-initiated kill)", events[1].GetKind())
	}
	if crash := events[1].GetCrash(); crash != nil {
		t.Errorf("peer-initiated kill classified as a crash: %+v", crash)
	}
	if err := f.rt.Shutdown(f.ctx); err != nil {
		t.Errorf("shutdown: %v", err)
	}
}

// TestServer_LogStreamEndJournalsFlushed verifies the ADR-0007 emission
// point: when the child's log stream ends, the peer journals
// StreamFlushed{channel: "log", up_to_seq} with up_to_seq equal to the
// journal's highest sequence recorded before the Flushed fact itself. The
// bridge holds the host-facing Log RPC open after the backend returns (the
// host cancels it at session close), so the fact is journaled while the RPC
// is still open; cancelling the host ctx ends the RPC.
func TestServer_LogStreamEndJournalsFlushed(t *testing.T) {
	f := newPeerServeFixture(t)
	f.append(&criteriav1.SupervisionEvent_Spawned{Spawned: &criteriav1.ProcessSpawned{Binary: "bin", Pid: 1}})
	upToBefore := f.rt.Journal().LastSeq()

	conn, _, _ := f.startConn()
	cc := f.hostClient(conn)
	client := adapterhost.NewClientForConn(cc)

	var lines atomic.Int32
	logCtx, logCancel := context.WithCancel(context.Background())
	defer logCancel()
	logDone := make(chan error, 1)
	go func() {
		logDone <- client.Log(logCtx, &v2.LogRequest{SessionId: "s-log"}, logSinkFn(func(ev *v2.LogEvent) error {
			lines.Add(1)
			return nil
		}))
	}()

	// Wait for the event to flow to the host.
	deadline := time.Now().Add(5 * time.Second)
	for lines.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := lines.Load(); got != 1 {
		t.Fatalf("host saw %d log events, want 1", got)
	}

	// The backend log stream ended (the fake returns after one event), so
	// the flushed fact is already journaled.
	events := f.rt.Journal().Replay(0)
	if len(events) != 2 {
		t.Fatalf("journal has %d events, want spawned+flushed", len(events))
	}
	flushed := events[1].GetFlushed()
	if flushed == nil {
		t.Fatalf("event 2 = %T, want flushed", events[1].GetKind())
	}
	if flushed.GetChannel() != "log" {
		t.Errorf("flushed channel = %q, want log", flushed.GetChannel())
	}
	if flushed.GetUpToSeq() != upToBefore {
		t.Errorf("flushed up_to_seq = %d, want %d (journal seq recorded before the flushed append)", flushed.GetUpToSeq(), upToBefore)
	}

	// The host-side Log RPC stays open until cancelled; cancelling it
	// surfaces as a client-side Canceled on the stream (session close).
	logCancel()
	select {
	case err := <-logDone:
		if err != nil && status.Code(err) != codes.Canceled {
			t.Errorf("host Log = %v, want clean end or canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("host Log did not return after cancel")
	}
}

// TestServer_ServeShutdown verifies the peer shutdown sequence driven by
// Serve: a context cancel ends the serve loop (nil error, exit 0), the
// host's served sessions are closed on the child, the child is killed after
// the grace period, and the journal ends with a graceful ProcessExited.
func TestServer_ServeShutdown(t *testing.T) {
	if testing.Short() {
		t.Skip("serve shutdown boots a runtime child")
	}
	f := newPeerServeFixture(t)
	fake := &fakeHandle{}
	f.rt.loader.RegisterBuiltin("fakex", func() adapterhost.Handle { return fake })
	f.rt.exitPoll = 10 * time.Millisecond
	f.rt.shutdownGrace = 50 * time.Millisecond
	if err := f.rt.Boot(f.ctx); err != nil {
		t.Fatalf("boot: %v", err)
	}

	serverConn, clientConn := net.Pipe()
	var dialed atomic.Bool
	f.server.dialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if !dialed.CompareAndSwap(false, true) {
			return nil, errors.New("fixture dialer: one-shot")
		}
		return serverConn, nil
	}

	serveRet := make(chan error, 1)
	go func() { serveRet <- f.server.Serve(f.ctx) }()

	// The peer writes the identity frame before any gRPC traffic; read it
	// off the host side first (same as startConn) so the transport's first
	// bytes are the client preface.
	frame := make(chan []byte, 1)
	go func() {
		data, err := readFrameLine(clientConn)
		if err != nil {
			return
		}
		frame <- data
	}()
	select {
	case <-frame:
	case <-time.After(5 * time.Second):
		t.Fatal("identity frame not written within 5s")
	}

	cc := f.hostClient(clientConn)
	client := adapterhost.NewClientForConn(cc)
	if _, err := client.OpenSession(context.Background(), &v2.OpenSessionRequest{SessionId: "s-shut"}); err != nil {
		t.Fatalf("OpenSession through bridge: %v", err)
	}
	f.rt.mu.Lock()
	tracked := len(f.rt.openSessions)
	f.rt.mu.Unlock()
	if tracked != 1 {
		t.Fatalf("tracked open sessions = %d, want 1", tracked)
	}

	f.cancel()
	select {
	case err := <-serveRet:
		if err != nil {
			t.Fatalf("Serve = %v, want nil (context-caused end maps to exit 0)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}

	// The tracked session was closed on the served child surface.
	found := false
	f.child.mu.Lock()
	for _, id := range f.child.closedSessions {
		if id == "s-shut" {
			found = true
		}
	}
	f.child.mu.Unlock()
	if !found {
		t.Errorf("served child close sessions = %v, want s-shut", f.child.closedSessions)
	}

	// Journal ends with the graceful exit fact.
	events := f.rt.Journal().Replay(0)
	if len(events) != 2 {
		t.Fatalf("journal has %d events, want spawned+exited", len(events))
	}
	exited := events[1].GetExited()
	if exited == nil || !exited.GetGraceful() {
		t.Fatalf("event 2 = %+v, want graceful exited", events[1].GetKind())
	}
	if !fake.killed.Load() {
		t.Error("child was not killed by shutdown")
	}
}

// TestServer_ControlRejectsUnsupportedAndDeadChildren covers the
// Accepted=false paths of the Control RPC.
func TestServer_ControlRejectsUnsupportedAndDeadChildren(t *testing.T) {
	f := newPeerServeFixture(t)
	fake := &fakeHandle{}
	f.rt.loader.RegisterBuiltin("fakex", func() adapterhost.Handle { return fake })
	if err := f.rt.Boot(f.ctx); err != nil {
		t.Fatalf("boot: %v", err)
	}
	// Unsupported action.
	if resp := f.server.Control(f.ctx, &criteriav1.ControlRequest{}); resp.GetAccepted() {
		t.Errorf("unsupported action accepted: %+v", resp)
	}
	// Already-exited child.
	fake.exited.Store(true)
	resp := f.server.Control(f.ctx, &criteriav1.ControlRequest{
		Kind: &criteriav1.ControlRequest_KillChild{KillChild: &criteriav1.KillChild{}},
	})
	if resp.GetAccepted() {
		t.Errorf("kill_child accepted for a dead child: %+v", resp)
	}
}

// TestServer_BackoffProgression verifies the reconnect backoff is full
// jitter (delay in [floor, min(2*prev, max))) and never the legacy runner's
// fixed 2s lockstep. The sleep seam is stubbed so no real wall time passes.
func TestServer_BackoffProgression(t *testing.T) {
	cfg, err := LoadConfig(getenvFrom(map[string]string{EnvRemoteHost: "pipe"}))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.AdapterName = "fakex"
	cfg.BackoffMin = time.Second
	cfg.BackoffMax = 30 * time.Second
	rt := NewRuntime(&cfg, captureLogger(&bytes.Buffer{}))
	server := NewServer(&cfg, rt, captureLogger(&bytes.Buffer{}))
	server.childClient = func() (adapterhost.Client, bool) { return nil, false }

	// Fixed rand=0.5 with floor 1s and ceilings 2s,3s,4s,5s yields delays
	// 1.5s, 2s, 2.5s, 3s — strictly growing, and not the legacy constant 2s.
	want := []time.Duration{1500 * time.Millisecond, 2 * time.Second, 2500 * time.Millisecond, 3 * time.Second}

	var delays []time.Duration
	server.rand = func() float64 { return 0.5 }
	server.dialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return nil, errors.New("dial refused")
	}
	server.sleep = func(ctx context.Context, d time.Duration) error {
		delays = append(delays, d)
		if len(delays) >= len(want) {
			return context.Canceled
		}
		return nil
	}
	_ = server.Run(context.Background())

	if len(delays) != len(want) {
		t.Fatalf("backoff delays = %v (%d), want %v", delays, len(delays), want)
	}
	allTwoSeconds := true
	for i, d := range delays {
		if d != want[i] {
			t.Errorf("backoff delay %d = %s, want %s", i, d, want[i])
		}
		if d != 2*time.Second {
			allTwoSeconds = false
		}
		if i > 0 && d <= delays[i-1] {
			t.Errorf("backoff delay %d = %s not greater than previous %s", i, d, delays[i-1])
		}
	}
	if allTwoSeconds {
		t.Error("backoff is the legacy fixed 2s lockstep; want full jitter")
	}
}

// TestServer_NextBackoffCapsAtMax verifies the ceiling clamp at BackoffMax.
func TestServer_NextBackoffCapsAtMax(t *testing.T) {
	cfg, err := LoadConfig(getenvFrom(map[string]string{EnvRemoteHost: "pipe"}))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.AdapterName = "fakex"
	cfg.BackoffMin = time.Second
	cfg.BackoffMax = 3 * time.Second
	rt := NewRuntime(&cfg, captureLogger(&bytes.Buffer{}))
	server := NewServer(&cfg, rt, captureLogger(&bytes.Buffer{}))
	server.rand = func() float64 { return 0.99 }

	prev := server.nextBackoff(cfg.BackoffMin) // [1s, 2s) @ 0.99 → 1.99s
	if prev != 1990*time.Millisecond {
		t.Fatalf("first backoff = %s, want 1.99s", prev)
	}
	prev = server.nextBackoff(prev) // ceiling = min(3.98s, 3s) = 3s → 2.98s
	if prev != 2980*time.Millisecond {
		t.Fatalf("second backoff = %s, want 2.98s", prev)
	}
	prev = server.nextBackoff(prev) // ceiling clamped to 3s → 2.98s
	if prev > cfg.BackoffMax {
		t.Errorf("backoff %s exceeds BackoffMax %s", prev, cfg.BackoffMax)
	}
}

// TestServer_RunCancelsDuringBlockedDial verifies the phone-home loop aborts
// promptly when SIGTERM lands mid-dial (net.Dialer.DialContext honors a
// cancelled context; the loop itself must too).
func TestServer_RunCancelsDuringBlockedDial(t *testing.T) {
	cfg, err := LoadConfig(getenvFrom(map[string]string{EnvRemoteHost: "pipe"}))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.AdapterName = "fakex"
	rt := NewRuntime(&cfg, captureLogger(&bytes.Buffer{}))
	server := NewServer(&cfg, rt, captureLogger(&bytes.Buffer{}))
	release := make(chan struct{})
	server.dialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return nil, errors.New("unreachable")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- server.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-runErr:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not abort the blocked dial attempt on cancel")
	}
	close(release)
}

// TestServer_DialHonoursCancelledContext pins the transport-level mechanism:
// net.Dialer.DialContext (not bare net.Dial) must fail fast on a cancelled
// context so SIGTERM aborts an in-flight dial.
func TestServer_DialHonoursCancelledContext(t *testing.T) {
	cfg, err := LoadConfig(getenvFrom(map[string]string{EnvRemoteHost: "127.0.0.1:1"}))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.AdapterName = "fakex"
	rt := NewRuntime(&cfg, captureLogger(&bytes.Buffer{}))
	server := NewServer(&cfg, rt, captureLogger(&bytes.Buffer{}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err = server.dial(ctx, server.network(), server.cfg.Host)
	if err == nil {
		t.Fatal("dial succeeded with a cancelled context")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("dial took %s to fail on a cancelled context", elapsed)
	}
}

// TestServer_DialClassifiesUnixHosts verifies the network classification for
// unix socket paths (TLS never applies to unix dials).
func TestServer_DialClassifiesUnixHosts(t *testing.T) {
	cfg, err := LoadConfig(getenvFrom(map[string]string{EnvRemoteHost: "/run/criteria/peer.sock"}))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	rt := NewRuntime(&cfg, captureLogger(&bytes.Buffer{}))
	server := NewServer(&cfg, rt, captureLogger(&bytes.Buffer{}))
	if got := server.network(); got != peerNetworkUnix {
		t.Errorf("network = %q, want unix for absolute socket path", got)
	}
	cfg.Host = "127.0.0.1:7999"
	if got := server.network(); got != peerNetworkTCP {
		t.Errorf("network = %q, want tcp for host:port", got)
	}
}
