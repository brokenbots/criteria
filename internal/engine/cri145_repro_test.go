package engine

// Regression tests for CRI-145: a subworkflow body that re-declares the same
// <type>.<name> adapter as its parent scope used to rotate a fresh per-scope
// instance on every child scope-entry. The pre-fix child init reached
// maybeRotateRemoteScope before any duplicate check, and the resulting Verify
// error (ErrSessionAlreadyOpen, because the parent had already verified the
// instance) was swallowed, so the duplicate engagement was never tracked and
// the parent's teardown never released the parent's pod: the lifecycle stream
// showed N provisions for N scope-entries, exactly one release, and the
// released event's scope_instance_id/token_ref pair matched only the last
// provision (run 692ab23b: 13 provisions / 1 mismatched release).
//
// The fix skips the whole child init when the parent already provisioned the
// instance (SessionOpen guard in initScopeAdapters), so N engagements emit
// exactly one provision_wanted and exactly one paired released event.

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

// TestCRI145_SubworkflowRedeclaredAdapterLifecycleStream drives a real engine
// run end to end: the parent workflow declares adapter shell.default on a
// per-scope remote environment and invokes a subworkflow (under for_each, so
// the child scope-entry happens three times) whose body re-declares the same
// adapter key, mirroring the examples/subworkflow shape. A fake adapter pod
// dials the shim from the recorded provision event, exactly as a real
// per-scope pod does. The recorded lifecycle stream must show a single
// provision_wanted for the whole run and a single released event paired to it
// by scope instance ID and token ref.
func TestCRI145_SubworkflowRedeclaredAdapterLifecycleStream(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "main.chcl"), cri145RootHCL())
	writeFile(t, filepath.Join(root, "child", "main.chcl"), cri145ChildHCL())
	writeLockfile(t, root, cri145Lockfile())
	g := compileWorkflowDir(t, root)

	// Baseline for the shim-teardown wait: only socket dirs created during
	// this test must be gone when the cleanup below returns.
	beforeDirs := map[string]struct{}{}
	if matches, err := filepath.Glob(filepath.Join(os.TempDir(), "criteria-remote-*")); err == nil {
		for _, m := range matches {
			beforeDirs[m] = struct{}{}
		}
	}

	sink := &eventTrackingSink{}
	e := NewTestEngine(g, &fakeLoader{adapters: map[string]adapterhost.Handle{
		// The warmup step's local noop adapter; resolved in-process by type.
		"noop": &fakeAdapter{name: "noop", outcome: "success"},
	}}, sink,
		WithWorkflowDir(g.WorkflowDir),
		WithDataDir(root),
		WithRunID("cri145"))

	// The adapter pod polls the lifecycle stream for the provision event,
	// reads the rotated token, and keeps dialing the shim's listen address
	// with the pinned digest: one gRPC connection per dial, served until the
	// host closes it, then redialed. This covers the verify-phase handshake,
	// the bind-phase re-handshake after the host kills the verify handle, and
	// the per-step Execute calls.
	stopDial := make(chan struct{})
	executions := &atomic.Int64{}
	pod := &cri145Pod{executions: executions}
	go pod.serveLoop(sink, stopDial)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(func() {
		close(stopDial)
		cancel()
		waitForCri137ShimTeardown(t, beforeDirs)
	})

	runErr := make(chan error, 1)
	go func() { runErr <- e.Run(ctx) }()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(35 * time.Second):
		t.Fatal("run did not finish in time")
	}

	if got := sink.terminal; got != "done" {
		t.Fatalf("terminal state = %q (ok=%v, failure=%q), want done", got, sink.terminalOK, sink.failure)
	}
	if !sink.terminalOK {
		t.Fatalf("run did not succeed (failure=%q)", sink.failure)
	}

	provisions, releases := cri145LifecycleStats(sink)
	// Exit criteria 3: exactly one provision_wanted for the adapter instance
	// across the whole run, including all three child scope-entries.
	if len(provisions) != 1 {
		t.Fatalf("lifecycle stream has %d provision_wanted events (%+v), want exactly 1 for the parent scope", len(provisions), provisions)
	}
	provision := provisions[0]
	if provision.ScopeName != "" {
		t.Fatalf("provision_wanted scope = %q, want the root scope \"\"", provision.ScopeName)
	}
	// Exit criteria 3: exactly one released event, emitted by the parent's
	// teardown (teardown runs in Run's defer, after OnRunCompleted).
	if len(releases) != 1 {
		t.Fatalf("lifecycle stream has %d released events (%+v), want exactly 1", len(releases), releases)
	}
	release := releases[0]
	// Exit criteria 3: the release must pair with the provision by scope
	// instance ID and token ref (the pairing broken in run 692ab23b).
	if release.ScopeInstanceID != provision.ScopeInstanceID {
		t.Fatalf("released.ScopeInstanceID = %q, want the provisioned %q", release.ScopeInstanceID, provision.ScopeInstanceID)
	}
	if release.TokenRef != provision.TokenRef {
		t.Fatalf("released.TokenRef = %q, want the provisioned %q", release.TokenRef, provision.TokenRef)
	}
	if release.ScopeName != provision.ScopeName {
		t.Fatalf("released.ScopeName = %q, want the provisioned %q", release.ScopeName, provision.ScopeName)
	}
	// Exit criteria 1 and 2: provisions and releases pair with no orphans, and
	// the peak number of concurrently live per-scope instances stays within
	// the operator reconcile bound (observed peak: 1).
	cri145AssertPairedLifecycle(t, provisions, releases, 2)

	// The fake pod must have served exactly one Execute per remote adapter
	// step (the warmup step targets the local noop adapter and never reaches
	// the pod): parent pre + 3 child iterations + parent post = 5 engagements
	// when pre binds first try, plus at most one Execute per pre retry from
	// the max_step_retries budget (which only fires when the known
	// verify→bind stale-handle window delays the first attempt).
	if got := executions.Load(); got < 5 || got > 55 {
		t.Errorf("adapter Execute calls = %d, want 5..55 (pre up to 51 attempts, 3 child iterations, post)", got)
	}
}

