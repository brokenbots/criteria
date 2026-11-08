package engine

// container_envhost_test.go — pins the container-arm supervision binding
// (ADR-0014 M3): container environments get the in-process peer supervisor
// (internal/peer's InProcessHost) instead of the local docker/podman
// dispatch, and a container session's crash classification for a killed
// child comes from that supervisor's journal — the process-exited family —
// never from transport-string matching. The remote binding's own pins stay
// in envhost_binding_test.go/remote_env_dispatch_test.go.

import (
	"context"

	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/envhost"
	"github.com/brokenbots/criteria/internal/peer"
	"github.com/brokenbots/criteria/internal/testutil"
	"github.com/brokenbots/criteria/workflow"
	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin/runner"
)

// containerSupervisionWorkflow compiles the container-lane workflow the
// supervision tests drive: one container environment bound to the noop
// adapter, a fast warm step (spawns the supervised child), and a sleeper
// step (long enough that a mid-flight kill is deterministic) with
// on_crash=respawn so the crash arm exercises the journal classification
// followed by a keepalive-honoring respawn.
const containerSupervisionWorkflow = `
workflow {
  name = "container-supervision"
  version = "0.1"
  initial_state = "warm"
  target_state  = "done"
}

environment "container" "local" {
}

adapter "noop" "default" {
  environment = container.local
}

step "warm" {
  target = adapter.noop.default
  input {
    delay_ms = 0
  }
  on_crash = "respawn"
  outcome "success" { next = step.sleep }
  outcome "error"   { next = state.done }
  outcome "failure" { next = state.done }
}

step "sleep" {
  target = adapter.noop.default
  on_crash = "respawn"
  input {
    delay_ms = 1500
  }
  outcome "success" { next = state.done }
  outcome "error"   { next = state.done }
  outcome "failure" { next = state.done }
}

state "done" {
  terminal = true
  success  = true
}`

// containerLedger records spawned child pids in spawn order so tests can
// SIGKILL a specific generation and assert the respawn produced a new one.
type containerLedger struct {
	mu   sync.Mutex
	pids []int
}

func (l *containerLedger) record(pid int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pids = append(l.pids, pid)
}

func (l *containerLedger) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.pids)
}

func (l *containerLedger) pid(n int) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.pids[n]
}

// containerBinaryRunnerFunc is the ChildRunner the engine's container boot
// hook injects: it starts the resolved adapter binary through the go-plugin
// RunnerFunc contract and records each spawned child's pid into the ledger.
func containerBinaryRunnerFunc(bin string, ledger *containerLedger) peer.ChildRunner {
	return func(_ hclog.Logger, cmd *exec.Cmd, _ string) (runner.Runner, error) {
		cmd.Path = bin
		cmd.Args = []string{bin}
		r, err := newContainerExecCmdRunner(cmd, ledger.record)
		if err != nil {
			return nil, err
		}
		return r, nil
	}
}

func newContainerExecCmdRunner(cmd *exec.Cmd, onRecord func(pid int)) (runner.Runner, error) {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	return &containerExecCmdRunner{cmd: cmd, stdout: stdout, stderr: stderr, onRecord: onRecord}, nil
}

// containerExecCmdRunner delegates a runner.Runner to an exec.Cmd — the
// same minimal shape the container runner lane produces for image-backed
// adapter definitions.
type containerExecCmdRunner struct {
	cmd      *exec.Cmd
	stdout   io.ReadCloser
	stderr   io.ReadCloser
	onRecord func(pid int)
}

func (r *containerExecCmdRunner) Start(_ context.Context) error {
	if err := r.cmd.Start(); err != nil {
		return err
	}
	if r.onRecord != nil && r.cmd.Process != nil {
		r.onRecord(r.cmd.Process.Pid)
	}
	return nil
}

func (r *containerExecCmdRunner) Wait(_ context.Context) error { return r.cmd.Wait() }

func (r *containerExecCmdRunner) Kill(_ context.Context) error {
	if r.cmd.Process != nil {
		return r.cmd.Process.Kill()
	}
	return nil
}

func (r *containerExecCmdRunner) ID() string {
	if r.cmd.Process != nil {
		return strconv.Itoa(r.cmd.Process.Pid)
	}
	return "0"
}

