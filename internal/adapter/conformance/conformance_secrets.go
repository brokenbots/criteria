package conformance

// conformance_secrets.go — secret resolution contract tests.

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapterhost"
)

func testSecrets(t *testing.T, name string, loader adapterhost.Loader, opts *Options, info *adapterhost.Info) {
	t.Helper()

	if !hasCapability(info.Capabilities, "secret_resolution") {
		t.Skipf("%s: secret_resolution not advertised", name)
	}

	// 30 s matches the StartTimeout in the loader.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	plug, err := loader.Resolve(ctx, name)
	if err != nil {
		t.Fatalf("resolve adapter: %v", err)
	}
	defer plug.Kill()

	sessionID := newSessionID("secrets")
	secrets := map[string]string{"test_secret": "hunter2"}
	if err := plug.OpenSession(ctx, sessionID, cloneConfig(opts.OpenConfig), secrets); err != nil {
		t.Fatalf("open session: %v", err)
	}
	defer func() {
		_ = plug.CloseSession(context.Background(), sessionID)
	}()

	cfg := cloneConfig(opts.StepConfig)
	cfg["read_secret"] = "test_secret"
	step := baseStep(name, info.Name, cfg)
	sink := &recordingSink{}
	_, execErr := executeNoPanic(t, adapterSessionTarget{handle: plug, sessionID: sessionID, name: info.Name}, ctx, step, sink)
	if execErr != nil {
		t.Fatalf("execute: %v", execErr)
	}

	// Assert the adapter read the secret value from the event sink or output.
	if !sink.containsText("hunter2") {
		t.Fatal("expected adapter output to contain secret value confirming it was read via secrets.Get")
	}

	// Assert the secret does not appear in the process environment.
	// This is a best-effort assertion; we cannot inspect the adapter's
	// environment directly from the harness, so we rely on the adapter's
	// own declaration (e.g. via an adapter event).
	if pid, ok := adapterhost.ProcessPID(plug); ok && pid > 0 {
		env, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
		if err == nil && strings.Contains(string(env), "hunter2") {
			t.Fatal("secret value found in adapter process environment — must not leak")
		}
	}
}
