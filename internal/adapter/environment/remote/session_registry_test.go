// Scope session pending signal emission test suite (KB-232).
package remote

import (
	"context"
	"sync"
	"testing"
	"time"
)

// -----------------------------------------------------------------------------
// KB-232: Scope Session Pending Signal Emission
// -----------------------------------------------------------------------------

type pendingRecorder struct {
	mu  sync.Mutex
	got []*ScopeSessionPending
}

func (r *pendingRecorder) OnScopeSessionPending(p *ScopeSessionPending) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, p)
}

func (r *pendingRecorder) snapshot() []*ScopeSessionPending {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*ScopeSessionPending(nil), r.got...)
}

func waitForPendingRec(t *testing.T, rec *pendingRecorder, timeout time.Duration) []*ScopeSessionPending {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if got := rec.snapshot(); len(got) > 0 {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("no scope_session_pending signal within %s", timeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func startPendingWait(t *testing.T, shim *Shim, scope string, window time.Duration) (*pendingRecorder, *capturedLogs, chan error) {
	t.Helper()
	rec := &pendingRecorder{}
	shim.setScopeSessionSink(rec)
	logs := captureLogs(t)

	ctx, cancel := context.WithTimeout(context.Background(), window)
	t.Cleanup(cancel)
	errCh := make(chan error, 1)
	go func() {
		_, err := shim.WaitForFreshHandle(ctx, "shell", scope, nil)
		errCh <- err
	}()
	return rec, logs, errCh
}

func TestShim_SessionWaitEmitsScopeSessionPendingAtHalfWindow(t *testing.T) {
	shim, err := NewShim(&Config{ListenAddress: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatalf("NewShim: %v", err)
	}
	shim.SetPerScopeSessions(true)

	const window = 400 * time.Millisecond
	rec, logs, errCh := startPendingWait(t, shim, "kb232-run-handler/2e0c2b6e-6a92-4f31-9d0d-6a1e7712f001", window)

	got := waitForPendingRec(t, rec, 2*time.Second)
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 signal, got %d", len(got))
	}
	sig := got[0]
	if sig.AdapterType != "shell" {
		t.Errorf("signal adapter = %q, want shell", sig.AdapterType)
	}
	if sig.Scope != "kb232-run-handler/2e0c2b6e-6a92-4f31-9d0d-6a1e7712f001" {
		t.Errorf("signal scope = %q, want kb232-run-handler/2e0c2b6e-6a92-4f31-9d0d-6a1e7712f001", sig.Scope)
	}
	if sig.Window != window {
		t.Errorf("signal window = %v, want %v", sig.Window, window)
	}
	if sig.Waited < window/2 {
		t.Errorf("waited = %v; must be at least half the window (%v)", sig.Waited, window/2)
	}

	if err := <-errCh; err == nil {
		t.Fatal("expected wait to time out, got nil error")
	}

	if !waitForLog(t, logs, "scope session pending half-window elapsed without live connection", 2*time.Second) {
		t.Errorf("structured warning line missing from logs:\n%s", logs.String())
	}
}

func TestShim_SessionWaitWithStepDeadlineEmitsAtHalfStepWindow(t *testing.T) {
	shim, err := NewShim(&Config{
		ListenAddress:           "127.0.0.1:0",
		SessionSchedulingBudget: 30 * time.Minute,
	}, nil)
	if err != nil {
		t.Fatalf("NewShim: %v", err)
	}
	shim.SetPerScopeSessions(true)

	const stepDeadline = 400 * time.Millisecond
	rec, _, errCh := startPendingWait(t, shim, "kb232-run-handler/2e0c2b6e-6a92-4f31-9d0d-6a1e7712f002", stepDeadline)

	got := waitForPendingRec(t, rec, 2*time.Second)
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 signal, got %d", len(got))
	}
	sig := got[0]
	if sig.Window != stepDeadline {
		t.Errorf("signal window = %v, want step deadline window %v", sig.Window, stepDeadline)
	}
	if sig.Waited < stepDeadline/2 {
		t.Errorf("waited = %v; must be at least half the step deadline (%v)", sig.Waited, stepDeadline/2)
	}
	if sig.Waited > 2*stepDeadline {
		t.Errorf("waited = %v; should not have waited past scheduling budget", sig.Waited)
	}

	<-errCh
}

func TestShim_SessionWaitSignalDeliversPresentedAndRejectedCounts(t *testing.T) {
	shim, err := NewShim(&Config{
		ListenAddress: "127.0.0.1:0",
		AcceptToken:   "valid-token",
	}, nil)
	if err != nil {
		t.Fatalf("NewShim: %v", err)
	}
	shim.SetPerScopeSessions(true)
	const targetScope = "kb232-run-handler/2e0c2b6e-6a92-4f31-9d0d-6a1e7712f003"
	shim.RegisterScope(targetScope, "valid-token")

	key := shim.sessionKey("shell", targetScope)
	shim.dialCount[key] = 3
	shim.rejectCount[key] = 2

	const window = 400 * time.Millisecond
	rec, _, errCh := startPendingWait(t, shim, targetScope, window)

	got := waitForPendingRec(t, rec, 2*time.Second)
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 signal, got %d", len(got))
	}
	sig := got[0]
	if sig.Dials != 3 {
		t.Errorf("signal dials = %d, want 3", sig.Dials)
	}
	if sig.Rejections != 2 {
		t.Errorf("signal rejections = %d, want 2", sig.Rejections)
	}

	<-errCh
}
