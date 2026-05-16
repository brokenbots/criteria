package adapterhost

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/brokenbots/criteria/internal/adapter"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
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
func (c *canceledCtxHandle) Permit(context.Context, string, string, bool, string) error { return nil }
func (c *canceledCtxHandle) CloseSession(context.Context, string) error                 { return nil }
func (c *canceledCtxHandle) Kill()                                                      {}

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
func (e *eofHandle) Permit(context.Context, string, string, bool, string) error { return nil }
func (e *eofHandle) CloseSession(context.Context, string) error                 { return nil }
func (e *eofHandle) Kill()                                                      {}

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
	lastExecuteReq *pb.ExecuteRequest
}

func (r *recordingClient) Info(_ context.Context, _ *pb.InfoRequest) (*pb.InfoResponse, error) {
	return &pb.InfoResponse{Name: "recording-stub"}, nil
}

func (r *recordingClient) OpenSession(_ context.Context, _ *pb.OpenSessionRequest) (*pb.OpenSessionResponse, error) {
	return &pb.OpenSessionResponse{}, nil
}

func (r *recordingClient) Execute(_ context.Context, req *pb.ExecuteRequest) (ExecuteEventReceiver, error) {
	r.lastExecuteReq = req
	outcome := "success"
	if len(req.AllowedOutcomes) > 0 {
		outcome = req.AllowedOutcomes[0]
	}
	return &immediateResultReceiver{outcome: outcome}, nil
}

func (r *recordingClient) Permit(_ context.Context, _ *pb.PermitRequest) (*pb.PermitResponse, error) {
	return &pb.PermitResponse{}, nil
}

func (r *recordingClient) CloseSession(_ context.Context, _ *pb.CloseSessionRequest) (*pb.CloseSessionResponse, error) {
	return &pb.CloseSessionResponse{}, nil
}

// immediateResultReceiver returns one ExecuteResult event then io.EOF.
type immediateResultReceiver struct {
	outcome string
	done    bool
}

func (r *immediateResultReceiver) Recv() (*pb.ExecuteEvent, error) {
	if r.done {
		return nil, io.EOF
	}
	r.done = true
	return &pb.ExecuteEvent{
		Event: &pb.ExecuteEvent_Result{
			Result: &pb.ExecuteResult{Outcome: r.outcome},
		},
	}, nil
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
