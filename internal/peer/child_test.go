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
	"testing"
	"time"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
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
	if last.GetExitCode() != -1 {
		t.Errorf("exit code = %d, want -1 (go-plugin does not expose the status)", last.GetExitCode())
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
}

func (f *fakeHandle) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{Name: "fakex", Version: "9.9.9"}, nil
}
func (f *fakeHandle) OpenSession(context.Context, string, map[string]string, map[string]string) error {
	return nil
}
func (f *fakeHandle) Execute(context.Context, string, *workflow.StepNode, adapter.EventSink) (adapter.Result, error) {
	return adapter.Result{}, nil
}
func (f *fakeHandle) CloseSession(context.Context, string) error { return nil }
func (f *fakeHandle) Kill()                                      {}
func (f *fakeHandle) Pause(context.Context, string) error        { return nil }
func (f *fakeHandle) Resume(context.Context, string) error       { return nil }
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
