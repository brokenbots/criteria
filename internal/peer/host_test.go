package peer

// host_test.go exercises the in-process supervisor host (ADR-0014 M3) against
// a REAL adapter subprocess (the noop conformance fixture spawned through a
// go-plugin RunnerFunc — the container-runner injection lane): spawn + journal
// facts, keepalive-vs-idle child lifetime across engagements, journal-backed
// crash classification after a signal death (never string matching), the
// no-respawn-after-crash gate, and the fresh-handle recovery arm.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/envhost"
	"github.com/brokenbots/criteria/internal/testutil"
	"github.com/brokenbots/criteria/workflow"
	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin/runner"
)

// execCmdRunner is a minimal runner.Runner implementation that delegates to an
// exec.Cmd — the same shape loader_test proves out — kept local to the peer
// tests so the host's RunnerFunc injection is exercised through a real
// adapter child.
type execCmdRunner struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr io.ReadCloser
	// onRecord, when non-nil, receives the child's pid right after Start —
	// the ledger feeding signal-death and respawn-generation assertions.
	onRecord func(pid int)
}

func newExecCmdRunner(_ hclog.Logger, cmd *exec.Cmd, onRecord func(pid int)) (*execCmdRunner, error) {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	return &execCmdRunner{cmd: cmd, stdout: stdout, stderr: stderr, onRecord: onRecord}, nil
}

func (r *execCmdRunner) Start(_ context.Context) error {
	err := r.cmd.Start()
	if err != nil {
		return err
	}
	if r.onRecord != nil && r.cmd.Process != nil {
		r.onRecord(r.cmd.Process.Pid)
	}
	return nil
}

func (r *execCmdRunner) Wait(_ context.Context) error { return r.cmd.Wait() }

func (r *execCmdRunner) Kill(_ context.Context) error {
	if r.cmd.Process != nil {
		return r.cmd.Process.Kill()
	}
	return nil
}

func (r *execCmdRunner) ID() string {
	if r.cmd.Process != nil {
		return strconv.Itoa(r.cmd.Process.Pid)
	}
	return "0"
}

func (r *execCmdRunner) Name() string                      { return r.cmd.Path }
func (r *execCmdRunner) Stdout() io.ReadCloser             { return r.stdout }
func (r *execCmdRunner) Stderr() io.ReadCloser             { return r.stderr }
func (r *execCmdRunner) Diagnose(_ context.Context) string { return "" }

func (r *execCmdRunner) PluginToHost(pluginNet, pluginAddr string) (string, string, error) {
	return pluginNet, pluginAddr, nil
}

func (r *execCmdRunner) HostToPlugin(hostNet, hostAddr string) (string, string, error) {
	return hostNet, hostAddr, nil
}

var _ runner.Runner = (*execCmdRunner)(nil)

// runnerLedger records child pids in spawn order so tests can distinguish
// generation N from generation N+1 and SIGKILL a specific child.
type runnerLedger struct {
	mu   sync.Mutex
	pids []int
}

func (l *runnerLedger) record(pid int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pids = append(l.pids, pid)
}

func (l *runnerLedger) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.pids)
}

func (l *runnerLedger) pid(n int) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.pids[n]
}

// binaryRunnerFunc returns the ChildRunner the container lane injects: it
// starts the resolved adapter binary through the go-plugin RunnerFunc
// contract (go-plugin builds the cmd; the RunnerFunc pins path/args) and
// records each spawned child's pid.
func binaryRunnerFunc(bin string, ledger *runnerLedger) ChildRunner {
	return func(_ hclog.Logger, cmd *exec.Cmd, _ string) (runner.Runner, error) {
		cmd.Path = bin
		cmd.Args = []string{bin}
		r, err := newExecCmdRunner(hclog.NewNullLogger(), cmd, ledger.record)
		if err != nil {
			return nil, err
		}
		return r, nil
	}
}

// recordingSink is a small adapter.EventSink carrying the mid-Execute sink
// traffic while the call is in flight.
type recordingSink struct {
	mu      sync.Mutex
	adapter []string
	logs    []string
}

func (s *recordingSink) Adapter(kind string, _ any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adapter = append(s.adapter, kind)
}

func (s *recordingSink) Log(_ string, _ []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logs = append(s.logs, "")
}

var _ adapter.EventSink = (*recordingSink)(nil)

