// Remote shim session registry test suite.
// Unifies:
//   - KB-153: Post-kill registry cleanup & redial wait invariants (kb153_verify_kill_registry_test.go)
//   - KB-232: Live redial refusal, displacement on crash respawn, and placeholder reservation (kb232_live_redial_refusal_test.go)
//   - KB-232: Scope session pending signal emission and dial/rejection counts (kb232_scope_session_pending_test.go)
package remote

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapterhost"
)

// -----------------------------------------------------------------------------
// KB-153: Post-Kill Registry Cleanup & Re-Dial Invariants
// -----------------------------------------------------------------------------

// newKB153TestShim starts a legacy-mode shim with tight wait budgets so the
// tests finish inside their wall-clock deadlines while still exercising the
// budgeted-wait paths.
func newKB153TestShim(t *testing.T, waitBudget time.Duration) (shim *Shim, addr string) {
	t.Helper()
	verifier := &fixedDigestVerifier{allowed: map[string]string{"noop": "sha256:abcd1234"}}
	shim, err := NewShim(&Config{
		ListenAddress:           "127.0.0.1:0",
		AcceptToken:             "legacy-token",
		SessionHandshakeBudget:  waitBudget,
		SessionSchedulingBudget: waitBudget,
	}, verifier)
	if err != nil {
		t.Fatalf("NewShim: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	if err := shim.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = shim.Stop(context.Background()) })
	return shim, shim.listener.Addr().String()
}

func kb153Handshake() *handshakeMessage {
	return &handshakeMessage{Name: "noop", Version: "1.0.0", Digest: "sha256:abcd1234", Token: "legacy-token"}
}

func TestWaitForHandle_AfterVerifyStyleKill_WaitsForRedialNotDeadHandle(t *testing.T) {
	shim, addr := newKB153TestShim(t, 4*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for i := 0; i < 3; i++ {
		if err := dialFakeAdapter(addr, kb153Handshake(), nil); err != nil {
			t.Fatalf("iteration %d: dial shim: %v", i, err)
		}
		h1, err := shim.WaitForHandle(ctx, "noop", "")
		if err != nil {
			t.Fatalf("iteration %d: WaitForHandle: %v", i, err)
		}

		h1.Kill()

		key := shim.sessionKey("noop", "")
		shim.mu.Lock()
		_, stillRegistered := shim.sessions[key]
		shim.mu.Unlock()
		if stillRegistered {
			t.Errorf("iteration %d: torn-down verify handle is still registered immediately after Kill", i)
		}

		redialed := make(chan struct{})
		go func() {
			time.Sleep(250 * time.Millisecond)
			_ = dialFakeAdapter(addr, kb153Handshake(), nil)
			close(redialed)
		}()
		h2, err := shim.WaitForHandle(ctx, "noop", "")
		if err != nil {
			t.Fatalf("iteration %d: WaitForHandle after verify-style kill: %v", i, err)
		}
		if h2 == h1 {
			t.Errorf("iteration %d: fresh wait resolved the killed handle (KB-153 race)", i)
		}
		if _, err := h2.Info(ctx); err != nil {
			t.Errorf("iteration %d: bind resolved a dead handle: %v (KB-153 race: torn-down verify handle leaked to a fresh wait)", i, err)
		}
		h2.Kill()
		<-redialed
	}
}

func TestWaitForHandle_DeadAdapterAfterVerifyKill_FailsAtHandshakeBudget(t *testing.T) {
	shim, addr := newKB153TestShim(t, 400*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := dialFakeAdapter(addr, kb153Handshake(), nil); err != nil {
		t.Fatalf("dial shim: %v", err)
	}
	h1, err := shim.WaitForHandle(ctx, "noop", "")
	if err != nil {
		t.Fatalf("WaitForHandle: %v", err)
	}
	h1.Kill()

	start := time.Now()
	h2, err := shim.WaitForHandle(ctx, "noop", "")
	elapsed := time.Since(start)
	if err == nil {
		h2.Kill()
		t.Fatalf("wait returned a handle although no adapter ever re-dialed: the registry served the torn-down verify handle (KB-153 race)")
	}
	if strings.Contains(err.Error(), "connection is closing") {
		t.Errorf("wait failure must be the budgeted handshake diagnosis, got the torn-down transport error: %v", err)
	}
	if !strings.Contains(err.Error(), "identity handshake") {
		t.Errorf("error %q must name the bounded identity-handshake wait", err.Error())
	}
	if elapsed < 350*time.Millisecond {
		t.Errorf("wait returned after %v; the dead session must not fail a fresh wait instantly", elapsed)
	}
}

// -----------------------------------------------------------------------------
// KB-232: Live Redial Refusal, Crash Respawn Displacement, Placeholder Reservation
// -----------------------------------------------------------------------------

func kb232DialRefused(t *testing.T, shim *Shim, hs *handshakeMessage) bool {
	t.Helper()
	const window = 2 * time.Second
	conn := dialRawHandshake(t, shim.listener.Addr().String(), hs)
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(window)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	buf := make([]byte, 256)
	if _, readErr := conn.Read(buf); errors.Is(readErr, io.EOF) {
		return true
	}
	return false
}

func kb232SessionEntry(shim *Shim, key string) (*session, bool) {
	shim.mu.Lock()
	defer shim.mu.Unlock()
	sess, ok := shim.sessions[key]
	return sess, ok
}

func kb232SessionRetired(shim *Shim, key string, h1 adapterhost.Handle) bool {
	shim.mu.Lock()
	defer shim.mu.Unlock()
	sess, ok := shim.sessions[key]
	return ok && sess.handle == h1 && sess.retired
}

const kb232Scope = "root/scope-1"

func newKB232TestShim(t *testing.T) (shim *Shim, hs *handshakeMessage, key string) {
	t.Helper()
	verifier := &fixedDigestVerifier{allowed: map[string]string{"noop": "sha256:abcd1234"}}
	shim, err := NewShim(&Config{ListenAddress: "127.0.0.1:0"}, verifier)
	if err != nil {
		t.Fatalf("NewShim: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	if err := shim.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = shim.Stop(context.Background()) })
	shim.SetPerScopeSessions(true)
	const scope = "root/scope-1"
	const token = "scope-1-token"
	shim.RegisterScope(scope, token)
	key = shim.sessionKey("noop", scope)
	hs = &handshakeMessage{Name: "noop", Version: "1.0.0", Digest: "sha256:abcd1234", Scope: scope, Token: token}
	return shim, hs, key
}

func TestShim_RefusesRedialOfLiveSessionBridge(t *testing.T) {
	shim, hs, key := newKB232TestShim(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := dialFakeAdapter(shim.listener.Addr().String(), hs, nil); err != nil {
		t.Fatalf("first dial: %v", err)
	}
	h1, err := shim.WaitForHandle(ctx, "noop", kb232Scope)
	if err != nil {
		t.Fatalf("WaitForHandle: %v", err)
	}
	if _, err := h1.Info(ctx); err != nil {
		t.Fatalf("first bridge not live after its dial: %v", err)
	}
	if first, ok := kb232SessionEntry(shim, key); !ok || first.handle != h1 {
		t.Fatal("established session missing from the shim registry after the first dial")
	}

	if !kb232DialRefused(t, shim, hs) {
		t.Fatal("re-dial of the live session was accepted; want refusal (KB-232)")
	}

	if after, ok := kb232SessionEntry(shim, key); !ok || after.handle != h1 {
		t.Fatal("refused re-dial displaced or removed the live session bridge (KB-232)")
	}
	if _, err := h1.Info(ctx); err != nil {
		t.Fatalf("live bridge stopped serving after a refused re-dial: %v", err)
	}
}

func TestShim_WaitForFreshHandleArmsRetiredAndDisplacesOnNextDial(t *testing.T) {
	shim, hs, key := newKB232TestShim(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := dialFakeAdapter(shim.listener.Addr().String(), hs, nil); err != nil {
		t.Fatalf("first dial: %v", err)
	}
	h1, err := shim.WaitForHandle(ctx, "noop", kb232Scope)
	if err != nil {
		t.Fatalf("WaitForHandle: %v", err)
	}

	fresh := make(chan adapterhost.Handle, 1)
	errCh := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h2, err := shim.WaitForFreshHandle(ctx, "noop", kb232Scope, h1)
		if err != nil {
			errCh <- err
			return
		}
		fresh <- h2
	}()

	deadline := time.Now().Add(2 * time.Second)
	armed := false
	for time.Now().Before(deadline) {
		select {
		case <-done:
			t.Fatal("WaitForFreshHandle resolved before a fresh dial arrived")
		default:
		}
		if kb232SessionRetired(shim, key, h1) {
			armed = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !armed {
		t.Fatal("WaitForFreshHandle did not arm the retired displacement after the stale re-wait")
	}

	if err := dialFakeAdapter(shim.listener.Addr().String(), hs, nil); err != nil {
		t.Fatalf("respawn dial: %v", err)
	}
	var h2 adapterhost.Handle
	select {
	case h2 = <-fresh:
	case err := <-errCh:
		t.Fatalf("WaitForFreshHandle after the respawn dial: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("re-dial against a retired (respawned) session did not displace and resolve the wait; want displacement")
	}
	if h2 == h1 {
		t.Fatal("respawn re-wait resolved the crashed handle instead of the displaced bridge")
	}
	if _, err := h2.Info(ctx); err != nil {
		t.Fatalf("displaced bridge not live after respawn dial: %v", err)
	}
	sess, ok := kb232SessionEntry(shim, key)
	if !ok || sess.handle != h2 {
		t.Fatal("shim registry did not hold the displaced bridge after the respawn dial")
	}
	if sess.retired {
		t.Fatal("displaced session inherited the retired flag; the takeover must clear it")
	}
}

func TestShim_FreshHandleWaiterGetsCurrentHandleAndDoesNotArm(t *testing.T) {
	shim, hs, key := newKB232TestShim(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := dialFakeAdapter(shim.listener.Addr().String(), hs, nil); err != nil {
		t.Fatalf("first dial: %v", err)
	}
	h1, err := shim.WaitForHandle(ctx, "noop", kb232Scope)
	if err != nil {
		t.Fatalf("WaitForHandle: %v", err)
	}

	h2, err := shim.WaitForFreshHandle(ctx, "noop", kb232Scope, nil)
	if err != nil {
		t.Fatalf("WaitForFreshHandle(nil): %v", err)
	}
	if h2 != h1 {
		t.Fatal("stale-less wait returned a different handle than the live bridge")
	}
	sess, ok := kb232SessionEntry(shim, key)
	if !ok {
		t.Fatal("live session missing after the stale-less wait")
	}
	if sess.retired {
		t.Fatal("stale-less wait armed displaced-on-next-dial; only the crash-respawn re-wait may")
	}

	if !kb232DialRefused(t, shim, hs) {
		t.Fatal("re-dial was accepted although no respawn re-wait armed displacement")
	}

	h3, err := shim.WaitForFreshHandle(ctx, "noop", kb232Scope, nil)
	if err != nil {
		t.Fatalf("WaitForFreshHandle after refusal: %v", err)
	}
	if h3 != h1 {
		t.Fatal("refusal removed the live session the bind phase must still resolve")
	}
}

func TestShim_ReserveScopeSessionRefusesSiblingsUntilReleased(t *testing.T) {
	shim, hs, key := newKB232TestShim(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ph, err := shim.reserveScopeSession("noop", kb232Scope)
	if err != nil {
		t.Fatalf("reserveScopeSession: %v", err)
	}
	if ph == nil {
		t.Fatal("reserveScopeSession returned no placeholder for a per-scope dial")
	}
	if _, ok := kb232SessionEntry(shim, key); !ok {
		t.Fatal("placeholder reservation missing from the session registry")
	}

	if !kb232DialRefused(t, shim, hs) {
		t.Fatal("sibling dial was accepted while a placeholder reservation held the scope")
	}

	if ph2, rerr := shim.reserveScopeSession("noop", kb232Scope); rerr == nil {
		t.Fatal("second reserve claimed the scope while a flight already held it")
	} else if ph2 != nil {
		t.Fatal("second reserve returned a placeholder")
	}

	resolve := make(chan adapterhost.Handle, 1)
	go func() {
		h, werr := shim.WaitForFreshHandle(ctx, "noop", kb232Scope, nil)
		if werr == nil {
			resolve <- h
		}
	}()
	time.Sleep(50 * time.Millisecond)
	select {
	case h := <-resolve:
		if h == nil {
			t.Fatal("wait resolved a nil placeholder handle")
		}
		t.Fatal("WaitForFreshHandle resolved during establishing without a real session")
	default:
	}

	shim.releaseScopeSession("noop", kb232Scope, ph)
	if _, ok := kb232SessionEntry(shim, key); ok {
		t.Fatal("released placeholder still holds the session slot")
	}
	if err := dialFakeAdapter(shim.listener.Addr().String(), hs, nil); err != nil {
		t.Fatalf("post-release dial: %v", err)
	}
	select {
	case h := <-resolve:
		if _, err := h.Info(ctx); err != nil {
			t.Fatalf("post-release bridge not live: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter registered during establishing did not resolve after the session stored")
	}
	sess, ok := kb232SessionEntry(shim, key)
	if !ok || sess.handle == nil || sess.establishing {
		t.Fatal("stored session is not a real established entry after the placeholder release")
	}
}

func TestShim_ConcurrentSameScopeDialsEstablishExactlyOneSession(t *testing.T) {
	shim, hs, key := newKB232TestShim(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			if err := dialFakeAdapter(shim.listener.Addr().String(), hs, nil); err != nil {
				t.Errorf("storm dial: %v", err)
			}
		}()
	}
	close(start)

	deadline := time.Now().Add(5 * time.Second)
	for {
		sess, ok := kb232SessionEntry(shim, key)
		if ok && sess.handle != nil && !sess.establishing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no same-scope session established from the concurrent dials")
		}
		time.Sleep(10 * time.Millisecond)
	}

	sess, ok := kb232SessionEntry(shim, key)
	if !ok || sess.handle == nil || sess.establishing {
		t.Fatal("storm winner is not a real established session")
	}
	if _, err := sess.handle.Info(ctx); err != nil {
		t.Fatalf("winning bridge not live after the storm: %v", err)
	}

	if !kb232DialRefused(t, shim, hs) {
		t.Fatal("follow-up re-dial after the storm was accepted; want refusal")
	}
	if again, _ := kb232SessionEntry(shim, key); again.handle != sess.handle {
		t.Fatal("follow-up re-dial displaced the winning bridge")
	}
	if _, err := sess.handle.Info(ctx); err != nil {
		t.Fatalf("winning bridge stopped serving after the follow-up refusal: %v", err)
	}
}

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

func TestShim_ScopeSessionPendingCarriesDialAndRejectionCounts(t *testing.T) {
	shim, err := NewShim(&Config{ListenAddress: "127.0.0.1:0"}, nil)
	if err != nil {
		t.Fatalf("NewShim: %v", err)
	}
	shim.SetPerScopeSessions(true)

	const scope = "kb232-run-handler/11111111-2222-3333-4444-555555555555"
	rec, logs, errCh := startPendingWait(t, shim, scope, 400*time.Millisecond)

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
