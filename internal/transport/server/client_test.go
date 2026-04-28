package servertrans

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/brokenbots/criteria/events"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
)

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
	mux := http.NewServeMux()
	path, handler := criteriav1connect.NewCriteriaServiceHandler(f)
	mux.Handle(path, handler)
	srv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	srv.Start()
	t.Cleanup(srv.Close)
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

	// And exactly one event was persisted for each publish (no duplicates).
	f.mu.Lock()
	count := len(f.events[runID])
	f.mu.Unlock()
	if count != 2 {
		t.Fatalf("expected 2 persisted events, got %d", count)
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