// testHost builds an in-process host booting every definition from one
// RunnerFunc over a freshly built noop adapter. The returned buffer carries
// the supervisor's own structured log so failing tests show the spawn
// failure's cause instead of the swallowed "no respawn possible" wrapper.
// The boot hook counts resolutions into resolutions when non-nil.
func testHost(t *testing.T, keepAlive bool, booted func()) (*InProcessHost, *runnerLedger, *bytes.Buffer) {
	t.Helper()
	testutil.RequireUnixSockets(t)
	if testing.Short() {
		t.Skip("host test spawns a real adapter subprocess")
	}
	bin := buildNoopAdapter(t)
	ledger := &runnerLedger{}
	boot := func(context.Context, envhost.Definition) (ChildBoot, error) {
		if booted != nil {
			booted()
		}
		return ChildBoot{
			Version: "0.1.0",
			Binary:  bin,
			Digest:  "sha256:abcd1234",
			Runner:  binaryRunnerFunc(bin, ledger),
		}, nil
	}
	buf := &bytes.Buffer{}
	h := NewInProcessHost(HostConfig{
		KeepAlive:     keepAlive,
		ExitPoll:      10 * time.Millisecond,
		ShutdownGrace: 5 * time.Second,
		Log:           slog.New(slog.NewTextHandler(buf, nil)),
	}, boot)
	return h, ledger, buf
}

// ensureOnce is the root-shape Ensure helper: one definition, live wrapper
// returned.
func ensureOnce(t *testing.T, h *InProcessHost, scope envhost.ScopeRef, defName string, debug func() string) *supervisedHostHandle {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	handles, err := h.Ensure(ctx, scope, []envhost.Definition{{Name: defName, Type: "noop"}})
	if err != nil {
		if debug != nil {
			t.Fatalf("Ensure: %v\nsupervisor log:\n%s", err, debug())
		}
		t.Fatalf("Ensure: %v", err)
	}
	w, ok := handles[defName]
	if !ok || w == nil {
		t.Fatalf("Ensure did not settle a handle for %q", defName)
	}
	wh, ok := w.(*supervisedHostHandle)
	if !ok {
		t.Fatalf("Ensure settled %T, want *supervisedHostHandle", w)
	}
	return wh
}

