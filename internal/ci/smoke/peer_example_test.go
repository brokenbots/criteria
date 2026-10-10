package smoke

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/engine"
	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

// This file mirrors examples/peer-remote/: the compose stack there runs a
// criteria host with a per_scope_sessions remote environment plus peer
// containers dialed in from images/remote-adapters/Dockerfile.peer. These
// tests exercise the same building blocks without Docker: real `criteria
// peer` subprocesses against a real shim, driven by the lifecycle events the
// compose operator would consume.

// TestPeerExample_WorkflowShape compiles the shipped example workflow and
// asserts the shape the README documents: a per-scope remote environment,
// two adapter types bound to it, and the respawn policy on the crash-demo
// adapter. Ungated; it runs on every `go test` invocation.
func TestPeerExample_WorkflowShape(t *testing.T) {
	moduleRoot := findModuleRoot(t)
	contents, err := os.ReadFile(filepath.Join(moduleRoot, "examples", "peer-remote", "workflow.hcl"))
	if err != nil {
		t.Fatalf("read example workflow: %v", err)
	}
	raw := string(contents)
	if !strings.Contains(raw, "per_scope_sessions = true") {
		t.Error("example workflow must set per_scope_sessions = true on the remote environment")
	}
	if !strings.Contains(raw, `on_crash = "respawn"`) {
		t.Errorf("example workflow must declare on_crash = %q for the crash demo", "respawn")
	}

	shimAddr := pickFreeAddr(t)
	spec := parseWorkflow(t, strings.Replace(raw, "0.0.0.0:7778", shimAddr, 1))
	graph := compileWorkflow(t, spec)

	for _, key := range []string{"shell.main", "noop.gate"} {
		a, ok := graph.Adapters[key]
		if !ok {
			t.Fatalf("example workflow missing adapter %q; got %v", key, exampleAdapterKeys(graph))
		}
		if a.Environment != "remote.default" {
			t.Errorf("adapter %q bound to environment %q, want remote.default", key, a.Environment)
		}
	}
	if a := graph.Adapters["shell.main"]; a.OnCrash != "respawn" {
		t.Errorf("shell.main on_crash = %q, want respawn", a.OnCrash)
	}
	for _, step := range []string{"greet", "deliberate_failure", "recover", "crash_demo", "isolation_gate"} {
		if _, ok := graph.Steps[step]; !ok {
			t.Errorf("example workflow missing step %q", step)
		}
	}
}

func exampleAdapterKeys(graph *workflow.FSMGraph) []string {
	out := make([]string, 0, len(graph.Adapters))
	for k := range graph.Adapters {
		out = append(out, k)
	}
	return out
}

