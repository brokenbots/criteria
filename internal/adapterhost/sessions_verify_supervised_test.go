package adapterhost

// sessions_verify_supervised_test.go — the phase-1 verify handshake resolves
// an adapter handle only to call Info and validate the declared surface, and
// historically deferred plug.Kill() on the assumption that the handle is a
// throwaway. For a peer-supervised handle that assumption is wrong: the
// handle wraps the peer's ONE real adapter child, so killing it destroys the
// live child and poisons the peer's crash classification (killRequested turns
// the next genuine crash into a graceful exit, suppressing CrashClassified).
// regression tests pin all three guards: peer-supervised handles survive
// verification untouched, per-scope phone-home bridge handles survive via
// the dedicated marker (the pod is not a local child so it cannot report a
// supervision classification), and plain local handles are still killed as
// throwaways.

import (
	"context"
	"testing"
)

// verifyHandleStub wraps cri269Handle, counting Kill calls. It deliberately
// does NOT implement SupervisedHandle — plain handles are throwaway
// verification handles and must still be killed after the handshake.
type verifyHandleStub struct {
	cri269Handle
	kills int
}

func (h *verifyHandleStub) Kill() { h.kills++ }

// supervisedVerifyHandle adds SupervisionCrashReason, the type-assertion
// marker verifyAdapterInfo consults to leave peer-owned handles running.
type supervisedVerifyHandle struct {
	*verifyHandleStub
}

func (h *supervisedVerifyHandle) SupervisionCrashReason() (string, bool) {
	// No classification delivered yet (the child is alive) — the marker only
	// has to be present; the classification seam is tested elsewhere.
	return "", false
}

// verifyHandleShim hands verifyAdapterInfo a fresh stub per WaitForHandle call
// and records the calls by adapter type.
type verifyHandleShim struct {
	supervised bool
	phoneHome  bool
	calls      map[string]int
	stubs      map[string]*verifyHandleStub
}

func newVerifyHandleShim(supervised bool) *verifyHandleShim {
	return &verifyHandleShim{
		supervised: supervised,
		phoneHome:  false,
		calls:      map[string]int{},
		stubs:      map[string]*verifyHandleStub{},
	}
}

// phoneHome marks the shim as dialing a per-scope phone-home adapter: the
// resolved handle wraps the live bridge to the adapter pod and must carry
// the PhoneHomeBridgeHandle marker.
func (s *verifyHandleShim) withPhoneHome() *verifyHandleShim {
	s.phoneHome = true
	return s
}

func (s *verifyHandleShim) WaitForHandle(_ context.Context, adapterType, _ string) (Handle, error) {
	stub := &verifyHandleStub{}
	s.stubs[adapterType] = stub
	s.calls[adapterType]++
	if s.supervised {
		return &supervisedVerifyHandle{stub}, nil
	}
	if s.phoneHome {
		return &phoneHomeVerifyHandle{stub}, nil
	}
	return stub, nil
}

// phoneHomeVerifyHandle is the KB-232 regression case: the handle returned
// by the shim for a per-scope phone-home adapter wraps the live bridge to
// the adapter pod. It is supervised only in spirit — the pod never runs as
// a local child and cannot implement SupervisionCrashReason — so the guard
// recognizes it through the dedicated phone-home marker instead.
type phoneHomeVerifyHandle struct {
	*verifyHandleStub
}

func (h *phoneHomeVerifyHandle) IsPhoneHomeBridge() {}

func (s *verifyHandleShim) WaitForFreshHandle(_ context.Context, adapterType, _ string, _ Handle) (Handle, error) {
	return s.stubs[adapterType], nil
}

func (s *verifyHandleShim) RegisterScope(string, string)                      {}
func (s *verifyHandleShim) UnregisterScope(string)                            {}
func (s *verifyHandleShim) CloseHandle(context.Context, string, string) error { return nil }
func (s *verifyHandleShim) ListenAddr() string                                { return "127.0.0.1:1" }
func (s *verifyHandleShim) Stop(context.Context) error                        { return nil }

// TestSessions_VerifyHandshakeLeavesPeerSupervisedHandleAlive is the
// regression test for the verify-Kill bug: the eager VerifyGraph handshake
// resolves the remote adapter over the shim and must NOT kill the returned
// handle, because for a peer-backed handle that Kill tears down the peer's
// real supervised child.
func TestSessions_VerifyHandshakeLeavesPeerSupervisedHandleAlive(t *testing.T) {
	ctx := context.Background()
	g, _ := compileCRI269SessionsGraph(t, false)
	const developer = "noop.developer"

	m := NewSessionManager(&cri269Loader{})
	m.SetGraph(g)
	shim := newVerifyHandleShim(true)
	m.SetRemoteShim(shim)

	if err := m.VerifyGraph(ctx, g, nil); err != nil {
		t.Fatalf("VerifyGraph: %v", err)
	}

	// resolveAdapterHandle dispatches over the shim with the adapter TYPE.
	h := shim.stubs["noop"]
	if h == nil {
		t.Fatalf("verify handshake must resolve %q over the shim", developer)
	}
	if h.kills != 0 {
		t.Fatalf("verify handshake killed the peer-supervised handle %d time(s); the handle wraps the peer's real supervised child and must be left running", h.kills)
	}
}

// TestSessions_VerifyHandshakeKillsPlainThrowawayHandle pins the other half
// of the guard: a plain local verification handle is still a throwaway and
// must be killed after the handshake, so the fix does not leak local handles.
func TestSessions_VerifyHandshakeKillsPlainThrowawayHandle(t *testing.T) {
	ctx := context.Background()
	g, _ := compileCRI269SessionsGraph(t, false)
	const developer = "noop.developer"

	m := NewSessionManager(&cri269Loader{})
	m.SetGraph(g)
	shim := newVerifyHandleShim(false)
	m.SetRemoteShim(shim)

	if err := m.VerifyGraph(ctx, g, nil); err != nil {
		t.Fatalf("VerifyGraph: %v", err)
	}

	// resolveAdapterHandle dispatches over the shim with the adapter TYPE.
	h := shim.stubs["noop"]
	if h == nil {
		t.Fatalf("verify handshake must resolve %q over the shim", developer)
	}
	if h.kills != 1 {
		t.Fatalf("verify handshake killed the plain handle %d time(s), want exactly 1 (the verification handle is a throwaway)", h.kills)
	}
}

// TestSessions_VerifyHandshakeLeavesPhoneHomeBridgeHandleAlive is the KB-232
// regression test: for a per-scope phone-home adapter the verify handshake
// resolves the handle that wraps the live bridge to the adapter pod. That
// handle is neither supervised (the pod is not a plain local child) nor a
// throwaway — killing it severs the pod's only registration channel — so
// verification must leave it running via the marker guard.
func TestSessions_VerifyHandshakeLeavesPhoneHomeBridgeHandleAlive(t *testing.T) {
	ctx := context.Background()
	g, _ := compileCRI269SessionsGraph(t, false)
	const developer = "noop.developer"

	m := NewSessionManager(&cri269Loader{})
	m.SetGraph(g)
	shim := newVerifyHandleShim(false).withPhoneHome()
	m.SetRemoteShim(shim)

	if err := m.VerifyGraph(ctx, g, nil); err != nil {
		t.Fatalf("VerifyGraph: %v", err)
	}

	h := shim.stubs["noop"]
	if h == nil {
		t.Fatalf("verify handshake must resolve %q over the shim", developer)
	}
	if h.kills != 0 {
		t.Fatalf("verify handshake killed the phone-home bridge handle %d time(s); the handle wraps the live bridge to the adapter pod and must be left running", h.kills)
	}
}