// waitPIDDead proves the child process is truly gone (reaped, not a zombie).
func waitPIDDead(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d still alive after 5s", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitJournalKind polls the host journal until the given kind shows up the
// requested number of times for the definition, deadline-bounded so watcher
// timing is deterministic from the test's point of view.
func waitJournalKind(t *testing.T, h *InProcessHost, scope envhost.ScopeRef, adapterName string, kind envhost.JournalKind, minCount int) []envhost.JournalEntry {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, err := h.Journal(scope, adapterName).Entries(context.Background())
		if err != nil {
			t.Fatalf("journal entries: %v", err)
		}
		n := 0
		for _, e := range entries {
			if e.Kind == kind {
				n++
			}
		}
		if n >= minCount {
			return entries
		}
		if time.Now().After(deadline) {
			t.Fatalf("journal has %d %q entries, want >=%d after 5s: %+v", n, kind, minCount, entries)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// journalKindsOf narrows entries to one kind.
func journalKindsOf(entries []envhost.JournalEntry, kind envhost.JournalKind) []envhost.JournalEntry {
	var out []envhost.JournalEntry
	for _, e := range entries {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// TestInProcessHost_EnsureSpawnJournalsAndKeepaliveHoldsChild pins the live
// path: Ensure resolves the boot contract once (first-wins spec ledger),
// spawns a real child, journals the spawn fact readable through the envhost
// journal — child facts carry an empty attribution scope so every scope
// query sees them — and keepalive=true keeps the child alive after the
// engagement ends, with a later Ensure re-adopting the SAME child.
func TestInProcessHost_EnsureSpawnJournalsAndKeepaliveHoldsChild(t *testing.T) {
	booted := 0
	h, ledger, logBuf := testHost(t, true, func() { booted++ })
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		h.StopEnvHost(ctx)
	})

	scope := envhost.ScopeRef{Scope: "run", Instance: "inst-a"}
	w := ensureOnce(t, h, scope, "noop.sandbox", logBuf.String)
	if ledger.count() != 1 || ledger.pid(0) <= 0 {
		t.Fatalf("child spawn ledger = %d pids, first %d", ledger.count(), safeAt(ledger, 0))
	}

	// The wrapper wraps a live child the manager drives directly; the boot
	// contract's identities match the adapter's own handshake and stay
	// journaled.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info, err := w.Info(ctx)
	if err != nil {
		t.Fatalf("wrapper Info: %v", err)
	}
	if info.Version != "0.1.0" {
		t.Fatalf("wrapper Info version = %q, want the boot contract's version", info.Version)
	}

	// The engagement ledger: open + close a session; keepalive keeps the
	// child alive afterwards.
	if err := w.OpenSession(ctx, "sess-1", nil, nil); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	if err := w.CloseSession(ctx, "sess-1"); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	if w.ProcessExited() {
		t.Fatal("child exited after engagement close; want keepalive to keep it alive")
	}
	if reason, ok := w.SupervisionCrashReason(); ok {
		t.Fatalf("SupervisionCrashReason on a live child = (%q, true), want no classification", reason)
	}

	// Re-Ensure re-adopts the same child: no second spawn, no re-resolution
	// of the boot contract.
	w2 := ensureOnce(t, h, scope, "noop.sandbox", logBuf.String)
	if ledger.count() != 1 || booted != 1 {
		t.Fatalf("re-Ensure re-spawned (%d pids, %d boot resolutions); want re-adoption and first-wins boot", ledger.count(), booted)
	}
	if w2.Handle != w.Handle {
		t.Fatal("re-Ensure re-bound the wrapper to a different child")
	}

	// The journal carries the spawn fact and is readable from ANY scope
	// query, including instances the child never touched (empty child scope
	// attribution visible to all).
	for _, q := range []envhost.ScopeRef{
		{Scope: "run", Instance: "inst-a"},
		{Scope: "run", Instance: "inst-B"},
		{},
	} {
		entries, err := h.Journal(q, "noop.sandbox").Entries(ctx)
		if err != nil {
			t.Fatalf("journal query %+v: %v", q, err)
		}
		spawned := journalKindsOf(entries, envhost.JournalSpawned)
		if len(spawned) != 1 {
			t.Fatalf("journal query %+v: %d spawned entries, want 1: %+v", q, len(spawned), entries)
		}
		if spawned[0].Adapter != "noop.sandbox" {
			t.Errorf("spawn entry adapter = %q", spawned[0].Adapter)
		}
		if !strings.Contains(spawned[0].Detail, "sha256:abcd1234") ||
			!strings.Contains(spawned[0].Detail, "0.1.0") {
			t.Errorf("spawn entry detail = %q, want the pinned digest and version identities", spawned[0].Detail)
		}
	}
}

func safeAt(l *runnerLedger, n int) int {
	if l.count() <= n {
		return 0
	}
	return l.pid(n)
}

// TestInProcessHost_SignalKillMidExecuteClassifiesFromJournal is the
// acceptance-shaped gate: a real adapter child killed with SIGKILL while an
// Execute is in flight fails the call, and the wrapper's step-outcome
// classification comes from the journal's CrashClassified fact — the
// process-exited-family reason — not from transport string matching whose
// arms would name a transport close. Recovery flows: Ensure refuses to
// respawn the crashed child, and the Control fresh-handle arm displaces and
// respawns from the registered boot spec.
func TestInProcessHost_SignalKillMidExecuteClassifiesFromJournal(t *testing.T) {
	h, ledger, logBuf := testHost(t, true, nil)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		h.StopEnvHost(ctx)
	})

	scope := envhost.ScopeRef{Scope: "run", Instance: "inst-a"}
	w := ensureOnce(t, h, scope, "noop.sandbox", logBuf.String)
	pid := ledger.pid(0)

	execCtx, execCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer execCancel()
	if err := w.OpenSession(execCtx, "sess-exec", nil, nil); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}

	// Execute parked on the noop adapter's delay input: the child is
	// observably mid-Execute when the signal lands.
	sink := &recordingSink{}
	type execAttempt struct {
		res adapter.Result
		err error
	}
	resCh := make(chan execAttempt, 1)
	go func() {
		res, err := w.Execute(execCtx, "sess-exec", &workflow.StepNode{Name: "probe", Input: map[string]string{"delay_ms": "10000"}}, sink, nil)
		resCh <- execAttempt{res: res, err: err}
	}()
	time.Sleep(300 * time.Millisecond)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL child: %v", err)
	}

	select {
	case attempt := <-resCh:
		if attempt.err == nil {
			t.Fatal("Execute succeeded on a killed child, want the transport failure")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Execute on the killed child did not return within 30s")
	}

	// The classification the step outcome consumes comes from the journal —
	// the process-exited-family reason, NOT a transport-close string match.
	// The settle wait closes the go-plugin client's exited-flag flip window
	// so the unrecorded-exit push fires deterministically.
	time.Sleep(100 * time.Millisecond)
	reason, ok := w.SupervisionCrashReason()
	if !ok || reason != adapterhost.CrashReasonProcessTerminated {
		t.Fatalf("SupervisionCrashReason = (%q, %t), want (%q, true)", reason, ok, adapterhost.CrashReasonProcessTerminated)
	}

	// The journal is readable with the full supervision truth: spawn, the
	// non-graceful signal exit, and the crash classification.
	entries := waitJournalKind(t, h, scope, "noop.sandbox", envhost.JournalCrashClassified, 1)
	spawned := journalKindsOf(entries, envhost.JournalSpawned)
	exited := journalKindsOf(entries, envhost.JournalExited)
	if len(spawned) != 1 || len(exited) != 1 {
		t.Fatalf("journal spawn/exit shapes = %d/%d, want 1/1: %+v", len(spawned), len(exited), entries)
	}
	if !strings.Contains(exited[0].Detail, "graceful false") {
		t.Errorf("exited detail = %q, want the non-graceful kill death", exited[0].Detail)
	}
	// The runner-injected child owns its own exec.Cmd (go-plugin lane), so
	// the wait status is not on the host side: the exit fact carries the
	// unknown-exit fallback (-1, 0) while the crash classification still
	// lands in the process-terminated family from the journal.
	if !strings.Contains(exited[0].Detail, "exit_code -1") || !strings.Contains(exited[0].Detail, "signal 0") {
		t.Errorf("exited detail = %q, want the unknown-wait-status fallback shape", exited[0].Detail)
	}
	if detail := spawned[0].Detail; !strings.Contains(detail, "pid ") || strings.Contains(detail, "pid 0") {
		t.Errorf("spawn entry detail = %q, want a real child pid", detail)
	}
	if !strings.Contains(entries[len(entries)-1].Detail, adapterhost.CrashReasonProcessTerminated) {
		t.Errorf("last journal entry detail = %q, want the classification reason", entries[len(entries)-1].Detail)
	}
	// A crashed child is never respawned by Ensure — crash policy owns
	// recovery through the fresh-handle arm.
	ensureCtx, ensureCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer ensureCancel()
	if _, err := h.Ensure(ensureCtx, scope, []envhost.Definition{{Name: "noop.sandbox", Type: "noop"}}); err == nil {
		t.Fatal("Ensure after a signal-crashed child succeeded, want the hard no-respawn error")
	}

	// The fresh-handle arm settles a live handle wrapping a DIFFERENT
	// generation: the respawn honors the registered boot spec verbatim
	// (same runner lane, pinned identities).
	resp, err := h.Control(execCtx, &envhost.ControlRequest{
		Scope:   scope,
		Adapter: "noop.sandbox",
		Kind:    envhost.ControlEnsureFreshHandle,
		Stale:   w,
	})
	if err != nil {
		t.Fatalf("Control EnsureFreshHandle: %v", err)
	}
	fresh, isWrapper := resp.Handle.(*supervisedHostHandle)
	if !isWrapper || fresh == nil {
		t.Fatalf("fresh handle = %T, want *supervisedHostHandle", resp.Handle)
	}
	if fresh.ProcessExited() {
		t.Fatal("fresh handle wraps an exited child")
	}
	if reason, ok := fresh.SupervisionCrashReason(); ok {
		t.Fatalf("fresh handle crash reason = (%q, true), want none", reason)
	}
	if fresh.Handle == w.Handle {
		t.Fatal("fresh handle is the stale child; want the displaced respawn")
	}
	if ledger.count() != 2 || ledger.pid(1) == pid {
		t.Fatalf("spawn ledger = %d pids (%d, %d), want two distinct generations", ledger.count(), safeAt(ledger, 0), safeAt(ledger, 1))
	}
	waitJournalKind(t, h, scope, "noop.sandbox", envhost.JournalSpawned, 2)

	// The fresh handle is usable: close its session engagement cleanly.
	if err := fresh.OpenSession(execCtx, "sess-after", nil, nil); err != nil {
		t.Fatalf("fresh OpenSession: %v", err)
	}
	if err := fresh.CloseSession(execCtx, "sess-after"); err != nil {
		t.Fatalf("fresh CloseSession: %v", err)
	}
}

