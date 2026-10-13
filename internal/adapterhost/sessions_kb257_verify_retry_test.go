package adapterhost

// sessions_kb257_verify_retry_test.go — the KB-257 fresh-line retry lane for
// the phase-1 verify handshake. A transport-class loss of the pod's single
// accepted dial while Verify's Info is in flight (gRPC Canceled /
// Unavailable, the legacy "grpc: the client connection is closing" text)
// used to fail the whole subworkflow body init atomically even though the
// pod re-dials within seconds. The lane re-waits for the re-dial through the
// shim's WaitForFreshHandle under the existing KB-70 session-wait budgets;
// semantic failures, host teardown, and adapters without a remote lane stay
// fail-fast; budget expiry fails with a typed reason. These tests pin the
// lane at the sessions level: classification, convergence, abandon
// semantics, fail-fast shapes, and the typed exhaustion reason.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const kb257DeveloperInstance = "noop.developer"

// newKB257VerifyManager pins the wiring every KB-257 Verify test shares: the
// compiled CRI-269 graph registers the subworkflow adapter in the
// per-instance cache as deferred (VerifyGraph handshakes nothing and stores
// no verified record), so the direct Verify calls below dispatch through the
// shim exactly like scope-entry phase B does. shim may be nil for the
// no-remote-lane case.
func newKB257VerifyManager(t *testing.T, loader Loader, shim RemoteShim) *SessionManager {
	t.Helper()
	ctx := context.Background()
	g, _ := compileCRI269SessionsGraph(t, true)
	m := NewSessionManager(loader)
	m.SetGraph(g)
	if shim != nil {
		m.SetRemoteShim(shim)
	}
	m.SetDeferredRemoteAdapters([]string{kb257DeveloperInstance})
	if err := m.VerifyGraph(ctx, g, nil); err != nil {
		t.Fatalf("VerifyGraph: %v", err)
	}
	return m
}

// kb257VerifyHandle is a verify handle whose Info calls consume a scripted
// outcome queue (the last outcome repeats) and which counts Kill calls.
type kb257VerifyHandle struct {
	cri269Handle
	mu       sync.Mutex
	outcomes []error
	kills    int
}

func newKB257VerifyHandle(outcomes ...error) *kb257VerifyHandle {
	return &kb257VerifyHandle{outcomes: outcomes}
}

func (h *kb257VerifyHandle) Info(context.Context) (Info, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.outcomes) == 0 {
		return Info{}, nil
	}
	err := h.outcomes[0]
	if len(h.outcomes) > 1 {
		h.outcomes = h.outcomes[1:]
	}
	return Info{}, err
}

func (h *kb257VerifyHandle) Kill() {
	h.mu.Lock()
	h.kills++
	h.mu.Unlock()
}

func (h *kb257VerifyHandle) killCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.kills
}

// kb257VerifyShim hands verifyAdapterInfo a scripted initial handle and a
// scripted fresh-handle sequence per adapter, recording every fresh-handle
// wait (adapter, scope, stale handle).
type kb257VerifyShim struct {
	initial map[string]Handle
	fresh   map[string][]kb257FreshResult

	mu         sync.Mutex
	freshCalls []kb257FreshCall
}

// kb257FreshResult is one scripted WaitForFreshHandle result.
type kb257FreshResult struct {
	handle Handle
	err    error
}

// kb257FreshCall records one WaitForFreshHandle invocation.
type kb257FreshCall struct {
	adapter string
	scope   string
	stale   Handle
}

func newKB257VerifyShim(initial map[string]Handle, freshResults map[string][]kb257FreshResult) *kb257VerifyShim {
	return &kb257VerifyShim{initial: initial, fresh: freshResults}
}

func (s *kb257VerifyShim) WaitForHandle(_ context.Context, adapter, _ string) (Handle, error) {
	return s.initial[adapter], nil
}

func (s *kb257VerifyShim) WaitForFreshHandle(_ context.Context, adapter, scope string, stale Handle) (Handle, error) {
	s.mu.Lock()
	s.freshCalls = append(s.freshCalls, kb257FreshCall{adapter: adapter, scope: scope, stale: stale})
	results := s.fresh[adapter]
	s.mu.Unlock()
	if len(results) == 0 {
		return nil, nil
	}
	res := results[0]
	if len(results) > 1 {
		s.mu.Lock()
		s.fresh[adapter] = s.fresh[adapter][1:]
		s.mu.Unlock()
	}
	return res.handle, res.err
}

