package servertrans

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"go.uber.org/goleak"
	"golang.org/x/net/http2"

	"github.com/brokenbots/criteria/events"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
)

// requireNoGoroutineLeak registers a t.Cleanup that asserts no new goroutines
// were leaked by the test. It snapshots the current goroutine set with
// goleak.IgnoreCurrent() at call time so that goroutines already running (e.g.
// from a previous test in the same binary run) do not cause spurious failures;
// only goroutines spawned after this call are subject to the leak check.
//
// It must be called before any setup that starts goroutines so that its cleanup
// (LIFO order) runs after all server-shutdown and connection-close cleanup.
func requireNoGoroutineLeak(t *testing.T) {
	t.Helper()
	snapshot := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, snapshot) })
}

// writeTempCertKey generates a minimal self-signed RSA certificate, writes the
// PEM-encoded cert and key to temporary files, and returns their paths. It is
// used by tests that need valid cert/key files to exercise TLSMutual construction
// paths without connecting to a real server.
func writeTempCertKey(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return
}

// --- Fake Connect server -----------------------------------------------------

type fakeServer struct {
	criteriav1connect.UnimplementedCriteriaServiceHandler

	mu          sync.Mutex
	criteriaID  string
	token       string
	runs        []*pb.Run
	events      map[string][]*pb.Envelope
	sinceSeqHdr []string
	controls    chan *pb.ControlMessage
	ctlAttached chan struct{}

	// heartbeats counts Heartbeat RPCs received.
	heartbeats int

	// lastResumeReq captures the most recent Resume RPC request payload.
	lastResumeReq *pb.ResumeRequest

	// streamOpenTimes records when each SubmitEvents stream was opened; used
	// to assert exponential reconnect backoff spacing.
	streamOpenTimes []time.Time

	// failAfterAcks, when > 0, causes SubmitEvents to return an error
	// after sending that many acks. Decremented as it fires.
	failAfterAcks int

	// dropAcksBeforeSend, when > 0, causes SubmitEvents to persist the
	// envelope but return an error *before* sending the ack. This
	// simulates the persist-before-ack reconnect window (server wrote,
	// connection dropped, ack never flushed).
	dropAcksBeforeSend int
}

func newFakeServer() *fakeServer {
	return &fakeServer{
		criteriaID:  "crt-1",
		token:       "tok-1",
		events:      make(map[string][]*pb.Envelope),
		controls:    make(chan *pb.ControlMessage, 8),
		ctlAttached: make(chan struct{}, 1),
	}
}

func (f *fakeServer) Register(_ context.Context, _ *connect.Request[pb.RegisterRequest]) (*connect.Response[pb.RegisterResponse], error) {
	return connect.NewResponse(&pb.RegisterResponse{CriteriaId: f.criteriaID, Token: f.token}), nil
}

func (f *fakeServer) Heartbeat(_ context.Context, _ *connect.Request[pb.HeartbeatRequest]) (*connect.Response[pb.HeartbeatResponse], error) {
	f.mu.Lock()
	f.heartbeats++
	f.mu.Unlock()
	return connect.NewResponse(&pb.HeartbeatResponse{}), nil
}

func (f *fakeServer) CreateRun(_ context.Context, req *connect.Request[pb.CreateRunRequest]) (*connect.Response[pb.Run], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := "run-" + strconv.Itoa(len(f.runs)+1)
	r := &pb.Run{RunId: id, CriteriaId: req.Msg.CriteriaId, WorkflowName: req.Msg.WorkflowName, Status: "pending"}
	f.runs = append(f.runs, r)
	return connect.NewResponse(r), nil
}

