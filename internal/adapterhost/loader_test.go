package adapterhost

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin/runner"
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

func TestLoaderResolveWithCustomizer(t *testing.T) {
	adapterBin := buildNoopAdapter(t)
	loader := NewLoaderWithDiscovery(func(string) (string, error) {
		return adapterBin, nil
	})
	t.Cleanup(func() {
		_ = loader.Shutdown(context.Background())
	})

	var gotName string
	var gotCmd *exec.Cmd
	customizer := func(name string, cmd *exec.Cmd) {
		gotName = name
		gotCmd = cmd
	}

	p, err := loader.ResolveWithCustomizer(context.Background(), "noop", customizer)
	if err != nil {
		t.Fatalf("ResolveWithCustomizer: %v", err)
	}
	defer p.Kill()

	if gotName != "noop" {
		t.Fatalf("customizer name=%q want noop", gotName)
	}
	if gotCmd == nil {
		t.Fatal("expected customizer to receive non-nil exec.Cmd")
	}
	if gotCmd.Path != adapterBin {
		t.Fatalf("customizer cmd.Path=%q want %q", gotCmd.Path, adapterBin)
	}

	// Verify the adapter still works (customizer didn't break the command).
	info, err := p.Info(context.Background())
	if err != nil {
		t.Fatalf("info after customizer: %v", err)
	}
	if info.Name != "noop" {
		t.Fatalf("adapter name=%q want noop", info.Name)
	}
}

// canceledCtxHandle is a minimal Handle stub that always returns a
// context-canceled error from Execute. Used to test log-level gating for
// host-canceled context expected-close path (W12).
type canceledCtxHandle struct{}

func (c *canceledCtxHandle) Info(context.Context) (Info, error) {
	return Info{Name: "cancel-stub"}, nil
}
func (c *canceledCtxHandle) OpenSession(context.Context, string, map[string]string, map[string]string) error {
	return nil
}
func (c *canceledCtxHandle) Execute(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	return adapter.Result{Outcome: "failure"}, context.Canceled
}
func (c *canceledCtxHandle) CloseSession(context.Context, string) error { return nil }
func (c *canceledCtxHandle) Kill()                                      {}
func (c *canceledCtxHandle) Pause(context.Context, string) error        { return nil }
func (c *canceledCtxHandle) Resume(context.Context, string) error       { return nil }
func (c *canceledCtxHandle) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}

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

func (e *eofHandle) Info(context.Context) (Info, error) { return Info{Name: "eof-stub"}, nil }
func (e *eofHandle) OpenSession(context.Context, string, map[string]string, map[string]string) error {
	return nil
}
func (e *eofHandle) Execute(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	return adapter.Result{Outcome: "failure"}, errors.New("eof: connection terminated")
}
func (e *eofHandle) CloseSession(context.Context, string) error { return nil }
func (e *eofHandle) Kill()                                      {}
func (e *eofHandle) Pause(context.Context, string) error        { return nil }
func (e *eofHandle) Resume(context.Context, string) error       { return nil }
func (e *eofHandle) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}

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

func (r *recordingClient) Permissions(_ context.Context, _ <-chan *v2.PermissionEvent) error {
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

// testCmdRunner is a minimal runner.Runner implementation that delegates to an
// exec.Cmd. It is used to verify ResolveWithRunnerFunc wires the RunnerFunc
// into the go-plugin client correctly.
type testCmdRunner struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr io.ReadCloser
}

func newTestCmdRunner(_ hclog.Logger, cmd *exec.Cmd, _ string) (*testCmdRunner, error) {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	return &testCmdRunner{cmd: cmd, stdout: stdout, stderr: stderr}, nil
}

func (r *testCmdRunner) Start(_ context.Context) error { return r.cmd.Start() }
func (r *testCmdRunner) Wait(_ context.Context) error  { return r.cmd.Wait() }
func (r *testCmdRunner) Kill(_ context.Context) error {
	if r.cmd.Process != nil {
		return r.cmd.Process.Kill()
	}
	return nil
}
func (r *testCmdRunner) ID() string                        { return "test-runner" }
func (r *testCmdRunner) Name() string                      { return r.cmd.Path }
func (r *testCmdRunner) Stdout() io.ReadCloser             { return r.stdout }
func (r *testCmdRunner) Stderr() io.ReadCloser             { return r.stderr }
func (r *testCmdRunner) Diagnose(_ context.Context) string { return "" }
func (r *testCmdRunner) PluginToHost(pluginNet, pluginAddr string) (string, string, error) { //nolint:gocritic // test helper; named results trigger paramTypeCombine false positive
	return pluginNet, pluginAddr, nil
}
func (r *testCmdRunner) HostToPlugin(hostNet, hostAddr string) (string, string, error) { //nolint:gocritic // test helper; named results trigger paramTypeCombine false positive
	return hostNet, hostAddr, nil
}

func TestLoaderResolveWithRunnerFunc(t *testing.T) {
	adapterBin := buildNoopAdapter(t)
	loader := NewLoaderWithDiscovery(func(string) (string, error) {
		return adapterBin, nil
	})
	t.Cleanup(func() { _ = loader.Shutdown(context.Background()) })

	// RunnerFunc that simply starts the cmd directly, bypassing discovery.
	rf := func(_ hclog.Logger, cmd *exec.Cmd, socketDir string) (runner.Runner, error) {
		// go-plugin passes an empty cmd when RunnerFunc is used; we must set
		// the binary path ourselves.
		cmd.Path = adapterBin
		cmd.Args = []string{adapterBin}
		return newTestCmdRunner(hclog.NewNullLogger(), cmd, socketDir)
	}

	handle, err := loader.ResolveWithRunnerFunc(context.Background(), "noop", rf)
	if err != nil {
		t.Fatalf("resolve with runner func: %v", err)
	}
	if handle == nil {
		t.Fatal("expected non-nil handle")
	}

	info, err := handle.Info(context.Background())
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if info.Name != "noop" {
		t.Errorf("adapter name=%q want noop", info.Name)
	}
	handle.Kill()
}
