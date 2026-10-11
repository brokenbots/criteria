package peer

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	criteriav1 "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	"github.com/brokenbots/criteria/workflow"
)

// buildNoopAdapter compiles the in-tree noop conformance fixture into a
// directory and returns the binary path. Building from source keeps the boot
// test self-contained (no external OCI pull). Requires the go toolchain on
// PATH — `go test` already implies it.
func buildNoopAdapter(t *testing.T) string {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH; cannot build the noop fixture")
	}
	bin := filepath.Join(t.TempDir(), "criteria-adapter-noop")
	cmd := exec.Command(goBin, "build", "-o", bin, "./internal/adapter/conformance/testdata/noop")
	cmd.Dir = "../.." // package test dir → repo root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build noop adapter: %v\n%s", err, out)
	}
	return bin
}

func captureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// TestPeerRuntime_BootSpawnsRealChild is the loopback boot test: the peer
// resolves a bundled test adapter, spawns it as a real go-plugin child,
// Info() succeeds, structured logs are present, and shutdown terminates the
// process.
func TestPeerRuntime_BootSpawnsRealChild(t *testing.T) {
	if testing.Short() {
		t.Skip("boot test spawns a real adapter subprocess")
	}
	bin := buildNoopAdapter(t)

	cfg, err := LoadConfig(getenvFrom(map[string]string{EnvRemoteHost: "127.0.0.1:7999"}))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.AdapterName = "noop"
	cfg.AdapterBinary = bin
	cfg.JournalLimit = 8

	var logs bytes.Buffer
	rt := NewRuntime(&cfg, captureLogger(&logs))
	rt.exitPoll = 20 * time.Millisecond
	rt.shutdownGrace = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Boot(ctx); err != nil {
		t.Fatalf("boot: %v", err)
	}
	if rt.Child() == nil {
		t.Fatal("boot left child handle nil")
	}

	// Spawned fact: pid, binary, digest, version.
	events := rt.Journal().Replay(0)
	if len(events) != 1 {
		t.Fatalf("journal has %d events after boot, want 1 spawned", len(events))
	}
	spawned := events[0].GetSpawned()
	if spawned == nil {
		t.Fatalf("event 1 is %T, want spawned", events[0].GetKind())
	}
	if spawned.GetPid() <= 0 {
		t.Errorf("spawned pid = %d, want a real OS pid", spawned.GetPid())
	}
	if spawned.GetBinary() != bin {
		t.Errorf("spawned binary = %q, want %q", spawned.GetBinary(), bin)
	}
	if spawned.GetVersion() != "0.1.0" {
		t.Errorf("spawned version = %q, want adapter-reported 0.1.0", spawned.GetVersion())
	}
	if spawned.GetDigest() != "" {
		t.Errorf("spawned digest = %q, want empty (no digest configured)", spawned.GetDigest())
	}
	if events[0].GetAdapterType() != "noop" {
		t.Errorf("adapter_type = %q, want noop", events[0].GetAdapterType())
	}

	// The child is a live go-plugin process, not a builtin stub.
	child := rt.Child()
	if _, err := child.Info(ctx); err != nil {
		t.Fatalf("Info after boot: %v", err)
	}
	if adapterhost.ProcessExited(child) {
		t.Fatal("child reported exited right after boot")
	}

	// Structured logs present.
	logText := logs.String()
	if !strings.Contains(logText, "peer child spawned") {
		t.Errorf("missing 'peer child spawned' log:\n%s", logText)
	}
	for _, field := range []string{`"adapter":"noop"`, `"binary":`, `"pid":`, `"version":"0.1.0"`} {
		if !strings.Contains(logText, field) {
			t.Errorf("spawn log missing %s:\n%s", field, logText)
		}
	}

	// Shutdown kills the child and journals the graceful exit.
	if err := rt.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !adapterhost.ProcessExited(child) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !adapterhost.ProcessExited(child) {
		t.Fatal("child process still alive after shutdown")
	}
	last := rt.LastExit()
	if last == nil {
		t.Fatal("LastExit = nil after shutdown")
	}
	if !last.GetGraceful() {
		t.Errorf("exit graceful = false, want true (peer-initiated)")
	}
	// The peer reaps its own child: Shutdown's teardown (SIGINT to the
	// process group, then go-plugin Kill) lands as a clean exit, and the
	// wait status read after the reap carries the real code (0, 0).
	if last.GetExitCode() != 0 || last.GetSignal() != 0 {
		t.Errorf("exit status = (code %d, signal %d), want (0, 0) for the peer's clean teardown", last.GetExitCode(), last.GetSignal())
	}

	events = rt.Journal().Replay(0)
	if len(events) != 2 {
		t.Fatalf("journal has %d events after shutdown, want spawned+exited", len(events))
	}
	if exited := events[1].GetExited(); exited == nil || !exited.GetGraceful() {
		t.Errorf("event 2 = %+v, want graceful ProcessExited", events[1].GetKind())
	}
	if !strings.Contains(logs.String(), "peer shutdown complete") {
		t.Errorf("missing 'peer shutdown complete' log:\n%s", logs.String())
	}
}

