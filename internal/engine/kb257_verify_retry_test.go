package engine

// kb257_verify_retry_test.go — engine-level contract for the KB-257 fresh-line
// retry lane in the phase-B verify handshake (verifyStagedScopeAdapters →
// SessionManager.Verify). The production killer cycle (attempt-13, KB-255):
// the host closed the freshly served phone-home conn 12ms after accept while
// the verify-Info was in flight, the handshake died with
// "grpc: the client connection is closing", and one child death failed the
// entire subworkflow body init. The lane classifies that transport-class loss
// as retryable and re-waits for the pod's re-dial through the shim's
// WaitForFreshHandle under the existing session budgets.
//
// The pod loop fixture replays the killer cycle deterministically: on the
// first accepted line's first Info it tears the pod side's connection down
// mid-handshake. Two shapes:
//
//  1. converge — the pod keeps phone-homing (like the repro's healthy
//     first entry, ~6s handshake) and initScopeAdapters must complete on the
//     fresh line with exactly two accepted handshakes; and
//  2. budget expiry — the pod kills the line and stops redialing, the
//     shim's session budgets expire, and initScopeAdapters must fail with
//     the typed VerifyHandshakeRetryExhaustedError carrying the
//     reason=handshake_retry_exhausted tag and the shim's CRI-137 diagnosis.

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter/environment/remote"
	"github.com/brokenbots/criteria/internal/adapterhost"
)

// kb257KillFirstInfoServer delegates to the pod server but tears the pod
// side's phone-home connection down the moment the shim's verify handshake
// reaches the first Info — the host-closes-the-served-conn killer cycle. The
// handler continues locally (the response is discarded when the conn dies);
// what matters is that the shim's in-flight Info loses its transport.
type kb257KillFirstInfoServer struct {
	v2.AdapterServiceServer
	inner    v2.AdapterServiceServer
	kill     func()
	infoSeen atomic.Int64
}

func (s *kb257KillFirstInfoServer) Info(ctx context.Context, req *v2.InfoRequest) (*v2.InfoResponse, error) {
	if s.infoSeen.Add(1) == 1 {
		s.kill()
	}
	return s.inner.Info(ctx, req)
}

// kb257PodSleep waits d for the loop's stop channels; it reports whether the
// wait completed normally. A nil channel never fires, so a nil stopOnKill is
// a no-op.
func kb257PodSleep(stop, stopOnKill <-chan struct{}, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-stop:
		return false
	case <-stopOnKill:
		return false
	case <-timer.C:
		return true
	}
}

// kb257KillerHomeLoop is the phone-home loop with the killer cycle injected:
// across ALL its lines, the very first Info served kills that line's
// pod-side connection (the host-closes-the-served-conn cycle). Exactly one
// line is killed, so the loop's subsequent lines serve normally — the
// redial/convergence shape. When stopOnKill is non-nil the kill also closes
// it, which stops the loop from redialing (the budget-expiry shape) — the
// pod then dies with the line, like the production child death.
func kb257KillerHomeLoop(proxyAddr string, hs *cri137Handshake, stop <-chan struct{}, done chan<- struct{}, accepted *atomic.Int64, pod *cri276PodServer, stopOnKill chan struct{}) {
	defer close(done)
	killOnce := &sync.Once{}       // first Info across all lines
	stopOnKillOnce := &sync.Once{} // die-plan stop, closed exactly once
	for {
		select {
		case <-stop:
			return
		case <-stopOnKill:
			return
		default:
		}
		conn, err := net.Dial("tcp", proxyAddr)
		if err != nil {
			if !kb257PodSleep(stop, stopOnKill, 20*time.Millisecond) {
				return
			}
			continue
		}
		hsBytes, err := json.Marshal(hs)
		if err != nil {
			_ = conn.Close()
			return
		}
		if _, err := conn.Write(append(hsBytes, '\n')); err != nil {
			_ = conn.Close()
			if !kb257PodSleep(stop, stopOnKill, 20*time.Millisecond) {
				return
			}
			continue
		}

		observed := &countingConn{Conn: conn}
		server := &kb257KillFirstInfoServer{
			inner: pod,
			kill: func() {
				killOnce.Do(func() {
					_ = conn.Close()
					stopOnKillOnce.Do(func() {
						if stopOnKill != nil {
							close(stopOnKill)
						}
					})
				})
			},
		}
		grpcServer := grpc.NewServer()
		v2.RegisterAdapterServiceServer(grpcServer, server)
		serveDone := make(chan struct{})
		go func() {
			defer close(serveDone)
			_ = grpcServer.Serve(&cri137SingleConnListener{conn: observed})
		}()

		handshakeAccepted := false
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if observed.received.Load() {
				handshakeAccepted = true
				break
			}
			if observed.closed.Load() {
				break
			}
			if !kb257PodSleep(stop, stopOnKill, 5*time.Millisecond) {
				grpcServer.Stop()
				_ = conn.Close()
				return
			}
		}
		if handshakeAccepted {
			accepted.Add(1)
			for {
				if observed.closed.Load() {
					break
				}
				if !kb257PodSleep(stop, stopOnKill, 10*time.Millisecond) {
					grpcServer.Stop()
					_ = conn.Close()
					return
				}
			}
		}
		grpcServer.Stop()
		_ = conn.Close()
		<-serveDone
	}
}

