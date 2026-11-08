package engine

// envhost_binding_test.go — pins the production binding that makes the
// envhost seam live (ADR-0014 D1 stage 1): starting a remote environment
// must register a dedicated envhost.Host for its environment key under the
// session manager, not merely leave the per-env shim so every dispatch
// silently resolves through the shimHost fallback. If the engine's
// SetEnvHostForEnv wiring is removed, this test fails while every seam
// fallback still passes — the exact inert-carve regression the binding
// exists to prevent.

import (
	"context"
	"testing"
	"time"
)

func TestMaybeStartRemoteShim_BindsEnvHostForEnv(t *testing.T) {
	const declared = "127.0.0.1:7807"
	g := cri293Graph(t, declared)
	eng := New(g, nil, &fakeSink{}, WithLocalShimIsolation())
	sessions := eng.newCRI293Sessions(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := eng.maybeStartRemoteShim(ctx, sessions); err != nil {
		t.Fatalf("maybeStartRemoteShim: %v", err)
	}
	t.Cleanup(func() {
		_ = sessions.Shutdown(context.WithoutCancel(ctx))
	})

	for _, envKey := range []string{"remote.worktree", "remote.primary"} {
		if sessions.RemoteShimForEnv(envKey) == nil {
			t.Fatalf("%s: pre-carve shim must still be registered", envKey)
		}
		if !sessions.EnvHostRegisteredForEnv(envKey) {
			t.Errorf("%s: EnvHostRegisteredForEnv = false, want a dedicated envhost.Host binding (the engine must wire remote.NewHost under the seam)", envKey)
		}
	}

	// Bindings are exact: no binding exists for an environment the engine
	// did not start, and the legacy default slot stays unbound so legacy
	// knobs keep resolving through the shim fallback.
	if sessions.EnvHostRegisteredForEnv("remote.missing") {
		t.Error("remote.missing: EnvHostRegisteredForEnv = true, want false")
	}
	if sessions.EnvHostRegisteredForEnv("") {
		t.Error("default slot: EnvHostRegisteredForEnv = true, want false (legacy default resolves via shim fallback)")
	}
}