// fakeHandle is a builtin adapter stub exercising the exit watcher without a
// real subprocess (registered via loader builtins, which take precedence
// over discovery).
type fakeHandle struct {
	exited atomic.Bool
	killed atomic.Bool
}

func (f *fakeHandle) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: "fakex", Version: "9.9.9"}, nil
}
func (f *fakeHandle) OpenSession(context.Context, string, map[string]string, map[string]string) error {
	return nil
}
func (f *fakeHandle) Execute(_ context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink, _ *v2.ExecutionRejection) (adapter.Result, error) {
	return adapter.Result{}, nil
}
func (f *fakeHandle) CloseSession(context.Context, string) error { return nil }
func (f *fakeHandle) Kill() {
	f.killed.Store(true)
	// A real Kill terminates the process; the exit watcher observes the
	// resulting exit state on its next poll.
	f.exited.Store(true)
}
func (f *fakeHandle) Pause(context.Context, string) error  { return nil }
func (f *fakeHandle) Resume(context.Context, string) error { return nil }
func (f *fakeHandle) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}
func (f *fakeHandle) Snapshot(context.Context, string) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{}, nil
}
func (f *fakeHandle) Restore(context.Context, string, []byte, uint32) error { return nil }
func (f *fakeHandle) ProcessExited() bool                                   { return f.exited.Load() }

// TestPeerRuntime_ExitWatcherClassifiesCrash journals Exited + CrashClassified
// with the shared taxonomy reason when the child dies without peer shutdown.
func TestPeerRuntime_ExitWatcherClassifiesCrash(t *testing.T) {
	fake := &fakeHandle{}
	cfg, err := LoadConfig(getenvFrom(map[string]string{EnvRemoteHost: "h:1"}))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.AdapterName = "fakex"
	cfg.AdapterBinary = "/unused" // builtins bypass discovery

	rt := NewRuntime(&cfg, captureLogger(&bytes.Buffer{}))
	rt.exitPoll = 10 * time.Millisecond
	rt.loader.RegisterBuiltin("fakex", func() adapterhost.Handle { return fake })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Boot(ctx); err != nil {
		t.Fatalf("boot: %v", err)
	}
	fake.exited.Store(true)
	<-rt.watchDone // watcher observed the exit and stopped

	events := rt.Journal().Replay(0)
	if len(events) != 3 {
		t.Fatalf("journal has %d events, want spawned+exited+crash", len(events))
	}
	if events[1].GetExited() == nil || events[1].GetExited().GetGraceful() {
		t.Errorf("event 2 = %T graceful=%v, want ungraceful exited", events[1].GetKind(), events[1].GetExited().GetGraceful())
	}
	crash := events[2].GetCrash()
	if crash == nil {
		t.Fatalf("event 3 = %T, want crash", events[2].GetKind())
	}
	if crash.GetReason() != adapterhost.CrashReasonProcessTerminated {
		t.Errorf("crash reason = %q, want %q", crash.GetReason(), adapterhost.CrashReasonProcessTerminated)
	}
	if last := rt.LastExit(); last == nil || last.GetGraceful() {
		t.Errorf("LastExit = %+v, want ungraceful", last)
	}

	// Shutdown after the crash is a no-op for the journal (exit already
	// recorded) and must not re-classify the crash as graceful.
	if err := rt.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if got := len(rt.Journal().Replay(0)); got != 3 {
		t.Errorf("journal has %d events after shutdown, want 3 (exit recorded once)", got)
	}
}

