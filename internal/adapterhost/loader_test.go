package adapterhost

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/brokenbots/criteria/internal/adapter"
	v2 "github.com/brokenbots/criteria/sdk/pb/criteria/v2"
	"github.com/brokenbots/criteria/workflow"
)

func TestLoaderResolveNoopAdapter(t *testing.T) {
	adapterBin := buildNoopAdapter(t)
	loader := NewLoaderWithDiscovery(func(string) (string, error) {
		return adapterBin, nil
	})
	t.Cleanup(func() {
		_ = loader.Shutdown(context.Background())
	})

	p, err := loader.Resolve(context.Background(), "noop")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	info, err := p.Info(context.Background())
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if info.Name != "noop" {
		t.Fatalf("adapter name=%q want noop", info.Name)
	}
	if info.Version == "" {
		t.Fatal("expected non-empty adapter version")
	}
}

// canceledCtxHandle is a minimal Handle stub that always returns a
// context-canceled error from Execute. Used to test log-level gating for
// host-canceled context expected-close path (W12).
type canceledCtxHandle struct{}

func (c *canceledCtxHandle) Info(context.Context) (Info, error) {
	return Info{Name: "cancel-stub"}, nil
}
func (c *canceledCtxHandle) OpenSession(context.Context, string, map[string]string) error { return nil }
func (c *canceledCtxHandle) Execute(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	return adapter.Result{Outcome: "failure"}, context.Canceled
}
func (c *canceledCtxHandle) CloseSession(context.Context, string) error { return nil }
func (c *canceledCtxHandle) Kill()                                      {}

// TestLoader_HostCanceledContextLogsAtDebug verifies that when the surrounding
// context is canceled by the host (and the session closing flag is NOT set),
// Execute still logs at DEBUG rather than WARN, treating host cancellation as
// an expected close (W12 step 2).
func TestLoader_HostCanceledContextLogsAtDebug(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })

	sm := &SessionManager{
		loader:   nil,
		sessions: map[string]*Session{},
	}
	sess := &Session{Name: "agent", Adapter: "cancel-stub", handle: &canceledCtxHandle{}}
	// closing flag intentionally NOT set — this simulates the host canceling
	// the run context rather than an explicit SessionManager.Close call.
	sm.mu.Lock()
	sm.sessions["agent"] = sess
	sm.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel to simulate host-initiated cancellation

	sink := &adapterEventCollector{}
	_, _ = sm.Execute(ctx, "agent", &workflow.StepNode{Name: "run"}, sink)

	out := buf.String()
	if !strings.Contains(out, "DEBUG") {
		t.Fatalf("expected DEBUG log entry for host-canceled context, got:\n%s", out)
	}
	if strings.Contains(out, "WARN") {
		t.Errorf("expected no WARN log entry for host-canceled context, got:\n%s", out)
	}
}

// from Execute. Used to test log-level gating for expected closes (W12).
type eofHandle struct{}

func (e *eofHandle) Info(context.Context) (Info, error)                           { return Info{Name: "eof-stub"}, nil }
func (e *eofHandle) OpenSession(context.Context, string, map[string]string) error { return nil }
func (e *eofHandle) Execute(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	return adapter.Result{Outcome: "failure"}, errors.New("eof: connection terminated")
}
func (e *eofHandle) CloseSession(context.Context, string) error { return nil }
func (e *eofHandle) Kill()                                      {}

// TestLoader_ExpectedCloseLogsAtDebug verifies that when the closing flag is
// set on a session and Execute returns an EOF-like error, the session manager
// logs at DEBUG (not WARN), indicating an expected close (W12 step 2).
func TestLoader_ExpectedCloseLogsAtDebug(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })

	sm := &SessionManager{
		loader:   nil,
		sessions: map[string]*Session{},
	}
	sess := &Session{Name: "agent", Adapter: "eof-stub", handle: &eofHandle{}}
	sess.closing.Store(true)
	sm.mu.Lock()
	sm.sessions["agent"] = sess
	sm.mu.Unlock()

	sink := &adapterEventCollector{}
	_, _ = sm.Execute(context.Background(), "agent", &workflow.StepNode{Name: "run"}, sink)

	out := buf.String()
	if !strings.Contains(out, "DEBUG") {
		t.Fatalf("expected DEBUG log entry for expected close, got:\n%s", out)
	}
	if strings.Contains(out, "WARN") {
		t.Errorf("expected no WARN log entry for expected close, got:\n%s", out)
	}
}

