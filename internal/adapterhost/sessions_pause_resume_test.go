package adapterhost

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/workflow"
	v2 "github.com/brokenbots/criteria/sdk/pb/criteria/v2"
)

// pauseResumeMockHandle tracks Pause/Resume/Inspect calls.
type pauseResumeMockHandle struct {
	mu            sync.Mutex
	pauseCount    int
	resumeCount   int
	inspectResp   *v2.InspectResponse
	inspectErr    error
	pauseErr      error
	resumeErr     error
}

func (m *pauseResumeMockHandle) Info(context.Context) (Info, error) {
	return Info{Name: "mock"}, nil
}
func (m *pauseResumeMockHandle) OpenSession(context.Context, string, map[string]string, map[string]string) error {
	return nil
}
func (m *pauseResumeMockHandle) Execute(ctx context.Context, sessionID string, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	return adapter.Result{}, nil
}
func (m *pauseResumeMockHandle) CloseSession(context.Context, string) error { return nil }
func (m *pauseResumeMockHandle) Kill()                                      {}
func (m *pauseResumeMockHandle) Pause(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pauseCount++
	return m.pauseErr
}
func (m *pauseResumeMockHandle) Resume(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resumeCount++
	return m.resumeErr
}
func (m *pauseResumeMockHandle) Inspect(ctx context.Context, id string) (*v2.InspectResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.inspectResp, m.inspectErr
}

func TestSession_Pause_ResumesPermissionState(t *testing.T) {
	h := &pauseResumeMockHandle{}
	ps := NewPermissionState("s1", nil)
	ps.SetStreamCancel(func() {})
	ps.SetPolicy(denyAllPolicy{})
	ps.active = true

	sess := &Session{Name: "s1", handle: h, PermissionState: ps}

	if err := sess.Pause(context.Background()); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if h.pauseCount != 1 {
		t.Fatalf("expected 1 pause call, got %d", h.pauseCount)
	}
	ps.mu.Lock()
	if ps.active {
		t.Fatal("expected permission state to be inactive after pause")
	}
	ps.mu.Unlock()
}

func TestSession_Resume_ResumesPermissionState(t *testing.T) {
	h := &pauseResumeMockHandle{}
	ps := NewPermissionState("s1", nil)
	ps.SetStreamCancel(func() {})
	ps.SetPolicy(denyAllPolicy{})
	ps.active = false

	sess := &Session{Name: "s1", handle: h, PermissionState: ps}

	if err := sess.Resume(context.Background()); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if h.resumeCount != 1 {
		t.Fatalf("expected 1 resume call, got %d", h.resumeCount)
	}
	ps.mu.Lock()
	if !ps.active {
		t.Fatal("expected permission state to be active after resume")
	}
	ps.mu.Unlock()
}

func TestSession_Inspect_DelegatesToHandle(t *testing.T) {
	want := &v2.InspectResponse{CurrentStep: "generate_outline"}
	h := &pauseResumeMockHandle{inspectResp: want}
	sess := &Session{Name: "s1", handle: h}

	got, err := sess.Inspect(context.Background())
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if got.CurrentStep != want.CurrentStep {
		t.Fatalf("inspect response mismatch: got %q, want %q", got.CurrentStep, want.CurrentStep)
	}
}

func TestSessionManager_PauseAll_ResumesAllSessions(t *testing.T) {
	sm := NewSessionManager(nil)
	h1 := &pauseResumeMockHandle{}
	h2 := &pauseResumeMockHandle{}
	ps1 := NewPermissionState("s1", nil)
	ps1.SetStreamCancel(func() {})
	ps1.active = true
	ps2 := NewPermissionState("s2", nil)
	ps2.SetStreamCancel(func() {})
	ps2.active = true

	sm.sessions["s1"] = &Session{Name: "s1", handle: h1, PermissionState: ps1}
	sm.sessions["s2"] = &Session{Name: "s2", handle: h2, PermissionState: ps2}

	if err := sm.PauseAll(context.Background()); err != nil {
		t.Fatalf("pause all: %v", err)
	}
	if h1.pauseCount != 1 || h2.pauseCount != 1 {
		t.Fatalf("expected 1 pause each, got h1=%d h2=%d", h1.pauseCount, h2.pauseCount)
	}
}

