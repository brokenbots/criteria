package conformance

// conformance_permissions.go — bidi permission stream contract tests.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapterhost"
)

func testPermissions(t *testing.T, name string, loader adapterhost.Loader, opts *Options, info *adapterhost.Info) {
	t.Helper()

	if !hasCapability(info.Capabilities, "permission_gating") && !hasCapability(info.Capabilities, "permission_request_forwarding") {
		t.Skipf("%s: adapter does not advertise permission_gating or permission_request_forwarding", name)
	}

	if opts.PermissionDenyPaths {
		t.Run("deny_with_error", func(t *testing.T) {
			testDenyWithError(t, name, loader, opts, info)
		})
		t.Run("deny_after_timeout", func(t *testing.T) {
			testDenyAfterTimeout(t, name, loader, opts, info)
		})
		t.Run("deny_after_session_close", func(t *testing.T) {
			testDenyAfterSessionClose(t, name, loader, opts, info)
		})
	}
}

func testDenyWithError(t *testing.T, name string, loader adapterhost.Loader, opts *Options, info *adapterhost.Info) {
	t.Helper()
	// 30 s matches the StartTimeout in the loader.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	plug, err := loader.Resolve(ctx, name)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	defer plug.Kill()

	sessionID := newSessionID("deny-err")
	if err := plug.OpenSession(ctx, sessionID, cloneConfig(opts.OpenConfig), cloneConfig(opts.Secrets)); err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer func() {
		_ = plug.CloseSession(context.Background(), sessionID)
	}()

	step := baseStep(name, info.Name, opts.StepConfig)
	sink := &recordingSink{}
	_, execErr := executeNoPanic(t, adapterSessionTarget{handle: plug, sessionID: sessionID, name: info.Name}, ctx, step, sink)

	if execErr == nil {
		t.Fatal("expected error when permission is denied with error, got nil")
	}
	// Assert the error is structured and indicates a permission denial.
	errStr := execErr.Error()
	if !strings.Contains(errStr, "permission") && !strings.Contains(errStr, "denied") && !strings.Contains(errStr, "unauthorized") {
		t.Fatalf("expected structured permission-denied error, got: %v", execErr)
	}
}

func testDenyAfterTimeout(t *testing.T, name string, loader adapterhost.Loader, opts *Options, info *adapterhost.Info) {
	t.Helper()
	// 30 s matches the StartTimeout in the loader.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	plug, err := loader.Resolve(ctx, name)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	defer plug.Kill()

	sessionID := newSessionID("deny-to")
	if err := plug.OpenSession(ctx, sessionID, cloneConfig(opts.OpenConfig), cloneConfig(opts.Secrets)); err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer func() {
		_ = plug.CloseSession(context.Background(), sessionID)
	}()

	// Use a short step timeout so the host-side decision wait times out.
	step := baseStep(name, info.Name, opts.StepConfig)
	step.Timeout = 100 * time.Millisecond

	sink := &recordingSink{}
	_, execErr := executeNoPanic(t, adapterSessionTarget{handle: plug, sessionID: sessionID, name: info.Name}, ctx, step, sink)

	if execErr == nil {
		t.Fatal("expected timeout-like error when host decision times out, got nil")
	}
	if !isDeadlineLikeError(execErr) && !isCancellationLikeError(execErr) {
		t.Fatalf("expected deadline exceeded error, got: %v", execErr)
	}
}

func testDenyAfterSessionClose(t *testing.T, name string, loader adapterhost.Loader, opts *Options, info *adapterhost.Info) {
	t.Helper()
	// 30 s matches the StartTimeout in the loader.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	plug, err := loader.Resolve(ctx, name)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	defer plug.Kill()

	sessionID := newSessionID("deny-close")
	if err := plug.OpenSession(ctx, sessionID, cloneConfig(opts.OpenConfig), cloneConfig(opts.Secrets)); err != nil {
		t.Fatalf("open session: %v", err)
	}

	// Close the session while the adapter may be awaiting a permission decision.
	if err := plug.CloseSession(ctx, sessionID); err != nil {
		t.Fatalf("close session: %v", err)
	}

	step := baseStep(name, info.Name, opts.StepConfig)
	sink := &recordingSink{}
	_, execErr := executeNoPanic(t, adapterSessionTarget{handle: plug, sessionID: sessionID, name: info.Name}, ctx, step, sink)

	if execErr == nil {
		t.Fatal("expected error when executing on closed session, got nil")
	}
}
