package adapterhost

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// failoverShim is a test RemoteShim that scripts the KB-257 line-loss
// shape: the initial WaitForHandle hands out a queue head, and each
// WaitForFreshHandle pops the first queued handle that is not the stale
// one. A non-empty failN [adapter] makes that many fresh-handle waits
// fail with a transport error first (a re-dial that never lands).
type failoverShim struct {
	lines     map[string][]Handle
	failN     map[string]int
	waits     int
	lastStale map[string]Handle
}

func newFailoverShim(adapter string, order ...Handle) *failoverShim {
	return &failoverShim{
		lines:     map[string][]Handle{adapter: order},
		lastStale: map[string]Handle{},
	}
}

var errFreshWaitLost = status.Error(codes.Unavailable, "connection refused")

func (s *failoverShim) pop(ctx context.Context, adapter string, stale Handle, freshOnly bool) (Handle, error) {
	q := s.lines[adapter]
	for i, h := range q {
		if freshOnly && stale != nil && h == stale {
			continue
		}
		s.lines[adapter] = append(append([]Handle{}, q[i+1:]...), q[:i]...)
		return h, nil
	}
	if s.failN[adapter] > 0 {
		s.failN[adapter]--
		return nil, errFreshWaitLost
	}
	// Registry drained: simulate the shim-side budget expiry the awaitWaiter
	// path returns so the caller does not hang past its own budget.
	<-ctx.Done()
	return nil, context.DeadlineExceeded
}

func (s *failoverShim) WaitForHandle(ctx context.Context, adapter, _ string) (Handle, error) {
	return s.pop(ctx, adapter, nil, false)
}

func (s *failoverShim) WaitForFreshHandle(ctx context.Context, adapter, scope string, stale Handle) (Handle, error) {
	s.waits++
	s.lastStale[adapter] = stale
	return s.pop(ctx, adapter, stale, true)
}

func (s *failoverShim) RegisterScope(string, string)                      {}
func (s *failoverShim) UnregisterScope(string)                            {}
func (s *failoverShim) CloseHandle(context.Context, string, string) error { return nil }
func (s *failoverShim) ListenAddr() string                                { return "127.0.0.1:1" }
func (s *failoverShim) Stop(context.Context) error                        { return nil }

// transportLossHandle wraps a valid verify stub but loses every Info call
// with the exact attempt-13 transport signature.
type transportLossHandle struct {
	*verifyHandleStub
	infoCalls int
}

func (h *transportLossHandle) Info(context.Context) (Info, error) {
	h.infoCalls++
	return Info{}, status.Error(codes.Canceled, "grpc: the client connection is closing")
}

// semanticLossHandle loses Info with a non-transport error.
type semanticLossHandle struct {
	*verifyHandleStub
	infoCalls int
}

func (h *semanticLossHandle) Info(context.Context) (Info, error) {
	h.infoCalls++
	return Info{}, errors.New("adapter booted into an invalid state")
}

// countingVerifyHandle delegates to verifyHandleStub but counts Info calls.
type countingVerifyHandle struct {
	*verifyHandleStub
	infoCalls int
}

func (h *countingVerifyHandle) Info(ctx context.Context) (Info, error) {
	h.infoCalls++
	return h.verifyHandleStub.Info(ctx)
}

func TestKB257_VerifyConvergesOntoFreshLine(t *testing.T) {
	ctx := context.Background()
	g, _ := compileCRI269SessionsGraph(t, false)
	const developer = "noop.developer"

	dead := &transportLossHandle{verifyHandleStub: &verifyHandleStub{}}
	fresh := &countingVerifyHandle{verifyHandleStub: &verifyHandleStub{}}
	shim := newFailoverShim(developer, dead, fresh)

	m := NewSessionManager(&cri269Loader{})
	m.SetGraph(g)
	m.VerifyHandshakeFailoverBudget = 30 * time.Second
	m.SetRemoteShim(shim)

	if err := m.VerifyGraph(ctx, g, nil); err != nil {
		t.Fatalf("VerifyGraph must converge onto the fresh line after the mid-handshake loss: %v", err)
	}
	if dead.infoCalls != 1 || fresh.infoCalls != 1 {
		t.Errorf("info calls dead=%d fresh=%d, want exactly one handshake round on each line", dead.infoCalls, fresh.infoCalls)
	}
	if shim.waits != 1 {
		t.Errorf("WaitForFreshHandle calls = %d, want exactly one failover wait", shim.waits)
	}
	if shim.lastStale[developer] != Handle(dead) {
		t.Errorf("failover must pass the dead handle as the displacement argument, got %v", shim.lastStale[developer])
	}
}