// TestLoader_HostCanceledContextWithEOFLogsAtDebug is the regression test for
// the specific boundary: host cancels the context AND the adapter returns an
// EOF-like error (not context.Canceled). EOF matches the crash heuristic, but
// the canceled context must suppress crash classification → DEBUG not WARN
// (W12 step 2).
func TestLoader_HostCanceledContextWithEOFLogsAtDebug(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })

	sm := &SessionManager{
		loader:   nil,
		sessions: map[string]*Session{},
	}
	// eofHandle returns "eof: connection terminated" — matches the crash heuristic.
	// closing flag NOT set; only ctx.Err() should suppress crash classification.
	sess := &Session{Name: "agent", Adapter: "eof-stub", handle: &eofHandle{}}
	sm.mu.Lock()
	sm.sessions["agent"] = sess
	sm.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // simulate host aborting the run

	sink := &adapterEventCollector{}
	_, _ = sm.Execute(ctx, "agent", &workflow.StepNode{Name: "run"}, sink)

	out := buf.String()
	if !strings.Contains(out, "DEBUG") {
		t.Fatalf("expected DEBUG log for canceled-context + EOF error, got:\n%s", out)
	}
	if strings.Contains(out, "WARN") {
		t.Errorf("expected no WARN log for canceled-context + EOF error, got:\n%s", out)
	}
}

// recordingClient implements Client and captures the last ExecuteRequest it
// received. It returns a single ExecuteResult with the first outcome it finds
// in the request's AllowedOutcomes list (or "success" if the list is empty).
type recordingClient struct {
	lastExecuteReq *v2.ExecuteRequest
}

func (r *recordingClient) Info(_ context.Context, _ *v2.InfoRequest) (*v2.InfoResponse, error) {
	return &v2.InfoResponse{Name: "recording-stub"}, nil
}

func (r *recordingClient) OpenSession(_ context.Context, _ *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	return &v2.OpenSessionResponse{}, nil
}

func (r *recordingClient) Execute(_ context.Context, req *v2.ExecuteRequest, sink ExecuteEventSink) error {
	r.lastExecuteReq = req
	outcome := "success"
	if len(req.AllowedOutcomes) > 0 {
		outcome = req.AllowedOutcomes[0]
	}
	return sink.Emit(&v2.ExecuteEvent{
		Event: &v2.ExecuteEvent_Result{
			Result: &v2.ExecuteResult{Outcome: outcome},
		},
	})
}

func (r *recordingClient) Log(_ context.Context, _ *v2.LogRequest, _ LogEventSink) error {
	return nil
}

func (r *recordingClient) Permissions(_ context.Context, _ <-chan *v2.PermissionEvent, _ chan<- *v2.PermissionDecision) error {
	return nil
}

func (r *recordingClient) Pause(_ context.Context, _ *v2.PauseRequest) (*v2.PauseResponse, error) {
	return &v2.PauseResponse{}, nil
}

func (r *recordingClient) Resume(_ context.Context, _ *v2.ResumeRequest) (*v2.ResumeResponse, error) {
	return &v2.ResumeResponse{}, nil
}

func (r *recordingClient) Snapshot(_ context.Context, _ *v2.SnapshotRequest) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{}, nil
}

func (r *recordingClient) Restore(_ context.Context, _ *v2.RestoreRequest) (*v2.RestoreResponse, error) {
	return &v2.RestoreResponse{}, nil
}

func (r *recordingClient) Inspect(_ context.Context, _ *v2.InspectRequest) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}

func (r *recordingClient) CloseSession(_ context.Context, _ *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
	return &v2.CloseSessionResponse{}, nil
}

// TestLoader_PopulatesAllowedOutcomes verifies that ExecuteRequest is
// constructed with AllowedOutcomes derived from the step's declared
// outcome set, sorted ascending.
func TestLoader_PopulatesAllowedOutcomes(t *testing.T) {
	rc := &recordingClient{}
	p := &rpcHandle{name: "recording-stub", rpc: rc}

	step := &workflow.StepNode{
		Name: "review",
		// Insert in non-sorted order to verify sorting.
		Outcomes: map[string]*workflow.CompiledOutcome{
			"failure":           {Next: "failed"},
			"approved":          {Next: "done"},
			"changes_requested": {Next: "rework"},
		},
	}

	sink := &adapterEventCollector{}
	result, err := p.Execute(context.Background(), "sess-1", step, sink)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	req := rc.lastExecuteReq
	if req == nil {
		t.Fatal("no ExecuteRequest was captured")
	}

	want := []string{"approved", "changes_requested", "failure"}
	if len(req.AllowedOutcomes) != len(want) {
		t.Fatalf("AllowedOutcomes = %v, want %v", req.AllowedOutcomes, want)
	}
	for i, v := range want {
		if req.AllowedOutcomes[i] != v {
			t.Errorf("AllowedOutcomes[%d] = %q, want %q", i, req.AllowedOutcomes[i], v)
		}
	}

	// The recording client returns the first allowed outcome.
	if result.Outcome != "approved" {
		t.Errorf("result.Outcome = %q, want %q", result.Outcome, "approved")
	}
}

