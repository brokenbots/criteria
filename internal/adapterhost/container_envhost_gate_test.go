package adapterhost

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/envhost"
	"github.com/brokenbots/criteria/workflow"
)

// TestContainerGate_ContainerEnvForAdapter pins the isolation-kind dispatch:
// an adapter declaration bound to a container environment resolves through
// containerEnvForAdapter; local and remote environments do not.
func TestContainerGate_ContainerEnvForAdapter(t *testing.T) {
	graph := &workflow.FSMGraph{
		DefaultEnvironment: "container.sandbox",
		Adapters: map[string]*workflow.AdapterNode{
			"container.pinned":   {Name: "pinned", Environment: "container.sandbox"},
			"container.implicit": {Name: "implicit"},
			"other.remote":       {Name: "remote", Environment: "remote.ops"},
			"other.local":        {Name: "local", Environment: "local.dev"},
		},
		Environments: map[string]*workflow.EnvironmentNode{
			"container.sandbox": {Name: "sandbox", Type: "container"},
			"remote.ops":        {Name: "ops", Type: "remote"},
			"local.dev":         {Name: "dev", Type: "local"},
		},
	}
	m := NewSessionManager(nil)
	m.SetGraph(graph)

	if key, ok := m.containerEnvForAdapter("container.pinned"); !ok || key != "container.sandbox" {
		t.Fatalf("containerEnvForAdapter(pinned) = (%q, %t)", key, ok)
	}
	if key, ok := m.containerEnvForAdapter("container.implicit"); !ok || key != "container.sandbox" {
		t.Fatalf("containerEnvForAdapter(implicit, default env) = (%q, %t)", key, ok)
	}
	if _, ok := m.containerEnvForAdapter("other.remote"); ok {
		t.Fatal("containerEnvForAdapter classified a remote environment as container")
	}
	if _, ok := m.containerEnvForAdapter("other.local"); ok {
		t.Fatal("containerEnvForAdapter classified a local environment as container")
	}
}

// TestContainerGate_EnvHostForAdapterContainerArm pins the container arm of
// the adapter dispatch: container-bound adapters resolve through the bound
// supervisor host ONLY, and an unbound container environment keeps the
// pre-carve dispatch (no host) — even when a shim is registered for the
// same key (the container lane has no shim substrate to fold). The remote
// arm keeps its shim fold.
func TestContainerGate_EnvHostForAdapterContainerArm(t *testing.T) {
	graph := &workflow.FSMGraph{
		Adapters: map[string]*workflow.AdapterNode{
			"container.a": {Name: "a", Environment: "container.x"},
			"remote.a":    {Name: "a", Environment: "remote.x"},
		},
		Environments: map[string]*workflow.EnvironmentNode{
			"container.x": {Name: "x", Type: "container"},
			"remote.x":    {Name: "x", Type: "remote"},
		},
	}
	m := NewSessionManager(nil)
	m.SetGraph(graph)

	// An unbound container environment with a shim registered still keeps
	// the pre-carve dispatch for container handles; the remote arm folds.
	m.SetRemoteShimForEnv("container.x", stubShim{})
	m.SetRemoteShimForEnv("remote.x", stubShim{})
	if _, ok := m.containerEnvHostForAdapter("container.a"); ok {
		t.Fatal("containerEnvHostForAdapter folded the shim into a container bound host")
	}
	if host, ok := m.envHostForAdapter("container.a"); ok {
		t.Fatalf("envHostForAdapter dispatched an unbound container adapter to %T, want the local fallthrough", host)
	}

	if host, ok := m.envHostForAdapter("remote.a"); !ok {
		t.Fatal("envHostForAdapter resolved no host for a remote adapter with a shim")
	} else if _, isShimHost := host.(shimHost); !isShimHost {
		t.Fatalf("envHostForAdapter(remote) = %T, want the shim fold", host)
	}

	m.SetContainerEnvHost("container.x", stubEnvHost{})
	if host, ok := m.containerEnvHostForAdapter("container.a"); !ok {
		t.Fatal("containerEnvHostForAdapter did not dispatch after the explicit bind")
	} else if _, isStub := host.(stubEnvHost); !isStub {
		t.Fatalf("containerEnvHostForAdapter = %T, want the bound stub host", host)
	}
	if host, ok := m.envHostForAdapter("container.a"); !ok {
		t.Fatal("envHostForAdapter container arm lost the bound host")
	} else if _, isStub := host.(stubEnvHost); !isStub {
		t.Fatalf("envHostForAdapter(container, bound) = %T, want the bound stub host", host)
	}
	if host, ok := m.envHostForAdapter("remote.a"); !ok {
		t.Fatal("envHostForAdapter remote arm broke after the container bind")
	} else if _, isShimHost := host.(shimHost); !isShimHost {
		t.Fatalf("envHostForAdapter(remote, after container bind) = %T, want the shim fold", host)
	}
}