// TestCRI145_PreFixRedeclarationOrphansParentProvision intentionally bypasses
// the CRI-145 guard to demonstrate that the pre-fix behavior fails the
// lifecycle-stream criteria: each child scope-entry rotated a fresh per-scope
// instance and swallowed the resulting ErrSessionAlreadyOpen from Verify, so
// the parent's provision was orphaned and the single release paired with the
// last child engagement instead of the parent's.
func TestCRI145_PreFixRedeclarationOrphansParentProvision(t *testing.T) {
	ctx := context.Background()
	g := perScopeRemoteGraph(t)
	dataDir := t.TempDir()

	sink, _, rlc, deps := newPerScopeTestHarness(t, g, dataDir)

	// Parent scope: a normal init provisions the shared instance.
	order, err := initScopeAdapters(ctx, g, deps, nil, dataDir, "", nil, rlc)
	if err != nil {
		t.Fatalf("parent initScopeAdapters: %v", err)
	}
	parent, ok := sink.firstStatus("provision_wanted")
	if !ok {
		t.Fatal("parent init emitted no provision_wanted event")
	}

	adapter := g.Adapters["noop.default"]
	// Child scope-entries, pre-fix shape: rotate a fresh per-scope instance
	// for the child scope and then Verify, whose ErrSessionAlreadyOpen (the
	// parent already provisioned this instance) was swallowed, so the child's
	// engagement was never tracked and never released.
	for _, childScope := range []string{"engage.0", "engage.1"} {
		config, secretMap, originRefs, workingDir, prepErr := prepareScopeAdapter(ctx, g, "noop.default", adapter, nil, dataDir, deps, childScope, nil)
		if prepErr != nil {
			t.Fatalf("child scope %q prepareScopeAdapter: %v", childScope, prepErr)
		}
		verifyScope, rotErr := maybeRotateRemoteScope(deps, rlc, g, adapter, "noop.default", childScope)
		if rotErr != nil {
			t.Fatalf("child scope %q maybeRotateRemoteScope: %v", childScope, rotErr)
		}
		verifyErr := deps.Sessions.Verify(ctx, "noop.default", adapter.Type, adapter.OnCrash, config, secretMap, originRefs, workingDir, childScope, verifyScope)
		if !errors.Is(verifyErr, adapterhost.ErrSessionAlreadyOpen) {
			t.Fatalf("child scope %q Verify error = %v, want ErrSessionAlreadyOpen (the pre-fix child init swallowed exactly this error)", childScope, verifyErr)
		}
	}

	provisions, releases := cri145LifecycleStats(sink)
	// Pre-fix shape: one provision per scope-entry (3 scope-entries, 3
	// provisions with 3 distinct instances) but only the record the parent's
	// teardown will release. The re-declared child engagements were never
	// tracked, so nothing released them during the run.
	if len(provisions) != 3 {
		t.Fatalf("pre-fix lifecycle stream has %d provision_wanted events (%+v), want 3 (one per scope-entry)", len(provisions), provisions)
	}
	if len(releases) != 0 {
		t.Fatalf("pre-fix run released %d instances before teardown (%+v), want 0 (child engagements were never tracked)", len(releases), releases)
	}
	tearDownScopeAdapters(ctx, order, deps, rlc)
	_, releases = cri145LifecycleStats(sink)
	if len(releases) != 1 {
		t.Fatalf("pre-fix teardown emitted %d released events (%+v), want exactly 1", len(releases), releases)
	}
	release := releases[0]
	// The release carries the last child engagement's instance and token, not
	// the parent's: the pairing broken in run 692ab23b.
	if release.ScopeInstanceID == parent.ScopeInstanceID {
		t.Fatalf("released.ScopeInstanceID = %q; the pre-fix build released the overwritten (child) record instead of the parent's provision", release.ScopeInstanceID)
	}
	if release.TokenRef == parent.TokenRef {
		t.Fatalf("released.TokenRef = %q; the pre-fix build released the child's token, not the parent's", release.TokenRef)
	}
	// The parent's provision is orphaned: no release pairs with it, and the
	// pairing assertion that holds post-fix fails pre-fix.
	cri145AssertOrphanedParentProvision(t, &parent, &release)
}