func (r *containerExecCmdRunner) Name() string                      { return r.cmd.Path }
func (r *containerExecCmdRunner) Stdout() io.ReadCloser             { return r.stdout }
func (r *containerExecCmdRunner) Stderr() io.ReadCloser             { return r.stderr }
func (r *containerExecCmdRunner) Diagnose(_ context.Context) string { return "" }

func (r *containerExecCmdRunner) PluginToHost(pluginNet, pluginAddr string) (string, string, error) {
	return pluginNet, pluginAddr, nil
}

func (r *containerExecCmdRunner) HostToPlugin(hostNet, hostAddr string) (string, string, error) {
	return hostNet, hostAddr, nil
}

var _ runner.Runner = (*containerExecCmdRunner)(nil)

// containerNoopBin compiles the in-tree noop conformance fixture. The fixture
// serves the adapter v2 contract through the real adapterhost.Serve path, so
// the spawned child is indistinguishable from a container-launch adapter.
func containerNoopBin(t *testing.T) string {
	t.Helper()
	testutil.RequireUnixSockets(t)
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH; cannot build the noop fixture")
	}
	bin := filepath.Join(t.TempDir(), "criteria-adapter-noop")
	cmd := exec.Command(goBin, "build", "-o", bin, "./internal/adapter/conformance/testdata/noop")
	cmd.Dir = "../.." // engine test dir → repo root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build noop adapter: %v\n%s", err, out)
	}
	return bin
}

// containerChildBoot returns the boot contract the peer host consumes
// directly: every definition's spawn is a real RunnerFunc-launched child
// whose pids land in the ledger.
func containerChildBoot(bin string, ledger *containerLedger) func(context.Context, envhost.Definition) (peer.ChildBoot, error) {
	return func(context.Context, envhost.Definition) (peer.ChildBoot, error) {
		return peer.ChildBoot{
			Version: "0.1.0",
			Binary:  bin,
			Digest:  "sha256:abcd1234",
			Runner:  containerBinaryRunnerFunc(bin, ledger),
		}, nil
	}
}

// containerBoot adapts containerChildBoot to the engine's container-boot
// hook signature (which carries the environment key).
func containerBoot(bin string, ledger *containerLedger) func(envKey string, def envhost.Definition) (peer.ChildBoot, error) {
	direct := containerChildBoot(bin, ledger)
	return func(envKey string, def envhost.Definition) (peer.ChildBoot, error) {
		return direct(context.Background(), def)
	}
}

// containerTriggerSink wraps fakeSink with two observation points: step
// entries (the deterministic kill trigger — it fires while the step's
// adapter Execute is in flight) and session-event traffic (session.crash /
// session.respawned ride the step event sink).
type containerTriggerSink struct {
	*fakeSink

	// onStepEntered, when set, runs in-line on each step entry.
	onStepEntered func(step string)

	mu      sync.Mutex
	kinds   []string
	events  []map[string]any
	entered []string
}

func (s *containerTriggerSink) OnStepEntered(step, adapterName string, attempt int) {
	s.mu.Lock()
	s.entered = append(s.entered, step)
	s.mu.Unlock()
	if s.onStepEntered != nil {
		s.onStepEntered(step)
	}
	s.fakeSink.OnStepEntered(step, adapterName, attempt)
}

func (s *containerTriggerSink) StepEventSink(string) adapter.EventSink {
	return &containerEventRecorder{parent: s}
}

func (s *containerTriggerSink) recorded(kind string) (map[string]any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, k := range s.kinds {
		if k == kind {
			return s.events[i], true
		}
	}
	return nil, false
}

// containerEventRecorder captures the session-event traffic one executing
// step produces.
type containerEventRecorder struct {
	parent *containerTriggerSink
}

func (r *containerEventRecorder) Log(string, []byte) {}

func (r *containerEventRecorder) Adapter(kind string, data any) {
	payload, _ := data.(map[string]any)
	r.parent.mu.Lock()
	defer r.parent.mu.Unlock()
	r.parent.kinds = append(r.parent.kinds, kind)
	r.parent.events = append(r.parent.events, payload)
}