// runKB257KillerChain wires the full remote chain (real shim, real
// SessionManager verify path, killer phone-home loop) and starts
// initScopeAdapters in the background, exactly like runKB153BindChain. The
// caller reads initDone for the init result; accepted counts completed
// handshakes. stopOnKill is closed by the first Info kill when non-nil (the
// no-redial, budget-expiry shape).
type kb257KillerChain struct {
	sessions   *adapterhost.SessionManager
	shim       *remote.Shim
	scopeName  string
	scopeKey   string
	initDone   chan error
	accepted   *atomic.Int64
	stopDial   func()
	podDone    chan struct{}
	stopOnKill chan struct{}
}

func runKB257KillerChain(t *testing.T, budget time.Duration, stopOnKill chan struct{}) *kb257KillerChain {
	t.Helper()
	ctx := context.Background()
	g := perScopeRemoteGraph(t)
	dataDir := t.TempDir()

	beforeDirs := map[string]struct{}{}
	if matches, err := filepath.Glob(filepath.Join(os.TempDir(), "criteria-remote-*")); err == nil {
		for _, m := range matches {
			beforeDirs[m] = struct{}{}
		}
	}

	cfg := &remote.Config{ListenAddress: "127.0.0.1:0", PerScopeSessions: true}
	if budget > 0 {
		cfg.SessionHandshakeBudget = budget
		cfg.SessionSchedulingBudget = budget
	}
	realShim, err := remote.NewShim(cfg, nil)
	if err != nil {
		t.Fatalf("NewShim: %v", err)
	}
	shimCtx, cancelShim := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancelShim()
		_ = realShim.Stop(context.Background())
		waitForCri137ShimTeardown(t, beforeDirs)
	})
	if err := realShim.Start(shimCtx); err != nil {
		t.Fatalf("start real shim: %v", err)
	}

	sessions := adapterhost.NewSessionManager(&fakeLoader{})
	sessions.SetGraph(g)
	sessions.SetRemoteShim(realShim)
	sink := &eventTrackingSink{}
	lifecycle := newScopeLifecycleState(dataDir)
	lifecycle.setRunID("run-kb257")
	rlc := &remoteLifecycleContext{scopeLifecycle: lifecycle}
	deps := Deps{Sessions: sessions, Sink: sink}

	initDone := make(chan error, 1)
	go func() {
		_, err := initScopeAdapters(ctx, g, deps, nil, dataDir, "", nil, rlc)
		initDone <- err
	}()

	var first AdapterLifecycleEvent
	eventDeadline := time.Now().Add(3 * time.Second)
	for {
		ev, ok := sink.firstStatus("provision_wanted")
		if ok {
			first = ev
			break
		}
		if time.Now().After(eventDeadline) {
			t.Fatal("init emitted no provision_wanted event")
		}
		time.Sleep(5 * time.Millisecond)
	}

	tokenBytes, err := os.ReadFile(first.TokenRef)
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	scopeKey := first.ScopeName + "/" + first.ScopeInstanceID
	hs := &cri137Handshake{
		Name:    "noop.default",
		Version: "1.0.0",
		Digest:  "sha256:abcd1234",
		Token:   string(tokenBytes),
		Scope:   scopeKey,
	}

	stopDial := make(chan struct{})
	podDone := make(chan struct{})
	pod := &cri276PodServer{name: "noop.default", version: "1.0.0"}
	accepted := &atomic.Int64{}
	stopDialOnce := &sync.Once{}
	stopDialing := func() { stopDialOnce.Do(func() { close(stopDial) }) }
	go kb257KillerHomeLoop(realShim.ListenAddr(), hs, stopDial, podDone, accepted, pod, stopOnKill)
	t.Cleanup(func() {
		stopDialing()
		<-podDone
	})
	// Shut the manager down first (LIFO): no handle outlives the test (goleak).
	t.Cleanup(func() {
		_ = sessions.Shutdown(context.Background())
	})

	return &kb257KillerChain{
		sessions:   sessions,
		shim:       realShim,
		scopeName:  first.ScopeName,
		scopeKey:   scopeKey,
		initDone:   initDone,
		accepted:   accepted,
		stopDial:   stopDialing,
		podDone:    podDone,
		stopOnKill: stopOnKill,
	}
}

