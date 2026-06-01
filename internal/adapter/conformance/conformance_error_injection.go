package conformance

// conformance_error_injection.go — error injection contract tests.

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
)

func testErrorInjection(t *testing.T, name string, loader adapterhost.Loader, opts *Options, info *adapterhost.Info) {
	t.Helper()

	if !opts.ErrorInjection {
		t.Skipf("%s: error_injection not enabled in options", name)
	}

	t.Run("handshake_drop", func(t *testing.T) {
		testHandshakeDrop(t, name, loader, opts, info)
	})
	t.Run("partial_failure_recovery", func(t *testing.T) {
		testPartialFailureRecovery(t, name, loader, opts, info)
	})
}

func testHandshakeDrop(t *testing.T, _ string, _ adapterhost.Loader, _ *Options, _ *adapterhost.Info) {
	t.Helper()
	defer goleak.VerifyNone(t)

	// Build the handshake dropper fixture.
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve caller path")
	}
	moduleRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "testfixtures", "handshake_dropper"))
	dropperBin := filepath.Join(t.TempDir(), "criteria-adapter-handshake-dropper")
	cmd := exec.Command("go", "build", "-o", dropperBin, ".")
	cmd.Dir = moduleRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build handshake dropper: %v\n%s", err, string(output))
	}

	ld := adapterhost.NewLoaderWithDiscovery(func(requested string) (string, error) {
		if requested != "handshake-dropper" {
			return "", errors.New("unexpected adapter")
		}
		return dropperBin, nil
	})
	defer func() { _ = ld.Shutdown(context.Background()) }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	plug, err := ld.Resolve(ctx, "handshake-dropper")
	if err != nil {
		t.Fatalf("resolve handshake dropper: %v", err)
	}
	defer plug.Kill()

	sessionID := newSessionID("handshake-drop")
	if err := plug.OpenSession(ctx, sessionID, nil, nil); err != nil {
		t.Fatalf("open session: %v", err)
	}

	// The fixture drops the connection before Execute returns.
	step := baseStep("handshake-dropper", "handshake-dropper", nil)
	sink := &recordingSink{}
	_, execErr := executeNoPanic(t, adapterSessionTarget{handle: plug, sessionID: sessionID, name: "handshake-dropper"}, ctx, step, sink)

	if execErr == nil {
		t.Fatal("expected error after handshake drop, got nil")
	}
	// Must be a well-defined error, not a hang or panic.
	if execErr.Error() == "" {
		t.Fatal("expected non-empty error after handshake drop")
	}
}

func testPartialFailureRecovery(t *testing.T, name string, loader adapterhost.Loader, opts *Options, info *adapterhost.Info) {
	t.Helper()
	defer goleak.VerifyNone(t)

	// 30 s matches the StartTimeout in the loader.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	plug, err := loader.Resolve(ctx, name)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	defer plug.Kill()

	sessionID := newSessionID("partial-fail")
	if err := plug.OpenSession(ctx, sessionID, cloneConfig(opts.OpenConfig), nil); err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer func() {
		_ = plug.CloseSession(context.Background(), sessionID)
	}()

	cfg := cloneConfig(opts.StepConfig)
	cfg["partial_failure_after_events"] = "3"
	step := baseStep(name, info.Name, cfg)
	sink := &recordingSink{}
	_, execErr := executeNoPanic(t, adapterSessionTarget{handle: plug, sessionID: sessionID, name: info.Name}, ctx, step, sink)

	if execErr == nil {
		t.Fatal("expected partial-failure error, got nil")
	}

	var fwc adapter.FailureWithContext
	if !errors.As(execErr, &fwc) {
		t.Fatalf("expected error implementing adapter.FailureWithContext, got: %T %v", execErr, execErr)
	}
	if fwc.EventIndex() < 0 {
		t.Fatalf("expected EventIndex >= 0, got %d", fwc.EventIndex())
	}
	phase := fwc.Phase()
	if phase != "open" && phase != "execute" && phase != "close" {
		t.Fatalf("unexpected phase %q", phase)
	}

	// Assert pre-failure events were delivered to the sink.
	if sink.totalEvents() == 0 {
		t.Fatal("expected pre-failure events in sink, got none")
	}
}
