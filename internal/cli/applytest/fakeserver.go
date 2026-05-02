// Package applytest provides a fake Connect server harness for testing
// server-mode apply functions in package cli. It stands up an in-memory
// Connect server over an httptest.Server (h2c) and exposes hooks that drive
// run lifecycle scenarios without requiring a real orchestrator.
package applytest

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
)

// FakeStep describes a single step in a scripted execution.
// Included for future step-level scripting; unused in current hooks.
type FakeStep struct {
	Name string
}

// ApplyExecution is the script the fake server drives:
//   - InjectPauseAt: when a WaitEntered event is received for this node name,
//     the fake waits ResumeAfter and then sends a ResumeRun control message.
//   - DropStreamAt: when a StepEntered event is received for this step name,
//     the fake closes the SubmitEvents stream once (forcing a client reconnect).
//   - CancelAt: when a StepEntered event is received for this step name,
//     the fake sends a RunCancel control message.
type ApplyExecution struct {
	Steps         []FakeStep
	InjectPauseAt string        // wait node name; empty = no pause injection
	ResumeAfter   time.Duration // delay before ResumeRun; defaults to 10ms when zero
	DropStreamAt  string        // step name; empty = no stream drop
	CancelAt      string        // step name; empty = no cancellation
}

// Fake stands up an in-memory server endpoint over loopback and exposes
// hooks tests use to drive the run lifecycle.
type Fake struct {
	// Execution prescribes the scripted lifecycle the fake drives.
	Execution ApplyExecution

	mu      sync.Mutex
	allEvts []*pb.Envelope

	handler *fakeHandler
	srv     *httptest.Server

	// goroutine lifecycle: cancel stops InjectPauseAt goroutines; wg blocks
	// until they exit so t.Cleanup can call srv.Close without racing.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New starts a fake server on a random loopback port and registers t.Cleanup
// to cancel pending goroutines, wait for them to exit, then close the server.
func New(t testing.TB) *Fake {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	f := &Fake{ctx: ctx, cancel: cancel}
	f.handler = &fakeHandler{
		parent:      f,
		criteriaID:  "test-criteria-id",
		token:       "test-token",
		events:      make(map[string][]*pb.Envelope),
		controls:    make(chan *pb.ControlMessage, 32),
		ctlAttached: make(chan struct{}, 1),
	}

	mux := http.NewServeMux()
	path, h := criteriav1connect.NewCriteriaServiceHandler(f.handler)
	mux.Handle(path, h)

	srv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	srv.Start()
	f.srv = srv

	t.Cleanup(func() {
		cancel()
		f.wg.Wait()
		srv.Close()
	})
	return f
}

// URL returns the base URL of the fake server (http scheme over h2c).
func (f *Fake) URL() string { return f.srv.URL }

// Events returns a point-in-time snapshot of all envelopes the fake received.
func (f *Fake) Events() []*pb.Envelope {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*pb.Envelope, len(f.allEvts))
	copy(out, f.allEvts)
	return out
}

// HasStepEntered reports whether the fake received a StepEntered event for
// the named step.
func (f *Fake) HasStepEntered(step string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, env := range f.allEvts {
		if se := env.GetStepEntered(); se != nil && se.Step == step {
			return true
		}
	}
	return false
}

// HasEventOfType reports whether the fake received at least one event with
// the given payload type name (e.g. "WaitEntered", "RunCompleted").
func (f *Fake) HasEventOfType(typeName string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, env := range f.allEvts {
		if envelopeTypeName(env) == typeName {
			return true
		}
	}
	return false
}

// SinceSeqHeaders returns a snapshot of the since_seq header values received
// across all SubmitEvents connections. An empty string means no since_seq was
// sent (first connection); a numeric string comes from a reconnect.
func (f *Fake) SinceSeqHeaders() []string {
	f.handler.mu.Lock()
	defer f.handler.mu.Unlock()
	out := make([]string, len(f.handler.sinceSeqHdr))
	copy(out, f.handler.sinceSeqHdr)
	return out
}