func (f *fakeServer) SubmitEvents(ctx context.Context, stream *connect.BidiStream[pb.Envelope, pb.Ack]) error {
	sinceRaw := stream.RequestHeader().Get("since_seq")
	f.mu.Lock()
	f.sinceSeqHdr = append(f.sinceSeqHdr, sinceRaw)
	f.streamOpenTimes = append(f.streamOpenTimes, time.Now())
	f.mu.Unlock()

	var sinceSeq uint64
	replayRequested := false
	if sinceRaw != "" {
		if v, err := strconv.ParseUint(sinceRaw, 10, 64); err == nil {
			sinceSeq = v
			replayRequested = true
		}
	}

	replayed := map[string]bool{}

	for {
		msg, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}

		if replayRequested && !replayed[msg.RunId] {
			f.mu.Lock()
			prior := append([]*pb.Envelope(nil), f.events[msg.RunId]...)
			f.mu.Unlock()
			for _, p := range prior {
				if p.Seq <= sinceSeq {
					continue
				}
				if err := stream.Send(&pb.Ack{RunId: p.RunId, Seq: p.Seq, CorrelationId: p.CorrelationId}); err != nil {
					return err
				}
			}
			replayed[msg.RunId] = true
		}

		f.mu.Lock()
		list := f.events[msg.RunId]
		// Dedup on (run_id, correlation_id) so retries after a dropped
		// ack don't double-persist. Mirrors the server's real behavior.
		var seq uint64
		duplicate := false
		if msg.CorrelationId != "" {
			for _, e := range list {
				if e.CorrelationId == msg.CorrelationId {
					seq = e.Seq
					duplicate = true
					break
				}
			}
		}
		if !duplicate {
			msg.Seq = uint64(len(list) + 1)
			f.events[msg.RunId] = append(list, msg)
			seq = msg.Seq
		}
		cid := msg.CorrelationId
		shouldDrop := f.dropAcksBeforeSend > 0 && !duplicate
		if shouldDrop {
			f.dropAcksBeforeSend--
		}
		f.mu.Unlock()

		if shouldDrop {
			return connect.NewError(connect.CodeUnavailable, errors.New("ack dropped"))
		}

		if err := stream.Send(&pb.Ack{RunId: msg.RunId, Seq: seq, CorrelationId: cid}); err != nil {
			return err
		}

		f.mu.Lock()
		shouldFail := f.failAfterAcks > 0
		if shouldFail {
			f.failAfterAcks--
		}
		f.mu.Unlock()
		if shouldFail {
			return connect.NewError(connect.CodeUnavailable, errors.New("forced disconnect"))
		}
		_ = ctx
	}
}

func (f *fakeServer) Resume(_ context.Context, req *connect.Request[pb.ResumeRequest]) (*connect.Response[pb.ResumeResponse], error) {
	f.mu.Lock()
	f.lastResumeReq = req.Msg
	f.mu.Unlock()
	return connect.NewResponse(&pb.ResumeResponse{}), nil
}

func (f *fakeServer) Control(ctx context.Context, _ *connect.Request[pb.ControlSubscribeRequest], stream *connect.ServerStream[pb.ControlMessage]) error {
	if err := stream.Send(&pb.ControlMessage{Command: &pb.ControlMessage_ControlReady{ControlReady: &pb.ControlReady{}}}); err != nil {
		return err
	}
	select {
	case f.ctlAttached <- struct{}{}:
	default:
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-f.controls:
			if !ok {
				return nil
			}
			if err := stream.Send(msg); err != nil {
				return err
			}
		}
	}
}

// --- test helpers ------------------------------------------------------------

func startFakeServer(t *testing.T, f *fakeServer) string {
	t.Helper()
	// Register goleak check before any goroutine-spawning setup so that its
	// cleanup (LIFO order) runs after the server and connection cleanup below.
	requireNoGoroutineLeak(t)

	mux := http.NewServeMux()
	path, handler := criteriav1connect.NewCriteriaServiceHandler(f)
	mux.Handle(path, handler)
	srv := httptest.NewUnstartedServer(mux)
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	srv.Config.Protocols = &protocols

	// Track hijacked h2c connections so cleanup can close them explicitly.
	// httptest.Server.Close() cannot reach hijacked connections (they are
	// removed from httptest's internal tracking after hijack), so without this
	// the h2c serve goroutines outlive the test and trip goleak assertions.
	var hijackedMu sync.Mutex
	var hijackedConns []net.Conn
	srv.Config.ConnState = func(c net.Conn, cs http.ConnState) {
		if cs == http.StateHijacked {
			hijackedMu.Lock()
			hijackedConns = append(hijackedConns, c)
			hijackedMu.Unlock()
		}
	}

	srv.Start()
	t.Cleanup(func() {
		hijackedMu.Lock()
		for _, c := range hijackedConns {
			_ = c.Close()
		}
		hijackedMu.Unlock()
		_ = srv.Config.Close()
		srv.Close()
	})
	return srv.URL
}

