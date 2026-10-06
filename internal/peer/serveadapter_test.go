package peer

// Serve-adapter child-role tests (ADR-0008): the phone-home server built by
// NewServeAdapterServer registers the in-process adapter unwrapped, streams
// supervision from the caller-supplied journal, routes CancelChildRun
// control to the adapter's ChildRunCanceler surface, and ends the loop —
// without reconnecting — when the adapter requests process exit.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"

	criteriav1 "github.com/brokenbots/criteria/sdk/pb/criteria/v1"

	adapterhost "github.com/brokenbots/criteria/internal/adapterhost"
)

// fakeWorkflowAdapter is the in-process adapter surface for serve-adapter
// server tests: only the methods under test are stubbed.
type fakeWorkflowAdapter struct {
	adapterhost.Client

	mu             sync.Mutex
	infoCalls      int
	openSessions   []string
	closedSessions []string
	cancelRequests []string
	acceptedRunID  string
	// exitFn, when set, runs as CloseSession's last act inside the handler —
	// exactly how production wires it (runServeAdapter calls
	// impl.setExit(peer.Server.RequestExit), the CLI's own adapter impl calls
	// exit() before returning the response).
	exitFn func()
}

func (f *fakeWorkflowAdapter) Info(ctx context.Context, _ *v2.InfoRequest) (*v2.InfoResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.infoCalls++
	return &v2.InfoResponse{Name: "workflow.child", Version: "1.0.0"}, nil
}

func (f *fakeWorkflowAdapter) OpenSession(ctx context.Context, req *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.openSessions = append(f.openSessions, req.GetSessionId())
	return &v2.OpenSessionResponse{}, nil
}

func (f *fakeWorkflowAdapter) CloseSession(ctx context.Context, req *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
	f.mu.Lock()
	f.closedSessions = append(f.closedSessions, req.GetSessionId())
	exit := f.exitFn
	f.mu.Unlock()
	if exit != nil {
		exit()
	}
	return &v2.CloseSessionResponse{}, nil
}

// gatedReadConn wraps the host-side conn so a test can hold back bytes the
// gRPC transport would otherwise read. While the gate is closed no byte is
// consumed from the underlying conn: CloseGate pokes (via a short read
// deadline) any reader already parked inside a raw read so it re-parks at the
// gate, leaving nothing to drain the (unbuffered net.Pipe) server-side writes
// — a response written while gated stays blocked in the pipe, so a teardown
// that kills the transport while it is gated destroys the response for good,
// exactly like a host TCP receive queue destroyed by an RST. Once the gate
// opens, the deadline is cleared and reads flow through.
type gatedReadConn struct {
	net.Conn
	mu       sync.Mutex
	cond     *sync.Cond
	gateOpen bool
	buf      bytes.Buffer
	rawErr   error
	closed   bool
}

func newGatedReadConn(c net.Conn) *gatedReadConn {
	g := &gatedReadConn{Conn: c, gateOpen: true}
	g.cond = sync.NewCond(&g.mu)
	return g
}

func (g *gatedReadConn) OpenGate() {
	g.mu.Lock()
	defer g.mu.Unlock()
	// Clear the poke deadline before waking readers so their next raw read
	// is not cut off by a stale poke.
	_ = g.Conn.SetReadDeadline(time.Time{})
	g.gateOpen = true
	g.cond.Broadcast()
}

func (g *gatedReadConn) CloseGate() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.gateOpen = false
	// Poke readers parked inside the raw read so they re-evaluate the gate
	// instead of consuming bytes while it is held.
	_ = g.Conn.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	g.cond.Broadcast()
}