// TestInProcessHost_KeepAliveFalseIdleKillsAndRespawns pins the idle-kill
// arm: with child keepalive FALSE, the last engagement drop kills the child
// as peer-initiated — a graceful Exited journal fact, no crash
// classification — and the next Ensure respawns a fresh child from the boot
// spec.
func TestInProcessHost_KeepAliveFalseIdleKillsAndRespawns(t *testing.T) {
	h, ledger, logBuf := testHost(t, false, nil)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		h.StopEnvHost(ctx)
	})

	scope := envhost.ScopeRef{Scope: "iter", Instance: "inst-a"}
	w := ensureOnce(t, h, scope, "noop.sandbox", logBuf.String)
	pid1 := ledger.pid(0)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := w.OpenSession(ctx, "sess-iter", nil, nil); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	if err := w.CloseSession(ctx, "sess-iter"); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	waitPIDDead(t, pid1)

	// The idle kill is intentional: a graceful Exited fact, NO crash
	// classification, no crash reason served by the wrapper.
	entries := waitJournalKind(t, h, scope, "noop.sandbox", envhost.JournalExited, 1)
	for _, e := range journalKindsOf(entries, envhost.JournalCrashClassified) {
		t.Errorf("idle-kill journaled a crash classification: %v", e)
	}
	jExited := journalKindsOf(entries, envhost.JournalExited)
	if len(jExited) != 1 || !strings.Contains(jExited[0].Detail, "graceful true") {
		t.Fatalf("exited entries = %+v, want one graceful peer-initiated death", jExited)
	}
	if reason, ok := w.SupervisionCrashReason(); ok {
		t.Fatalf("SupervisionCrashReason after an idle kill = (%q, true), want (\"\", false)", reason)
	}

	// The next Ensure respawns from the boot spec: a new, distinct child.
	w2 := ensureOnce(t, h, scope, "noop.sandbox", logBuf.String)
	if ledger.count() != 2 {
		t.Fatalf("spawn ledger = %d pids, want two generations", ledger.count())
	}
	pid2 := ledger.pid(1)
	if pid1 == pid2 {
		t.Fatalf("Ensure re-adopted the idle-killed child pid %d, want a fresh respawn", pid2)
	}
	if w2.ProcessExited() {
		t.Fatal("respawned child is dead")
	}
	if reason, ok := w2.SupervisionCrashReason(); ok {
		t.Fatalf("respawned child crash reason = (%q, true), want none", reason)
	}
	waitJournalKind(t, h, scope, "noop.sandbox", envhost.JournalSpawned, 2)
}