func h2cHTTPClient() *http.Client {
	return &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}}
}

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- Tests -------------------------------------------------------------------

func TestClientHappyPath(t *testing.T) {
	f := newFakeServer()
	url := startFakeServer(t, f)

	c, err := NewClient(url, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer c.Close()

	if err := c.Register(ctx, "n", "h", "v"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if c.CriteriaID() != "crt-1" {
		t.Fatalf("criteria id: %q", c.CriteriaID())
	}
	runID, err := c.CreateRun(ctx, "wf", "hcl")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := c.StartStreams(ctx, runID); err != nil {
		t.Fatalf("start streams: %v", err)
	}

	// Publish a single envelope and verify it is persisted and acknowledged.
	env := events.NewEnvelope(runID, &pb.StepEntered{Step: "s1", Adapter: "shell", Attempt: 1})
	c.Publish(ctx, env)

	if !waitForCond(t, 2*time.Second, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.events[runID]) == 1
	}) {
		t.Fatalf("event never persisted")
	}
	if !waitForCond(t, 2*time.Second, func() bool {
		return c.lastAckedSeq.Load() == 1
	}) {
		t.Fatalf("ack never observed: lastAcked=%d", c.lastAckedSeq.Load())
	}
	if got := len(c.snapshotPending()); got != 0 {
		t.Fatalf("pending should be empty after ack, got %d", got)
	}
}

func TestClientReconnectSendsSinceSeq(t *testing.T) {
	f := newFakeServer()
	// After the first ack, the fake server will close the stream with an
	// error, forcing the client to reconnect.
	f.failAfterAcks = 1
	url := startFakeServer(t, f)

	c, err := NewClient(url, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer c.Close()

	if err := c.Register(ctx, "n", "h", "v"); err != nil {
		t.Fatal(err)
	}
	runID, err := c.CreateRun(ctx, "wf", "hcl")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.StartStreams(ctx, runID); err != nil {
		t.Fatal(err)
	}

	env1 := events.NewEnvelope(runID, &pb.StepEntered{Step: "s1", Adapter: "shell", Attempt: 1})
	c.Publish(ctx, env1)
	if !waitForCond(t, 2*time.Second, func() bool { return c.lastAckedSeq.Load() == 1 }) {
		t.Fatalf("first ack not observed")
	}

	// Second publish after forced disconnect should be delivered after the
	// client reconnects with since_seq=1.
	env2 := events.NewEnvelope(runID, &pb.StepEntered{Step: "s2", Adapter: "shell", Attempt: 1})
	c.Publish(ctx, env2)

	if !waitForCond(t, 5*time.Second, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.events[runID]) == 2
	}) {
		f.mu.Lock()
		got := len(f.events[runID])
		f.mu.Unlock()
		t.Fatalf("second event never persisted after reconnect; persisted=%d", got)
	}

	// Confirm a reconnect carried since_seq=1.
	f.mu.Lock()
	hdrs := append([]string(nil), f.sinceSeqHdr...)
	f.mu.Unlock()
	foundSince := false
	for _, h := range hdrs {
		if h == "1" {
			foundSince = true
			break
		}
	}
	if !foundSince {
		t.Fatalf("expected a reconnect with since_seq=1, got headers: %v", hdrs)
	}

	// Verify the exact event sequence: a count-only check would pass if one
	// event were duplicated and the other lost.
	f.mu.Lock()
	evts := append([]*pb.Envelope(nil), f.events[runID]...)
	f.mu.Unlock()
	wantSteps := []string{"s1", "s2"}
	if len(evts) != len(wantSteps) {
		t.Fatalf("expected %d events, got %d", len(wantSteps), len(evts))
	}
	for i, wantStep := range wantSteps {
		se := evts[i].GetStepEntered()
		if se == nil || se.Step != wantStep {
			t.Errorf("event[%d]: want StepEntered{Step:%q}, got %v", i, wantStep, evts[i])
		}
	}
}