// TestPeerRuntime_KillChildJournalsCrashSequence verifies the crash emission
// points against a real child: SIGKILL of the adapter process journals
// ProcessSpawned → ProcessExited (ungraceful; go-plugin does not expose the
// signal or exit status, so exit_code is recorded as -1) → CrashClassified
// with the shared taxonomy reason, in journal order with gapless sequence
// numbers.
func TestPeerRuntime_KillChildJournalsCrashSequence(t *testing.T) {
	if testing.Short() {
		t.Skip("crash test spawns a real adapter subprocess")
	}
	bin := buildNoopAdapter(t)

	cfg, err := LoadConfig(getenvFrom(map[string]string{EnvRemoteHost: "127.0.0.1:7998"}))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.AdapterName = "noop"
	cfg.AdapterBinary = bin
	cfg.JournalLimit = 8

	rt := NewRuntime(&cfg, captureLogger(&bytes.Buffer{}))
	rt.exitPoll = 20 * time.Millisecond
	rt.shutdownGrace = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Boot(ctx); err != nil {
		t.Fatalf("boot: %v", err)
	}
	events := rt.Journal().Replay(0)
	if len(events) != 1 || events[0].GetSpawned() == nil {
		t.Fatalf("journal after boot = %v, want 1 spawned", events)
	}
	pid := events[0].GetSpawned().GetPid()
	if pid <= 0 {
		t.Fatalf("spawned pid = %d, want a real OS pid", pid)
	}

	// kill -9: the go-plugin client reaps the child and reports the exit;
	// the peer's exit watcher classifies it against the shared taxonomy.
	if err := syscall.Kill(int(pid), syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL child: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for len(rt.Journal().Replay(0)) < 3 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	events = rt.Journal().Replay(0)
	if len(events) != 3 {
		t.Fatalf("journal has %d events after SIGKILL, want spawned+exited+crash: %+v", len(events), events)
	}
	for i, ev := range events {
		if want := events[0].GetEventSeq() + uint64(i); ev.GetEventSeq() != want {
			t.Errorf("event %d seq = %d, want gapless %d", i, ev.GetEventSeq(), want)
		}
	}
	exited := events[1].GetExited()
	if exited == nil {
		t.Fatalf("event 2 = %T, want exited", events[1].GetKind())
	}
	if exited.GetGraceful() {
		t.Errorf("exit graceful = true, want false (SIGKILL crash)")
	}
	if exited.GetExitCode() != -1 || exited.GetSignal() != 9 {
		t.Errorf("exit status = (code %d, signal %d), want (-1, 9) — the real wait status of a SIGKILLed child", exited.GetExitCode(), exited.GetSignal())
	}
	crash := events[2].GetCrash()
	if crash == nil {
		t.Fatalf("event 3 = %T, want crash", events[2].GetKind())
	}
	if crash.GetReason() != adapterhost.CrashReasonProcessTerminated {
		t.Errorf("crash reason = %q, want %q", crash.GetReason(), adapterhost.CrashReasonProcessTerminated)
	}
	if !strings.Contains(crash.GetDetail(), "signal 9") {
		t.Errorf("crash detail = %q, want the real wait-status facts (signal 9)", crash.GetDetail())
	}

	// Shutdown after the crash records nothing further.
	if err := rt.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if got := len(rt.Journal().Replay(0)); got != 3 {
		t.Errorf("journal has %d events after shutdown, want 3 (exit recorded once)", got)
	}
}

func TestPeerRuntime_BootRequiresResolvedIdentity(t *testing.T) {
	cfg, err := LoadConfig(getenvFrom(map[string]string{EnvRemoteHost: "h:1"}))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.AdapterName = ""
	cfg.AdapterBinary = ""
	rt := NewRuntime(&cfg, captureLogger(&bytes.Buffer{}))
	err = rt.Boot(context.Background())
	if err == nil || !strings.Contains(err.Error(), "adapter name not resolved") {
		t.Fatalf("want unresolved-name error, got %v", err)
	}
}

func TestPeerRuntime_BootFailsWhenBinaryMissing(t *testing.T) {
	cfg, err := LoadConfig(getenvFrom(map[string]string{EnvRemoteHost: "h:1"}))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.AdapterName = "missingx"
	cfg.AdapterBinary = "/nonexistent/criteria-adapter-missingx"

	rt := NewRuntime(&cfg, captureLogger(&bytes.Buffer{}))
	err = rt.Boot(context.Background())
	if err == nil || !strings.Contains(err.Error(), "start adapter") {
		t.Fatalf("want start failure, got %v", err)
	}
	if n := len(rt.Journal().Replay(0)); n != 0 {
		t.Errorf("journal has %d events, want 0 (nothing spawned)", n)
	}
}

func TestPeerRuntime_ShutdownWithoutBoot(t *testing.T) {
	cfg, err := LoadConfig(getenvFrom(map[string]string{EnvRemoteHost: "h:1"}))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	rt := NewRuntime(&cfg, captureLogger(&bytes.Buffer{}))
	if err := rt.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown without boot: %v", err)
	}
	if rt.LastExit() != nil {
		t.Errorf("LastExit = %+v, want nil (nothing was spawned)", rt.LastExit())
	}
	if n := len(rt.Journal().Replay(0)); n != 0 {
		t.Errorf("journal has %d events, want 0", n)
	}
	// Idempotent: second shutdown must not panic.
	if err := rt.Shutdown(context.Background()); err != nil {
		t.Fatalf("second shutdown: %v", err)
	}
}