func (g *gatedReadConn) Read(p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for {
		if g.closed {
			g.buf.Reset()
			if g.rawErr != nil {
				return 0, g.rawErr
			}
			return 0, io.EOF
		}
		if !g.gateOpen {
			g.cond.Wait()
			continue
		}
		if g.buf.Len() > 0 {
			return g.buf.Read(p)
		}
		raw := make([]byte, len(p))
		g.mu.Unlock()
		n, err := g.Conn.Read(raw)
		g.mu.Lock()
		if n > 0 && (!g.gateOpen || g.closed) {
			// Should not happen while the gate is held (CloseGate poked any
			// parked raw reader away), but if a raw read raced the gate and
			// consumed bytes, park them rather than delivering them early.
			g.buf.Write(raw[:n])
		}
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				// A CloseGate poke, not a transport event: re-evaluate the
				// gate; the deadline was just consumed.
				continue
			}
			g.rawErr = err
			g.closed = true
			continue
		}
		if n > 0 && g.gateOpen {
			copy(p, raw[:n])
			return n, nil
		}
	}
}

func (g *gatedReadConn) Close() error {
	g.mu.Lock()
	g.closed = true
	g.cond.Broadcast()
	g.mu.Unlock()
	return g.Conn.Close()
}

// CancelChildRun implements ChildRunCanceler: it accepts exactly the run id
// acceptedRunID (a concrete in-flight run); anything else is not in-flight.
func (f *fakeWorkflowAdapter) CancelChildRun(runID string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelRequests = append(f.cancelRequests, runID)
	if f.acceptedRunID == "" || (runID != "" && runID != f.acceptedRunID) {
		return "", false
	}
	return f.acceptedRunID, true
}

// newServeAdapterFixture builds a serve-adapter-role server over the shared
// peerServeFixture transport helpers (rt/child stay nil/unused).
func newServeAdapterFixture(t *testing.T, journal *EventJournal) *peerServeFixture {
	t.Helper()
	cfg, err := LoadConfig(getenvFrom(map[string]string{EnvRemoteHost: "pipe"}))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.AdapterName = "workflow.child"
	cfg.Token = "tok"
	cfg.Scope = "sc"
	cfg.Digest = "sha256:aa"
	impl := &fakeWorkflowAdapter{acceptedRunID: "child-run-1"}
	server := NewServeAdapterServer(&cfg, impl, journal, captureLogger(&bytes.Buffer{}))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &peerServeFixture{t: t, cfg: cfg, server: server, impl: impl, ctx: ctx, cancel: cancel}
}

func TestServeAdapterServer_ServesInProcessAdapter(t *testing.T) {
	f := newServeAdapterFixture(t, nil)
	conn, _, _ := f.startConn()
	cc := f.hostClient(conn)
	client := adapterhost.NewClientForConn(cc)
	defer f.cancel()

	// The in-process adapter answers Info/OpenSession unwrapped: no
	// serveChildClient layering (the child-run arms come from the CLI's own
	// journal, not the runtime's).
	resp, err := client.Info(context.Background(), &v2.InfoRequest{})
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if resp.GetName() != "workflow.child" || resp.GetVersion() != "1.0.0" {
		t.Errorf("Info = %q/%q, want workflow.child/1.0.0", resp.GetName(), resp.GetVersion())
	}
	if _, err := client.OpenSession(context.Background(), &v2.OpenSessionRequest{SessionId: "s1"}); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	f.impl.mu.Lock()
	opens := len(f.impl.openSessions)
	f.impl.mu.Unlock()
	if opens != 1 {
		t.Errorf("adapter saw %d OpenSession calls, want 1", opens)
	}
}

func TestServeAdapterServer_SuperviseStreamsCallerJournal(t *testing.T) {
	f := newServeAdapterFixture(t, NewEventJournal(0))
	ev, err := f.server.Journal().Append(&criteriav1.SupervisionEvent_ChildRunStarted{
		ChildRunStarted: &criteriav1.ChildRunStarted{
			RunId:          "child-run-1",
			WorkflowDigest: "sha256:aa",
		},
	}, f.cfg.AdapterName, f.cfg.Scope, "wf")
	if err != nil {
		t.Fatalf("journal append: %v", err)
	}

	conn, _, _ := f.startConn()
	cc := f.hostClient(conn)
	ss := f.openSupervise(cc, ev.GetEventSeq()-1)
	got := f.mustRecvSupervisionEvent(ss, 5*time.Second)
	if got.GetChildRunStarted() == nil || got.GetChildRunStarted().GetRunId() != "child-run-1" {
		t.Fatalf("supervision event = %+v, want child_run_started for child-run-1", got.GetKind())
	}
	f.cancel()
}

