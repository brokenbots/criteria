// KB-153 contract test: the engine's first step bind must survive the
// verify-kill race. The engine's remote-adapter init verifies the adapter
// through a throwaway handshake handle that is killed synchronously once
// verification completes; on Gate-3 the bind phase's WaitForHandle raced the
// shim's asynchronous registry cleanup, resolved the dead bridge, and the
// step hard-failed with "rpc error: code = Canceled desc = grpc: the client
// connection is closing" after 1 attempts (the retag-forcing Gate-3 flake).
//
// This test runs the real chain — real shim, real SessionManager bind path,
// pod phone-homing with a 20ms re-dial — and binds at the exact CI window:
// immediately after init (verify kill) returns, with no settle wait. The bind
// must wait for the adapter pod's reconnect and the step must succeed; the
// failure signature must never be the torn-down transport error. A genuinely
// dead adapter is covered at the shim level (see the remote package's KB-153
// regression tests).
package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapter/environment/remote"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

type kb153BindChain struct {
	sessions *adapterhost.SessionManager
	pod      *cri276PodServer
	accepted *atomic.Int64
}

// runKB153BindChain wires the full remote chain and returns it in the exact
// state Gate-3 raced: verify completed, the throwaway handshake handle was
// killed, and init has just returned — no settle wait for the pod's re-dial.
func runKB153BindChain(t *testing.T) *kb153BindChain {
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

	realShim, err := remote.NewShim(&remote.Config{ListenAddress: "127.0.0.1:0", PerScopeSessions: true}, nil)
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
	lifecycle.setRunID("run-kb153")
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
		Name:    "noop",
		Version: "1.0.0",
		Digest:  "sha256:abcd1234",
		Token:   string(tokenBytes),
		Scope:   scopeKey,
	}

	stopDial := make(chan struct{})
	podDone := make(chan struct{})
	pod := &cri276PodServer{name: "noop", version: "1.0.0"}
	accepted := &atomic.Int64{}
	go podHomeLoop(realShim.ListenAddr(), hs, stopDial, podDone, accepted, pod, nil)
	t.Cleanup(func() {
		close(stopDial)
		<-podDone
	})
	// Shut the manager down first (LIFO): it closes bound sessions through
	// the still-live shim, so no handle outlives the test (goleak).
	t.Cleanup(func() {
		_ = sessions.Shutdown(context.Background())
	})

	if err := <-initDone; err != nil {
		t.Fatalf("initScopeAdapters: %v", err)
	}

	// Intentionally NO settle wait for the pod's re-dial: the next step bind
	// here races the just-killed verify handle's registry cleanup, which is
	// the Gate-3 window.
	return &kb153BindChain{sessions: sessions, pod: pod, accepted: accepted}
}

func TestBindAfterVerifyKill_WaitsForAdapterRedial(t *testing.T) {
	chain := runKB153BindChain(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	events := cri276AdapterEvents{}
	step := &workflow.StepNode{Name: "kb153-bind"}

	res, err := chain.sessions.Execute(ctx, "noop.default", step, events, nil)
	if err != nil {
		if strings.Contains(err.Error(), "connection is closing") {
			t.Fatalf("bind resolved the killed verify handle and failed on the torn-down transport (KB-153 race): %v", err)
		}
		t.Fatalf("first step after verify kill failed: %v", err)
	}
	if res.Outcome != "success" {
		t.Fatalf("first step outcome = %q, want success", res.Outcome)
	}

	// The bind must have waited for the adapter pod's post-kill re-dial and
	// served the step off the fresh connection. The pod's phone-home loop
	// counts handshakes asynchronously (its accept poll runs after the
	// bridge traffic arrives), so settle briefly before asserting the counts.
	settleDeadline := time.Now().Add(5 * time.Second)
	for {
		if chain.pod.execCalls.Load() >= 1 && chain.accepted.Load() >= 2 {
			break
		}
		if time.Now().After(settleDeadline) {
			t.Fatalf("pod served %d Execute calls and counted %d accepted handshakes within 5s of a successful first step; want the verify handshake and the post-kill re-dial (+1 Execute)", chain.pod.execCalls.Load(), chain.accepted.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
}