// TestInProcessHost_ControlNoListener pins the contract pins that need no
// spawn: scope registration is accepted (no tokens to rotate), listener
// discovery is refused deterministically, and unknown arms fail closed.
// A boot hook that refuses every resolution proves the arms resolve nothing.
func TestInProcessHost_ControlNoListener(t *testing.T) {
	h := NewInProcessHost(HostConfig{
		KeepAlive: true,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, func(context.Context, envhost.Definition) (ChildBoot, error) {
		return ChildBoot{}, errors.New("no child resolution expected")
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		h.StopEnvHost(ctx)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := h.Control(ctx, &envhost.ControlRequest{
		Scope: envhost.ScopeRef{Scope: "reg"},
		Kind:  envhost.ControlRegisterScope,
		Token: "tok-1",
	}); err != nil {
		t.Fatalf("ControlRegisterScope: %v", err)
	}
	if _, err := h.Control(ctx, &envhost.ControlRequest{
		Scope: envhost.ScopeRef{Scope: "reg"},
		Kind:  envhost.ControlListenAddress,
	}); err == nil {
		t.Fatal("ControlListenAddress succeeded; an in-process host serves no listener")
	}
	if _, err := h.Control(ctx, &envhost.ControlRequest{Kind: envhost.ControlKind(99)}); err == nil {
		t.Fatal("unknown control arm accepted")
	}
}