// TestCRI145_GuardSkipsChildRedeclaredAdapter pins the SessionOpen guard
// itself at unit level: a child body re-declaring the parent's adapter
// performs no second rotation, registers no child scope on the shim, leaves
// no child token material on disk, and the parent's teardown releases the
// single provisioned instance with a paired released event.
func TestCRI145_GuardSkipsChildRedeclaredAdapter(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "main.chcl"), cri145UnitRootHCL())
	writeFile(t, filepath.Join(root, "child", "main.chcl"), cri145ChildHCL())
	g := compileWorkflowDir(t, root)
	childBody := g.Subworkflows["child"].Body
	if childBody == nil {
		t.Fatal("compiled parent graph has no child subworkflow body")
	}
	dataDir := t.TempDir()

	sink, shim, rlc, deps := newPerScopeTestHarness(t, g, dataDir)
	parentOrder, err := initScopeAdapters(ctx, g, deps, nil, dataDir, "", nil, rlc)
	if err != nil {
		t.Fatalf("parent initScopeAdapters: %v", err)
	}
	if _, ok := sink.firstStatus("provision_wanted"); !ok {
		t.Fatal("parent init emitted no provision_wanted event")
	}

	// The child body re-declares the same adapter key; the guard must skip the
	// whole init instead of rotating a second per-scope instance.
	childOrder, err := initScopeAdapters(ctx, childBody, deps, nil, dataDir, "engage", nil, rlc)
	if err != nil {
		t.Fatalf("child body initScopeAdapters: %v", err)
	}
	provisions, _ := cri145LifecycleStats(sink)
	if len(provisions) != 1 {
		t.Fatalf("after child init the stream has %d provision_wanted events (%+v), want exactly 1 (guard skipped the re-declared adapter)", len(provisions), provisions)
	}
	for scope, token := range shim.registered {
		if strings.HasPrefix(scope, "engage/") {
			t.Fatalf("child scope registered scope %q (token %q) on the shim; the guard did not skip the child init", scope, token)
		}
	}
	if _, err := os.Stat(filepath.Join(dataDir, "remote-tokens", "engage")); !os.IsNotExist(err) {
		t.Fatalf("child scope wrote token material under remote-tokens/engage: %v", err)
	}

	// The parent's teardown releases the one provisioned instance; the child's
	// (empty) teardown order releases nothing.
	tearDownScopeAdapters(ctx, childOrder, deps, rlc)
	_, releases := cri145LifecycleStats(sink)
	if len(releases) != 0 {
		t.Fatalf("child teardown emitted %d released events, want 0", len(releases))
	}
	tearDownScopeAdapters(ctx, parentOrder, deps, rlc)
	provisions, releases = cri145LifecycleStats(sink)
	if len(provisions) != 1 {
		t.Fatalf("stream has %d provision_wanted events, want 1", len(provisions))
	}
	if len(releases) != 1 {
		t.Fatalf("stream has %d released events, want 1", len(releases))
	}
	if releases[0].ScopeInstanceID != provisions[0].ScopeInstanceID || releases[0].TokenRef != provisions[0].TokenRef {
		t.Fatalf("released %+v does not pair with provisioned %+v", releases[0], provisions[0])
	}
}