// TestMaybeStartContainerEnvHosts_BindsSupervisorHosts pins the container
// bind path: starting the run must register a dedicated envhost.Host for
// every container environment key — a real in-process supervisor, not the
// shim fallback lane (container environments carry no shim substrate).
func TestMaybeStartContainerEnvHosts_BindsSupervisorHosts(t *testing.T) {
	g := &workflow.FSMGraph{
		Environments: map[string]*workflow.EnvironmentNode{
			"container.a": {Type: "container", Name: "a"},
		},
	}
	eng := New(g, nil, &fakeSink{})
	sessions := adapterhost.NewSessionManager(nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := eng.maybeStartContainerEnvHosts(ctx, sessions); err != nil {
		t.Fatalf("maybeStartContainerEnvHosts: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = sessions.Shutdown(ctx)
	})

	if !sessions.EnvHostRegisteredForEnv("container.a") {
		t.Fatal("container.a: EnvHostRegisteredForEnv = false, want a dedicated in-process supervisor binding")
	}

	// Bindings are exact: nothing for an environment the graph did not
	// declare, nothing in the legacy default slot.
	if sessions.EnvHostRegisteredForEnv("container.missing") {
		t.Error("container.missing: EnvHostRegisteredForEnv = true, want false")
	}
	if sessions.EnvHostRegisteredForEnv("") {
		t.Error("default slot: EnvHostRegisteredForEnv = true, want false")
	}
	if sessions.RemoteShimForEnv("container.a") != nil {
		t.Error("container.a: RemoteShimForEnv = non-nil, want nil (container environments have no shim substrate)")
	}
}

// TestContainerChildKeepAlivePolicy pins the keepalive-resolution contract
// the binder applies per environment: unset keeps the peer machinery's
// default (children survive engagement ends and respawn on demand), a
// falsy value disables it, and an unparseable value fails run init instead
// of silently flipping the supervision policy.
func TestContainerChildKeepAlivePolicy(t *testing.T) {
	t.Run("unset-defaults-to-true", func(t *testing.T) {
		t.Setenv("CRITERIA_PEER_CHILD_KEEPALIVE", "")
		keepalive, err := containerChildKeepAlive("env1")
		if err != nil {
			t.Fatalf("unset: %v", err)
		}
		if !keepalive {
			t.Fatal("unset: keepalive = false, want true")
		}
	})
	t.Run("disabled", func(t *testing.T) {
		t.Setenv("CRITERIA_PEER_CHILD_KEEPALIVE", "0")
		keepalive, err := containerChildKeepAlive("env1")
		if err != nil {
			t.Fatalf("0: %v", err)
		}
		if keepalive {
			t.Fatal("0: keepalive = true, want false")
		}
	})
	t.Run("invalid-fails-run-init", func(t *testing.T) {
		t.Setenv("CRITERIA_PEER_CHILD_KEEPALIVE", "not-a-bool")
		_, err := containerChildKeepAlive("env1")
		if err == nil {
			t.Fatal("not-a-bool: want an error")
		}
		if want := "CRITERIA_PEER_CHILD_KEEPALIVE"; !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	})
}

// TestContainerEnvExecute_KillClassifiesFromJournal is the M3 acceptance:
// a real adapter child dies mid-Execute; the step's crash classification
// comes from the supervisor journal (the process-exited family, never a
// transport string), the respawn produces a fresh child generation
// (keepalive policy honored — children are not torn down between
// engagements), and the run continues to success on the fresh child.
func TestContainerEnvExecute_KillClassifiesFromJournal(t *testing.T) {
	if testing.Short() {
		t.Skip("acceptance test spawns real adapter children")
	}
	g := compile(t, containerSupervisionWorkflow)
	bin := containerNoopBin(t)
	ledger := &containerLedger{}

	var killed atomic.Bool
	sink := &containerTriggerSink{fakeSink: &fakeSink{}}
	sink.onStepEntered = func(step string) {
		if step != "sleep" || !killed.CompareAndSwap(false, true) {
			return
		}
		go func() {
			// The kill lands inside the sleeper's Execute: the child
			// dies after the transport started serving the call, so its
			// death must be classified from the supervisor journal.
			time.Sleep(200 * time.Millisecond)
			if n := ledger.count(); n < 1 {
				return
			}
			if err := syscall.Kill(ledger.pid(0), syscall.SIGKILL); err != nil {
				t.Errorf("kill child: %v", err)
			}
		}()
	}

	eng := NewTestEngine(g, nil, sink)
	eng.containerBootHook = containerBoot(bin, ledger)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := runSupervisedEngine(eng, ctx); err != nil {
		t.Fatalf("run: %v", err)
	}

	// The respawn honors the supervision policy: the killed child was
	// replaced by a fresh generation and the retried step ran on it.
	if n := ledger.count(); n < 2 {
		t.Fatalf("spawned %d child generation(s), want >= 2 (kill + respawn)", n)
	}

	// The step outcome was classified from the journal, not string
	// matching: the crash_reason riding session.respawned is the
	// peer's CrashClassified fact (process-exited family).
	killFacts := []string{
		adapterhost.CrashReasonTransportClosed,
		adapterhost.CrashReasonStdioEOF,
		adapterhost.CrashReasonStdioPipeBroken,
		adapterhost.CrashReasonEndpointUnavailable,
	}
	data, ok := sink.recorded("session.respawned")
	if !ok {
		t.Fatalf("expected a session.respawned event; sink recorded %v", sink.kinds)
	}
	reason, _ := data["crash_reason"].(string)
	if reason != adapterhost.CrashReasonProcessTerminated {
		t.Errorf("session.respawned crash_reason = %q; want the peer journal's process-exited-family classification (%q)", reason, adapterhost.CrashReasonProcessTerminated)
	}
	for _, kill := range killFacts {
		if reason == kill {
			t.Errorf("crash_reason = %q; the transport-string family must not classify a supervised kill", kill)
		}
	}

	// The run completed on the fresh child after the crash recovery.
	state, ok := sink.terminalState()
	if !ok {
		t.Fatal("run did not complete")
	}
	if state != "done" || !sink.terminalOK {
		t.Errorf("terminal state %q ok=%v; want done/true (retry after respawn finished the run)", state, sink.terminalOK)
	}
}

// TestContainerEnvHostJournalKillFacts pins the journal readability the
// classification rides: after a killed child, the supervisor journal holds
// per-generation spawn facts, the non-graceful exit, and the crash
// classification — in sequence order and attributed to the definition.
func TestContainerEnvHostJournalKillFacts(t *testing.T) {
	if testing.Short() {
		t.Skip("journal test spawns real adapter children")
	}
	g := compile(t, containerSupervisionWorkflow)
	bin := containerNoopBin(t)
	ledger := &containerLedger{}

	boot := containerChildBoot(bin, ledger)
	host := peer.NewInProcessHost(peer.HostConfig{
		KeepAlive: true,
		Log:       slog.Default(),
	}, boot)

	m := adapterhost.NewSessionManager(nil)
	m.SetGraph(g)
	if err := m.SetContainerEnvHost("container.local", host); err != nil {
		t.Fatalf("bind container env host: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		m.Shutdown(stopCtx)
		host.StopEnvHost(stopCtx)
	})

	// The first open spawns the definition's child (per-definition keying:
	// the child is the adapter instance identity, not per-session).
	if err := m.Open(ctx, "noop.default", "noop", adapterhost.OnCrashRespawn, nil, nil); err != nil {
		t.Fatalf("open noop session: %v", err)
	}
	if ledger.count() != 1 {
		t.Fatalf("spawned %d child(ren) at open, want 1", ledger.count())
	}
	if n := ledger.count(); n != 1 {
		t.Fatalf("spawned %d child(ren) at open, want 1", n)
	}

	warm := g.Steps["warm"]
	if warm == nil {
		t.Fatal("fixture must declare step warm")
	}
	if _, err := m.Execute(ctx, "noop.default", warm, noopEventSink{}, nil); err != nil {
		t.Fatalf("execute warm: %v", err)
	}

	// Journal readability BEFORE the kill: the spawn fact is recorded.
	entries := waitJournalCounts(t, host, "noop.default", 1, 0, 0, 10*time.Second)
	if n := countKinds(entries, envhost.JournalSpawned); n != 1 {
		t.Fatalf("spawned entries = %d, want 1 (attribution scope %q)", n, entriesScopeOf(entries))
	}

	if err := syscall.Kill(ledger.pid(0), syscall.SIGKILL); err != nil {
		t.Fatalf("kill child: %v", err)
	}

	// The sleeper step's Execute on the dead child's session: classification
	// from the journal, respawn, and the fresh child serves the retry.
	sleep := g.Steps["sleep"]
	if sleep == nil {
		t.Fatal("fixture must declare step sleep")
	}
	if _, err := m.Execute(ctx, "noop.default", sleep, noopEventSink{}, nil); err != nil {
		t.Fatalf("execute sleep after kill (want the respawn retry to complete it): %v", err)
	}

	// The respawn honored the keepalive policy: a fresh generation was
	// spawned on demand (and the old generation was already dead).
	entries = waitJournalCounts(t, host, "noop.default", 2, 1, 1, 10*time.Second)
	spawned := kindsOf(entries, envhost.JournalSpawned)
	exited := kindsOf(entries, envhost.JournalExited)
	crashes := kindsOf(entries, envhost.JournalCrashClassified)
	if len(spawned) != 2 {
		t.Fatalf("spawned entries = %d, want 2 (original + respawned generation): %+v", len(spawned), spawned)
	}
	if len(exited) != 1 {
		t.Fatalf("exited entries = %d, want 1: %+v", len(exited), exited)
	}
	if len(crashes) != 1 {
		t.Fatalf("crash entries = %d, want 1: %+v", len(crashes), crashes)
	}
	// The crash classification is the process-exited family fact the
	// step outcome consumed; the exit record must not be graceful (the
	// child was SIGKILLed outside supervision, never peer-initiated).
	crashEntry := entryOfKind(entries, envhost.JournalCrashClassified)
	if !strings.HasPrefix(crashEntry.Detail, adapterhost.CrashReasonProcessTerminated+": ") {
		t.Errorf("journal crash reason = %q; want %q: <detail> (the process-exited family)", crashEntry.Detail, adapterhost.CrashReasonProcessTerminated)
	}
	// The exit record carries the child's wait status (SIGKILL: no exit
	// code, no graceful arm).
	exitEntry := entryOfKind(entries, envhost.JournalExited)
	if !strings.Contains(exitEntry.Detail, "graceful false") {
		t.Errorf("journal exit detail = %q; want the non-graceful SIGKILL shape", exitEntry.Detail)
	}
}

// --- helpers ------------------------------------------------------------

// runSupervisedEngine runs the supervision-test engine to completion.
func runSupervisedEngine(eng *Engine, ctx context.Context) error {
	return eng.Run(ctx)
}

// waitJournalCounts polls the host's journal until the definition's
// process facts include the expected spawn/exit/crash counts, or fails
// after the budget with the retained entries for diagnosis.
func waitJournalCounts(t *testing.T, host *peer.InProcessHost, adapter string, wantSpawned, wantExited, wantCrash int, budget time.Duration) []envhost.JournalEntry {
	t.Helper()
	deadline := time.Now().Add(budget)
	ctx := context.Background()
	for {
		entries, err := host.Journal(envhost.ScopeRef{}, adapter).Entries(ctx)
		if err != nil {
			t.Fatalf("journal read: %v", err)
		}
		if countKinds(entries, envhost.JournalSpawned) >= wantSpawned &&
			countKinds(entries, envhost.JournalExited) >= wantExited &&
			countKinds(entries, envhost.JournalCrashClassified) >= wantCrash {
			return entries
		}
		if time.Now().After(deadline) {
			t.Fatalf("journal did not reach spawned=%d exited=%d crash=%d within %s; retained: %+v",
				wantSpawned, wantExited, wantCrash, budget, entries)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func kindsOf(entries []envhost.JournalEntry, kind envhost.JournalKind) []envhost.JournalEntry {
	var out []envhost.JournalEntry
	for _, e := range entries {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func countKinds(entries []envhost.JournalEntry, kind envhost.JournalKind) int {
	return len(kindsOf(entries, kind))
}

func entryOfKind(entries []envhost.JournalEntry, kind envhost.JournalKind) envhost.JournalEntry {
	var found envhost.JournalEntry
	for _, e := range entries {
		if e.Kind == kind {
			found = e
		}
	}
	return found
}

func entriesScopeOf(entries []envhost.JournalEntry) string {
	if len(entries) == 0 {
		return ""
	}
	return entries[0].Scope.Key()
}