func (s *kb257VerifyShim) freshCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.freshCalls)
}

func (s *kb257VerifyShim) lastFreshCall() kb257FreshCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.freshCalls[len(s.freshCalls)-1]
}

func (s *kb257VerifyShim) RegisterScope(string, string)                      {}
func (s *kb257VerifyShim) UnregisterScope(string)                            {}
func (s *kb257VerifyShim) CloseHandle(context.Context, string, string) error { return nil }
func (s *kb257VerifyShim) ListenAddr() string                                { return "127.0.0.1:1" }
func (s *kb257VerifyShim) Stop(context.Context) error                        { return nil }

// kb257PhoneHomeHandle carries the phone-home bridge marker: the abandon
// guard must leave such handles for their owners (the shim tears dead
// sessions down itself).
type kb257PhoneHomeHandle struct {
	*kb257VerifyHandle
}

func (h *kb257PhoneHomeHandle) IsPhoneHomeBridge() {}

// kb257LocalLoader resolves local (non-remote) handles so the no-lane
// fail-fast shape can be tested without a shim: the scripted handle is
// returned for the target adapter type, a healthy stub for every other name.
type kb257LocalLoader struct {
	mu       sync.Mutex
	resolved []string
	target   string
	handle   Handle
	fallback Handle
}

func (l *kb257LocalLoader) Resolve(_ context.Context, name string) (Handle, error) {
	l.mu.Lock()
	l.resolved = append(l.resolved, name)
	l.mu.Unlock()
	if name == l.target {
		return l.handle, nil
	}
	return l.fallback, nil
}

func (l *kb257LocalLoader) Shutdown(context.Context) error { return nil }

func (l *kb257LocalLoader) resolutionsOf(name string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, resolved := range l.resolved {
		if resolved == name {
			n++
		}
	}
	return n
}

func kb257CanceledErr() error {
	return status.Error(codes.Canceled, "grpc: the client connection is closing")
}