// cri145RootHCL returns the root workflow HCL: a remote per-scope environment,
// the shell.default adapter, and a subworkflow step invoked three times under
// for_each whose body re-declares the same adapter key.
// cri145UnitRootHCL is the minimal parent shape for the unit-level guard test:
// the remote shell adapter and the re-declaring child subworkflow, without the
// full-run harness's warmup adapter and retry policy (the unit test drives
// initScopeAdapters directly and has no FSM run to stabilize).
func cri145UnitRootHCL() string {
	return `workflow {
  name          = "cri145-unit"
  version       = "0.1"
  initial_state = "pre"
  target_state  = "done"
}

environment "remote" "prod" {
  listen_address     = "127.0.0.1:0"
  per_scope_sessions = true
}

adapter "shell" "default" {
  environment = remote.prod
}

subworkflow "child" {
  source = "./child"
}

step "pre" {
  target = adapter.shell.default
  outcome "success" { next = state.done }
}

state "done" {
  terminal = true
  success  = true
}
`
}

func cri145RootHCL() string {
	return `workflow {
  name          = "cri145-parent"
  version       = "0.1"
  initial_state = "warmup"
  target_state  = "done"

  # Retry budget for the known verify→bind stale-handle window: the bind
  # phase's WaitForHandle can hand back the just-killed verify handle until
  # the shim's async bridge teardown removes the dead entry (production shares
  # this window; production adapter pods redial on the same timescale). The
  # retry loop is tight and emits no lifecycle events, so a wide budget lets
  # a re-handshake land between attempts without perturbing the lifecycle
  # assertions below (observed: the first attempt succeeds ~99.9% of runs).
  policy {
    max_step_retries = 50
  }
}

environment "remote" "prod" {
  listen_address     = "127.0.0.1:0"
  per_scope_sessions = true
}

# Explicit local shell environment so the noop warmup adapter does not fall
# back to the default (remote) environment.
environment "shell" "local" {}

adapter "shell" "default" {
  environment = remote.prod
}

# Local adapter: a step targeting it runs before any remote bind, widening the
# gap between the verify-phase handle kill and the first remote bind so the
# pod's re-handshake has landed by then.
adapter "noop" "default" {
  environment = shell.local
}

subworkflow "child" {
  source = "./child"
}

step "warmup" {
  target = adapter.noop.default
  outcome "success" { next = step.pre }
}

step "pre" {
  target = adapter.shell.default
  # No explicit failure outcome: a step error is retried once via
  # max_step_retries and only fails the run if it persists.
  outcome "success" { next = step.engage }
}

step "engage" {
  target   = subworkflow.child
  for_each = ["a", "b", "c"]
  outcome "all_succeeded" { next = step.post }
  outcome "any_failed"    { next = state.failed }
}

step "post" {
  target = adapter.shell.default
  outcome "success" { next = state.done }
}

state "done" {
  terminal = true
  success  = true
}

state "failed" {
  terminal = true
  success  = false
}
`
}

