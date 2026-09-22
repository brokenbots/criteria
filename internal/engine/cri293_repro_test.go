package engine

// cri293_repro_test.go — CRI-293: a local run declares two remote
// environments sharing one listen_address. The engine binds one shim per
// environment in a single process, so the second bind failed with EADDRINUSE
// and the run died at startup. With local shim isolation (wired via
// WithLocalShimIsolation on local run entrypoints) each colliding
// environment's shim binds its own auto-chosen loopback port and both remain
// reachable by their adapters. Server mode keeps the declared address (no
// isolation), and a fixed port occupied by a foreign process still fails with
// the bind error naming the address.

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"

	"github.com/brokenbots/criteria/internal/adapter/environment/remote"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

// cri293RemoteBody builds a remote environment body declaring the given TCP
// listen address in insecure mode (loopback needs no accept token).
func cri293RemoteBody(t *testing.T, listen string) hcl.Body {
	t.Helper()
	parser := hclparse.NewParser()
	file, diags := parser.ParseHCL([]byte(fmt.Sprintf(`
listen_address = %q
insecure = true
`, listen)), "remote.hcl")
	if diags.HasErrors() {
		t.Fatalf("parse remote body: %s", diags)
	}
	return file.Body
}

// cri293Env returns a remote environment node declaring the given address.
func cri293Env(t *testing.T, name, listen string) *workflow.EnvironmentNode {
	t.Helper()
	return &workflow.EnvironmentNode{
		Type:    "remote",
		Name:    name,
		Process: &workflow.ProcessPolicy{Exec: []string{"*"}},
		RawBody: cri293RemoteBody(t, listen),
	}
}

// cri293Graph returns a workflow graph with two remote environments (the
// workstream_handler_v1 shape: worktree + primary) that may share a listen
// address, pinned to a noop adapter digest.
func cri293Graph(t *testing.T, listen string) *workflow.FSMGraph {
	t.Helper()
	return &workflow.FSMGraph{
		PinSet: &lockfile.Lockfile{
			Adapters: []lockfile.LockedAdapter{
				{Type: "noop", ResolvedDigest: "sha256:abcd1234"},
			},
		},
		Environments: map[string]*workflow.EnvironmentNode{
			"remote.worktree": cri293Env(t, "worktree", listen),
			"remote.primary":  cri293Env(t, "primary", listen),
		},
	}
}

// newCRI293Sessions builds the session manager the engine test harness uses,
// mirroring the Run wiring (graph pin set applied to sessions).
func (e *Engine) newCRI293Sessions(t *testing.T) *adapterhost.SessionManager {
	t.Helper()
	sessions := adapterhost.NewSessionManager(nil)
	e.setLockfileOnSessions(sessions)
	return sessions
}