// TestSessions_VerifyHandshakeTransportLostClassifiesTransportErrors pins the
// lane's classification: gRPC Canceled/Unavailable and the legacy
// "grpc: the client connection is closing" text are transport-class losses;
// semantic handshake failures and arbitrary errors are not.
func TestSessions_VerifyHandshakeTransportLostClassifiesTransportErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"canceled connection closing", status.Error(codes.Canceled, "grpc: the client connection is closing"), true},
		{"unavailable EOF", status.Error(codes.Unavailable, "EOF"), true},
		{"unavailable transport draining", status.Error(codes.Unavailable, "transport is draining"), true},
		{"legacy text without status", errors.New("rpc error: code = Canceled desc = grpc: the client connection is closing"), true},
		{"plain connection closing text", errors.New("the client connection is closing"), true},
		{"permission denied", status.Error(codes.PermissionDenied, "accept_token verification failed"), false},
		{"internal", status.Error(codes.Internal, "digest mismatch"), false},
		{"arbitrary", errors.New("boom"), false},
		{"digest mismatch", errors.New("adapter digest mismatch: sha256:abcd1234"), false},
	}
	for _, tc := range cases {
		if got := verifyHandshakeTransportLost(tc.err); got != tc.want {
			t.Errorf("%s: verifyHandshakeTransportLost(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
	if verifyHandshakeTransportLost(nil) {
		t.Error("verifyHandshakeTransportLost(nil) = true, want false")
	}
}

// TestSessions_VerifyRetryLaneConvergesOnFreshPhoneHomeLine is the lane's
// happy path, in the shape it serves in production: the first phone-home
// verify line dies mid-Info with Canceled, the shim hands back a fresh line,
// and Verify completes on it. The dead line must be left for the shim's
// teardown (phone-home bridge handles are never killed) and the adopted
// fresh line must survive the exit un-killed too.
func TestSessions_VerifyRetryLaneConvergesOnFreshPhoneHomeLine(t *testing.T) {
	ctx := context.Background()
	developer := kb257DeveloperInstance

	dead := &kb257PhoneHomeHandle{newKB257VerifyHandle(kb257CanceledErr())}
	fresh := &kb257PhoneHomeHandle{newKB257VerifyHandle()}
	shim := newKB257VerifyShim(
		map[string]Handle{developer: dead},
		map[string][]kb257FreshResult{developer: {{handle: fresh}}},
	)

	m := newKB257VerifyManager(t, &cri269Loader{}, shim)

	if err := m.Verify(ctx, developer, "noop", "fail", nil, nil, nil, "", "subflow", "run-1/inst-1"); err != nil {
		t.Fatalf("Verify with a killed first line must converge on the fresh line: %v", err)
	}

	if got := shim.freshCallCount(); got != 1 {
		t.Fatalf("WaitForFreshHandle call count = %d, want 1", got)
	}
	call := shim.lastFreshCall()
	if call.adapter != developer || call.scope != "run-1/inst-1" {
		t.Fatalf("WaitForFreshHandle called with adapter=%q scope=%q; want %q/%q", call.adapter, call.scope, developer, "run-1/inst-1")
	}
	if call.stale != Handle(dead) {
		t.Fatal("WaitForFreshHandle must receive the current (stale) handle, not a fresh or nil handle")
	}
	if dead.killCount() != 0 {
		t.Fatalf("dead phone-home line was killed %d time(s); the lane must leave dead bridge lines for the shim's teardown", dead.killCount())
	}
	if fresh.killCount() != 0 {
		t.Fatalf("adopted fresh line was killed %d time(s); verify must leave phone-home bridge handles running", fresh.killCount())
	}
}

// TestSessions_VerifyRetryLaneKillsPlainDeadLines pins the abandon semantics
// for plain throwaway handles: a dead plain line is killed when the lane
// moves on to the fresh line, and the adopted fresh line is killed at exit.
func TestSessions_VerifyRetryLaneKillsPlainDeadLines(t *testing.T) {
	ctx := context.Background()
	developer := kb257DeveloperInstance

	dead := newKB257VerifyHandle(kb257CanceledErr())
	fresh := newKB257VerifyHandle()
	shim := newKB257VerifyShim(
		map[string]Handle{developer: dead},
		map[string][]kb257FreshResult{developer: {{handle: fresh}}},
	)

	m := newKB257VerifyManager(t, &cri269Loader{}, shim)

	if err := m.Verify(ctx, developer, "noop", "fail", nil, nil, nil, "", "subflow", "run-1/inst-1"); err != nil {
		t.Fatalf("Verify with a killed first line must converge on the fresh line: %v", err)
	}
	if dead.killCount() != 1 {
		t.Fatalf("dead plain verify handle killed %d time(s), want exactly 1 (mid-loop abandon)", dead.killCount())
	}
	if fresh.killCount() != 1 {
		t.Fatalf("adopted plain verify handle killed %d time(s), want exactly 1 (exit cleanup)", fresh.killCount())
	}
}

// TestSessions_VerifyRetryLaneLoopsAcrossRepeatedDeaths covers the churn
// cycle: two consecutive lines die mid-Info before the third converges. Each
// dead line is abandoned (killed for plain handles) and each wait receives
// the then-current stale handle.
func TestSessions_VerifyRetryLaneLoopsAcrossRepeatedDeaths(t *testing.T) {
	ctx := context.Background()
	developer := kb257DeveloperInstance

	dead1 := newKB257VerifyHandle(kb257CanceledErr())
	dead2 := newKB257VerifyHandle(status.Error(codes.Unavailable, "EOF"))
	alive := newKB257VerifyHandle()
	shim := newKB257VerifyShim(
		map[string]Handle{developer: dead1},
		map[string][]kb257FreshResult{developer: {{handle: dead2}, {handle: alive}}},
	)

	m := newKB257VerifyManager(t, &cri269Loader{}, shim)

	if err := m.Verify(ctx, developer, "noop", "fail", nil, nil, nil, "", "subflow", "run-1/inst-1"); err != nil {
		t.Fatalf("Verify must converge on the second fresh line: %v", err)
	}
	if got := shim.freshCallCount(); got != 2 {
		t.Fatalf("WaitForFreshHandle call count = %d, want 2 (one per killed line)", got)
	}
	if dead1.killCount() != 1 || dead2.killCount() != 1 || alive.killCount() != 1 {
		t.Fatalf("kill counts dead1=%d dead2=%d alive=%d; want each abandoned dead line killed once and the adopted line killed at exit",
			dead1.killCount(), dead2.killCount(), alive.killCount())
	}
}

// TestSessions_VerifyRetryLaneFailFastOnSemanticHandshakeError pins the
// fail-fast half of the contract in its exact historical shape: a semantic
// handshake failure (token/digest/config class) must never enter the lane —
// no fresh-line wait, and the error is the plain handshake wrap.
func TestSessions_VerifyRetryLaneFailFastOnSemanticHandshakeError(t *testing.T) {
	ctx := context.Background()
	developer := kb257DeveloperInstance

	semantic := status.Error(codes.PermissionDenied, "accept_token verification failed")
	dead := newKB257VerifyHandle(semantic)
	shim := newKB257VerifyShim(
		map[string]Handle{developer: dead},
		map[string][]kb257FreshResult{developer: nil},
	)

	m := newKB257VerifyManager(t, &cri269Loader{}, shim)

	err := m.Verify(ctx, developer, "noop", "fail", nil, nil, nil, "", "subflow", "run-1/inst-1")
	if err == nil {
		t.Fatal("semantic handshake failure must fail Verify")
	}
	want := `adapter "noop" handshake: rpc error: code = PermissionDenied desc = accept_token verification failed`
	if err.Error() != want {
		t.Fatalf("fail-fast error shape changed: got %q, want %q", err.Error(), want)
	}
	if got := shim.freshCallCount(); got != 0 {
		t.Fatalf("semantic handshake failure entered the retry lane (%d WaitForFreshHandle calls); it must stay fail-fast", got)
	}
	if dead.killCount() != 1 {
		t.Fatalf("fail-fast path killed the throwaway handle %d time(s), want 1", dead.killCount())
	}
}

// TestSessions_VerifyRetryLaneFailFastWithoutRemoteLane pins that a
// transport loss on a NON-remote adapter (no shim serves it) stays fail-fast:
// there is no re-dial lane for a local handle, so the historical shape and
// behavior are unchanged.
func TestSessions_VerifyRetryLaneFailFastWithoutRemoteLane(t *testing.T) {
	ctx := context.Background()
	developer := kb257DeveloperInstance

	dead := newKB257VerifyHandle(kb257CanceledErr())
	loader := &kb257LocalLoader{target: "noop", handle: dead, fallback: &cri269Handle{}}
	// No shim registered at all: resolution falls through to the local
	// loader, whose stub dies with the transport-class error.

	m := newKB257VerifyManager(t, loader, nil)

	err := m.Verify(ctx, developer, "noop", "fail", nil, nil, nil, "", "subflow", "run-1/inst-1")
	if err == nil {
		t.Fatal("transport loss without a remote lane must fail Verify")
	}
	want := `adapter "noop" handshake: rpc error: code = Canceled desc = grpc: the client connection is closing`
	if err.Error() != want {
		t.Fatalf("non-remote transport-loss error shape changed: got %q, want %q", err.Error(), want)
	}
	if n := loader.resolutionsOf("noop"); n != 1 {
		t.Fatalf("local loader resolved the adapter %d time(s), want exactly 1 (no retry re-resolution)", n)
	}
	if dead.killCount() != 1 {
		t.Fatalf("fail-fast path killed the local handle %d time(s), want 1", dead.killCount())
	}
}

// TestSessions_VerifyRetryLaneTypedExhaustedOnBudgetExpiry pins the typed
// budget-expiry failure: when the fresh-line wait ends at the shim's budget
// (here a scripted CRI-137-style diagnosis), Verify fails with a
// VerifyHandshakeRetryExhaustedError carrying the machine-matchable reason
// tag, the last transport error, and the shim's diagnosis as Unwrap.
func TestSessions_VerifyRetryLaneTypedExhaustedOnBudgetExpiry(t *testing.T) {
	ctx := context.Background()
	developer := kb257DeveloperInstance

	lastTransport := kb257CanceledErr()
	waitDiagnosis := errors.New(`remote adapter "noop.developer" session wait for scope "run-1/inst-1" exceeded 400ms without a successful identity handshake: no identity handshake observed at all for scope "run-1/inst-1"; adapter Job may be complete or dead (CRI-137)`)
	dead := newKB257VerifyHandle(lastTransport)
	shim := newKB257VerifyShim(
		map[string]Handle{developer: dead},
		map[string][]kb257FreshResult{developer: {{err: waitDiagnosis}}},
	)

	m := newKB257VerifyManager(t, &cri269Loader{}, shim)

	err := m.Verify(ctx, developer, "noop", "fail", nil, nil, nil, "", "subflow", "run-1/inst-1")
	if err == nil {
		t.Fatal("budget-expired fresh-line wait must fail Verify")
	}
	var exhausted *VerifyHandshakeRetryExhaustedError
	if !errors.As(err, &exhausted) {
		t.Fatalf("budget expiry must surface the typed exhausted error, got: %v", err)
	}
	if exhausted.Reason != VerifyHandshakeRetryExhaustedReason {
		t.Fatalf("typed reason = %q, want %q", exhausted.Reason, VerifyHandshakeRetryExhaustedReason)
	}
	if exhausted.Adapter != developer || exhausted.Scope != "run-1/inst-1" {
		t.Fatalf("typed adapter/scope = %q/%q, want %q/%q", exhausted.Adapter, exhausted.Scope, developer, "run-1/inst-1")
	}
	if !errors.Is(exhausted.LastErr, lastTransport) {
		t.Fatal("typed LastErr must carry the last transport-class handshake error")
	}
	if !errors.Is(err, waitDiagnosis) {
		t.Fatalf("exhausted error must unwrap to the shim's typed wait diagnosis; got %v", err)
	}
	for _, fragment := range []string{"reason=handshake_retry_exhausted", "the client connection is closing", "(CRI-137)"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("exhausted error text must carry %q; got %q", fragment, err.Error())
		}
	}
	if got := shim.freshCallCount(); got != 1 {
		t.Fatalf("exhausted lane made %d WaitForFreshHandle calls, want 1 per expired wait", got)
	}
	if dead.killCount() != 1 {
		t.Fatalf("exhausted path killed the stale handle %d time(s), want exactly 1 (exit cleanup)", dead.killCount())
	}
}

// TestSessions_VerifyRetryLaneFailFastOnHostContextCancellation pins the
// teardown precedence: when the run context is already canceled the lane
// must not classify the handshake failure as a transport loss nor hand a
// typed exhausted error back — host shutdown keeps the plain fail-fast wrap.
func TestSessions_VerifyRetryLaneFailFastOnHostContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	developer := kb257DeveloperInstance

	dead := newKB257VerifyHandle(kb257CanceledErr())
	shim := newKB257VerifyShim(
		map[string]Handle{developer: dead},
		map[string][]kb257FreshResult{developer: nil},
	)

	m := newKB257VerifyManager(t, &cri269Loader{}, shim)

	err := m.Verify(ctx, developer, "noop", "fail", nil, nil, nil, "", "subflow", "run-1/inst-1")
	if err == nil {
		t.Fatal("host-canceled Verify must fail")
	}
	var exhausted *VerifyHandshakeRetryExhaustedError
	if errors.As(err, &exhausted) {
		t.Fatalf("host cancellation must never produce the typed exhausted error: %v", err)
	}
	want := `adapter "noop" handshake: rpc error: code = Canceled desc = grpc: the client connection is closing`
	if err.Error() != want {
		t.Fatalf("host-cancellation fail-fast shape changed: got %q, want %q", err.Error(), want)
	}
	if got := shim.freshCallCount(); got != 0 {
		t.Fatalf("host cancellation entered the retry lane (%d WaitForFreshHandle calls); it must stay fail-fast", got)
	}
}