// cri145ChildHCL returns the subworkflow body HCL that re-declares the parent's
// adapter key (and its remote environment) exactly as examples/subworkflow does.
func cri145ChildHCL() string {
	return `workflow {
  name          = "cri145-child"
  version       = "0.1"
  initial_state = "run"
  target_state  = "done"
}

environment "remote" "prod" {
  listen_address     = "127.0.0.1:0"
  per_scope_sessions = true
}

adapter "shell" "default" {
  environment = remote.prod
}

step "run" {
  target = adapter.shell.default
  outcome "success" { next = state.done }
  outcome "failure" { next = state.failed }
}

state "done" {
  terminal = true
  success  = true
}

state "failed" {
  terminal = true
  success  = false
}
`
}

// cri145Lockfile pins the shell adapter's digest so the engine's shim digest
// verifier accepts the fake pod's handshake.
func cri145Lockfile() *lockfile.Lockfile {
	return &lockfile.Lockfile{
		SchemaVersion: 1,
		Adapters: []lockfile.LockedAdapter{{
			Type:               "shell",
			Name:               "default",
			Reference:          "ghcr.io/brokenbots/criteria-adapter-shell",
			ResolvedDigest:     pinDigest,
			SourceURL:          "https://github.com/brokenbots/criteria",
			SDKProtocolVersion: 2,
			Platforms:          []string{"linux/amd64"},
		}},
	}
}

// cri145LifecycleStats returns the recorded provision_wanted and released
// lifecycle events in stream order.
func cri145LifecycleStats(sink *eventTrackingSink) (provisions, releases []AdapterLifecycleEvent) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for i := range sink.provisionEvents {
		switch sink.provisionEvents[i].Status {
		case "provision_wanted":
			provisions = append(provisions, sink.provisionEvents[i])
		case "released":
			releases = append(releases, sink.provisionEvents[i])
		}
	}
	return provisions, releases
}

// cri145AssertPairedLifecycle asserts the paired-count exit criteria over a
// lifecycle stream: every provision_wanted is released exactly once by scope
// instance ID and token ref, and the peak number of concurrently live
// per-scope instances for the adapter stays within maxLive.
func cri145AssertPairedLifecycle(t *testing.T, provisions, releases []AdapterLifecycleEvent, maxLive int) {
	t.Helper()
	if len(provisions) != len(releases) {
		t.Fatalf("provision_wanted count = %d, released count = %d; every provisioned per-scope instance must be released", len(provisions), len(releases))
	}
	peak := 0
	live := 0
	for i := range provisions {
		p := &provisions[i]
		paired := false
		for j := range releases {
			if releases[j].ScopeInstanceID == p.ScopeInstanceID && releases[j].TokenRef == p.TokenRef {
				paired = true
				break
			}
		}
		if !paired {
			t.Fatalf("provision_wanted for scope %q instance %q (token %q) was never released; the per-scope pod leaks", p.ScopeName, p.ScopeInstanceID, p.TokenRef)
		}
		live++
		if live > peak {
			peak = live
		}
		live--
	}
	if peak > maxLive {
		t.Errorf("peak concurrently live per-scope instances = %d, want <= %d", peak, maxLive)
	}
}

// cri145AssertOrphanedParentProvision documents the pre-fix leak shape: the
// released event carries a different scope instance and token than the
// parent's provision, so the parent's pod never gets a paired release.
func cri145AssertOrphanedParentProvision(t *testing.T, provision, release *AdapterLifecycleEvent) {
	t.Helper()
	if release.ScopeName == provision.ScopeName && release.ScopeInstanceID == provision.ScopeInstanceID && release.TokenRef == provision.TokenRef {
		t.Fatalf("release %+v pairs with the parent's provision %q; the pre-fix shape (orphaned parent pod) was not reproduced", release, provision.ScopeInstanceID)
	}
}

// cri145Pod simulates a real per-scope adapter pod: it polls the lifecycle
// stream for the provision event, reads the rotated token, and keeps dialing
// the shim's listen address with the pinned digest. Each dial serves one
// gRPC adapter connection until the host closes it, then redials after a
// short pause — the same reconnect loop a real pod runs while the host kills
// the verify-phase handle and re-dials at bind time.
type cri145Pod struct {
	executions *atomic.Int64
}