func TestKB257_SemanticLossOnFreshLineStaysFailFast(t *testing.T) {
	ctx := context.Background()
	g, _ := compileCRI269SessionsGraph(t, false)
	const developer = "noop.developer"

	dead := &transportLossHandle{verifyHandleStub: &verifyHandleStub{}}
	semantic := &semanticLossHandle{verifyHandleStub: &verifyHandleStub{}}
	shim := newFailoverShim(developer, dead, semantic)

	m := NewSessionManager(&cri269Loader{})
	m.SetGraph(g)
	m.VerifyHandshakeFailoverBudget = 30 * time.Second
	m.SetRemoteShim(shim)

	err := m.VerifyGraph(ctx, g, nil)
	if err == nil {
		t.Fatal("VerifyGraph must fail on a non-transport Info error")
	}
	if !strings.Contains(err.Error(), "adapter booted into an invalid state") {
		t.Errorf("error must surface the semantic failure verbatim, got: %v", err)
	}
	if shim.waits != 1 {
		t.Errorf("WaitForFreshHandle calls = %d, want exactly one; semantic failures must not re-wait", shim.waits)
	}
}

func TestKB257_RepeatedLossesExhaustTypedBudget(t *testing.T) {
	ctx := context.Background()
	g, _ := compileCRI269SessionsGraph(t, false)
	const developer = "noop.developer"

	// Every line folds mid-handshake: the first during the eager
	// resolve→Info, the second right after the failover wait pops it, and
	// the registry never yields a healthy line — the convergence lane
	// must spend its budget re-waiting, then fail typed.
	dead := &transportLossHandle{verifyHandleStub: &verifyHandleStub{}}
	dead2 := &transportLossHandle{verifyHandleStub: &verifyHandleStub{}}
	shim := newFailoverShim(developer, dead, dead2)

	m := NewSessionManager(&cri269Loader{})
	m.SetGraph(g)
	m.VerifyHandshakeFailoverBudget = 50 * time.Millisecond
	m.SetRemoteShim(shim)

	err := m.VerifyGraph(ctx, g, nil)
	if err == nil {
		t.Fatal("VerifyGraph must fail when the failover budget is exhausted")
	}
	if !strings.Contains(err.Error(), "handshake budget expired after transport loss") {
		t.Errorf("error must carry the typed budget-expired reason, got: %v", err)
	}
}

func TestKB257_ClassifierShapes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"attempt-13 canceled", status.Error(codes.Canceled, "grpc: the client connection is closing"), true},
		{"unavailable refused", status.Error(codes.Unavailable, "dial tcp: connect: connection refused"), true},
		{"legacy closing text", errors.New("rpc error: code = Canceled desc = grpc: the client connection is closing"), true},
		{"legacy refused text", errors.New("dial 10.42.0.103:34141: connect: connection refused"), true},
		{"caller canceled", context.Canceled, false},
		{"caller deadline", context.DeadlineExceeded, false},
		{"digest mismatch", errors.New("digest verification: digest mismatch for adapter \"noop\": got \"x\", want \"y\""), false},
		{"internal status", status.Error(codes.Internal, "boom"), false},
	}
	for _, tc := range cases {
		if got := classifyTransportLoss(tc.err); got != tc.want {
			t.Errorf("%s: classifyTransportLoss = %v, want %v", tc.name, got, tc.want)
		}
	}
}