func TestSessionManager_ResumeAll_ResumesAllSessions(t *testing.T) {
	sm := NewSessionManager(nil)
	h1 := &pauseResumeMockHandle{}
	h2 := &pauseResumeMockHandle{}
	ps1 := NewPermissionState("s1", nil)
	ps1.SetStreamCancel(func() {})
	ps1.active = false
	ps2 := NewPermissionState("s2", nil)
	ps2.SetStreamCancel(func() {})
	ps2.active = false

	sm.sessions["s1"] = &Session{Name: "s1", handle: h1, PermissionState: ps1}
	sm.sessions["s2"] = &Session{Name: "s2", handle: h2, PermissionState: ps2}

	if err := sm.ResumeAll(context.Background()); err != nil {
		t.Fatalf("resume all: %v", err)
	}
	if h1.resumeCount != 1 || h2.resumeCount != 1 {
		t.Fatalf("expected 1 resume each, got h1=%d h2=%d", h1.resumeCount, h2.resumeCount)
	}
}

func TestSessionManager_PauseAll_Idempotent(t *testing.T) {
	sm := NewSessionManager(nil)
	h := &pauseResumeMockHandle{}
	ps := NewPermissionState("s1", nil)
	ps.SetStreamCancel(func() {})
	ps.active = true

	sm.sessions["s1"] = &Session{Name: "s1", handle: h, PermissionState: ps}

	_ = sm.PauseAll(context.Background())
	_ = sm.PauseAll(context.Background())

	if h.pauseCount != 2 {
		t.Fatalf("expected 2 pause calls (idempotent at handle level), got %d", h.pauseCount)
	}
}

func TestSessionManager_ResumeAll_Idempotent(t *testing.T) {
	sm := NewSessionManager(nil)
	h := &pauseResumeMockHandle{}
	ps := NewPermissionState("s1", nil)
	ps.SetStreamCancel(func() {})
	ps.active = false

	sm.sessions["s1"] = &Session{Name: "s1", handle: h, PermissionState: ps}

	_ = sm.ResumeAll(context.Background())
	_ = sm.ResumeAll(context.Background())

	if h.resumeCount != 2 {
		t.Fatalf("expected 2 resume calls (idempotent at handle level), got %d", h.resumeCount)
	}
}

func TestSessionManager_InspectSession_ReturnsHandleResponse(t *testing.T) {
	sm := NewSessionManager(nil)
	want := &v2.InspectResponse{CurrentStep: "step_1"}
	h := &pauseResumeMockHandle{inspectResp: want}
	sm.sessions["s1"] = &Session{Name: "s1", handle: h}

	got, err := sm.InspectSession(context.Background(), "s1")
	if err != nil {
		t.Fatalf("inspect session: %v", err)
	}
	if got.CurrentStep != want.CurrentStep {
		t.Fatalf("inspect mismatch: got %q, want %q", got.CurrentStep, want.CurrentStep)
	}
}

func TestSessionManager_PauseAll_FirstErrorReturned(t *testing.T) {
	sm := NewSessionManager(nil)
	wantErr := errors.New("boom")
	h1 := &pauseResumeMockHandle{pauseErr: wantErr}
	h2 := &pauseResumeMockHandle{}
	ps1 := NewPermissionState("s1", nil)
	ps1.SetStreamCancel(func() {})
	ps2 := NewPermissionState("s2", nil)
	ps2.SetStreamCancel(func() {})

	sm.sessions["s1"] = &Session{Name: "s1", handle: h1, PermissionState: ps1}
	sm.sessions["s2"] = &Session{Name: "s2", handle: h2, PermissionState: ps2}

	err := sm.PauseAll(context.Background())
	if err != wantErr {
		t.Fatalf("expected %v, got %v", wantErr, err)
	}
	if h2.pauseCount != 1 {
		t.Fatalf("expected h2 pause to still be called despite h1 error, got %d", h2.pauseCount)
	}
}

func TestSessionManager_ConcurrentPauseResume(t *testing.T) {
	sm := NewSessionManager(nil)
	h := &pauseResumeMockHandle{}
	ps := NewPermissionState("s1", nil)
	ps.SetStreamCancel(func() {})

	sm.sessions["s1"] = &Session{Name: "s1", handle: h, PermissionState: ps}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = sm.PauseAll(context.Background())
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = sm.ResumeAll(context.Background())
		}()
	}
	wg.Wait()

	if h.pauseCount != 10 {
		t.Fatalf("expected 10 pause calls, got %d", h.pauseCount)
	}
	if h.resumeCount != 10 {
		t.Fatalf("expected 10 resume calls, got %d", h.resumeCount)
	}
}