// TestSessions_VerifyRetryLaneSemanticErrorOnFreshLineIsFailFast pins that a
// semantic failure arriving on a CONVERGED fresh line still fails the
// handshake with the plain wrap — the lane retries transport losses only,
// never semantic ones.
func TestSessions_VerifyRetryLaneSemanticErrorOnFreshLineIsFailFast(t *testing.T) {
	ctx := context.Background()
	developer := kb257DeveloperInstance

	dead := newKB257VerifyHandle(status.Error(codes.Unavailable, "EOF"))
	semantic := status.Error(codes.PermissionDenied, "accept_token verification failed")
	fresh := newKB257VerifyHandle(semantic)
	shim := newKB257VerifyShim(
		map[string]Handle{developer: dead},
		map[string][]kb257FreshResult{developer: {{handle: fresh}}},
	)

	m := newKB257VerifyManager(t, &cri269Loader{}, shim)

	err := m.Verify(ctx, developer, "noop", "fail", nil, nil, nil, "", "subflow", "run-1/inst-1")
	if err == nil {
		t.Fatal("semantic failure on the fresh line must fail Verify")
	}
	want := `adapter "noop" handshake: rpc error: code = PermissionDenied desc = accept_token verification failed`
	if err.Error() != want {
		t.Fatalf("fresh-line semantic failure shape changed: got %q, want %q", err.Error(), want)
	}
	if got := shim.freshCallCount(); got != 1 {
		t.Fatalf("fresh-line semantic failure triggered another wait (%d calls), want 1", got)
	}
}