func TestServeAdapterServer_ControlCancelChildRun(t *testing.T) {
	f := newServeAdapterFixture(t, nil)
	ctx := context.Background()

	// Cancellation routes to the adapter's canceler: accepted when an
	// in-flight child run matches, rejected when nothing is in-flight.
	f.impl.mu.Lock()
	f.impl.acceptedRunID = "child-run-1"
	f.impl.mu.Unlock()

	resp, err := f.server.Control(ctx, &criteriav1.ControlRequest{
		Kind: &criteriav1.ControlRequest_CancelChildRun{CancelChildRun: &criteriav1.CancelChildRun{RunId: "child-run-1"}},
	})
	if err != nil {
		t.Fatalf("CancelChildRun: %v", err)
	}
	if !resp.GetAccepted() {
		t.Errorf("CancelChildRun accepted = %v, detail %q, want accepted", resp.GetAccepted(), resp.GetDetail())
	}
	f.impl.mu.Lock()
	reqs := append([]string(nil), f.impl.cancelRequests...)
	f.impl.mu.Unlock()
	if len(reqs) != 1 || reqs[0] != "child-run-1" {
		t.Errorf("cancel requests = %q, want one for \"child-run-1\"", reqs)
	}

	// An unknown run id is accepted=false with a named detail.
	resp, err = f.server.Control(ctx, &criteriav1.ControlRequest{
		Kind: &criteriav1.ControlRequest_CancelChildRun{CancelChildRun: &criteriav1.CancelChildRun{RunId: "missing"}},
	})
	if err != nil {
		t.Fatalf("CancelChildRun(missing): %v", err)
	}
	if resp.GetAccepted() {
		t.Errorf("CancelChildRun(missing) accepted = %v, want rejected", resp.GetAccepted())
	}
	if resp.GetDetail() == "" {
		t.Error("CancelChildRun(missing) detail empty, want a name")
	}

	// KillChild targets a spawned process; serve-adapter mode has none.
	resp, err = f.server.Control(ctx, &criteriav1.ControlRequest{
		Kind: &criteriav1.ControlRequest_KillChild{KillChild: &criteriav1.KillChild{}},
	})
	if err != nil {
		t.Fatalf("KillChild: %v", err)
	}
	if resp.GetAccepted() {
		t.Errorf("KillChild accepted = %v, want rejected", resp.GetAccepted())
	}
}

func TestServeAdapterServer_ControlWithoutCanceler(t *testing.T) {
	cfg, err := LoadConfig(getenvFrom(map[string]string{EnvRemoteHost: "pipe"}))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.AdapterName = "workflow.child"
	// An adapter that does NOT implement ChildRunCanceler: control request
	// is rejected with a detail, not a Go-side error.
	server := NewServeAdapterServer(&cfg, &fakeWorkflowAdapter{}, nil, captureLogger(&bytes.Buffer{}))
	resp := server.controlServeAdapter(&criteriav1.ControlRequest{
		Kind: &criteriav1.ControlRequest_CancelChildRun{CancelChildRun: &criteriav1.CancelChildRun{}},
	})
	if resp.GetAccepted() {
		t.Errorf("CancelChildRun on non-canceler adapter accepted = %v", resp.GetAccepted())
	}
	if resp.GetDetail() == "" {
		t.Error("CancelChildRun on non-canceler adapter detail empty")
	}
}

func TestServeAdapterServer_ControlWorkflowGate(t *testing.T) {
	f := newServeAdapterFixture(t, nil)
	// Simulate an older peer that did not negotiate workflow.v1.
	f.server.capabilities = []string{peerAdapterV2FullCapability}

	_, err := f.server.Control(context.Background(), &criteriav1.ControlRequest{
		Kind: &criteriav1.ControlRequest_CancelChildRun{CancelChildRun: &criteriav1.CancelChildRun{}},
	})
	if err == nil {
		t.Fatal("CancelChildRun without workflow.v1 negotiated: want Unimplemented error")
	}
}