func TestPeerRuntime_BootTwiceRejected(t *testing.T) {
	cfg, err := LoadConfig(getenvFrom(map[string]string{EnvRemoteHost: "h:1"}))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.AdapterName = "missingx"
	cfg.AdapterBinary = "/nonexistent/criteria-adapter-missingx"
	rt := NewRuntime(&cfg, captureLogger(&bytes.Buffer{}))
	ctx := context.Background()
	if err := rt.Boot(ctx); err == nil {
		t.Fatal("first boot should fail (missing binary)")
	}
	// The booted flag is set even on failure: a second Boot is rejected, the
	// runtime must be recreated instead.
	if err := rt.Boot(ctx); err == nil || !strings.Contains(err.Error(), "already booted") {
		t.Fatalf("second boot error = %v, want already booted", err)
	}
}

// TestPeerRuntime_SpawnLogIsJSONShape guards the structured-log contract.
func TestPeerRuntime_SpawnLogIsJSONShape(t *testing.T) {
	var logs bytes.Buffer
	log := captureLogger(&logs)
	log.Info("peer child spawned", "adapter", "noop", "pid", 123)
	line := strings.TrimSpace(logs.String())
	var entry map[string]any
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		t.Fatalf("log line is not JSON: %v (%s)", err, line)
	}
	if entry["msg"] != "peer child spawned" || entry["adapter"] != "noop" {
		t.Errorf("log entry keys wrong: %v", entry)
	}
}