// TestLoader_PopulatesAllowedOutcomes_Empty verifies that a step with no
// declared outcomes produces a non-nil empty AllowedOutcomes slice in the
// constructed ExecuteRequest (host-side pre-serialization contract). On the
// wire, proto3 repeated fields treat nil and empty equivalently; adapters
// must not use nil vs empty to infer host version or behavior.
func TestLoader_PopulatesAllowedOutcomes_Empty(t *testing.T) {
	rc := &recordingClient{}
	p := &rpcHandle{name: "recording-stub", rpc: rc}

	step := &workflow.StepNode{Name: "open", Outcomes: nil}

	sink := &adapterEventCollector{}
	if _, err := p.Execute(context.Background(), "sess-2", step, sink); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	req := rc.lastExecuteReq
	if req == nil {
		t.Fatal("no ExecuteRequest was captured")
	}
	if req.AllowedOutcomes == nil {
		t.Fatal("AllowedOutcomes should be non-nil empty slice, got nil")
	}
	if len(req.AllowedOutcomes) != 0 {
		t.Fatalf("AllowedOutcomes = %v, want empty", req.AllowedOutcomes)
	}
}

// TestLoader_ExecuteUsesInputNotConfig verifies that the ExecuteRequest uses
// the Input field (v2 rename from Config).
func TestLoader_ExecuteUsesInputNotConfig(t *testing.T) {
	rc := &recordingClient{}
	p := &rpcHandle{name: "recording-stub", rpc: rc}

	step := &workflow.StepNode{
		Name:  "task",
		Input: map[string]string{"prompt": "hello"},
		Outcomes: map[string]*workflow.CompiledOutcome{
			"success": {Next: "done"},
		},
	}

	sink := &adapterEventCollector{}
	if _, err := p.Execute(context.Background(), "sess-3", step, sink); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	req := rc.lastExecuteReq
	if req == nil {
		t.Fatal("no ExecuteRequest was captured")
	}
	if req.Input["prompt"] != "hello" {
		t.Errorf("ExecuteRequest.Input[prompt] = %q; want %q", req.Input["prompt"], "hello")
	}
}

