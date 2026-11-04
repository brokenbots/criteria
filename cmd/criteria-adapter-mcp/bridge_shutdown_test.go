package main

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

// textHidingError wraps an error while hiding its message text, but keeps
// the chain intact for errors.Is/As. A message-based killed detection (the
// pre-typed strings.Contains(err.Error(), "signal: killed") check) cannot
// see through it; the typed WaitStatus inspection must.
type textHidingError struct{ err error }

func (e textHidingError) Error() string {
	return "child process terminated, underlying message withheld"
}

func (e textHidingError) Unwrap() error { return e.err }

// waitErrorAfterSignal runs a long-lived child, tears it down with sig and
// returns the Wait error (SIGTERM kills sleep via its default disposition,
// so both signals produce real signal-death wait errors).
func waitErrorAfterSignal(t *testing.T, sig syscall.Signal) error {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	if err := cmd.Process.Signal(sig); err != nil {
		t.Fatalf("signal child with %v: %v", sig, err)
	}
	err := cmd.Wait()
	if err == nil {
		t.Fatalf("expected a wait error after %v, got nil", sig)
	}
	return err
}

func TestIsSignalExitTypedDetection(t *testing.T) {
	sigKilled := waitErrorAfterSignal(t, syscall.SIGKILL)
	if !isSignalExit(sigKilled, syscall.SIGKILL) {
		t.Fatalf("isSignalExit(%q, SIGKILL) = false, want true", sigKilled)
	}

	// Regression guard for the typed detection itself: the same signal
	// death stays detected when the wrapped error's text does not carry
	// stdlib's "signal:" wording. Message matching would fail here; only
	// the *exec.ExitError identity route survives a Go release that
	// rewords its process-death strings.
	hidden := textHidingError{err: sigKilled}
	if !isSignalExit(hidden, syscall.SIGKILL) {
		t.Fatalf("isSignalExit(text-hidden SIGKILL exit, SIGKILL) = false, want true")
	}

	// Signal deaths other than SIGKILL are not treated as clean teardowns.
	termErr := waitErrorAfterSignal(t, syscall.SIGTERM)
	if isSignalExit(termErr, syscall.SIGKILL) {
		t.Fatalf("isSignalExit(%q, SIGKILL) = true, want false", termErr)
	}

	// A nonzero self-exit is an exit, not a signal death.
	shCmd := exec.Command("sh", "-c", "exit 7")
	startErr := shCmd.Start()
	if startErr != nil {
		t.Fatalf("start exit-7 child: %v", startErr)
	}
	exitErr := shCmd.Wait()
	if exitErr == nil {
		t.Fatal("expected wait error from exit-7 child")
	}
	if isSignalExit(exitErr, syscall.SIGKILL) {
		t.Fatalf("isSignalExit(%q, SIGKILL) = true, want false", exitErr)
	}

	// Non-ExitError and nil inputs: never a signal death.
	if isSignalExit(context.DeadlineExceeded, syscall.SIGKILL) {
		t.Fatal("isSignalExit(context.DeadlineExceeded) = true, want false")
	}
	if isSignalExit(nil, syscall.SIGKILL) {
		t.Fatal("isSignalExit(nil) = true, want false")
	}
}

func TestShutdownSessionToleratesSigKilledServer(t *testing.T) {
	state, err := startMCPServer(map[string]string{"command": "sleep", "args": "60"})
	if err != nil {
		t.Fatalf("start server: %v", err)
	}
	if err := state.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("kill server: %v", err)
	}
	if err := shutdownSession(context.Background(), state); err != nil {
		t.Fatalf("shutdown of SIGKILLed server: want nil, got %v", err)
	}
}

func TestShutdownSessionSurfacesRealExitFailure(t *testing.T) {
	state, err := startMCPServer(map[string]string{"command": "sh", "args": "-c,exit 17"})
	if err != nil {
		t.Fatalf("start server: %v", err)
	}
	shutdownErr := shutdownSession(context.Background(), state)
	if shutdownErr == nil {
		t.Fatal("shutdown of exited server: want error, got nil")
	}
	// Teardown failures surface wrapped and still identify the child death
	// through typed error chaining, not only through the message text.
	var exitErr *exec.ExitError
	if !errors.As(shutdownErr, &exitErr) {
		t.Fatalf("shutdown error should wrap the child's *exec.ExitError, got %T: %v", shutdownErr, shutdownErr)
	}
	if code := exitErr.ExitCode(); code != 17 {
		t.Fatalf("child exit code = %d, want 17", code)
	}
	if !strings.Contains(shutdownErr.Error(), "mcp: wait server exit") {
		t.Fatalf("shutdown error %q missing prefix %q", shutdownErr, "mcp: wait server exit")
	}
}