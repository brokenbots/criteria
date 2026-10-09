package remote

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// pendingRecorder collects the Shim's scope_session_pending signals (KB-232).
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

// waitForPendingRec polls the recorder until at least one pending signal has
// arrived or the deadline elapses: the signal fires on the waiting
// goroutine's wake timer, asynchronously relative to the test goroutine.
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

// startPendingWait runs WaitForFreshHandle for a per-scope session whose
// adapter never dials: the wait must fail at the context deadline, with the
// half-window signal emitted once while it waited. Returns the recorder, the
// captured logs, and the wait's error channel.
func startPendingWait(t *testing.T, shim *Shim, scope string, window time.Duration) (*pendingRecorder, *capturedLogs, chan error) {
	t.Helper()
	rec := &pendingRecorder{}
	shim.SetScopeSessionSink(rec)
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

// TestShim_SessionWaitEmitsScopeSessionPendingAtHalfWindow pins the KB-232
// observability contract for item 3: a per-scope session wait that crosses
// half its effective window without a completed handshake emits exactly one
// named scope_session_pending signal — shim Structured Warn plus the
// registered sink — instead of burning the remaining deadline silently.
func TestShim_SessionWaitEmitsScopeSessionPendingAtHalfWindow(t *testing.T) {
	shim, err := NewShim(&Config{ListenAddress: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatalf("NewShim: %v", err)
	}
	shim.SetPerScopeSessions(true)

	const window = 400 * time.Millisecond
	rec, logs, errCh := startPendingWait(t, shim, "kb232-run-handler/2e0c2b6e-6a92-4f31-9d0d-6a1e7712f001", window)

	got := waitForPendingRec(t, rec, 2*time.Second)
	pending := got[0]
	if pending.AdapterType != "shell" {
		t.Errorf("adapter_type: got %q want shell", pending.AdapterType)
	}
	if pending.Scope != "kb232-run-handler/2e0c2b6e-6a92-4f31-9d0d-6a1e7712f001" {
		t.Errorf("scope: got %q want the keyed scope", pending.Scope)
	}
	if pending.Dials != 0 {
		t.Errorf("dialed: got %d want 0 (no adapter dialed)", pending.Dials)
	}
	if pending.Rejections != 0 {
		t.Errorf("rejected: got %d want 0 (no identity rejections)", pending.Rejections)
	}
	if pending.Window <= 300*time.Millisecond || pending.Window > window {
		t.Errorf("window: got %v want ~%v (the context deadline)", pending.Window, window)
	}
	if pending.Waited < 150*time.Millisecond {
		t.Errorf("waited: got %v want at least half the window (~%v)", pending.Waited, window/2)
	}

	select {
	case err := <-errCh:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("wait after pending signal: want context deadline error, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("session wait did not return at its deadline")
	}

	if got := rec.snapshot(); len(got) != 1 {
		t.Fatalf("scope_session_pending emitted %d times, want exactly 1", len(got))
	}
	if !waitForLog(t, logs, "scope_session_pending", time.Second) {
		t.Fatal("shim must log the scope_session_pending warning")
	}
}

// TestShim_ScopeSessionPendingCarriesDialAndRejectionCounts pins the payload
// the half-window signal carries: the shim's per-key diagnostics counters —
// identity frames presented (a dial, even one later rejected) and identity
// rejections attributed to the scope while a session wait is pending — reach
// the event so an operator reads dialed=Y rejected=Z off the signal alone.
func TestShim_ScopeSessionPendingCarriesDialAndRejectionCounts(t *testing.T) {
	shim, err := NewShim(&Config{ListenAddress: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatalf("NewShim: %v", err)
	}
	shim.SetPerScopeSessions(true)

	const scope = "kb232-run-handler/11111111-2222-3333-4444-555555555555"
	rec, logs, errCh := startPendingWait(t, shim, scope, 400*time.Millisecond)

	// The pending wait registered its waiter channel before it can receive
	// attributed rejections; poll for it like the shim's own attribution does.
	key := shim.sessionKey("shell", scope)
	deadline := time.Now().Add(2 * time.Second)
	for {
		shim.mu.Lock()
		n := len(shim.waiters[key])
		shim.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("session wait never registered its waiter channel")
		}
		time.Sleep(5 * time.Millisecond)
	}

	for i := 0; i < 3; i++ {
		shim.noteDialActivity("shell", scope)
		shim.noteVerifyFailure([]string{"shell"}, scope, rejectScopeNotRegistered,
			&ScopeNotRegisteredError{AdapterType: "shell", Scope: scope})
	}

	got := waitForPendingRec(t, rec, 2*time.Second)
	if len(got) != 1 {
		t.Fatalf("scope_session_pending emitted %d times, want exactly 1", len(got))
	}
	if got[0].Dials != 3 {
		t.Errorf("dialed: got %d want 3", got[0].Dials)
	}
	if got[0].Rejections != 3 {
		t.Errorf("rejected: got %d want 3", got[0].Rejections)
	}
	if !waitForLog(t, logs, "rejected=3", time.Second) || !waitForLog(t, logs, "dialed=3", time.Second) {
		t.Fatal("shim warning must carry dialed/rejected counts")
	}

	select {
	case err := <-errCh:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("wait after pending signal: want context deadline error, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("session wait did not return at its deadline")
	}
	if got := rec.snapshot(); len(got) != 1 {
		t.Fatalf("scope_session_pending emitted %d times after the wait ended, want exactly 1", len(got))
	}
}