func TestClientControlStreamDeliversRunCancel(t *testing.T) {
	f := newFakeServer()
	url := startFakeServer(t, f)

	c, err := NewClient(url, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer c.Close()

	if err := c.Register(ctx, "n", "h", "v"); err != nil {
		t.Fatal(err)
	}
	runID, err := c.CreateRun(ctx, "wf", "hcl")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.StartStreams(ctx, runID); err != nil {
		t.Fatal(err)
	}

	// Wait for control attach.
	select {
	case <-f.ctlAttached:
	case <-time.After(2 * time.Second):
		t.Fatal("control never attached")
	}

	f.controls <- &pb.ControlMessage{Command: &pb.ControlMessage_RunCancel{RunCancel: &pb.RunCancel{RunId: runID, Reason: "x"}}}

	select {
	case got := <-c.RunCancelCh():
		if got != runID {
			t.Fatalf("got cancel for %q want %q", got, runID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run cancel not observed")
	}
}

func waitForCond(t *testing.T, d time.Duration, fn func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fn()
}

// silence unused import when tests don't reach certain code paths
var _ = h2cHTTPClient

// TestClientPersistBeforeAckReconnect simulates the window where the server has
// persisted an envelope but the connection drops before the ack is delivered.
// On reconnect, the server replays the ack from its prior persisted events.
// The transport must not re-persist (exactly-once) and must advance
// lastAckedSeq / clear pending on the replayed ack.
func TestClientPersistBeforeAckReconnect(t *testing.T) {
	f := newFakeServer()
	f.dropAcksBeforeSend = 1
	url := startFakeServer(t, f)

	c, err := NewClient(url, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer c.Close()

	if err := c.Register(ctx, "n", "h", "v"); err != nil {
		t.Fatal(err)
	}
	runID, err := c.CreateRun(ctx, "wf", "hcl")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.StartStreams(ctx, runID); err != nil {
		t.Fatal(err)
	}

	env := events.NewEnvelope(runID, &pb.StepEntered{Step: "s1", Adapter: "shell", Attempt: 1})
	c.Publish(ctx, env)

	if !waitForCond(t, 5*time.Second, func() bool { return c.lastAckedSeq.Load() == 1 }) {
		t.Fatalf("ack never observed after persist-before-ack reconnect: lastAcked=%d", c.lastAckedSeq.Load())
	}
	if !waitForCond(t, 1*time.Second, func() bool { return len(c.snapshotPending()) == 0 }) {
		t.Fatalf("pending should drain, got %d", len(c.snapshotPending()))
	}

	f.mu.Lock()
	count := len(f.events[runID])
	f.mu.Unlock()
	if count != 1 {
		t.Fatalf("expected exactly 1 persisted event (dedup), got %d", count)
	}
}

// TestClientPublishBlocksWhenBufferFull verifies that Publish blocks (rather
// than dropping events) when the send buffer is full, and unblocks once the
// backlog drains.
func TestClientPublishBlocksWhenBufferFull(t *testing.T) {
	f := newFakeServer()
	url := startFakeServer(t, f)

	c, err := NewClient(url, newTestLogger(), Options{SendBuffer: 2})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := c.Register(ctx, "n", "h", "v"); err != nil {
		t.Fatal(err)
	}
	runID, err := c.CreateRun(ctx, "wf", "hcl")
	if err != nil {
		t.Fatal(err)
	}

	// Fill the buffer *before* starting the streams so nothing drains it.
	for i := 0; i < 2; i++ {
		env := events.NewEnvelope(runID, &pb.StepEntered{Step: "s", Adapter: "shell", Attempt: 1})
		c.Publish(ctx, env)
	}

	done := make(chan struct{})
	extra := events.NewEnvelope(runID, &pb.StepEntered{Step: "s", Adapter: "shell", Attempt: 1})
	go func() {
		c.Publish(ctx, extra)
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("publish should have blocked when buffer is full")
	case <-time.After(50 * time.Millisecond):
	}

	if err := c.StartStreams(ctx, runID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publish never unblocked after buffer drained")
	}

	if !waitForCond(t, 2*time.Second, func() bool { return c.lastAckedSeq.Load() == 3 }) {
		t.Fatalf("expected 3 acks, got lastAcked=%d", c.lastAckedSeq.Load())
	}
	c.Close()
}

// TestClientCloseWithConcurrentPublish exercises Close() vs. in-flight
// Publish(). Must be run with -race to catch send-on-closed-channel bugs or
// field races on sendCh.
func TestClientCloseWithConcurrentPublish(t *testing.T) {
	f := newFakeServer()
	url := startFakeServer(t, f)

	c, err := NewClient(url, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := c.Register(ctx, "n", "h", "v"); err != nil {
		t.Fatal(err)
	}
	runID, err := c.CreateRun(ctx, "wf", "hcl")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.StartStreams(ctx, runID); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 32; j++ {
				env := events.NewEnvelope(runID, &pb.StepEntered{Step: "s", Adapter: "shell", Attempt: 1})
				c.Publish(ctx, env)
			}
		}()
	}

	time.Sleep(5 * time.Millisecond)
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	wg.Wait()
}

// TestClientReconnectMultipleFailures verifies that the client retries after
// N consecutive stream failures and all events are ultimately persisted exactly
// once (no duplicates).
func TestClientReconnectMultipleFailures(t *testing.T) {
	const numEvents = 3
	f := newFakeServer()
	// Fail after every ack, N times, before finally succeeding.
	f.failAfterAcks = numEvents
	url := startFakeServer(t, f)

	c, err := NewClient(url, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer c.Close()

	if err := c.Register(ctx, "n", "h", "v"); err != nil {
		t.Fatal(err)
	}
	runID, err := c.CreateRun(ctx, "wf", "hcl")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.StartStreams(ctx, runID); err != nil {
		t.Fatal(err)
	}

	// Publish numEvents events; each triggers a disconnect after its ack.
	for i := 0; i < numEvents; i++ {
		env := events.NewEnvelope(runID, &pb.StepEntered{Step: "s" + strconv.Itoa(i+1), Adapter: "shell", Attempt: 1})
		c.Publish(ctx, env)
	}

	// After numEvents reconnects the fake allows acks through indefinitely.
	// Publish one final event on the stable connection.
	final := events.NewEnvelope(runID, &pb.StepEntered{Step: "final", Adapter: "shell", Attempt: 1})
	c.Publish(ctx, final)

	const want = numEvents + 1
	if !waitForCond(t, 10*time.Second, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.events[runID]) == want
	}) {
		f.mu.Lock()
		got := len(f.events[runID])
		f.mu.Unlock()
		t.Fatalf("expected %d persisted events after multi-failure reconnect, got %d", want, got)
	}

	// Verify the exact event sequence (s1, s2, s3, final): a count-only check
	// would pass if one event were duplicated and another lost.
	f.mu.Lock()
	evts := append([]*pb.Envelope(nil), f.events[runID]...)
	f.mu.Unlock()
	wantSteps := []string{"s1", "s2", "s3", "final"}
	if len(evts) != len(wantSteps) {
		t.Fatalf("expected %d events, got %d", len(wantSteps), len(evts))
	}
	for i, wantStep := range wantSteps {
		se := evts[i].GetStepEntered()
		if se == nil || se.Step != wantStep {
			t.Errorf("event[%d]: want StepEntered{Step:%q}, got %v", i, wantStep, evts[i])
		}
	}

	// Assert exponential backoff: stream open timestamps must show gaps that
	// grow by at least 1.5× per failure. The implementation doubles the delay
	// on each failure (500ms→1000ms→2000ms), giving an actual ratio of ~2.0 —
	// well above the 1.5 threshold. A regression to a fixed delay (ratio 1.0)
	// would fail this check.
	f.mu.Lock()
	openTimes := make([]time.Time, len(f.streamOpenTimes))
	copy(openTimes, f.streamOpenTimes)
	f.mu.Unlock()

	if len(openTimes) < numEvents+1 {
		t.Fatalf("expected at least %d stream openings, got %d", numEvents+1, len(openTimes))
	}
	// First reconnect gap must be at least 100ms (min backoff is 500ms;
	// loose threshold reliably catches tight-loop regression).
	firstGap := openTimes[1].Sub(openTimes[0])
	if firstGap < 100*time.Millisecond {
		t.Errorf("first reconnect gap %s is too short — expected ≥100ms (backoff removed?)", firstGap)
	}
	// Each subsequent gap must be at least 1.5× the previous, proving
	// exponential growth. A fixed-delay regression (ratio 1.0) fails here.
	for i := 2; i < len(openTimes); i++ {
		prev := openTimes[i-1].Sub(openTimes[i-2])
		curr := openTimes[i].Sub(openTimes[i-1])
		if curr < 3*prev/2 {
			t.Errorf("reconnect gap[%d]=%s < 1.5× gap[%d]=%s — expected exponential growth (fixed-delay regression?)",
				i-1, curr, i-2, prev)
		}
	}
}

// TestClientSinceSeqZeroEventReplay verifies that a reconnect with since_seq
// pointing past all stored events produces no phantom replay acks, and the
// client still delivers any outstanding pending events correctly.
func TestClientSinceSeqZeroEventReplay(t *testing.T) {
	f := newFakeServer()
	// Force one disconnect after the first ack so we get a since_seq reconnect.
	f.failAfterAcks = 1
	url := startFakeServer(t, f)

	c, err := NewClient(url, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer c.Close()

	if err := c.Register(ctx, "n", "h", "v"); err != nil {
		t.Fatal(err)
	}
	runID, err := c.CreateRun(ctx, "wf", "hcl")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.StartStreams(ctx, runID); err != nil {
		t.Fatal(err)
	}

	// Publish and wait for first ack; triggers disconnect.
	env1 := events.NewEnvelope(runID, &pb.StepEntered{Step: "s1", Adapter: "shell", Attempt: 1})
	c.Publish(ctx, env1)
	if !waitForCond(t, 2*time.Second, func() bool { return c.lastAckedSeq.Load() == 1 }) {
		t.Fatalf("first ack not observed before disconnect")
	}

	// Publish a second event; reconnect carries since_seq=1 (pointing past all
	// stored events, so zero events are replayed).
	env2 := events.NewEnvelope(runID, &pb.StepEntered{Step: "s2", Adapter: "shell", Attempt: 1})
	c.Publish(ctx, env2)

	if !waitForCond(t, 5*time.Second, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.events[runID]) == 2
	}) {
		f.mu.Lock()
		got := len(f.events[runID])
		f.mu.Unlock()
		t.Fatalf("second event never persisted after zero-event replay; persisted=%d", got)
	}

	// since_seq was sent on the reconnect.
	f.mu.Lock()
	hdrs := append([]string(nil), f.sinceSeqHdr...)
	f.mu.Unlock()
	foundSince1 := false
	for _, h := range hdrs {
		if h == "1" {
			foundSince1 = true
			break
		}
	}
	if !foundSince1 {
		t.Fatalf("expected a reconnect with since_seq=1, got headers: %v", hdrs)
	}

	// Verify the exact event sequence: a count-only check would pass if s1
	// were re-persisted and s2 were dropped.
	f.mu.Lock()
	evts := append([]*pb.Envelope(nil), f.events[runID]...)
	f.mu.Unlock()
	wantSteps2 := []string{"s1", "s2"}
	if len(evts) != len(wantSteps2) {
		t.Fatalf("expected %d events (no replay duplicates), got %d", len(wantSteps2), len(evts))
	}
	for i, wantStep := range wantSteps2 {
		se := evts[i].GetStepEntered()
		if se == nil || se.Step != wantStep {
			t.Errorf("event[%d]: want StepEntered{Step:%q}, got %v", i, wantStep, evts[i])
		}
	}
}

// TestClientTLSErrors exercises buildHTTPClient error and alternative success
// paths that are unreachable via the default h2c test setup.
func TestClientTLSErrors(t *testing.T) {
	log := newTestLogger()

	t.Run("disable_with_https_url", func(t *testing.T) {
		if _, err := NewClient("https://example.com", log, Options{TLSMode: TLSDisable}); err == nil {
			t.Fatal("expected error for TLSDisable + https URL")
		}
	})
	t.Run("mutual_missing_certs", func(t *testing.T) {
		if _, err := NewClient("https://example.com", log, Options{TLSMode: TLSMutual}); err == nil {
			t.Fatal("expected error for TLSMutual without cert/key")
		}
	})
	t.Run("tls_enable_no_ca", func(t *testing.T) {
		// No CA file means accept system roots — buildHTTPClient succeeds.
		c, err := NewClient("https://example.com", log, Options{TLSMode: TLSEnable})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer c.Close()
	})
	t.Run("unknown_tls_mode", func(t *testing.T) {
		if _, err := NewClient("http://example.com", log, Options{TLSMode: "bad"}); err == nil {
			t.Fatal("expected error for unknown TLS mode")
		}
	})
	t.Run("tls_enable_with_http_url_rejected", func(t *testing.T) {
		_, err := NewClient("http://example.com", log, Options{TLSMode: TLSEnable})
		if err == nil {
			t.Fatal("expected error for TLSEnable + http URL; got nil")
		}
		if !strings.Contains(err.Error(), string(TLSEnable)) {
			t.Errorf("error should mention TLS mode %q; got: %v", TLSEnable, err)
		}
		if !strings.Contains(err.Error(), "http://example.com") {
			t.Errorf("error should mention the offending URL; got: %v", err)
		}
	})
	t.Run("tls_mutual_with_http_url_rejected", func(t *testing.T) {
		certFile, keyFile := writeTempCertKey(t)
		_, err := NewClient("http://example.com", log, Options{TLSMode: TLSMutual, CertFile: certFile, KeyFile: keyFile})
		if err == nil {
			t.Fatal("expected error for TLSMutual + http URL; got nil")
		}
		if !strings.Contains(err.Error(), string(TLSMutual)) {
			t.Errorf("error should mention TLS mode %q; got: %v", TLSMutual, err)
		}
		if !strings.Contains(err.Error(), "http://example.com") {
			t.Errorf("error should mention the offending URL; got: %v", err)
		}
	})
}

// TestClientAccessors verifies Token, ResumeCh, TLSMode, and SetCredentials.
func TestClientAccessors(t *testing.T) {
	c, err := NewClient("http://localhost:9999", newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if c.Token() != "" {
		t.Errorf("expected empty token before Register, got %q", c.Token())
	}
	if c.ResumeCh() == nil {
		t.Error("ResumeCh() should return a non-nil channel")
	}
	if c.TLSMode() != TLSDisable {
		t.Errorf("expected TLSDisable default, got %q", c.TLSMode())
	}
	c.SetCredentials("crt-x", "tok-x")
	if c.Token() != "tok-x" {
		t.Errorf("Token after SetCredentials: got %q want tok-x", c.Token())
	}
	if c.CriteriaID() != "crt-x" {
		t.Errorf("CriteriaID after SetCredentials: got %q want crt-x", c.CriteriaID())
	}
}

// TestClientHeartbeat verifies that StartHeartbeat fires periodic Heartbeat
// RPCs and exits cleanly when the context is cancelled.
func TestClientHeartbeat(t *testing.T) {
	f := newFakeServer()
	url := startFakeServer(t, f)

	c, err := NewClient(url, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())

	if err := c.Register(ctx, "n", "h", "v"); err != nil {
		t.Fatal(err)
	}

	c.StartHeartbeat(ctx, 15*time.Millisecond)

	// Poll (up to 2s) for at least 3 heartbeat RPCs so the assertion is not
	// sensitive to scheduler jitter or CI load.
	deadline := time.Now().Add(2 * time.Second)
	var n int
	for time.Now().Before(deadline) {
		f.mu.Lock()
		n = f.heartbeats
		f.mu.Unlock()
		if n >= 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n < 3 {
		t.Errorf("expected at least 3 Heartbeat RPCs, got %d", n)
	}

	cancel()
	// Wait (up to 1 s) for the heartbeat goroutine to observe ctx.Done and
	// stop firing. Poll until the count has been stable for at least
	// 3× the ticker interval; this is more robust than a fixed sleep on
	// loaded hosts.
	var last int
	stableCount := 0
	if !waitForCond(t, 1*time.Second, func() bool {
		f.mu.Lock()
		cur := f.heartbeats
		f.mu.Unlock()
		if cur == last {
			stableCount++
		} else {
			last = cur
			stableCount = 0
		}
		return stableCount >= 3
	}) {
		t.Error("heartbeat goroutine did not stop within 1s after cancel")
		return
	}

	f.mu.Lock()
	snapshot := f.heartbeats
	f.mu.Unlock()

	// Verify count does not grow further (wait 3× the interval).
	time.Sleep(3 * 15 * time.Millisecond)
	f.mu.Lock()
	nAfter := f.heartbeats
	f.mu.Unlock()
	if nAfter != snapshot {
		t.Errorf("heartbeat did not stop after cancel: count grew from %d to %d", snapshot, nAfter)
	}
}

// TestClientResume verifies the Resume RPC is dispatched to the server and the
// response is forwarded to the caller.
func TestClientResume(t *testing.T) {
	f := newFakeServer()
	url := startFakeServer(t, f)

	c, err := NewClient(url, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer c.Close()

	if err := c.Register(ctx, "n", "h", "v"); err != nil {
		t.Fatal(err)
	}

	resp, err := c.Resume(ctx, "run-1", "received", map[string]string{"outcome": "ok"})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resp == nil {
		t.Fatal("Resume returned nil response")
	}

	f.mu.Lock()
	got := f.lastResumeReq
	f.mu.Unlock()
	if got == nil {
		t.Fatal("server did not receive a Resume request")
	}
	if got.RunId != "run-1" {
		t.Errorf("Resume RunId: got %q want %q", got.RunId, "run-1")
	}
	if got.Signal != "received" {
		t.Errorf("Resume Signal: got %q want %q", got.Signal, "received")
	}
	if got.Payload["outcome"] != "ok" {
		t.Errorf("Resume Payload[outcome]: got %q want %q", got.Payload["outcome"], "ok")
	}
}

// TestClientDrain verifies that Drain returns immediately when no events are
// pending and that it unblocks on context cancellation.
func TestClientDrain(t *testing.T) {
	t.Run("empty_returns_immediately", func(t *testing.T) {
		c, err := NewClient("http://localhost:9999", newTestLogger())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()

		done := make(chan struct{})
		go func() { c.Drain(context.Background()); close(done) }()
		select {
		case <-done:
		case <-time.After(500 * time.Millisecond):
			t.Fatal("Drain did not return when no events pending")
		}
	})

	t.Run("ctx_cancel_unblocks_drain", func(t *testing.T) {
		f := newFakeServer()
		url := startFakeServer(t, f)
		c, err := NewClient(url, newTestLogger())
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer c.Close()

		if err := c.Register(ctx, "n", "h", "v"); err != nil {
			t.Fatal(err)
		}
		runID, err := c.CreateRun(ctx, "wf", "hcl")
		if err != nil {
			t.Fatal(err)
		}

		// Publish without starting streams so the event stays in sendCh,
		// causing Drain to block on the select rather than returning early.
		env := events.NewEnvelope(runID, &pb.StepEntered{Step: "s1", Adapter: "shell", Attempt: 1})
		c.Publish(ctx, env)

		done := make(chan struct{})
		go func() { c.Drain(ctx); close(done) }()

		// cancel() is safe to call immediately: Drain's select handles
		// ctx.Done() whether or not the goroutine has started yet, so no
		// sleep is needed before cancelling.
		cancel()
		select {
		case <-done:
		case <-time.After(500 * time.Millisecond):
			t.Fatal("Drain did not return after context cancel")
		}
	})
}

// TestClientStartPublishStream exercises StartPublishStream: the
// credentials-not-set error path and the success path via SetCredentials.
func TestClientStartPublishStream(t *testing.T) {
	// Error path: no credentials set.
	c, err := NewClient("http://localhost:9999", newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.StartPublishStream(context.Background(), "run-1"); err == nil {
		t.Fatal("expected 'credentials not set' error")
	}

	// Success path via SetCredentials (simulates crash-recovery).
	f := newFakeServer()
	url := startFakeServer(t, f)
	c2, err := NewClient(url, newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer c2.Close()
	defer cancel()

	c2.SetCredentials(f.criteriaID, f.token)
	runID, err := c2.CreateRun(ctx, "wf", "hcl")
	if err != nil {
		t.Fatal(err)
	}
	if err := c2.StartPublishStream(ctx, runID); err != nil {
		t.Fatalf("StartPublishStream: %v", err)
	}
	// Second call returns an error because the stream is already started.
	if err := c2.StartPublishStream(ctx, runID); err == nil {
		t.Fatal("expected error on duplicate StartPublishStream")
	}
}

// TestClientStartStreamsNotRegistered verifies that StartStreams returns an
// error before Register has been called.
func TestClientStartStreamsNotRegistered(t *testing.T) {
	c, err := NewClient("http://localhost:9999", newTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.StartStreams(context.Background(), "run-1"); err == nil {
		t.Fatal("expected 'not registered' error")
	}
}