func TestServeAdapterServer_RequestExitEndsRunWithoutReconnecting(t *testing.T) {
	f := newServeAdapterFixture(t, nil)
	conn, _, serveErr := f.startConn()
	defer conn.Close()

	// A CloseSession teardown requests process exit; the loop must stop
	// serving and return nil — no reconnect, no second dial (the fixture's
	// one-shot dialer would error a second attempt).
	f.server.RequestExit()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("Serve returned %v, want nil after RequestExit", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after RequestExit")
	}
	if !f.server.ExitRequested() {
		t.Fatal("ExitRequested = false after RequestExit")
	}
}

func TestServeAdapterServer_ExitRequestedDefaultsFalse(t *testing.T) {
	f := newServeAdapterFixture(t, nil)
	if f.server.ExitRequested() {
		t.Fatal("ExitRequested = true before any request")
	}
}

// TestServeAdapterServer_CloseSessionResponseReachesHostBeforeTeardown is the
// wire-level regression for the CloseSession exit race: the served adapter
// requests process exit from inside the CloseSession handler (exactly how
// production wires it — runServeAdapter calls impl.setExit with
// peer.Server.RequestExit), and the host must have received the successful
// CloseSessionResponse BEFORE the watcher starts stopping the server. With
// the old immediate server.Stop() the teardown killed the transport while the
// response was still blocked on the unbuffered net.Pipe write, so the host saw
// the RPC fail as Unavailable; the watcher now drains with a bounded
// GracefulStop and only then force-closes.
func TestServeAdapterServer_CloseSessionResponseReachesHostBeforeTeardown(t *testing.T) {
	f := newServeAdapterFixture(t, nil)
	f.impl.exitFn = f.server.RequestExit

	conn, _, serveErr := f.startConn()
	defer conn.Close()

	// Hold the host-side reads so the server's CloseSession response cannot
	// leave the transport while the gate is held: the response write stays
	// blocked on the unbuffered pipe for the whole teardown-decision window.
	gated := newGatedReadConn(conn)
	cc := f.hostClient(gated)
	client := adapterhost.NewClientForConn(cc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := client.OpenSession(ctx, &v2.OpenSessionRequest{SessionId: "s-wire"}); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}

	gated.CloseGate()
	// Let the CloseGate poke expire: within ~20ms every transport reader is
	// re-parked at the gate and nothing is draining the pipe, so the
	// CloseSession response will block server-side until the gate opens.
	time.Sleep(30 * time.Millisecond)

	closeErr := make(chan error, 1)
	go func() {
		_, err := client.CloseSession(ctx, &v2.CloseSessionRequest{SessionId: "s-wire"})
		closeErr <- err
	}()

	// Give the watcher a full window to make its teardown decision while the
	// response is still pinned behind the gate.
	time.Sleep(100 * time.Millisecond)
	gated.OpenGate()

	select {
	case err := <-closeErr:
		if err != nil {
			t.Fatalf("CloseSession response never reached the host: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CloseSession response never reached the host (timeout)")
	}

	select {
	case err := <-serveErr:
		if err != nil {
			t.Errorf("serveOnce = %v, want nil (exit requested, no teardown error)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveOnce did not return after CloseSession")
	}

	f.impl.mu.Lock()
	closedSessions := len(f.impl.closedSessions)
	f.impl.mu.Unlock()
	if closedSessions != 1 {
		t.Errorf("adapter saw %d CloseSession calls, want 1", closedSessions)
	}
}

func TestServeAdapterServer_ImplicitNilJournal(t *testing.T) {
	cfg, err := LoadConfig(getenvFrom(map[string]string{EnvRemoteHost: "pipe"}))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.AdapterName = "workflow.child"
	impl := &fakeWorkflowAdapter{}
	server := NewServeAdapterServer(&cfg, impl, nil, captureLogger(&bytes.Buffer{}))
	if server.Journal() == nil {
		t.Fatal("Journal() = nil, want the implicit empty journal")
	}
	if server.impl != impl {
		t.Fatal("NewServeAdapterServer did not hold the provided adapter")
	}
}