// WaitForCond polls pred at 5ms intervals until it returns true or d elapses,
// then fails the test.
func (f *Fake) WaitForCond(t testing.TB, d time.Duration, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !pred() {
		t.Fatalf("WaitForCond: condition not met within %s", d)
	}
}

// envelopeTypeName returns a human-readable payload type name for an envelope.
func envelopeTypeName(env *pb.Envelope) string {
	switch {
	case env.GetStepEntered() != nil:
		return "StepEntered"
	case env.GetStepOutcome() != nil:
		return "StepOutcome"
	case env.GetStepTransition() != nil:
		return "StepTransition"
	case env.GetRunStarted() != nil:
		return "RunStarted"
	case env.GetRunCompleted() != nil:
		return "RunCompleted"
	case env.GetRunFailed() != nil:
		return "RunFailed"
	case env.GetWaitEntered() != nil:
		return "WaitEntered"
	case env.GetWaitResumed() != nil:
		return "WaitResumed"
	default:
		return "Unknown"
	}
}

// --- internal handler -------------------------------------------------------

type fakeHandler struct {
	criteriav1connect.UnimplementedCriteriaServiceHandler

	parent     *Fake
	criteriaID string
	token      string

	mu          sync.Mutex
	events      map[string][]*pb.Envelope // run_id → ordered, persisted envelopes
	sinceSeqHdr []string                  // since_seq header values per connection
	dropDone    bool                      // true after DropStreamAt has fired once

	controls    chan *pb.ControlMessage
	ctlAttached chan struct{}
}

func (h *fakeHandler) Register(_ context.Context, _ *connect.Request[pb.RegisterRequest]) (*connect.Response[pb.RegisterResponse], error) {
	return connect.NewResponse(&pb.RegisterResponse{
		CriteriaId: h.criteriaID,
		Token:      h.token,
	}), nil
}

func (h *fakeHandler) Heartbeat(_ context.Context, _ *connect.Request[pb.HeartbeatRequest]) (*connect.Response[pb.HeartbeatResponse], error) {
	return connect.NewResponse(&pb.HeartbeatResponse{}), nil
}

func (h *fakeHandler) CreateRun(_ context.Context, req *connect.Request[pb.CreateRunRequest]) (*connect.Response[pb.Run], error) {
	h.mu.Lock()
	id := "run-" + strconv.Itoa(len(h.events)+1)
	h.events[id] = nil
	h.mu.Unlock()
	return connect.NewResponse(&pb.Run{
		RunId:        id,
		CriteriaId:   req.Msg.CriteriaId,
		WorkflowName: req.Msg.WorkflowName,
		Status:       "pending",
	}), nil
}