func TestMaybeStartRemoteShim_CRI293_CollidingAddressesIsolatedLocally(t *testing.T) {
	const declared = "127.0.0.1:7779"
	g := cri293Graph(t, declared)
	eng := New(g, nil, &fakeSink{}, WithLocalShimIsolation())
	sessions := eng.newCRI293Sessions(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := eng.maybeStartRemoteShim(ctx, sessions); err != nil {
		t.Fatalf("maybeStartRemoteShim with isolation: %v", err)
	}
	t.Cleanup(func() {
		_ = sessions.Shutdown(context.WithoutCancel(ctx))
	})

	shimWorktree, ok := sessions.RemoteShimForEnv("remote.worktree").(*remote.Shim)
	if !ok {
		t.Fatalf("worktree shim is %T, want *remote.Shim", sessions.RemoteShimForEnv("remote.worktree"))
	}
	shimPrimary, ok := sessions.RemoteShimForEnv("remote.primary").(*remote.Shim)
	if !ok {
		t.Fatalf("primary shim is %T, want *remote.Shim", sessions.RemoteShimForEnv("remote.primary"))
	}

	addrWorktree := shimWorktree.ListenAddr()
	addrPrimary := shimPrimary.ListenAddr()
	if addrWorktree == addrPrimary {
		t.Fatalf("both environment shims bound %q; want distinct auto-chosen ports", addrWorktree)
	}
	for name, addr := range map[string]string{"worktree": addrWorktree, "primary": addrPrimary} {
		if strings.HasSuffix(addr, ":7779") {
			t.Errorf("%s shim bound the declared port %q; want an auto-chosen port", name, addr)
		}
	}

	// Both shims must be reachable: a fake adapter phones home to each one
	// with a digest matching the pin set, and each shim hands back a handle.
	var infoCalls atomic.Int64
	stop := make(chan struct{})
	defer close(stop)
	hs := &cri137Handshake{Name: "noop", Version: "1.0.0", Digest: "sha256:abcd1234"}
	go dialCri137AdapterLoop(addrWorktree, hs, stop, &infoCalls)
	go dialCri137AdapterLoop(addrPrimary, hs, stop, &infoCalls)
	for name, shim := range map[string]*remote.Shim{"worktree": shimWorktree, "primary": shimPrimary} {
		if _, err := shim.WaitForHandle(ctx, "noop", ""); err != nil {
			t.Errorf("%s shim: wait for adapter handle: %v", name, err)
		}
	}
}

func TestMaybeStartRemoteShim_CRI293_CollidingAddressesStillCollideWithoutIsolation(t *testing.T) {
	// Server-mode behavior: without WithLocalShimIsolation the declared
	// address is honored as-is and the second bind still fails with
	// EADDRINUSE. That is the pre-fix behavior server runs rely on (each
	// environment's shim lives in its own adapter pod there, so the shared
	// port never collides on the wire).
	const declared = "127.0.0.1:7779"
	g := cri293Graph(t, declared)
	eng := New(g, nil, &fakeSink{})
	sessions := eng.newCRI293Sessions(t)

	// When the second bind fails, the first environment's shim has already
	// started and must be torn down with the run (the engine's deferred
	// sessions.Shutdown does this in production).
	defer func() {
		if shim, ok := sessions.RemoteShim().(*remote.Shim); ok {
			_ = shim.Stop(context.Background())
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := eng.maybeStartRemoteShim(ctx, sessions)
	if err == nil {
		t.Fatal("maybeStartRemoteShim without isolation: want collision error, got nil")
	}
	// Environment iteration is sorted, so "remote.primary" binds first and
	// "remote.worktree" fails second; the error names both the environment
	// and the colliding address — the signature reported in CRI-293.
	want := fmt.Sprintf("remote environment %q: remote shim: listen %q:", "worktree", declared)
	if !strings.Contains(err.Error(), want) {
		t.Errorf("collision error = %q, want prefix %q", err.Error(), want)
	}
	if !strings.Contains(err.Error(), "bind: address already in use") {
		t.Errorf("collision error = %q, want bind: address already in use", err.Error())
	}
}

func TestMaybeStartRemoteShim_CRI293_ForeignProcessCollisionKeepsAddressedError(t *testing.T) {
	// Isolation covers collisions BETWEEN the workflow's environments. A
	// fixed port occupied by a foreign process is not isolatable: the run
	// must still fail, with the error naming the occupied address so the
	// operator can find the conflicting listener.
	foreign, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("foreign listener: %v", err)
	}
	defer foreign.Close()
	declared := foreign.Addr().String()

	g := &workflow.FSMGraph{
		PinSet: &lockfile.Lockfile{
			Adapters: []lockfile.LockedAdapter{
				{Type: "noop", ResolvedDigest: "sha256:abcd1234"},
			},
		},
		Environments: map[string]*workflow.EnvironmentNode{
			"remote.primary": cri293Env(t, "primary", declared),
		},
	}
	eng := New(g, nil, &fakeSink{}, WithLocalShimIsolation())
	sessions := eng.newCRI293Sessions(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	runErr := eng.maybeStartRemoteShim(ctx, sessions)
	if runErr == nil {
		t.Fatal("maybeStartRemoteShim on an occupied fixed port: want bind error, got nil")
	}
	want := fmt.Sprintf("remote environment %q: remote shim: listen %q:", "primary", declared)
	if !strings.Contains(runErr.Error(), want) {
		t.Errorf("foreign-process error = %q, want prefix %q", runErr.Error(), want)
	}
	if !strings.Contains(runErr.Error(), "bind: address already in use") {
		t.Errorf("foreign-process error = %q, want bind: address already in use", runErr.Error())
	}
}

// TestIsolatedShimEnvs pins the collision-classification rules: only
// environments sharing the SAME fixed TCP port isolate; port-0 addresses
// cannot collide (each bind gets its own OS-assigned port) and unix-socket
// paths keep today's bind-failure error path.
func TestIsolatedShimEnvs(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "cri293.sock")
	sockEnv := func(t *testing.T) *workflow.EnvironmentNode {
		parser := hclparse.NewParser()
		file, diags := parser.ParseHCL([]byte(fmt.Sprintf(`
listen_address = %q
insecure = true
`, sock)), "remote.hcl")
		if diags.HasErrors() {
			t.Fatalf("parse remote body: %s", diags)
		}
		return &workflow.EnvironmentNode{Type: "remote", Name: "x", RawBody: file.Body}
	}

	t.Run("same fixed port isolates both", func(t *testing.T) {
		envs := map[string]*workflow.EnvironmentNode{
			"remote.a": cri293Env(t, "a", "127.0.0.1:7779"),
			"remote.b": cri293Env(t, "b", "127.0.0.1:7779"),
		}
		got := isolatedShimEnvs(envs)
		if !got["remote.a"] || !got["remote.b"] {
			t.Errorf("isolatedShimEnvs = %v, want both isolated", got)
		}
	})
	t.Run("distinct ports do not isolate", func(t *testing.T) {
		envs := map[string]*workflow.EnvironmentNode{
			"remote.a": cri293Env(t, "a", "127.0.0.1:7779"),
			"remote.b": cri293Env(t, "b", "127.0.0.1:7780"),
		}
		if got := isolatedShimEnvs(envs); len(got) != 0 {
			t.Errorf("isolatedShimEnvs = %v, want none", got)
		}
	})
	t.Run("shared port zero does not isolate", func(t *testing.T) {
		envs := map[string]*workflow.EnvironmentNode{
			"remote.a": cri293Env(t, "a", "127.0.0.1:0"),
			"remote.b": cri293Env(t, "b", "127.0.0.1:0"),
		}
		if got := isolatedShimEnvs(envs); len(got) != 0 {
			t.Errorf("isolatedShimEnvs = %v, want none", got)
		}
	})
	t.Run("shared unix socket path does not isolate", func(t *testing.T) {
		envs := map[string]*workflow.EnvironmentNode{
			"remote.a": sockEnv(t),
			"remote.b": sockEnv(t),
		}
		if got := isolatedShimEnvs(envs); len(got) != 0 {
			t.Errorf("isolatedShimEnvs = %v, want none", got)
		}
	})
	t.Run("unparseable body does not isolate", func(t *testing.T) {
		envs := map[string]*workflow.EnvironmentNode{
			"remote.a": {Type: "remote", Name: "a", RawBody: nil},
			"remote.b": cri293Env(t, "b", "127.0.0.1:7779"),
		}
		if got := isolatedShimEnvs(envs); len(got) != 0 {
			t.Errorf("isolatedShimEnvs = %v, want none", got)
		}
	})
}

func TestHasFixedPort(t *testing.T) {
	for _, tc := range []struct {
		listen string
		want   bool
	}{
		{"0.0.0.0:7778", true},
		{"127.0.0.1:7779", true},
		{"[::1]:7779", true},
		{"127.0.0.1:0", false},
		{"localhost:0", false},
		{"/tmp/criteria.sock", false},
		{"", false},
	} {
		if got := hasFixedPort(tc.listen); got != tc.want {
			t.Errorf("hasFixedPort(%q) = %v, want %v", tc.listen, got, tc.want)
		}
	}
}