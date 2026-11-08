package adapterhost

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/brokenbots/criteria/internal/envhost"
	"github.com/brokenbots/criteria/workflow"
)

// The container crash lane inherits nothing from the remote pod-recreate
// arm: a container session's transport close follows its declared on_crash
// policy, because the in-process supervisor has no pod to recreate — the
// implicit retry arm exists for remote environments whose host may be
// re-dialed by a recreated pod.

// containerCrashGraph declares the definition-identity adapter bound to a
// container environment — the supervisor shape of the M3 container lane.
func containerCrashGraph() *workflow.FSMGraph {
	return &workflow.FSMGraph{
		Adapters: map[string]*workflow.AdapterNode{
			"shell.develop": {Name: "shell.develop", Type: "shell", Environment: "container.sandbox"},
		},
		AdapterOrder: []string{"shell.develop"},
		Environments: map[string]*workflow.EnvironmentNode{
			"container.sandbox": {Type: "container", Name: "sandbox"},
		},
	}
}

// controlCountingEnvHost records the Control verbs a dispatch sends — the
// recreate arm is the only source of ControlEnsureFreshHandle, so a
// container session under on_crash=fail must produce none.
type controlCountingEnvHost struct {
	stubEnvHost

	mu    sync.Mutex
	kinds []envhost.ControlKind
}

func (h *controlCountingEnvHost) Control(_ context.Context, req *envhost.ControlRequest) (envhost.ControlResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.kinds = append(h.kinds, req.Kind)
	return envhost.ControlResponse{}, fmt.Errorf("stub host serves no control")
}

func (h *controlCountingEnvHost) ensureFreshCalls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, k := range h.kinds {
		if k == envhost.ControlEnsureFreshHandle {
			n++
		}
	}
	return n
}

// TestSessions_ContainerTransportClosedFollowsOnCrashPolicy pins the
// container boundary: a transport close on a container-bound session with
// a live supervisor host keeps the default on_crash=fail arm — the step
// fails, no session.respawned event is emitted, the container host
// receives no EnsureFreshHandle control, and the (still-alive) child
// handle is left untouched.
func TestSessions_ContainerTransportClosedFollowsOnCrashPolicy(t *testing.T) {
	sm := NewSessionManager(kb246Loader{})
	sm.SetGraph(containerCrashGraph())
	host := &controlCountingEnvHost{}
	sm.SetContainerEnvHost("container.sandbox", host)

	stale := newKB246Handle("stale", true, "")
	sess := &Session{Name: "shell.develop", Adapter: "shell", handle: stale, ScopeInstanceID: "run-1"}
	sm.mu.Lock()
	sm.sessions["shell.develop"] = sess
	sm.mu.Unlock()
	sess.noteActivity()
	coll := &adapterEventCollector{}

	result, err := sm.Execute(context.Background(), "shell.develop", &workflow.StepNode{Name: "get_branch_name"}, coll, nil)

	var crashErr *SessionCrashError
	if !errors.As(err, &crashErr) {
		t.Fatalf("Execute err = %v, want *SessionCrashError (declared on_crash=fail)", err)
	}
	if result.Outcome != "failure" {
		t.Errorf("result.Outcome = %q, want failure", result.Outcome)
	}
	if _, ok := coll.first("session.respawned"); ok {
		t.Errorf("session.respawned event must not be emitted for a container session under on_crash=fail")
	}
	if calls := host.ensureFreshCalls(); calls != 0 {
		t.Errorf("container host EnsureFreshHandle controls = %d, want none (no pod to recreate)", calls)
	}
	if kills := stale.killCount(); kills != 0 {
		t.Errorf("child Kill() calls = %d, want none (the live child keeps its in-flight state)", kills)
	}
	if cur, _ := sm.lookup("shell.develop"); cur.handle != Handle(stale) {
		t.Errorf("session handle was swapped (%T), want the untouched stale handle", cur.handle)
	}
}

// TestSessions_RemoteEnvHostForAdapterBoundsRetryArm pins the retry arm's
// resolution: REMOTE-bound with a served host resolves; remote with no
// bound host does not (the pre-carve condition); container-bound adapters
// never resolve through the remote arm — not even with a bound supervisor
// host; local environments never do.
func TestSessions_RemoteEnvHostForAdapterBoundsRetryArm(t *testing.T) {
	t.Run("served_remote_and_non_remote_negatives", func(t *testing.T) {
		graph := &workflow.FSMGraph{
			Adapters: map[string]*workflow.AdapterNode{
				"remote.shimmed": {Name: "shimmed", Type: "shell", Environment: "remote.ops"},
				"container.a":    {Name: "a", Type: "shell", Environment: "container.sandbox"},
				"other.local":    {Name: "local", Type: "shell", Environment: "local.dev"},
			},
			AdapterOrder: []string{"remote.shimmed", "container.a", "other.local"},
			Environments: map[string]*workflow.EnvironmentNode{
				"remote.ops":        {Type: "remote", Name: "ops"},
				"container.sandbox": {Type: "container", Name: "sandbox"},
				"local.dev":         {Type: "local", Name: "dev"},
			},
		}
		m := NewSessionManager(kb246Loader{})
		m.SetGraph(graph)
		m.SetEnvHostForEnv("remote.ops", NewShimHost(stubShim{}))

		if host, ok := m.remoteEnvHostForAdapter("remote.shimmed"); !ok {
			t.Fatal("remoteEnvHostForAdapter lost the remote adapter with a served host")
		} else if _, isShim := host.(shimHost); !isShim {
			t.Fatalf("remoteEnvHostForAdapter(shimmed) = %T, want the shim fold", host)
		}
		if _, ok := m.remoteEnvHostForAdapter("container.a"); ok {
			t.Fatal("remoteEnvHostForAdapter resolved a container adapter with no host bound")
		}

		m.SetContainerEnvHost("container.sandbox", stubEnvHost{})
		if _, ok := m.remoteEnvHostForAdapter("container.a"); ok {
			t.Fatal("container-bound adapter resolved through the remote arm after the supervisor bind")
		}
		if _, ok := m.remoteEnvHostForAdapter("other.local"); ok {
			t.Fatal("remoteEnvHostForAdapter resolved a local adapter")
		}
	})

	t.Run("unserved_remote_stays_unarmed", func(t *testing.T) {
		// No shim and no host is registered for this manager: the remote
		// adapter must not arm the recreate retry (the pre-carve condition).
		graph := &workflow.FSMGraph{
			Adapters: map[string]*workflow.AdapterNode{
				"remote.bare": {Name: "bare", Type: "shell", Environment: "remote.isolated"},
			},
			AdapterOrder: []string{"remote.bare"},
			Environments: map[string]*workflow.EnvironmentNode{
				"remote.isolated": {Type: "remote", Name: "isolated"},
			},
		}
		m := NewSessionManager(kb246Loader{})
		m.SetGraph(graph)

		if _, ok := m.remoteEnvHostForAdapter("remote.bare"); ok {
			t.Fatal("remote adapter with no bound host must not arm the recreate retry (the pre-carve condition)")
		}
	})
}
