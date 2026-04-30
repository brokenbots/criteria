package conformance

// conformance_lifecycle.go — session lifecycle, cancellation, timeout, and
// crash-detection contract tests (plugin-only).

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/plugin"
)

func testCancel(t *testing.T, name string, factory targetFactory, opts Options) {
	t.Helper()

	cfg, ok := longRunningConfig(opts.StepConfig)
	if !ok {
		t.Skip("cancellation test skipped: no long-running config available")
	}
	target := factory(t)
	if !isPluginTarget(target) {
		defer goleak.VerifyNone(t)
	}
	step := baseStep(name, target.Name(), cfg)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	time.AfterFunc(50*time.Millisecond, cancel)

	done := make(chan struct{})
	var execErr error
	go func() {
		_, execErr = executeNoPanic(t, target, ctx, step, &recordingSink{})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Execute did not return within 500ms after cancellation")
	}

	if execErr == nil {
		t.Fatal("expected cancellation error, got nil")
	}
	if !isCancellationLikeError(execErr) {
		t.Fatalf("expected cancellation/deadline error, got: %v", execErr)
	}
}

func testTimeout(t *testing.T, name string, factory targetFactory, opts Options) {
	t.Helper()

	cfg, ok := longRunningConfig(opts.StepConfig)
	if !ok {
		t.Skip("timeout test skipped: no long-running config available")
	}
	target := factory(t)
	if !isPluginTarget(target) {
		defer goleak.VerifyNone(t)
	}
	step := baseStep(name, target.Name(), cfg)
	step.Timeout = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	t.Cleanup(cancel)

	done := make(chan struct{})
	var execErr error
	go func() {
		_, execErr = executeNoPanic(t, target, ctx, step, &recordingSink{})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Execute did not return within 500ms after timeout")
	}

	if execErr == nil {
		t.Fatal("expected timeout error, got nil")
	}
	// For out-of-process plugin targets the gRPC transport may surface a
	// deadline expiry as code=Canceled rather than code=DeadlineExceeded (or
	// vice-versa for RST_STREAM) depending on client/server timing. Accept
	// either error kind for plugin targets; require DeadlineExceeded for
	// in-process adapters.
	if isPluginTarget(target) {
		if !isDeadlineLikeError(execErr) && !isCancellationLikeError(execErr) {
			t.Fatalf("expected deadline or cancellation error from plugin, got: %v", execErr)
		}
	} else {
		if !isDeadlineLikeError(execErr) {
			t.Fatalf("expected deadline exceeded error, got: %v", execErr)
		}
	}
}