// TestCollectAllowedOutcomes_Sorted verifies that collectAllowedOutcomes
// returns outcome names sorted ascending regardless of map insertion order.
func TestCollectAllowedOutcomes_Sorted(t *testing.T) {
	step := &workflow.StepNode{Outcomes: map[string]*workflow.CompiledOutcome{
		"failure":           {Next: "failed"},
		"approved":          {Next: "done"},
		"changes_requested": {Next: "rework"},
	}}
	got := collectAllowedOutcomes(step)
	want := []string{"approved", "changes_requested", "failure"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i, v := range want {
		if got[i] != v {
			t.Errorf("got[%d] = %q, want %q", i, got[i], v)
		}
	}
}

// TestCollectAllowedOutcomes_Empty verifies that a step with no outcomes
// returns a non-nil empty slice (host-side contract). Adapters receive this
// over the wire where proto3 nil and empty are equivalent, but the host
// helper must produce []string{} rather than nil for clarity and consistency.
func TestCollectAllowedOutcomes_Empty(t *testing.T) {
	got := collectAllowedOutcomes(&workflow.StepNode{})
	if got == nil {
		t.Fatal("expected non-nil empty slice, got nil")
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want empty", got)
	}
}

// ── Permissions stream failure tests ─────────────────────────────────────────

// brokenPermClient is a recordingClient variant whose Permissions method
// returns the configured error immediately (simulating a broken bidi stream).
type brokenPermClient struct {
	recordingClient
	permErr error
}

func (r *brokenPermClient) Permissions(_ context.Context, reqs <-chan *v2.PermissionEvent, _ chan<- *v2.PermissionDecision) error {
	// Drain the channel so the sender goroutine is not blocked.
	go func() {
		for range reqs {
		}
	}()
	return r.permErr
}

// TestExecute_BrokenPermissionsStreamSurfacesError verifies that when the
// adapter's Permissions bidi stream returns an unexpected error, Execute
// surfaces it instead of silently swallowing it.
func TestExecute_BrokenPermissionsStreamSurfacesError(t *testing.T) {
	rc := &brokenPermClient{permErr: errors.New("permissions: rpc error: internal")}
	p := &rpcHandle{name: "test-stub", rpc: rc}
	step := &workflow.StepNode{Name: "run"}
	sink := &adapterEventCollector{}

	_, err := p.Execute(context.Background(), "sess", step, sink)
	if err == nil {
		t.Fatal("expected error for broken Permissions stream, got nil")
	}
	if !strings.Contains(err.Error(), "permissions") {
		t.Errorf("error %q should mention permissions", err.Error())
	}
}

// blockingExecuteClient is a Client stub where Execute blocks until the
// blockExec channel is closed (or ctx is canceled). Used to test the
// "Permissions failure aborts Execute" path.
type blockingExecuteClient struct {
	recordingClient
	blockExec chan struct{}
	permErr   error
}

func (r *blockingExecuteClient) Execute(ctx context.Context, _ *v2.ExecuteRequest, sink ExecuteEventSink) error {
	select {
	case <-r.blockExec:
		// Unblocked normally; emit a result.
		return sink.Emit(&v2.ExecuteEvent{
			Event: &v2.ExecuteEvent_Result{
				Result: &v2.ExecuteResult{Outcome: "success"},
			},
		})
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *blockingExecuteClient) Permissions(_ context.Context, reqs <-chan *v2.PermissionEvent, _ chan<- *v2.PermissionDecision) error {
	go func() {
		for range reqs {
		}
	}()
	return r.permErr
}

// TestExecute_PermissionsFailureAbortsExecute verifies that when Permissions
// fails while Execute is still running (blocking), the execCtx is canceled so
// Execute returns promptly and the Permissions error is surfaced as root cause.
func TestExecute_PermissionsFailureAbortsExecute(t *testing.T) {
	blockExec := make(chan struct{}) // never closed; Execute will block until ctx canceled
	rc := &blockingExecuteClient{
		blockExec: blockExec,
		permErr:   errors.New("permissions: stream reset"),
	}
	p := &rpcHandle{name: "test-stub", rpc: rc}
	step := &workflow.StepNode{Name: "run"}
	sink := &adapterEventCollector{}

	_, err := p.Execute(context.Background(), "sess", step, sink)
	if err == nil {
		t.Fatal("expected error when Permissions aborts Execute, got nil")
	}
	if !strings.Contains(err.Error(), "permissions") {
		t.Errorf("error %q should mention permissions as root cause", err.Error())
	}
}

// unimplementedPermClient returns a gRPC Unimplemented error from Permissions,
// simulating an adapter that does not support the Permissions bidi stream.
type unimplementedPermClient struct {
	recordingClient
}

func (r *unimplementedPermClient) Permissions(_ context.Context, reqs <-chan *v2.PermissionEvent, _ chan<- *v2.PermissionDecision) error {
	go func() {
		for range reqs {
		}
	}()
	return status.Error(codes.Unimplemented, "method Permissions not implemented")
}

// TestExecute_UnimplementedPermissionsIsOptOut verifies that a gRPC Unimplemented
// error from the Permissions stream is treated as an expected opt-out (the adapter
// does not implement the Permissions bidi stream) and Execute succeeds — no error
// is surfaced to the caller.
func TestExecute_UnimplementedPermissionsIsOptOut(t *testing.T) {
	rc := &unimplementedPermClient{}
	p := &rpcHandle{name: "test-stub", rpc: rc}
	step := &workflow.StepNode{Name: "run"}
	sink := &adapterEventCollector{}

	_, err := p.Execute(context.Background(), "sess", step, sink)
	if err != nil {
		t.Fatalf("expected no error for Unimplemented Permissions stream (opt-out), got: %v", err)
	}
}

// ── Chunk reassembly regression tests ────────────────────────────────────────

// TestEmitAdapter_ChunkOutOfOrder verifies that an out-of-order seq number in
// a multi-chunk adapter event produces an error and resets the reassembly state.
func TestEmitAdapter_ChunkOutOfOrder(t *testing.T) {
	cs := &executeCaptureSink{
		sink: &adapterEventCollector{},
	}

	// seq=0 — start a new sequence.
	if err := cs.emitAdapter(&v2.AdapterEvent{
		EventKind:   "test.event",
		PayloadJson: []byte(`{"a"`),
		Chunk:       &v2.Chunk{Seq: 0},
	}); err != nil {
		t.Fatalf("seq=0: unexpected error: %v", err)
	}

	// seq=2 — skipped seq=1; should be rejected.
	err := cs.emitAdapter(&v2.AdapterEvent{
		EventKind:   "test.event",
		PayloadJson: []byte(`:"b"}`),
		Chunk:       &v2.Chunk{Seq: 2},
	})
	if err == nil {
		t.Fatal("expected out-of-order error, got nil")
	}
	if !strings.Contains(err.Error(), "out-of-order") {
		t.Errorf("error %q should mention out-of-order", err.Error())
	}
	// Buffer must be reset after error.
	if len(cs.adapterChunkBuf) != 0 {
		t.Errorf("buffer not reset after out-of-order error, len=%d", len(cs.adapterChunkBuf))
	}
	if cs.adapterChunkNextSeq != 0 {
		t.Errorf("nextSeq not reset after out-of-order error, got %d", cs.adapterChunkNextSeq)
	}
}

// TestEmitAdapter_ChunkOversize verifies that accumulating chunk data beyond
// maxChunkBufBytes returns an error and resets the reassembly buffer.
func TestEmitAdapter_ChunkOversize(t *testing.T) {
	cs := &executeCaptureSink{
		sink: &adapterEventCollector{},
		// Pre-fill buffer to exactly maxChunkBufBytes-1 bytes, simulating
		// a long in-progress sequence. adapterChunkNextSeq=1 means seq=1 is next.
		adapterChunkBuf:     make([]byte, maxChunkBufBytes-1),
		adapterChunkNextSeq: 1,
	}

	// seq=1 with 2 bytes pushes total to maxChunkBufBytes+1 — must be rejected.
	err := cs.emitAdapter(&v2.AdapterEvent{
		EventKind:   "test.event",
		PayloadJson: []byte("xx"),
		Chunk:       &v2.Chunk{Seq: 1},
	})
	if err == nil {
		t.Fatal("expected oversize error, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error %q should mention exceeds", err.Error())
	}
	if len(cs.adapterChunkBuf) != 0 {
		t.Errorf("buffer not reset after oversize error, len=%d", len(cs.adapterChunkBuf))
	}
}

// TestLogForwardSink_ChunkOversize verifies that a log chunk sequence that
// would exceed maxLogLineBufBytes is rejected with an error.
func TestLogForwardSink_ChunkOversize(t *testing.T) {
	ls := &logForwardSink{
		sink: &adapterEventCollector{},
		// Pre-fill buffer near limit; chunkSeqs[stdout]=1 means seq=1 is expected.
		chunkBufs: map[string][]byte{
			"stdout": make([]byte, maxLogLineBufBytes-1),
		},
		chunkSeqs: map[string]uint32{
			"stdout": 1,
		},
	}

	// seq=1 with 2 bytes pushes total over maxLogLineBufBytes.
	err := ls.Emit(&v2.LogEvent{
		StreamName: "stdout",
		Line:       []byte("xx"),
		Chunk:      &v2.Chunk{Seq: 1},
	})
	if err == nil {
		t.Fatal("expected oversize error for log chunk, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error %q should mention exceeds", err.Error())
	}
	// Stream buffer should be removed after oversize error.
	if _, ok := ls.chunkBufs["stdout"]; ok {
		t.Error("stream buffer should be deleted after oversize error")
	}
}

// TestLogForwardSink_ChunkOutOfOrder verifies that an out-of-order seq number
// in a multi-chunk log event produces an error and resets the stream state.
func TestLogForwardSink_ChunkOutOfOrder(t *testing.T) {
	sink := &adapterEventCollector{}
	ls := &logForwardSink{sink: sink}

	// seq=0 starts a new sequence for stream "stdout".
	if err := ls.Emit(&v2.LogEvent{
		StreamName: "stdout",
		Line:       []byte("hello "),
		Chunk:      &v2.Chunk{Seq: 0},
	}); err != nil {
		t.Fatalf("seq=0: unexpected error: %v", err)
	}

	// seq=2 — skipped seq=1; must be rejected.
	err := ls.Emit(&v2.LogEvent{
		StreamName: "stdout",
		Line:       []byte("world"),
		Chunk:      &v2.Chunk{Seq: 2},
	})
	if err == nil {
		t.Fatal("expected out-of-order error, got nil")
	}
	if !strings.Contains(err.Error(), "out-of-order") {
		t.Errorf("error %q should mention out-of-order", err.Error())
	}
	// Buffer and seq must be cleared after error.
	if _, ok := ls.chunkBufs["stdout"]; ok {
		t.Error("stream buffer should be deleted after out-of-order error")
	}
	if ls.chunkSeqs["stdout"] != 0 {
		t.Errorf("chunkSeqs[stdout] should be 0 after error, got %d", ls.chunkSeqs["stdout"])
	}
}

// TestLogForwardSink_ChunkNonZeroSeqWithNoSequence verifies that a non-zero
// seq chunk received when no sequence is in progress for that stream is rejected.
func TestLogForwardSink_ChunkNonZeroSeqWithNoSequence(t *testing.T) {
	ls := &logForwardSink{sink: &adapterEventCollector{}}

	err := ls.Emit(&v2.LogEvent{
		StreamName: "stderr",
		Line:       []byte("orphan"),
		Chunk:      &v2.Chunk{Seq: 3},
	})
	if err == nil {
		t.Fatal("expected error for non-zero seq with no sequence in progress, got nil")
	}
	if !strings.Contains(err.Error(), "no sequence in progress") {
		t.Errorf("error %q should mention 'no sequence in progress'", err.Error())
	}
}

// TestLogForwardSink_AggregateCapRejectsNewStream verifies that the aggregate
// memory cap across all concurrent log chunk buffers is enforced so a
// misbehaving adapter cannot open many streams each near the per-stream limit.
func TestLogForwardSink_AggregateCapRejectsNewStream(t *testing.T) {
	// Fill several streams with near-cap buffers so total is close to maxTotalLogBufBytes.
	existing := make(map[string][]byte)
	existingSeqs := make(map[string]uint32)
	// Divide aggregate cap among 4 streams, leaving 1 byte of headroom total.
	perStream := maxTotalLogBufBytes / 4
	for _, name := range []string{"s1", "s2", "s3", "s4"} {
		existing[name] = make([]byte, perStream)
		existingSeqs[name] = 1
	}
	ls := &logForwardSink{
		sink:      &adapterEventCollector{},
		chunkBufs: existing,
		chunkSeqs: existingSeqs,
	}

	// A new stream starting at seq=0 pushes aggregate over maxTotalLogBufBytes.
	err := ls.Emit(&v2.LogEvent{
		StreamName: "overflow",
		Line:       make([]byte, 2),
		Chunk:      &v2.Chunk{Seq: 0},
	})
	if err == nil {
		t.Fatal("expected aggregate cap error, got nil")
	}
	if !strings.Contains(err.Error(), "aggregate") {
		t.Errorf("error %q should mention aggregate", err.Error())
	}
}

// TestPermissionsStreamUnimplemented verifies that an adapter returning
// Unimplemented for the Permissions RPC does not abort or block Execute.
// This is the regression test for the "dead Permissions stream cannot block
// Execute" hardening added in WS03.
func TestPermissionsStreamUnimplemented(t *testing.T) {
	handle := &rpcHandle{
		name: "stub",
		rpc:  &unimplementedPermissionsClient{},
	}

	step := &workflow.StepNode{
		Name: "test-step",
		Outcomes: map[string]*workflow.CompiledOutcome{
			"success": {},
			"failure": {},
		},
	}
	sink := &adapterEventCollector{}
	res, err := handle.Execute(context.Background(), "sess-1", step, sink)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if res.Outcome != "success" {
		t.Errorf("outcome = %q; want success", res.Outcome)
	}
}

// unimplementedPermissionsClient is a minimal Client stub whose Permissions
// method returns codes.Unimplemented immediately, simulating an adapter that
// has not implemented the Permissions RPC.
type unimplementedPermissionsClient struct{}

func (c *unimplementedPermissionsClient) Info(_ context.Context, _ *v2.InfoRequest) (*v2.InfoResponse, error) {
	return nil, errors.New("not called")
}
func (c *unimplementedPermissionsClient) OpenSession(_ context.Context, _ *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	return nil, errors.New("not called")
}
func (c *unimplementedPermissionsClient) Execute(_ context.Context, _ *v2.ExecuteRequest, sink ExecuteEventSink) error {
	// Send a valid success result so Execute completes normally.
	return sink.Emit(&v2.ExecuteEvent{
		Event: &v2.ExecuteEvent_Result{
			Result: &v2.ExecuteResult{Outcome: "success"},
		},
	})
}
func (c *unimplementedPermissionsClient) Log(_ context.Context, _ *v2.LogRequest, _ LogEventSink) error {
	return nil
}
func (c *unimplementedPermissionsClient) Permissions(_ context.Context, _ <-chan *v2.PermissionEvent, _ chan<- *v2.PermissionDecision) error {
	return status.Error(codes.Unimplemented, "Permissions not implemented")
}
func (c *unimplementedPermissionsClient) Pause(_ context.Context, _ *v2.PauseRequest) (*v2.PauseResponse, error) {
	return nil, errors.New("not called")
}
func (c *unimplementedPermissionsClient) Resume(_ context.Context, _ *v2.ResumeRequest) (*v2.ResumeResponse, error) {
	return nil, errors.New("not called")
}
func (c *unimplementedPermissionsClient) Snapshot(_ context.Context, _ *v2.SnapshotRequest) (*v2.SnapshotResponse, error) {
	return nil, errors.New("not called")
}
func (c *unimplementedPermissionsClient) Restore(_ context.Context, _ *v2.RestoreRequest) (*v2.RestoreResponse, error) {
	return nil, errors.New("not called")
}
func (c *unimplementedPermissionsClient) Inspect(_ context.Context, _ *v2.InspectRequest) (*v2.InspectResponse, error) {
	return nil, errors.New("not called")
}
func (c *unimplementedPermissionsClient) CloseSession(_ context.Context, _ *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
	return &v2.CloseSessionResponse{}, nil
}

// TestToolInvocationPayloadSchema verifies that ToolInvocation events are
// forwarded with the canonical {"name", "arguments"} payload shape, not
// the old {"tool_name", "args"} shape that was temporarily introduced.
func TestToolInvocationPayloadSchema(t *testing.T) {
	args, err := structpb.NewStruct(map[string]any{"path": "/tmp/x"})
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}

	collector := &adapterEventCollector{}
	cs := &executeCaptureSink{
		sink: collector,
	}

	if err := cs.emitTool(&v2.ToolInvocation{
		ToolName: "read_file",
		Args:     args,
	}); err != nil {
		t.Fatalf("emitTool: %v", err)
	}

	payload, ok := collector.first("tool.invocation")
	if !ok {
		t.Fatal("expected tool.invocation event")
	}
	if payload["name"] != "read_file" {
		t.Errorf("payload[name] = %v; want read_file", payload["name"])
	}
	if _, hasArgs := payload["arguments"]; !hasArgs {
		t.Error("payload missing 'arguments' key")
	}
	if _, hasBad := payload["tool_name"]; hasBad {
		t.Error("payload must not contain 'tool_name' (old schema)")
	}
	if _, hasBad := payload["args"]; hasBad {
		t.Error("payload must not contain 'args' (old schema)")
	}
}

// ── Permission policy enforcement tests ──────────────────────────────────────

// permissionRequestEvent creates an AdapterEvent with kind "permission.request"
// and the given request_id and tool fields.
func permissionRequestEvent(requestID, tool string) *v2.AdapterEvent {
	p, _ := structpb.NewStruct(map[string]any{
		"request_id": requestID,
		"tool":       tool,
	})
	return &v2.AdapterEvent{EventKind: "permission.request", Payload: p}
}

// TestHandlePermissionRequest_Allow verifies that when the step's allow_tools
// matches the requested tool, handlePermissionRequest emits permission.granted
// and forwards a PermissionEvent.request to the requests channel.
func TestHandlePermissionRequest_Allow(t *testing.T) {
	collector := &adapterEventCollector{}
	requests := make(chan *v2.PermissionEvent, 4)
	cs := &executeCaptureSink{
		sink:       collector,
		policy:     NewPolicy([]string{"read_file"}),
		allowTools: []string{"read_file"},
		requests:   requests,
		ctx:        context.Background(),
	}
	cs.handlePermissionRequest(permissionRequestEvent("req-1", "read_file"))

	if !collector.saw("permission.granted") {
		t.Fatal("expected permission.granted event")
	}
	if collector.saw("permission.denied") {
		t.Error("unexpected permission.denied event")
	}
	select {
	case ev := <-requests:
		if ev.GetRequest() == nil {
			t.Error("expected PermissionEvent.request (allow); got cancel")
		}
		if ev.GetRequest().GetRequestId() != "req-1" {
			t.Errorf("request_id = %q; want req-1", ev.GetRequest().GetRequestId())
		}
	default:
		t.Fatal("expected PermissionEvent forwarded to requests channel")
	}
}

// TestHandlePermissionRequest_Deny verifies that when the step has no allow_tools
// (deny-all policy), handlePermissionRequest emits permission.denied, sets
// anyDenied, and forwards a PermissionEvent.cancel to the requests channel.
func TestHandlePermissionRequest_Deny(t *testing.T) {
	collector := &adapterEventCollector{}
	requests := make(chan *v2.PermissionEvent, 4)
	cs := &executeCaptureSink{
		sink:       collector,
		policy:     NewPolicy(nil), // deny-all
		allowTools: []string{},
		requests:   requests,
		ctx:        context.Background(),
	}
	cs.handlePermissionRequest(permissionRequestEvent("req-2", "shell"))

	if !collector.saw("permission.denied") {
		t.Fatal("expected permission.denied event")
	}
	if collector.saw("permission.granted") {
		t.Error("unexpected permission.granted event")
	}
	if !cs.anyDenied {
		t.Error("anyDenied should be true after denial")
	}
	select {
	case ev := <-requests:
		if ev.GetCancel() == nil {
			t.Error("expected PermissionEvent.cancel (deny); got request")
		}
		if ev.GetCancel().GetRequestId() != "req-2" {
			t.Errorf("cancel request_id = %q; want req-2", ev.GetCancel().GetRequestId())
		}
	default:
		t.Fatal("expected PermissionEvent.cancel forwarded to requests channel")
	}
}

// TestExecute_DeniedPermissionOverridesSuccess verifies that when any
// permission request was denied during Execute, a "success" outcome from the
// adapter is overridden to "needs_review".
func TestExecute_DeniedPermissionOverridesSuccess(t *testing.T) {
	// Build an adapter event payload with request_id and tool.
	permPayload, _ := structpb.NewStruct(map[string]any{
		"request_id": "req-99",
		"tool":       "write_file",
	})
	rc := &deniedPermOverrideClient{permPayload: permPayload}
	p := &rpcHandle{name: "test-stub", rpc: rc}
	// Step with no allow_tools → deny-all policy.
	step := &workflow.StepNode{
		Name:       "run",
		AllowTools: nil,
		Outcomes: map[string]*workflow.CompiledOutcome{
			"success":      {Name: "success"},
			"needs_review": {Name: "needs_review"},
		},
	}
	sink := &adapterEventCollector{}
	res, err := p.Execute(context.Background(), "sess", step, sink)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if res.Outcome != "needs_review" {
		t.Errorf("outcome = %q; want needs_review (denied permission should override success)", res.Outcome)
	}
	if !sink.saw("permission.denied") {
		t.Error("expected permission.denied event in sink")
	}
}

// deniedPermOverrideClient simulates an adapter that emits a permission.request
// event and then returns outcome "success".
type deniedPermOverrideClient struct {
	recordingClient
	permPayload *structpb.Struct
}

func (r *deniedPermOverrideClient) Execute(_ context.Context, _ *v2.ExecuteRequest, sink ExecuteEventSink) error {
	// Emit a permission.request event.
	_ = sink.Emit(&v2.ExecuteEvent{
		Event: &v2.ExecuteEvent_Adapter{
			Adapter: &v2.AdapterEvent{
				EventKind: "permission.request",
				Payload:   r.permPayload,
			},
		},
	})
	// Emit success result.
	return sink.Emit(&v2.ExecuteEvent{
		Event: &v2.ExecuteEvent_Result{
			Result: &v2.ExecuteResult{Outcome: "success"},
		},
	})
}

// ── Concurrent EventSink serialization regression test ───────────────────────

// nonThreadSafeSink is an adapter.EventSink that panics if Adapter or Log are
// called concurrently. The race detector catches this, but the explicit check
// makes the test self-documenting and works without -race.
type nonThreadSafeSink struct {
	mu      sync.Mutex
	inUse   int32 // atomic: 0=idle, 1=in-use
	events  []string
	paniced bool
}

func (s *nonThreadSafeSink) Log(stream string, _ []byte) {
	if !atomic.CompareAndSwapInt32(&s.inUse, 0, 1) {
		s.mu.Lock()
		s.paniced = true
		s.mu.Unlock()
		return
	}
	time.Sleep(time.Microsecond) // hold briefly to expose races
	atomic.StoreInt32(&s.inUse, 0)
	s.mu.Lock()
	s.events = append(s.events, "log:"+stream)
	s.mu.Unlock()
}

func (s *nonThreadSafeSink) Adapter(kind string, _ any) {
	if !atomic.CompareAndSwapInt32(&s.inUse, 0, 1) {
		s.mu.Lock()
		s.paniced = true
		s.mu.Unlock()
		return
	}
	time.Sleep(time.Microsecond) // hold briefly to expose races
	atomic.StoreInt32(&s.inUse, 0)
	s.mu.Lock()
	s.events = append(s.events, "adapter:"+kind)
	s.mu.Unlock()
}

// TestSerializedEventSink_ConcurrentCallsAreOrdered verifies that
// serializedEventSink prevents concurrent calls to the underlying
// adapter.EventSink even when Adapter and Log are called from different
// goroutines simultaneously. The underlying nonThreadSafeSink detects
// concurrent access and sets paniced=true if serialization is missing.
func TestSerializedEventSink_ConcurrentCallsAreOrdered(t *testing.T) {
	inner := &nonThreadSafeSink{}
	wrapped := &serializedEventSink{inner: inner}

	const n = 500
	var wg sync.WaitGroup
	wg.Add(2 * n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			wrapped.Adapter("test.event", nil)
		}()
		go func() {
			defer wg.Done()
			wrapped.Log("stdout", []byte("hello"))
		}()
	}
	wg.Wait()

	inner.mu.Lock()
	paniced := inner.paniced
	inner.mu.Unlock()
	if paniced {
		t.Error("serializedEventSink allowed concurrent access to the underlying sink")
	}
}