// TestPeerExample_ComposeStackParses parses the shipped compose stack from
// disk and asserts the peer-operator anchor merge resolves for both peer
// services. YAML rejects forward alias references (anchor declared after its
// users), which would make `docker compose up` fail before a single peer is
// built or dialed, so an unparseable stack must fail this test.
func TestPeerExample_ComposeStackParses(t *testing.T) {
	moduleRoot := findModuleRoot(t)
	contents, err := os.ReadFile(filepath.Join(moduleRoot, "examples", "peer-remote", "docker-compose.yml"))
	if err != nil {
		t.Fatalf("read compose file: %v", err)
	}
	var doc struct {
		Services map[string]struct {
			Entrypoint []string `yaml:"entrypoint"`
			Command    []string `yaml:"command"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(contents, &doc); err != nil {
		t.Fatalf("examples/peer-remote/docker-compose.yml does not parse as YAML (docker compose would fail): %v", err)
	}
	for _, name := range []string{"criteria-host", "shell-peer", "noop-peer"} {
		if _, ok := doc.Services[name]; !ok {
			t.Errorf("compose stack missing service %q", name)
		}
	}
	for _, name := range []string{"shell-peer", "noop-peer"} {
		svc, ok := doc.Services[name]
		if !ok {
			continue
		}
		if len(svc.Entrypoint) != 2 || svc.Entrypoint[0] != "sh" || svc.Entrypoint[1] != "-c" {
			t.Errorf("service %q entrypoint = %v, want the merged [sh -c] peer-operator entrypoint", name, svc.Entrypoint)
		}
		if len(svc.Command) != 1 || !strings.Contains(svc.Command[0], "exec criteria peer") {
			t.Errorf("service %q command = %v, want the merged peer-operator dial loop", name, svc.Command)
		}
	}
}

// TestPeerExample_CompletesThroughIsolationGate asserts the shipped example
// can reach its done state through every step, including the multi-adapter
// isolation step, and that the crash-demo step is runnable without an
// operator. The shell adapter reports timeout expiry as the "failure"
// outcome (not a crash), so a crash_demo command that runs past its step
// timeout routes the run to state.failed and strands isolation_gate — which
// is exactly the bug this test guards against. Ungated; runs on every
// `go test` invocation.
func TestPeerExample_CompletesThroughIsolationGate(t *testing.T) {
	moduleRoot := findModuleRoot(t)
	contents, err := os.ReadFile(filepath.Join(moduleRoot, "examples", "peer-remote", "workflow.hcl"))
	if err != nil {
		t.Fatalf("read example workflow: %v", err)
	}
	spec := parseWorkflow(t, string(contents))
	graph := compileWorkflow(t, spec)

	if graph.TargetState != "done" {
		t.Errorf("target_state = %q, want done", graph.TargetState)
	}
	done, ok := graph.States["done"]
	if !ok || !done.Terminal || !done.Success {
		t.Errorf("state \"done\" = %+v (present=%t), want a terminal success state", done, ok)
	}

	// The documented happy-path chain: greet succeeds, deliberate_failure is
	// routed as a failure outcome, recover succeeds, then the crash demo and
	// the per-scope isolation gate complete the run in done.
	chain := [][3]string{
		{"greet", "success", "deliberate_failure"},
		{"deliberate_failure", "failure", "recover"},
		{"recover", "success", "crash_demo"},
		{"crash_demo", "success", "isolation_gate"},
		{"isolation_gate", "success", "done"},
	}
	for _, edge := range chain {
		step, ok := graph.Steps[edge[0]]
		if !ok {
			t.Errorf("example workflow missing step %q", edge[0])
			continue
		}
		outcome := step.Outcomes[edge[1]]
		if outcome == nil {
			t.Errorf("step %q is missing outcome %q", edge[0], edge[1])
			continue
		}
		if outcome.Next != edge[2] {
			t.Errorf("step %q outcome %q routes to %q, want %q (isolation_gate must stay reachable)",
				edge[0], edge[1], outcome.Next, edge[2])
		}
	}
	if gate := graph.Steps["isolation_gate"]; gate != nil && gate.AdapterRef != "noop.gate" {
		t.Errorf("isolation_gate targets %q, want the second adapter noop.gate", gate.AdapterRef)
	}

	// crash_demo must be runnable unattended: its command (sleep <n>) must
	// finish strictly inside the step timeout the shell adapter enforces.
	cd := graph.Steps["crash_demo"]
	if cd == nil {
		t.Fatal("example workflow missing step crash_demo")
	}
	sleepRe := regexp.MustCompile(`^sleep (\d+)$`)
	match := sleepRe.FindStringSubmatch(strings.TrimSpace(cd.Input["command"]))
	if match == nil {
		t.Fatalf("crash_demo command %q must be `sleep <seconds>` so the test can check it fits the timeout", cd.Input["command"])
	}
	sleepSeconds, err := strconv.Atoi(match[1])
	if err != nil {
		t.Fatalf("parse crash_demo sleep duration: %v", err)
	}
	timeoutStr := strings.TrimSpace(cd.Input["timeout"])
	if timeoutStr == "" {
		// The shell adapter's default step timeout is 5m.
		timeoutStr = "5m"
	}
	timeoutDur, err := time.ParseDuration(timeoutStr)
	if err != nil {
		t.Fatalf("parse crash_demo timeout %q: %v", timeoutStr, err)
	}
	if remaining := timeoutDur - time.Duration(sleepSeconds)*time.Second; remaining <= 0 {
		t.Errorf("crash_demo sleep %ds does not fit inside timeout %s (remaining %s): the shell adapter would report timeout expiry as the \"failure\" outcome and the run would terminate in state.failed before isolation_gate", sleepSeconds, timeoutStr, remaining)
	}
}

// TestPeerSmoke_PerScopeMultiAdapter drives a per_scope_sessions remote
// environment with two adapter types and the operator's (scope, pair) peer
// contract from examples/peer-remote: ONE peer per scope instance, hosting
// EVERY adapter kind the environment declares behind that scope's single
// dial. The CRITERIA_REMOTE_ADAPTERS manifest carries the sorted kind set
// and each hosted kind is staged behind its own CRITERIA_ADAPTER_<KIND>_*
// override. KB-213's fail-closed accept-time coverage check rejects any
// per-scope dial whose hosted set does not cover the environment's full
// declared-adapter set, so single-kind per-scope peers cannot satisfy this
// environment — the rejection itself is pinned by
// TestPeerSmoke_PerScopeChildSetRejection.
//
// The test asserts that each adapter instance is provisioned with its own
// scope instance (distinct scope keys and tokens), that a routed adapter
// failure outcome still lets the run succeed, and that the whole workflow
// completes through the peers.
//
// Gated by CRITERIA_PEER_E2E=1.
func TestPeerSmoke_PerScopeMultiAdapter(t *testing.T) {
	if os.Getenv("CRITERIA_PEER_E2E") != "1" {
		t.Skip("set CRITERIA_PEER_E2E=1 to run peer smoke tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	moduleRoot := findModuleRoot(t)
	criteriaBin := buildCriteriaBinary(t, moduleRoot)
	noopBin := buildNoopSmokeBinary(t, moduleRoot)
	failBin := buildFailSmokeBinary(t, moduleRoot)
	noopDigest := sha256OfFile(t, noopBin)
	failDigest := sha256OfFile(t, failBin)

	shimAddr := pickFreeAddr(t)
	workflowDir := t.TempDir()

	graph, lf := perScopeMultiAdapterFixture(t, shimAddr, noopDigest, failDigest)

	sink := newCapturingSink()
	eng := engine.New(graph, adapterhost.NewLoader(), sink,
		engine.WithWorkflowDir(workflowDir),
		engine.WithLockfile(lf),
		engine.WithDataDir(workflowDir),
	)

	engDone := make(chan error, 1)
	go func() { engDone <- eng.Run(ctx) }()

	// The operator's (scope, pair) peer contract: one criteria peer per
	// scope instance, hosting EVERY adapter kind the environment declares
	// behind that scope's single dial. The engine provisions adapters lazily
	// as steps execute and blocks each step until its scope's peer dials
	// in, so dial each scope's pair peer as its provision event arrives.
	// The hosted set is the sorted kind set (fail, noop): manifest order is
	// free (the dial identity is simply the first declared child), sorted
	// order mirrors the operator's stage order.
	peerCancels := make([]context.CancelFunc, 0, 4)
	defer func() {
		for _, c := range peerCancels {
			c()
		}
	}()
	prov := map[string]provisionInfo{}
	peerLogs := map[string]*bytes.Buffer{}
	peerBins := map[string]string{"noop": noopBin, "fail": failBin}
	peerDigests := map[string]string{"noop": noopDigest, "fail": failDigest}
	for _, adapterType := range []string{"noop", "fail"} {
		p := waitForProvisions(t, sink, []string{adapterType}, 45*time.Second)[adapterType]
		prov[adapterType] = p
		scopeKey := p.ScopeName + "/" + p.ScopeInstanceID
		if p.ScopeName == "" {
			scopeKey = "/" + p.ScopeInstanceID
		}
		_, logs, pc := startPerScopePeer(ctx, t, criteriaBin, p.ShimListenAddress,
			scopeKey, p.Token,
			[]hostedChild{
				{kind: "noop", binary: peerBins["noop"], digest: peerDigests["noop"]},
				{kind: "fail", binary: peerBins["fail"], digest: peerDigests["fail"]},
			})
		peerCancels = append(peerCancels, pc)
		peerLogs[p.ScopeInstanceID] = logs
	}

	select {
	case err := <-engDone:
		if err != nil {
			dumpPeerLogsMap(t, peerLogs)
			t.Fatalf("engine run: %v", err)
		}
	case <-ctx.Done():
		dumpPeerLogsMap(t, peerLogs)
		t.Fatal("timeout waiting for engine run to finish")
	}
	for _, c := range peerCancels {
		c()
	}
	dumpPeerLogsMap(t, peerLogs)

	if !sink.success {
		t.Fatalf("workflow did not complete successfully: terminal=%q", sink.terminal)
	}

	// Per-scope isolation: each adapter instance got its own scope instance.
	if prov["noop"].ScopeInstanceID == prov["fail"].ScopeInstanceID {
		t.Errorf("adapter instances shared one scope instance %q; per_scope_sessions must isolate them", prov["noop"].ScopeInstanceID)
	}
	if prov["noop"].Token == prov["fail"].Token {
		t.Error("adapter instances shared one accept token; per_scope_sessions must rotate per scope")
	}
	if prov["noop"].ShimListenAddress != shimAddr || prov["fail"].ShimListenAddress != shimAddr {
		t.Errorf("provision events advertised shim %q/%q, want %q",
			prov["noop"].ShimListenAddress, prov["fail"].ShimListenAddress, shimAddr)
	}

	// Failure routing: the fail adapter's step must have produced a failure
	// outcome that routed to the recover step (otherwise the run would have
	// ended in a failure terminal state above).
	if outcome, ok := sink.stepOutcome("break"); !ok || outcome != "failure" {
		t.Errorf("step break outcome = %q (seen=%v), want routed failure", outcome, ok)
	}
}

// TestPeerSmoke_PerScopeChildSetRejection pins the fail-closed accept-time
// coverage check (KB-213) end-to-end, against the exact fixture shape that
// drifted: a legacy single-adapter per-scope dial on a two-adapter
// per-scope environment. The dial is legitimate in every verified dimension
// (binary digest, version, scope token), but the hosted child set — one
// kind — does not cover the environment's full declared-adapter set, so
// acceptance must fail closed: the shim rejects the dial with the typed
// PeerChildSetError on every redial, the rejected peer's phone-home loop
// keeps reporting the lost connection, and the engine's per-scope session
// wait never resolves.
//
// Gated by CRITERIA_PEER_E2E=1.
func TestPeerSmoke_PerScopeChildSetRejection(t *testing.T) {
	if os.Getenv("CRITERIA_PEER_E2E") != "1" {
		t.Skip("set CRITERIA_PEER_E2E=1 to run peer smoke tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	moduleRoot := findModuleRoot(t)
	criteriaBin := buildCriteriaBinary(t, moduleRoot)
	noopBin := buildNoopSmokeBinary(t, moduleRoot)
	failBin := buildFailSmokeBinary(t, moduleRoot)
	noopDigest := sha256OfFile(t, noopBin)
	failDigest := sha256OfFile(t, failBin)

	shimAddr := pickFreeAddr(t)
	workflowDir := t.TempDir()

	graph, lf := perScopeMultiAdapterFixture(t, shimAddr, noopDigest, failDigest)

	sink := newCapturingSink()
	engCtx, engCancel := context.WithCancel(ctx)
	defer engCancel()
	engDone := make(chan error, 1)
	go func() {
		eng := engine.New(graph, adapterhost.NewLoader(), sink,
			engine.WithWorkflowDir(workflowDir),
			engine.WithLockfile(lf),
			engine.WithDataDir(workflowDir),
		)
		engDone <- eng.Run(engCtx)
	}()

	// The drift shape: a single noop-kind peer with the matching scope
	// token, dialed as soon as the noop scope is provisioned.
	p := waitForProvisions(t, sink, []string{"noop"}, 45*time.Second)["noop"]
	scopeKey := p.ScopeName + "/" + p.ScopeInstanceID
	if p.ScopeName == "" {
		scopeKey = "/" + p.ScopeInstanceID
	}
	hostLogs := captureSlog(t)
	_, peerLogs, peerCancel := startSingleKindPeer(ctx, t, criteriaBin, p.ShimListenAddress,
		scopeKey, p.Token, "noop", noopBin, noopDigest)
	defer peerCancel()

	// The rejection is the typed child-set class, repeated on every
	// reconnect — never a token/digest/scope failure and never an accept.
	// Two occurrences prove the first dial and the first reconnect were
	// both rejected; a single one could hide a race in the redial loop.
	// Declared order is sorted, so the missing kind is spelled "fail" and
	// the hosted single kind is "noop".
	waitForCondition(t, 20*time.Second, func() bool {
		return hostLogs.count("remote shim accept failed",
			fmt.Sprintf("peer for adapter %q (scope %q) does not host declared adapters fail (hosted: noop)",
				"noop", scopeKey)) >= 2
	}, fmt.Sprintf("host shim never rejected the single-kind dial with a child-set failure; host log:\n%s", hostLogs.dump()))

	// Peer side: the rejected dial's phone-home loop keeps reporting the
	// lost connection (reject -> reconnect -> reject).
	if logs := peerLogs.String(); strings.Count(logs, "peer phone-home connection lost") < 2 {
		t.Errorf("peer log missing the reconnect loop after the typed rejection; peer output:\n%s", logs)
	}

	// Engine side: no per-scope session materializes, so the step wait
	// never resolves — the greet step records no outcome and the run
	// reaches neither the break step nor the done state.
	if outcome, ok := sink.stepOutcome("greet"); ok {
		t.Errorf("greet step recorded outcome %q; the rejected dial must never resolve the step wait", outcome)
	}
	if sink.success {
		t.Error("workflow completed despite the rejected child set")
	}

	// Nothing to salvage: tear the run down; the only goal was the typed
	// rejection above, and the run cannot make progress past the blocked
	// step.
	peerCancel()
	engCancel()
	select {
	case <-engDone:
	case <-time.After(15 * time.Second):
		t.Error("engine run did not return after cancellation")
	}
}

// TestPeerSmoke_AdapterChildCrashFidelity is the acceptance proof for the
// crash-facts initiative behind examples/peer-remote's kill -9 demo: when the
// peer's supervised adapter child is SIGKILLed, the peer journals a precise
// ProcessExited fact (signal 9) and a CrashClassified diagnosis, the host
// logs the exact supervision event instead of guessing from a closed
// transport, and the on_crash=respawn policy recovers via a fresh peer dial.
//
// Gated by CRITERIA_PEER_E2E=1.
func TestPeerSmoke_AdapterChildCrashFidelity(t *testing.T) {
	if os.Getenv("CRITERIA_PEER_E2E") != "1" {
		t.Skip("set CRITERIA_PEER_E2E=1 to run peer smoke tests")
	}
	if runtime.GOOS != "linux" {
		t.Skip("adapter-child SIGKILL fidelity test requires /proc (linux)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	moduleRoot := findModuleRoot(t)
	criteriaBin := buildCriteriaBinary(t, moduleRoot)
	noopBin := buildNoopSmokeBinary(t, moduleRoot)
	digest := sha256OfFile(t, noopBin)

	shimAddr := pickFreeAddr(t)
	workflowDir := t.TempDir()

	spec := parseWorkflow(t, fmt.Sprintf(`
workflow {
  name = "peer-smoke-child-crash"
  version = "0.1"
  initial_state = "crash_demo"
  target_state  = "done"
}

environment "remote" "test" {
  listen_address = %q
  accept_token   = "smoke-token"
}

adapter "noop" "demo" {
  environment = remote.test
  on_crash = "respawn"
}

step "crash_demo" {
  target = adapter.noop.demo
  input {
    emit_log = "crash-fidelity"
    delay_ms = "15000"
  }
  outcome "success" { next = state.done }
}

state "done" {
  terminal = true
  success  = true
}
`, shimAddr))

	graph := compileWorkflow(t, spec)
	lf := buildLockfile("noop", "demo", digest)

	// Capture host-side slog while the engine runs: the supervision event
	// ("peer adapter process exited" with the exact signal) is the host-side
	// diagnosis the README's kill -9 demo points at. Restored via t.Cleanup.
	hostLogs := captureSlog(t)

	engCtx, engCancel := context.WithCancel(ctx)
	defer engCancel()
	sink := newCapturingSink()
	engDone := make(chan error, 1)
	go func() {
		eng := engine.New(graph, adapterhost.NewLoader(), sink,
			engine.WithWorkflowDir(workflowDir),
			engine.WithLockfile(lf),
		)
		engDone <- eng.Run(engCtx)
	}()

	// Connect peer #1; its supervised adapter child is spawned as soon as
	// the peer starts.
	time.Sleep(500 * time.Millisecond)
	peerCmd1, peerLogs1, peerCancel1 := startPeer(ctx, t, criteriaBin, shimAddr, noopBin, digest)
	defer dumpPeerLogs(t, peerLogs1)

	childPid := waitForAdapterChild(t, peerCmd1.Process.Pid, 20*time.Second)
	time.Sleep(1 * time.Second) // let the step's Execute sit in the noop delay

	// The kill -9 demo: SIGKILL the peer's adapter child, not the peer.
	if err := syscall.Kill(childPid, syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL adapter child %d: %v", childPid, err)
	}

	// The crash report must reach the host while peer #1 is still alive:
	// the journaled ProcessExited fact (signal 9) is the precise diagnosis
	// the old "transport closed" guess replaced.
	waitForCondition(t, 15*time.Second, func() bool {
		return hostLogs.find("peer adapter process exited", "signal=9") != nil
	}, "host log missing \"peer adapter process exited\" event carrying signal=9")

	// Retire the crashed peer so the respawn has exactly one fresh dial to
	// adopt — the compose demo achieves the same by restarting the peer
	// container before a replacement can interfere.
	peerCancel1()
	_ = peerCmd1.Wait()

	// Fresh dial for the respawn policy to adopt.
	_, peerLogs2, peerCancel2 := startPeer(ctx, t, criteriaBin, shimAddr, noopBin, digest)
	defer peerCancel2()
	defer dumpPeerLogs(t, peerLogs2)

	select {
	case err := <-engDone:
		if err != nil {
			t.Fatalf("engine run: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for engine run to finish")
	}

	if !sink.success {
		t.Fatalf("workflow did not complete successfully after adapter child crash: terminal=%q", sink.terminal)
	}

	// Peer side: the supervised child death is journaled with the precise
	// exit fact and classified on the peer (vs the old "transport closed"
	// guess on the host).
	if logs := peerLogs1.String(); !strings.Contains(logs, "adapter child exited unexpectedly") {
		t.Errorf("peer log missing \"adapter child exited unexpectedly\"; peer output:\n%s", logs)
	}

	// Host side: the supervision journal delivered both terminal facts, and
	// the host logged each verbatim. ProcessExited carries the exact signal;
	// the later CrashClassified overwrites the placeholder exit with the
	// peer's classification (exit code -1, signal 9 = SIGKILL).
	exitedLine := hostLogs.find("peer adapter process exited", "signal=9")
	if exitedLine == nil {
		t.Errorf("host log missing \"peer adapter process exited\" event carrying signal=9; captured:\n%s", hostLogs.dump())
	} else if exitedLine.Attrs["reason"] != "process_exited" {
		t.Errorf("host ProcessExited event reason = %q, want %q", exitedLine.Attrs["reason"], "process_exited")
	}
	crashLine := hostLogs.find("peer adapter process exited", "adapter process terminated")
	if crashLine == nil {
		t.Errorf("host log missing the CrashClassified diagnosis (reason=adapter process terminated); captured:\n%s", hostLogs.dump())
	} else if !strings.Contains(crashLine.Attrs["detail"], "signal 9") {
		t.Errorf("host CrashClassified detail = %q, want it to carry the signal 9 fact", crashLine.Attrs["detail"])
	}

	// Engine side: the respawn policy recovered via a fresh peer dial, with
	// the crash-time classification (the journal fact lands moments after
	// the respawn decision, so the event carries the transport-shape reason).
	evt, ok := sink.adapterEvent("session.respawned")
	if !ok {
		t.Fatalf("no session.respawned event captured; events seen: %v", sink.adapterEventKinds())
	}
	if got := evt["crash_reason"]; got == "" {
		t.Errorf("session.respawned missing crash_reason: %v", evt)
	}
}

// --- helpers shared by the peer example smoke tests ---

type lockedAdapter struct {
	adapterType string
	name        string
	digest      string
}

func buildMultiLockfile(entries ...lockedAdapter) *lockfile.Lockfile {
	lf := &lockfile.Lockfile{SchemaVersion: 1}
	for _, e := range entries {
		lf.Adapters = append(lf.Adapters, lockfile.LockedAdapter{
			Type:               e.adapterType,
			Name:               e.name,
			Reference:          "local",
			ResolvedDigest:     e.digest,
			SourceURL:          "local",
			SDKProtocolVersion: 2,
		})
	}
	return lf
}

// perScopeMultiAdapterFixture builds the compiled workflow and lockfile for
// the per-scope multi-adapter smoke tests: a per_scope_sessions remote
// environment bound to two adapter kinds — noop (greet/recover steps) and
// fail (break step, routed as a failure outcome) — plus a lockfile pinning
// both child digests at the shim. The environment's declared-adapter set is
// therefore {fail, noop}, and every per-scope dial must host both kinds.
func perScopeMultiAdapterFixture(t *testing.T, shimAddr, noopDigest, failDigest string) (*workflow.FSMGraph, *lockfile.Lockfile) {
	t.Helper()
	spec := parseWorkflow(t, fmt.Sprintf(`
workflow {
  name = "peer-smoke-per-scope"
  version = "0.1"
  initial_state = "greet"
  target_state  = "done"
}

environment "remote" "test" {
  listen_address     = %q
  accept_token       = "smoke-token"
  per_scope_sessions = true
}

adapter "noop" "demo" {
  environment = remote.test
}

adapter "fail" "breaker" {
  environment = remote.test
}

step "greet" {
  target = adapter.noop.demo
  input {
    emit_log = "per-scope-greet"
  }
  outcome "success" { next = step.break }
}

step "break" {
  target = adapter.fail.breaker
  outcome "failure" { next = step.recover }
  outcome "success" { next = state.done }
}

step "recover" {
  target = adapter.noop.demo
  input {
    emit_log = "per-scope-recover"
  }
  outcome "success" { next = state.done }
}

state "done" {
  terminal = true
  success  = true
}
`, shimAddr))
	graph := compileWorkflow(t, spec)
	lf := buildMultiLockfile(
		lockedAdapter{"noop", "demo", noopDigest},
		lockedAdapter{"fail", "breaker", failDigest},
	)
	return graph, lf
}

// buildFailSmokeBinary builds the always-failure conformance fixture used as
// the second peer's adapter child.
func buildFailSmokeBinary(t *testing.T, moduleRoot string) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "criteria-adapter-fail")
	cmd := exec.Command("go", "build", "-o", binary, "./internal/adapter/conformance/testdata/fail")
	cmd.Dir = moduleRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build fail adapter: %v\n%s", err, string(out))
	}
	return binary
}

// hostedChild is one hosted adapter child of a per-scope pair peer: an
// entry of the CRITERIA_REMOTE_ADAPTERS manifest, staged behind its own
// CRITERIA_ADAPTER_<KIND>_BINARY/_DIGEST override the way the operator's
// peer pod builds it from the run's adapter pair.
type hostedChild struct {
	kind   string
	binary string
	digest string
}

// startPerScopePeer starts ONE `criteria peer` subprocess for a scope
// instance, hosting every adapter kind the environment declares behind that
// scope's single dial — the operator's (scope, pair) peer contract. The
// manifest (CRITERIA_REMOTE_ADAPTERS) carries the sorted kind set, every
// hosted kind gets its own CRITERIA_ADAPTER_<KIND>_BINARY/_VERSION/_DIGEST
// override, and the dial's identity frame advertises the full hosted set.
// KB-213's fail-closed accept-time coverage check rejects any per-scope dial
// whose hosted set misses a declared kind of the environment, so the pair
// peer is the only dial shape a multi-adapter per-scope environment accepts.
func startPerScopePeer(ctx context.Context, t *testing.T, criteriaBin, addr, scope, token string, children []hostedChild) (*exec.Cmd, *bytes.Buffer, context.CancelFunc) {
	t.Helper()
	sorted := make([]hostedChild, len(children))
	copy(sorted, children)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].kind < sorted[j].kind })
	kinds := make([]string, 0, len(sorted))
	for _, c := range sorted {
		kinds = append(kinds, c.kind)
	}
	cmdCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(cmdCtx, criteriaBin, "peer")
	logs := &bytes.Buffer{}
	cmd.Stdout = logs
	cmd.Stderr = logs
	env := append(os.Environ(),
		"CRITERIA_REMOTE_HOST="+addr,
		"CRITERIA_REMOTE_TOKEN="+token,
		"CRITERIA_REMOTE_SCOPE="+scope,
		"CRITERIA_REMOTE_ADAPTERS="+strings.Join(kinds, ","),
		"CRITERIA_PEER_BACKOFF_MIN=200ms",
		"CRITERIA_PEER_BACKOFF_MAX=1s",
		"CRITERIA_LOG_LEVEL=debug",
	)
	for _, c := range sorted {
		env = append(env,
			"CRITERIA_ADAPTER_"+adapterEnvToken(c.kind)+"_BINARY="+c.binary,
			"CRITERIA_ADAPTER_"+adapterEnvToken(c.kind)+"_VERSION=0.1.0",
			"CRITERIA_ADAPTER_"+adapterEnvToken(c.kind)+"_DIGEST="+c.digest,
		)
	}
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start criteria peer: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
	})
	return cmd, logs, cancel
}

// adapterEnvToken normalizes an adapter kind into the CRITERIA_ADAPTER_<KIND>_
// override prefix the peer config accepts; mirrors internal/peer's
// CRITERIA_ADAPTER_<NAME>_* env-name rule (uppercased, every
// non-alphanumeric character replaced with an underscore).
func adapterEnvToken(kind string) string {
	var b strings.Builder
	for _, r := range kind {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - 'a' + 'A')
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// startSingleKindPeer starts the legacy single-adapter per-scope dial: one
// kind behind CRITERIA_ADAPTER_NAME, no multi-adapter manifest. On a
// multi-adapter per-scope environment this dial is legitimate in every
// verified dimension but its hosted child set, so acceptance fails closed
// with the typed PeerChildSetError; it is kept as the negative-control
// fixture for that contract (see TestPeerSmoke_PerScopeChildSetRejection).
func startSingleKindPeer(ctx context.Context, t *testing.T, criteriaBin, addr, scope, token, adapterType, adapterBin, digest string) (*exec.Cmd, *bytes.Buffer, context.CancelFunc) {
	t.Helper()
	cmdCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(cmdCtx, criteriaBin, "peer")
	logs := &bytes.Buffer{}
	cmd.Stdout = logs
	cmd.Stderr = logs
	cmd.Env = append(os.Environ(),
		"CRITERIA_REMOTE_HOST="+addr,
		"CRITERIA_REMOTE_TOKEN="+token,
		"CRITERIA_REMOTE_SCOPE="+scope,
		"CRITERIA_REMOTE_DIGEST="+digest,
		"CRITERIA_ADAPTER_NAME="+adapterType,
		"CRITERIA_ADAPTER_VERSION=0.1.0",
		"CRITERIA_ADAPTER_BINARY="+adapterBin,
		"CRITERIA_PEER_BACKOFF_MIN=200ms",
		"CRITERIA_PEER_BACKOFF_MAX=1s",
		"CRITERIA_LOG_LEVEL=debug",
	)
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start criteria peer: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
	})
	return cmd, logs, cancel
}

func dumpPeerLogsMap(t *testing.T, logs map[string]*bytes.Buffer) {
	t.Helper()
	for scope, buf := range logs {
		if buf != nil && buf.Len() > 0 {
			t.Logf("criteria peer (scope %s) output:\n%s", scope, buf.String())
		}
	}
}

// capturingSink extends testSink with recording of per-step adapter events
// (session.crash / session.respawned flow through StepEventSink), step
// outcomes, and adapter lifecycle events.
type capturingSink struct {
	*testSink

	mu        sync.Mutex
	adapter   []capturedAdapterEvent
	outcomes  map[string]string
	lifecycle []*engine.AdapterLifecycleEvent
}

type capturedAdapterEvent struct {
	Kind string
	Data map[string]any
}

func newCapturingSink() *capturingSink {
	return &capturingSink{testSink: &testSink{}, outcomes: map[string]string{}}
}

type recordingEventSink struct {
	sink *capturingSink
}

func (recordingEventSink) Log(string, []byte) {}

func (r recordingEventSink) Adapter(kind string, data any) {
	rec := capturedAdapterEvent{Kind: kind}
	if raw, err := json.Marshal(data); err == nil {
		_ = json.Unmarshal(raw, &rec.Data)
	}
	r.sink.mu.Lock()
	r.sink.adapter = append(r.sink.adapter, rec)
	r.sink.mu.Unlock()
}

func (s *capturingSink) StepEventSink(string) adapter.EventSink { return recordingEventSink{sink: s} }

func (s *capturingSink) OnStepOutcome(step, outcome string, _ time.Duration, _ error, comment string) {
	s.mu.Lock()
	s.outcomes[step] = outcome
	s.mu.Unlock()
}

func (s *capturingSink) OnStepOutcomeInvalid(step, outcome string, issues []string, _ int) {
	s.mu.Lock()
	s.outcomes[step] = outcome + " (invalid: " + strings.Join(issues, "; ") + ")"
	s.mu.Unlock()
}

func (s *capturingSink) OnAdapterLifecycleEvent(event *engine.AdapterLifecycleEvent) {
	if event == nil || event.Status != "provision_wanted" {
		return
	}
	s.mu.Lock()
	s.lifecycle = append(s.lifecycle, event)
	s.mu.Unlock()
}

func (s *capturingSink) adapterEvent(kind string) (map[string]any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.adapter) - 1; i >= 0; i-- {
		if s.adapter[i].Kind == kind {
			return s.adapter[i].Data, true
		}
	}
	return nil, false
}

func (s *capturingSink) adapterEventKinds() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.adapter))
	for _, e := range s.adapter {
		out = append(out, e.Kind)
	}
	return out
}

func (s *capturingSink) stepOutcome(step string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	outcome, ok := s.outcomes[step]
	return outcome, ok
}

type provisionInfo struct {
	ScopeName         string
	ScopeInstanceID   string
	Token             string
	ShimListenAddress string
}

// waitForProvisions polls the sink until every named adapter type has a
// provision_wanted event, returning the first one per type.
func waitForProvisions(t *testing.T, sink *capturingSink, adapterTypes []string, timeout time.Duration) map[string]provisionInfo {
	t.Helper()
	want := map[string]bool{}
	for _, typ := range adapterTypes {
		want[typ] = false
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		sink.mu.Lock()
		provisions := map[string]provisionInfo{}
		for _, evt := range sink.lifecycle {
			if _, needed := want[evt.AdapterType]; needed && !want[evt.AdapterType] {
				want[evt.AdapterType] = true
				provisions[evt.AdapterType] = provisionInfo{
					ScopeName:         evt.ScopeName,
					ScopeInstanceID:   evt.ScopeInstanceID,
					Token:             evt.Token,
					ShimListenAddress: evt.ShimListenAddress,
				}
			}
		}
		sink.mu.Unlock()

		complete := true
		for _, done := range want {
			if !done {
				complete = false
			}
		}
		if complete {
			return provisions
		}
		time.Sleep(100 * time.Millisecond)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	seen := map[string]int{}
	for _, evt := range sink.lifecycle {
		seen[evt.AdapterType]++
	}
	t.Fatalf("timed out waiting for provision_wanted events for %v; seen %v", adapterTypes, seen)
	return nil
}

// capturedLogs is a thread-safe slog record sink used to observe the
// host-side supervision logging during a smoke run.
type capturedLogs struct {
	mu    sync.Mutex
	lines []capturedLogLine
}

type capturedLogLine struct {
	Message string
	Attrs   map[string]string
}

type captureHandler struct {
	logs         *capturedLogs
	preformatted []slog.Attr
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

//nolint:gocritic // slog.Handler interface requires value receiver for Record.
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	line := capturedLogLine{Message: r.Message, Attrs: map[string]string{}}
	for _, a := range h.preformatted {
		line.Attrs[a.Key] = a.Value.String()
	}
	r.Attrs(func(a slog.Attr) bool {
		line.Attrs[a.Key] = a.Value.String()
		return true
	})
	h.logs.mu.Lock()
	h.logs.lines = append(h.logs.lines, line)
	h.logs.mu.Unlock()
	return nil
}

func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	merged := append(append([]slog.Attr{}, h.preformatted...), attrs...)
	return &captureHandler{logs: h.logs, preformatted: merged}
}

func (h *captureHandler) WithGroup(string) slog.Handler { return h }

// captureSlog installs a thread-safe capturing default slog handler and
// returns the capture buffer plus a restore func. The host logs supervision
// events through the package-level slog default logger, so this is the
// host-side observation surface the README's kill -9 demo describes.
func captureSlog(t *testing.T) *capturedLogs {
	t.Helper()
	orig := slog.Default()
	logs := &capturedLogs{}
	slog.SetDefault(slog.New(&captureHandler{logs: logs}))
	t.Cleanup(func() { slog.SetDefault(orig) })
	return logs
}

func (l *capturedLogs) find(message, attrNeedle string) *capturedLogLine {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range l.lines {
		line := &l.lines[i]
		if line.Message != message {
			continue
		}
		for _, v := range line.Attrs {
			if strings.Contains(v, attrNeedle) {
				return line
			}
		}
	}
	return nil
}

// count returns how many captured lines carry message and needle in an
// attribute value — the multiplicity companion of find.
func (l *capturedLogs) count(message, attrNeedle string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for i := range l.lines {
		line := &l.lines[i]
		if line.Message != message {
			continue
		}
		for _, v := range line.Attrs {
			if strings.Contains(v, attrNeedle) {
				n++
				break
			}
		}
	}
	return n
}

func (l *capturedLogs) dump() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var b strings.Builder
	for _, line := range l.lines {
		fmt.Fprintf(&b, "%s %v\n", line.Message, line.Attrs)
	}
	return b.String()
}

// waitForCondition polls cond until it returns true or the timeout elapses.
func waitForCondition(t *testing.T, timeout time.Duration, cond func() bool, failMsg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal(failMsg)
}

// waitForAdapterChild polls /proc until the peer process has spawned a child
// (the go-plugin adapter child) and returns its pid.
func waitForAdapterChild(t *testing.T, peerPid int, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pids := childPIDsOf(peerPid); len(pids) > 0 {
			return pids[0]
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("peer %d never spawned an adapter child within %s", peerPid, timeout)
	return 0
}

func childPIDsOf(parent int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		raw, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// stat: pid (comm) state ppid ... — comm may contain spaces and
		// parentheses, so parse fields after the last ')'.
		idx := bytes.LastIndexByte(raw, ')')
		if idx < 0 {
			continue
		}
		fields := bytes.Fields(raw[idx+1:])
		if len(fields) < 2 {
			continue
		}
		ppid, err := strconv.Atoi(string(fields[1]))
		if err == nil && ppid == parent {
			pid, _ := strconv.Atoi(e.Name())
			out = append(out, pid)
		}
	}
	return out
}