// TestInitScopeAdapters_KillerCycleConvergesOnFreshLine is the engine-level
// acceptance half: with the killer cycle injected into the verify window and
// the pod behaving like a real phone-home runner, the transport-class loss of
// the first line must NOT fail the subworkflow body init — the lane must
// converge on the pod's re-dial (handshake two) and init must complete.
func TestInitScopeAdapters_KillerCycleConvergesOnFreshLine(t *testing.T) {
	chain := runKB257KillerChain(t, 0, nil)

	select {
	case err := <-chain.initDone:
		if err != nil {
			t.Fatalf("init must converge on the pod's re-dial after the killed handshake, got: %v", err)
		}
	case <-time.After(30 * time.Second):
		chain.stopDial()
		t.Fatal("init did not converge within 30s of the killed handshake")
	}

	// The lane waits for a re-dial it cannot observe directly; accepted's
	// counter update lags the shim's registry insert, so settle briefly
	// before asserting exactly two handshakes (verify attempt one + the
	// fresh line the lane adopted).
	settleDeadline := time.Now().Add(5 * time.Second)
	for chain.accepted.Load() < 2 {
		if time.Now().After(settleDeadline) {
			t.Fatalf("pod counted %d accepted handshakes after init converged on a killed handshake; want 2 (original dial + fresh line)", chain.accepted.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := chain.accepted.Load(); got != 2 {
		t.Fatalf("pod counted %d accepted handshakes after init converged; want exactly 2", got)
	}
}

// TestInitScopeAdapters_KillerCycleBudgetExpiryIsTyped is the other
// acceptance half: the pod kills the verify line and never re-dials, the
// shim's session budgets expire, and init must fail across the KB-257 typed
// reason lane — VerifyHandshakeRetryExhaustedError with
// reason=handshake_retry_exhausted and the shim's CRI-137 wait diagnosis —
// rather than the bare transport error.
func TestInitScopeAdapters_KillerCycleBudgetExpiryIsTyped(t *testing.T) {
	const budget = 400 * time.Millisecond
	stopOnKill := make(chan struct{})
	chain := runKB257KillerChain(t, budget, stopOnKill)

	select {
	case err := <-chain.initDone:
		if err == nil {
			t.Fatal("init must fail when the pod kills the verify line and never re-dials")
		}
		var exhausted *adapterhost.VerifyHandshakeRetryExhaustedError
		if !errors.As(err, &exhausted) {
			t.Fatalf("budget-expired init failure must carry the typed exhausted error, got: %v", err)
		}
		if exhausted.Reason != adapterhost.VerifyHandshakeRetryExhaustedReason {
			t.Fatalf("typed reason = %q, want %q", exhausted.Reason, adapterhost.VerifyHandshakeRetryExhaustedReason)
		}
		if exhausted.Scope != chain.scopeKey {
			t.Fatalf("typed scope = %q, want %q", exhausted.Scope, chain.scopeKey)
		}
		for _, fragment := range []string{
			`initialize adapter "noop.default"`,
			"reason=handshake_retry_exhausted",
			"(CRI-137)",
		} {
			if !strings.Contains(err.Error(), fragment) {
				t.Fatalf("budget-expired init error must carry %q; got %q", fragment, err.Error())
			}
		}
	case <-time.After(20 * time.Second):
		chain.stopDial()
		t.Fatal("init did not fail within 20s after the budget expired")
	}
}
