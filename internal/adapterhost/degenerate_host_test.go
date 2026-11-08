package adapterhost

import (
	"context"
	"errors"
	"testing"

	"github.com/brokenbots/criteria/internal/envhost"
	"github.com/brokenbots/criteria/workflow"
)

// TestDegenerateHost_ArmContract pins the degenerate in-process host's
// five-arm contract (ADR-0014 M4): shell/sandbox sessions keep the local
// loader path, so this host of record owns no dispatch arm — engagement and
// fresh-handle verbs refuse, token registration and handle close are
// no-ops, the listener is unset, and the journal reports the same
// unavailable shape every host without a journal publishes.
func TestDegenerateHost_ArmContract(t *testing.T) {
	host := DegenerateHost()
	ctx := context.Background()
	scope := envhost.ScopeRef{Scope: "step.demo"}

	t.Run("ensure-refuses", func(t *testing.T) {
		handles, err := host.Ensure(ctx, scope, []envhost.Definition{{Name: "noop.demo", Type: "noop"}})
		if !errors.Is(err, errDegenerateDispatch) {
			t.Fatalf("Ensure err = %v, want errDegenerateDispatch", err)
		}
		if handles != nil {
			t.Errorf("Ensure handles = %v, want nil: nothing may be engaged in-process", handles)
		}
	})

	t.Run("ensure-fresh-handle-refuses", func(t *testing.T) {
		resp, err := host.Control(ctx, &envhost.ControlRequest{
			Scope:   scope,
			Adapter: "noop.demo",
			Kind:    envhost.ControlEnsureFreshHandle,
		})
		if !errors.Is(err, errDegenerateDispatch) {
			t.Fatalf("ControlEnsureFreshHandle err = %v, want errDegenerateDispatch", err)
		}
		if resp != (envhost.ControlResponse{}) {
			t.Errorf("ControlEnsureFreshHandle resp = %+v, want zero response", resp)
		}
	})

	t.Run("inert-arms-are-no-ops", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			kind envhost.ControlKind
			req  *envhost.ControlRequest
		}{
			{"register-scope", envhost.ControlRegisterScope, &envhost.ControlRequest{Scope: scope, Kind: envhost.ControlRegisterScope, Token: "tok"}},
			{"unregister-scope", envhost.ControlUnregisterScope, &envhost.ControlRequest{Scope: scope, Kind: envhost.ControlUnregisterScope}},
			{"close-handle", envhost.ControlCloseHandle, &envhost.ControlRequest{Scope: scope, Adapter: "noop.demo", Kind: envhost.ControlCloseHandle}},
			{"listen-address", envhost.ControlListenAddress, &envhost.ControlRequest{Scope: scope, Kind: envhost.ControlListenAddress}},
		} {
			resp, err := host.Control(ctx, tc.req)
			if err != nil {
				t.Errorf("%s: Control err = %v, want nil", tc.name, err)
			}
			if resp != (envhost.ControlResponse{}) {
				t.Errorf("%s: Control resp = %+v, want zero (inert arm)", tc.name, resp)
			}
		}
	})

	t.Run("unknown-arm-refused", func(t *testing.T) {
		if _, err := host.Control(ctx, &envhost.ControlRequest{Scope: scope, Kind: envhost.ControlKind(99)}); !errors.Is(err, errUnknownControlArm) {
			t.Errorf("unknown arm err = %v, want errUnknownControlArm", err)
		}
	})

	t.Run("release-is-a-no-op-for-every-fate", func(t *testing.T) {
		for _, fate := range []envhost.Fate{envhost.FateRelease, envhost.FateDetach} {
			if err := host.Release(ctx, scope, envhost.FatePolicy{Fate: fate}); err != nil {
				t.Errorf("Release(%s) err = %v, want nil", fate, err)
			}
		}
	})

	t.Run("journal-unavailable", func(t *testing.T) {
		entries, err := host.Journal(scope, "noop.demo").Entries(ctx)
		if !errors.Is(err, envhost.ErrJournalUnavailable) {
			t.Errorf("Journal err = %v, want envhost.ErrJournalUnavailable", err)
		}
		if entries != nil {
			t.Errorf("Journal entries = %v, want nil", entries)
		}
	})

	t.Run("no-shim-or-teardown-substrate", func(t *testing.T) {
		if _, ok := any(host).(HostShimSource); ok {
			t.Error("degenerate host must not implement HostShimSource: shell/sandbox carry no shim substrate and RemoteShimRegisteredForEnv must stay false")
		}
		if _, ok := any(host).(RemoteShim); ok {
			t.Error("degenerate host must not implement RemoteShim")
		}
		// A non-stopper host is deliberately skipped by Shutdown's
		// EnvHostStopper assertion — the in-process go-plugin parent is the
		// supervisor, so no host-side stop exists to own.
		if _, ok := any(host).(EnvHostStopper); ok {
			t.Error("degenerate host must not implement EnvHostStopper: shutdown stops no host-side supervisor for shell/sandbox")
		}
	})

	t.Run("binding-stays-off-the-shim-lane", func(t *testing.T) {
		m := NewSessionManager(nil)
		m.SetEnvHostForEnv("shell.demo", DegenerateHost())
		if !m.EnvHostRegisteredForEnv("shell.demo") {
			t.Error("EnvHostRegisteredForEnv(shell.demo) = false, want true (the binder owns the environment)")
		}
		if m.RemoteShimRegisteredForEnv("shell.demo") {
			t.Error("RemoteShimRegisteredForEnv(shell.demo) = true, want false")
		}
		if m.RemoteShimForEnv("shell.demo") != nil {
			t.Error("RemoteShimForEnv(shell.demo) = non-nil, want nil")
		}
		if err := m.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown with a bound degenerate host = %v, want nil (teardown is safe: no shim, no stopper, no sessions)", err)
		}
	})
}

// TestDegenerateHost_DoesNotChangeShellDispatch pins the dispatch
// non-change: an adapter bound to a shell environment resolves to no host
// of record on ANY dispatch lane (remote-only and container-only), so
// binding a degenerate host cannot reroute a shell/sandbox session away
// from the local loader — even though its environment now has a bound
// host for the seam queries.
func TestDegenerateHost_DoesNotChangeShellDispatch(t *testing.T) {
	g := &workflow.FSMGraph{
		Environments: map[string]*workflow.EnvironmentNode{
			"shell.demo": {Type: "shell", Name: "demo"},
		},
		Adapters: map[string]*workflow.AdapterNode{
			"noop.demo": {Type: "noop", Name: "demo", Environment: "shell.demo"},
		},
	}
	m := NewSessionManager(nil)
	m.SetGraph(g)
	m.SetEnvHostForEnv("shell.demo", DegenerateHost())

	if host, ok := m.envHostForAdapter("noop.demo"); ok || host != nil {
		t.Errorf("envHostForAdapter(noop.demo) = (%v, %t), want (nil, false): shell adapters keep the local loader path", host, ok)
	}
	if host, ok := m.remoteEnvHostForAdapter("noop.demo"); ok || host != nil {
		t.Errorf("remoteEnvHostForAdapter(noop.demo) = (%v, %t), want (nil, false)", host, ok)
	}
	if host, ok := m.containerEnvHostForAdapter("noop.demo"); ok || host != nil {
		t.Errorf("containerEnvHostForAdapter(noop.demo) = (%v, %t), want (nil, false)", host, ok)
	}
}