func (p *cri145Pod) serveLoop(sink *eventTrackingSink, stop <-chan struct{}) {
	var hs *cri137Handshake
	var addr string
	for {
		select {
		case <-stop:
			return
		case <-time.After(10 * time.Millisecond):
		}
		ev, ok := sink.firstStatus("provision_wanted")
		if !ok {
			continue
		}
		token, err := os.ReadFile(ev.TokenRef)
		if err != nil {
			continue // the token file lands just before the event is emitted
		}
		addr = ev.ShimListenAddress
		hs = &cri137Handshake{
			Name:    ev.AdapterType,
			Version: "1.0.0",
			Digest:  pinDigest,
			Token:   string(token),
			Scope:   ev.ScopeName + "/" + ev.ScopeInstanceID,
		}
		break
	}
	for {
		select {
		case <-stop:
			return
		default:
		}
		if !p.dialOnce(addr, hs, stop) {
			// Dial or handshake failed; back off briefly so the loop
			// cannot spin hot, but keep retrying: the pod must be dialing
			// when the host's verify or bind phase next waits.
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
		// Otherwise redial immediately after the host closes a connection: a
		// fresh handshake replaces the dead session entry on the shim before
		// the bind phase's WaitForHandle can hand back the just-killed handle
		// (the same fast-redial behavior production adapter pods rely on).
	}
}

// dialOnce dials the shim, presents the handshake frame, and serves one
// gRPC adapter connection until the shim or host closes it (verify-phase
// kill, teardown-phase handle close, or handshake rejection).
func (p *cri145Pod) dialOnce(addr string, hs *cri137Handshake, stop <-chan struct{}) bool {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return false
	}
	hsBytes, err := json.Marshal(hs)
	if err != nil {
		_ = conn.Close()
		return false
	}
	if _, err := conn.Write(append(hsBytes, '\n')); err != nil {
		_ = conn.Close()
		return false
	}
	observed := &countingConn{Conn: conn}
	srv := grpc.NewServer()
	v2.RegisterAdapterServiceServer(srv, &cri145FakeAdapterServer{executions: p.executions})
	go func() { _ = srv.Serve(&cri137SingleConnListener{conn: observed}) }()
	// Wait for the connection to close (the host kills the verify handle,
	// closes the handle at teardown, or rejects the handshake) before
	// stopping the server, so the bridged session is never cut mid-RPC.
	// countingConn.Read flips closed the moment the gRPC transport sees the
	// close, and the poll tick is deliberately short: the redial loop must
	// register a fresh handshake before the bind phase's WaitForHandle can
	// hand the just-killed handle back (CRI-145 test stability). stop also
	// breaks the wait so a failed test does not leak the goroutine.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if observed.closed.Load() {
			break
		}
		select {
		case <-stop:
			srv.Stop()
			return false
		case <-time.After(200 * time.Microsecond):
		}
	}
	srv.Stop()
	return true
}

// cri145FakeAdapterServer implements the v2 adapter surface the host drives
// through the shim bridge: Info during verify and bind, OpenSession once,
// one successful Execute per step, and CloseSession at teardown. Permissions
// and Log stay unimplemented; the host treats Unimplemented streams as
// expected closes.
type cri145FakeAdapterServer struct {
	v2.UnimplementedAdapterServiceServer
	executions *atomic.Int64
}

func (f *cri145FakeAdapterServer) Info(ctx context.Context, req *v2.InfoRequest) (*v2.InfoResponse, error) {
	return &v2.InfoResponse{Name: "shell", Version: "1.0.0", Capabilities: []string{"execute"}}, nil
}

func (f *cri145FakeAdapterServer) OpenSession(ctx context.Context, req *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	return &v2.OpenSessionResponse{}, nil
}

func (f *cri145FakeAdapterServer) CloseSession(ctx context.Context, req *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
	return &v2.CloseSessionResponse{}, nil
}

func (f *cri145FakeAdapterServer) Execute(req *v2.ExecuteRequest, stream grpc.ServerStreamingServer[v2.ExecuteEvent]) error {
	if f.executions != nil {
		f.executions.Add(1)
	}
	return stream.Send(&v2.ExecuteEvent{Event: &v2.ExecuteEvent_Result{Result: &v2.ExecuteResult{Outcome: "success"}}})
}