// TestContainerGate_SetContainerEnvHostContracts pins the bind contract:
// a nil host binds nothing (explicit bind is the only lane into the
// container dispatch), and an owned bind followed by Shutdown stops the
// host through the EnvHostStopper capability.
func TestContainerGate_SetContainerEnvHostContracts(t *testing.T) {
	m := NewSessionManager(nil)
	m.SetContainerEnvHost("container.empty", nil)
	if _, ok := m.envHostBoundForEnv("container.empty"); ok {
		t.Fatal("a nil container host was bound")
	}

	graph := &workflow.FSMGraph{
		Adapters:     map[string]*workflow.AdapterNode{"container.a": {Name: "a", Environment: "container.x"}},
		Environments: map[string]*workflow.EnvironmentNode{"container.x": {Name: "x", Type: "container"}},
	}
	m2 := NewSessionManager(nil)
	m2.SetGraph(graph)
	stub := &stubStoppingEnvHost{}
	m2.SetContainerEnvHost("container.x", stub)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m2.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !stub.stopped.Load() {
		t.Fatal("Shutdown did not stop the owned container supervisor host")
	}
}

// TestContainerGate_ShutdownOwnership pins the stop ownership boundary:
// this SessionManager stops only the supervisor hosts it OWNS (bound via
// SetContainerEnvHost); hosts borrowed from a parent stay the parent's to
// stop — a borrower's Shutdown must not stop hosts a sibling run still
// uses.
func TestContainerGate_ShutdownOwnership(t *testing.T) {
	parent := NewSessionManager(nil)
	parent.SetRemoteShimForEnv("remote.x", stubShim{})
	host := &stubStoppingEnvHost{}
	parent.SetContainerEnvHost("container.x", host)

	borrower := NewSessionManager(nil)
	borrower.BorrowRemoteProvisioningFrom(parent)
	if _, ok := borrower.envHostBoundForEnv("container.x"); !ok {
		t.Fatal("borrower did not receive the borrowed container host")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := borrower.Shutdown(ctx); err != nil {
		t.Fatalf("borrower Shutdown: %v", err)
	}
	if host.stopped.Load() {
		t.Fatal("borrower stopped a host borrowed from its parent")
	}
	if err := parent.Shutdown(ctx); err != nil {
		t.Fatalf("parent Shutdown: %v", err)
	}
	if !host.stopped.Load() {
		t.Fatal("parent Shutdown did not stop its owned container host")
	}
}

// stubEnvHost is a minimal envhost.Host for dispatch probes; every verb
// fails closed.
type stubEnvHost struct{}

func (stubEnvHost) Ensure(ctx context.Context, scope envhost.ScopeRef, defs []envhost.Definition) (envhost.Handles, error) {
	return nil, errors.New("stub host serves no children")
}

func (stubEnvHost) Release(ctx context.Context, scope envhost.ScopeRef, policy envhost.FatePolicy) error {
	return nil
}

func (stubEnvHost) Journal(scope envhost.ScopeRef, adapter string) envhost.JournalReader {
	return emptyJournalReader{}
}

func (stubEnvHost) Control(ctx context.Context, req *envhost.ControlRequest) (envhost.ControlResponse, error) {
	return envhost.ControlResponse{}, errors.New("stub host serves no control")
}

// stubStoppingEnvHost adds the EnvHostStopper capability the Shutdown
// collection requires for owned hosts and records the stop call.
type stubStoppingEnvHost struct {
	stopped atomic.Bool
}

func (s *stubStoppingEnvHost) Ensure(ctx context.Context, scope envhost.ScopeRef, defs []envhost.Definition) (envhost.Handles, error) {
	return nil, errors.New("stub host serves no children")
}

func (s *stubStoppingEnvHost) Release(ctx context.Context, scope envhost.ScopeRef, policy envhost.FatePolicy) error {
	return nil
}

func (s *stubStoppingEnvHost) Journal(scope envhost.ScopeRef, adapter string) envhost.JournalReader {
	return emptyJournalReader{}
}

func (s *stubStoppingEnvHost) Control(ctx context.Context, req *envhost.ControlRequest) (envhost.ControlResponse, error) {
	return envhost.ControlResponse{}, errors.New("stub host serves no control")
}

func (s *stubStoppingEnvHost) StopEnvHost(ctx context.Context) {
	s.stopped.Store(true)
}

// emptyJournalReader is the empty journal surface for stub hosts.
type emptyJournalReader struct{}

func (emptyJournalReader) Entries(ctx context.Context) ([]envhost.JournalEntry, error) {
	return nil, nil
}

// stubShim is a minimal RemoteShim for fold probes; every verb fails closed.
type stubShim struct{}

func (stubShim) WaitForHandle(ctx context.Context, adapterName, scope string) (Handle, error) {
	return nil, errors.New("stub shim serves no handles")
}

func (stubShim) WaitForFreshHandle(ctx context.Context, adapterName, scope string, stale Handle) (Handle, error) {
	return nil, errors.New("stub shim serves no handles")
}

func (stubShim) RegisterScope(scope, token string) {}

func (stubShim) UnregisterScope(scope string) {}

func (stubShim) CloseHandle(ctx context.Context, adapterName, scope string) error {
	return errors.New("stub shim closes nothing")
}

func (stubShim) ListenAddr() string { return "" }

func (stubShim) Stop(ctx context.Context) error { return nil }