func testSessionLifecycle(t *testing.T, name string, loader plugin.Loader, opts Options, info plugin.Info) {
	t.Helper()
	defer goleak.VerifyNone(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	plug, err := loader.Resolve(ctx, name)
	if err != nil {
		t.Fatalf("resolve plugin: %v", err)
	}
	defer plug.Kill()

	sessionID := newSessionID("lifecycle")
	if err := plug.OpenSession(ctx, sessionID, cloneConfig(opts.OpenConfig)); err != nil {
		t.Fatalf("open session: %v", err)
	}

	step := baseStep(name, info.Name, opts.StepConfig)
	res1, err := executeNoPanic(t, pluginSessionTarget{plugin: plug, sessionID: sessionID, name: info.Name}, context.Background(), step, &recordingSink{})
	if err != nil {
		t.Fatalf("first execute: %v", err)
	}
	assertValidOutcome(t, res1.Outcome, opts)

	res2, err := executeNoPanic(t, pluginSessionTarget{plugin: plug, sessionID: sessionID, name: info.Name}, context.Background(), step, &recordingSink{})
	if err != nil {
		t.Fatalf("second execute: %v", err)
	}
	assertValidOutcome(t, res2.Outcome, opts)

	if err := plug.CloseSession(ctx, sessionID); err != nil {
		t.Fatalf("close session: %v", err)
	}

	_, err = executeNoPanic(t, pluginSessionTarget{plugin: plug, sessionID: sessionID, name: info.Name}, context.Background(), step, &recordingSink{})
	if err == nil {
		t.Fatal("expected execute on closed session to fail")
	}
}

func testConcurrentSessions(t *testing.T, name string, loader plugin.Loader, opts Options, info plugin.Info) { //nolint:funlen // W03: concurrent session test requires full lifecycle setup for N goroutines with assertions
	t.Helper()
	defer goleak.VerifyNone(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	plugA, err := loader.Resolve(ctx, name)
	if err != nil {
		t.Fatalf("resolve plugin A: %v", err)
	}
	defer plugA.Kill()

	plugB, err := loader.Resolve(ctx, name)
	if err != nil {
		t.Fatalf("resolve plugin B: %v", err)
	}
	defer plugB.Kill()

	if pidA, okA := plugin.ProcessPID(plugA); okA {
		if pidB, okB := plugin.ProcessPID(plugB); okB && pidA == pidB {
			t.Fatalf("expected distinct plugin PIDs per session, got %d", pidA)
		}
	}

	sessionA := newSessionID("concurrent-a")
	sessionB := newSessionID("concurrent-b")
	if err := plugA.OpenSession(ctx, sessionA, cloneConfig(opts.OpenConfig)); err != nil {
		t.Fatalf("open session A: %v", err)
	}
	if err := plugB.OpenSession(ctx, sessionB, cloneConfig(opts.OpenConfig)); err != nil {
		t.Fatalf("open session B: %v", err)
	}
	defer func() {
		_ = plugA.CloseSession(context.Background(), sessionA)
		_ = plugB.CloseSession(context.Background(), sessionB)
	}()

	targetA := pluginSessionTarget{plugin: plugA, sessionID: sessionA, name: info.Name}
	targetB := pluginSessionTarget{plugin: plugB, sessionID: sessionB, name: info.Name}

	stepConfigA := cloneConfig(opts.StepConfig)
	stepConfigA["conformance_session_marker"] = sessionA
	stepConfigB := cloneConfig(opts.StepConfig)
	stepConfigB["conformance_session_marker"] = sessionB
	stepA := baseStep(name+"-a", info.Name, stepConfigA)
	stepB := baseStep(name+"-b", info.Name, stepConfigB)

	var wg sync.WaitGroup
	var resA, resB adapter.Result
	var errA, errB error
	sinkA := &recordingSink{}
	sinkB := &recordingSink{}

	wg.Add(2)
	go func() {
		defer wg.Done()
		resA, errA = executeNoPanic(t, targetA, context.Background(), stepA, sinkA)
	}()
	go func() {
		defer wg.Done()
		resB, errB = executeNoPanic(t, targetB, context.Background(), stepB, sinkB)
	}()
	wg.Wait()

	if errA != nil {
		t.Fatalf("session A execute: %v", errA)
	}
	if errB != nil {
		t.Fatalf("session B execute: %v", errB)
	}
	assertValidOutcome(t, resA.Outcome, opts)
	assertValidOutcome(t, resB.Outcome, opts)

	if sinkA.containsText(sessionB) {
		t.Fatalf("session A sink unexpectedly contains session B marker %q", sessionB)
	}
	if sinkB.containsText(sessionA) {
		t.Fatalf("session B sink unexpectedly contains session A marker %q", sessionA)
	}
}

func testSessionCrashDetection(t *testing.T, name string, loader plugin.Loader, opts Options, info plugin.Info) {
	t.Helper()
	defer goleak.VerifyNone(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	plug, err := loader.Resolve(ctx, name)
	if err != nil {
		t.Fatalf("resolve plugin: %v", err)
	}
	defer plug.Kill()

	sessionID := newSessionID("crash")
	if err := plug.OpenSession(ctx, sessionID, cloneConfig(opts.OpenConfig)); err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer func() {
		_ = plug.CloseSession(context.Background(), sessionID)
	}()

	target := pluginSessionTarget{plugin: plug, sessionID: sessionID, name: info.Name}
	step := baseStep(name, info.Name, opts.StepConfig)
	if _, err := executeNoPanic(t, target, context.Background(), step, &recordingSink{}); err != nil {
		t.Fatalf("initial execute before crash: %v", err)
	}

	pid, ok := plugin.ProcessPID(plug)
	if !ok || pid <= 0 {
		t.Skip("session crash detection skipped: plugin PID unavailable")
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		t.Fatalf("find plugin process %d: %v", pid, err)
	}
	if err := proc.Kill(); err != nil {
		t.Fatalf("kill plugin process %d: %v", pid, err)
	}

	timeoutCtx, timeoutCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer timeoutCancel()
	result, err := executeNoPanic(t, target, timeoutCtx, step, &recordingSink{})
	if err == nil {
		t.Fatalf("expected execute after crash to fail (outcome=%q)", result.Outcome)
	}
}
