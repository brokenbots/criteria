package engine

import (
	"context"
	"sync"
	"testing"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	v2 "github.com/brokenbots/criteria/sdk/pb/criteria/v2"
	"github.com/brokenbots/criteria/workflow"
)

// mockPauseResumeHandle implements Handle with pause/resume tracking.
type mockPauseResumeHandle struct {
	mu          sync.Mutex
	pauseCount  int
	resumeCount int
	inspectResp *v2.InspectResponse
	pauseErr    error
	resumeErr   error
}

func (m *mockPauseResumeHandle) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: "mock"}, nil
}
func (m *mockPauseResumeHandle) OpenSession(context.Context, string, map[string]string, map[string]string) error {
	return nil
}
func (m *mockPauseResumeHandle) Execute(context.Context, string, *workflow.StepNode, adapter.EventSink) (adapter.Result, error) {
	return adapter.Result{Outcome: "success"}, nil
}
func (m *mockPauseResumeHandle) CloseSession(context.Context, string) error { return nil }
func (m *mockPauseResumeHandle) Kill()                                      {}
func (m *mockPauseResumeHandle) Pause(context.Context, string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pauseCount++
	return m.pauseErr
}
func (m *mockPauseResumeHandle) Resume(context.Context, string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resumeCount++
	return m.resumeErr
}
func (m *mockPauseResumeHandle) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return m.inspectResp, nil
}
func (m *mockPauseResumeHandle) Snapshot(context.Context, string) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{}, nil
}
func (m *mockPauseResumeHandle) Restore(context.Context, string, []byte, uint32) error { return nil }

type mockPauseResumeLoader struct {
	handle adapterhost.Handle
}

func (l *mockPauseResumeLoader) Resolve(_ context.Context, _ string) (adapterhost.Handle, error) {
	return l.handle, nil
}
func (l *mockPauseResumeLoader) Shutdown(_ context.Context) error { return nil }

func openTestSession(t *testing.T, sm *adapterhost.SessionManager) {
	t.Helper()
	ctx := context.Background()
	if err := sm.Open(ctx, "s1", "test", "fail", nil, nil); err != nil {
		t.Fatalf("open session: %v", err)
	}
}

func TestEngine_Pause_NoActiveRun(t *testing.T) {
	e := &Engine{}
	if err := e.Pause(context.Background()); err == nil {
		t.Fatal("expected error when no active run")
	}
}

func TestEngine_Resume_NoActiveRun(t *testing.T) {
	e := &Engine{}
	if err := e.Resume(context.Background()); err == nil {
		t.Fatal("expected error when no active run")
	}
}

func TestEngine_InspectSession_NoActiveRun(t *testing.T) {
	e := &Engine{}
	if _, err := e.InspectSession(context.Background(), "s1"); err == nil {
		t.Fatal("expected error when no active run")
	}
}

func TestEngine_Pause_DelegatesToSessions(t *testing.T) {
	h := &mockPauseResumeHandle{}
	sm := adapterhost.NewSessionManager(&mockPauseResumeLoader{handle: h})
	openTestSession(t, sm)

	e := &Engine{}
	e.mu.Lock()
	e.liveSessions = sm
	e.mu.Unlock()

	if err := e.Pause(context.Background()); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if h.pauseCount != 1 {
		t.Fatalf("expected 1 pause call, got %d", h.pauseCount)
	}
}

func TestEngine_Resume_DelegatesToSessions(t *testing.T) {
	h := &mockPauseResumeHandle{}
	sm := adapterhost.NewSessionManager(&mockPauseResumeLoader{handle: h})
	openTestSession(t, sm)

	e := &Engine{}
	e.mu.Lock()
	e.liveSessions = sm
	e.mu.Unlock()

	// Resume on an active (non-paused) session is a no-op; pause first.
	if err := e.Pause(context.Background()); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := e.Resume(context.Background()); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if h.resumeCount != 1 {
		t.Fatalf("expected 1 resume call, got %d", h.resumeCount)
	}
}

func TestEngine_InspectSession_DelegatesToSessions(t *testing.T) {
	want := &v2.InspectResponse{CurrentStep: "step_1"}
	h := &mockPauseResumeHandle{inspectResp: want}
	sm := adapterhost.NewSessionManager(&mockPauseResumeLoader{handle: h})
	openTestSession(t, sm)

	e := &Engine{}
	e.mu.Lock()
	e.liveSessions = sm
	e.mu.Unlock()

	got, err := e.InspectSession(context.Background(), "s1")
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if got.CurrentStep != want.CurrentStep {
		t.Fatalf("inspect mismatch: got %q, want %q", got.CurrentStep, want.CurrentStep)
	}
}

func TestEngine_PauseResume_Reentrant(t *testing.T) {
	h := &mockPauseResumeHandle{}
	sm := adapterhost.NewSessionManager(&mockPauseResumeLoader{handle: h})
	openTestSession(t, sm)

	e := &Engine{}
	e.mu.Lock()
	e.liveSessions = sm
	e.mu.Unlock()

	if err := e.Pause(context.Background()); err != nil {
		t.Fatalf("pause 1: %v", err)
	}
	if err := e.Pause(context.Background()); err != nil {
		t.Fatalf("pause 2: %v", err)
	}
	if err := e.Resume(context.Background()); err != nil {
		t.Fatalf("resume 1: %v", err)
	}
	if err := e.Resume(context.Background()); err != nil {
		t.Fatalf("resume 2: %v", err)
	}

	// With session-level idempotency, each handle is called exactly once.
	if h.pauseCount != 1 || h.resumeCount != 1 {
		t.Fatalf("expected 1 pause and 1 resume (idempotent), got pause=%d resume=%d", h.pauseCount, h.resumeCount)
	}
}

func TestEngine_PauseResume_Concurrent(t *testing.T) {
	h := &mockPauseResumeHandle{}
	sm := adapterhost.NewSessionManager(&mockPauseResumeLoader{handle: h})
	openTestSession(t, sm)

	e := &Engine{}
	e.mu.Lock()
	e.liveSessions = sm
	e.mu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = e.Pause(context.Background())
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = e.Resume(context.Background())
		}()
	}
	wg.Wait()

	// With concurrent Pause/Resume interleaving, the counts may exceed 1.
	// This test exists to verify the race detector is clean.
	_ = h.pauseCount
	_ = h.resumeCount
}