// TestPeerRuntime_KillChildPerDefinitionContainment pins the KB-242 kill
// chain at the peer's only seam: with one child process PER ADAPTER
// DEFINITION (two copilot-class definitions under distinct names, the
// pair_programming_loop shape — KB-242), a Control(kill_child) addressed at
// one definition must terminate only the addressed child and leave the
// sibling definition's process running. Under the KB-213 kind-keyed grammar
// (children collapsed by adapter TYPE) the branch_repair scope's teardown
// kill landed on the developer/reviewer children too — the deterministic
// `initialize adapter "copilot.developer": adapter "copilot" handshake: rpc
// error: code = Canceled desc = grpc: the client connection is closing` death
// recorded in KB-242.
func TestPeerRuntime_KillChildPerDefinitionContainment(t *testing.T) {
	if testing.Short() {
		t.Skip("kill test spawns real adapter subprocesses")
	}
	bin := buildNoopAdapter(t)

	cfg, err := LoadConfig(getenvFrom(map[string]string{EnvRemoteHost: "127.0.0.1:7997"}))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Adapters = []AdapterSpec{
		{Name: "copilot.developer", Binary: bin},
		{Name: "copilot.branch_repair", Binary: bin},
	}
	if err := cfg.Resolve(); err != nil {
		t.Fatalf("resolve manifest children: %v", err)
	}
	cfg.JournalLimit = 8

	rt := NewRuntime(&cfg, captureLogger(&bytes.Buffer{}))
	rt.exitPoll = 20 * time.Millisecond
	rt.shutdownGrace = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Boot(ctx); err != nil {
		t.Fatalf("boot: %v", err)
	}

	rt.mu.Lock()
	dev := rt.children["copilot.developer"]
	repair := rt.children["copilot.branch_repair"]
	rt.mu.Unlock()
	if dev == nil || repair == nil {
		t.Fatalf("children = %v, want both copilot.developer and copilot.branch_repair spawned", func() []string {
			rt.mu.Lock()
			defer rt.mu.Unlock()
			names := make([]string, 0, len(rt.children))
			for name := range rt.children {
				names = append(names, name)
			}
			return names
		}())
	}
	if dev.handle == nil || repair.handle == nil {
		t.Fatal("both children must carry live go-plugin handles after boot")
	}

	// The branch_repair scope exits; its teardown sends kill_child for its
	// own definition. The peer must kill exactly that child.
	resp := rt.Control(ctx, &criteriav1.ControlRequest{
		Kind:        &criteriav1.ControlRequest_KillChild{KillChild: &criteriav1.KillChild{}},
		AdapterType: "copilot.branch_repair",
		GraceMs:     10,
	})
	if !resp.GetAccepted() {
		t.Fatalf("kill_child(copilot.branch_repair) rejected: %s", resp.GetDetail())
	}

	// The addressed child exits (grace is peer-initiated, so graceful); the
	// sibling definition's child must still be alive.
	deadline := time.Now().Add(5 * time.Second)
	for {
		rt.mu.Lock()
		exited := repair.exited
		rt.mu.Unlock()
		if exited || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	rt.mu.Lock()
	repairExited := repair.exited
	repairKilled := repair.killRequested
	devKilled := dev.killRequested
	rt.mu.Unlock()
	if !repairExited {
		t.Fatalf("copilot.branch_repair child neither exited nor died within 5s of its addressed kill_child")
	}
	if !repairKilled {
		t.Errorf("copilot.branch_repair killRequested = false, want true (peer-initiated exit)")
	}
	if devKilled {
		t.Errorf("copilot.developer killRequested = true; the sibling definition's child must not be killed by another definition's teardown (KB-242)")
	}
	if adapterhost.ProcessExited(dev.handle) {
		t.Errorf("copilot.developer child process exited; the sibling definition's child must stay live (KB-242 kill chain)")
	}

	// The sibling definition remains killable by its own address.
	resp = rt.Control(ctx, &criteriav1.ControlRequest{
		Kind:        &criteriav1.ControlRequest_KillChild{KillChild: &criteriav1.KillChild{}},
		AdapterType: "copilot.developer",
		GraceMs:     10,
	})
	if !resp.GetAccepted() {
		t.Fatalf("kill_child(copilot.developer) after the sibling kill rejected: %s", resp.GetDetail())
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		rt.mu.Lock()
		exited := dev.exited
		rt.mu.Unlock()
		if exited || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	rt.mu.Lock()
	devExited := dev.exited
	rt.mu.Unlock()
	if !devExited {
		t.Fatalf("copilot.developer child not exited within 5s of its addressed kill_child")
	}

	if err := rt.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}