func (h *fakeHandler) SubmitEvents(_ context.Context, stream *connect.BidiStream[pb.Envelope, pb.Ack]) error {
	sinceRaw := stream.RequestHeader().Get("since_seq")
	h.mu.Lock()
	h.sinceSeqHdr = append(h.sinceSeqHdr, sinceRaw)
	h.mu.Unlock()

	var sinceSeq uint64
	replayRequested := sinceRaw != ""
	if replayRequested {
		if v, err := strconv.ParseUint(sinceRaw, 10, 64); err == nil {
			sinceSeq = v
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

		// On first message for a run, replay any persisted events above sinceSeq.
		if replayRequested && !replayed[msg.RunId] {
			if err := h.replayAcks(stream, msg.RunId, sinceSeq); err != nil {
				return err
			}
			replayed[msg.RunId] = true
		}

		seq, cid, shouldDrop := h.persistMsg(msg)

		if shouldDrop {
			return connect.NewError(connect.CodeUnavailable, errors.New("applytest: stream drop injected"))
		}

		if err := stream.Send(&pb.Ack{RunId: msg.RunId, Seq: seq, CorrelationId: cid}); err != nil {
			return err
		}

		h.triggerActions(msg)
	}
}

// replayAcks sends ack messages for all persisted events above sinceSeq.
func (h *fakeHandler) replayAcks(stream *connect.BidiStream[pb.Envelope, pb.Ack], runID string, sinceSeq uint64) error {
	h.mu.Lock()
	prior := append([]*pb.Envelope(nil), h.events[runID]...)
	h.mu.Unlock()
	for _, p := range prior {
		if p.Seq <= sinceSeq {
			continue
		}
		if err := stream.Send(&pb.Ack{RunId: p.RunId, Seq: p.Seq, CorrelationId: p.CorrelationId}); err != nil {
			return err
		}
	}
	return nil
}

// persistMsg deduplicates the envelope, applies DropStreamAt logic, persists
// the event if it should be stored, and returns (seq, correlationID, shouldDrop).
func (h *fakeHandler) persistMsg(msg *pb.Envelope) (seq uint64, cid string, shouldDrop bool) {
	h.mu.Lock()
	list := h.events[msg.RunId]

	// Dedup on (run_id, correlation_id) mirrors the real server's behaviour.
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

	// DropStreamAt fires once, before the event is persisted.
	ex := h.parent.Execution
	shouldDrop = ex.DropStreamAt != "" && !h.dropDone
	if shouldDrop {
		if se := msg.GetStepEntered(); se == nil || se.Step != ex.DropStreamAt {
			shouldDrop = false
		}
	}
	if shouldDrop {
		h.dropDone = true
	}

	if !duplicate && !shouldDrop {
		msg.Seq = uint64(len(list) + 1)
		seq = msg.Seq
		h.events[msg.RunId] = append(list, msg)
		h.parent.mu.Lock()
		h.parent.allEvts = append(h.parent.allEvts, msg)
		h.parent.mu.Unlock()
	}
	cid = msg.CorrelationId
	h.mu.Unlock()
	return seq, cid, shouldDrop
}

// triggerActions fires scripted control messages in response to a received event.
func (h *fakeHandler) triggerActions(env *pb.Envelope) {
	ex := h.parent.Execution

	if ex.CancelAt != "" {
		if se := env.GetStepEntered(); se != nil && se.Step == ex.CancelAt {
			h.sendControl(&pb.ControlMessage{
				Command: &pb.ControlMessage_RunCancel{
					RunCancel: &pb.RunCancel{RunId: env.RunId, Reason: "applytest: cancel injected"},
				},
			})
		}
	}

	if ex.InjectPauseAt != "" {
		if we := env.GetWaitEntered(); we != nil && we.Node == ex.InjectPauseAt {
			h.schedulePauseResume(env.RunId)
		}
	}
}

// sendControl sends a ControlMessage non-blocking on a best-effort basis.
func (h *fakeHandler) sendControl(msg *pb.ControlMessage) {
	select {
	case h.controls <- msg:
	default:
	}
}

// schedulePauseResume starts a goroutine that sends a ResumeRun message after
// the configured delay.
func (h *fakeHandler) schedulePauseResume(runID string) {
	ex := h.parent.Execution
	delay := ex.ResumeAfter
	if delay <= 0 {
		delay = 10 * time.Millisecond
	}
	h.parent.wg.Add(1)
	go func() {
		defer h.parent.wg.Done()
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-h.parent.ctx.Done():
			return
		case <-timer.C:
		}
		h.sendControl(&pb.ControlMessage{
			Command: &pb.ControlMessage_ResumeRun{
				ResumeRun: &pb.ResumeRun{
					RunId:   runID,
					Signal:  "resume",
					Payload: map[string]string{"outcome": "received"},
				},
			},
		})
	}()
}

func (h *fakeHandler) Control(ctx context.Context, _ *connect.Request[pb.ControlSubscribeRequest], stream *connect.ServerStream[pb.ControlMessage]) error {
	if err := stream.Send(&pb.ControlMessage{
		Command: &pb.ControlMessage_ControlReady{ControlReady: &pb.ControlReady{}},
	}); err != nil {
		return err
	}
	select {
	case h.ctlAttached <- struct{}{}:
	default:
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-h.controls:
			if !ok {
				return nil
			}
			if err := stream.Send(msg); err != nil {
				return err
			}
		}
	}
}
