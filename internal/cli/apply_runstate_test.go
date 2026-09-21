package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/runstate"
)

// newTestLogger returns a quiet test logger.
func newTestLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// freePort asks the kernel for an unused loopback port (test-scoped).
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// waitHealth polls /health until the server answers 204.
func waitHealth(t *testing.T, base string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		res, err := http.Get(base + "/health")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusNoContent {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server at %s never became healthy", base)
}

// TestOpenRunEventsFile verifies the run-events tee file is created
// (append-only) under <home>/runs/<runID>/events.ndjson.
func TestOpenRunEventsFile(t *testing.T) {
	t.Setenv("CRITERIA_HOME", t.TempDir())
	runID := "run-events-1"
	w, closeFn, err := openRunEventsFile(runID)
	if err != nil {
		t.Fatalf("openRunEventsFile: %v", err)
	}
	if _, err := io.WriteString(w, "line-1\n"); err != nil {
		t.Fatal(err)
	}
	// Reopen: append-only, so the first line must survive.
	w2, closeFn2, err := openRunEventsFile(runID)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := io.WriteString(w2, "line-2\n"); err != nil {
		t.Fatal(err)
	}
	closeFn2()
	closeFn()

	path := filepath.Join(os.Getenv("CRITERIA_HOME"), "runs", runID, "events.ndjson")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(b) != "line-1\nline-2\n" {
		t.Errorf("events file = %q", b)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v err=%v, want 0600", fi.Mode(), err)
	}
}

// TestWorkflowSourceHash pins the workflowHash derivation.
func TestWorkflowSourceHash(t *testing.T) {
	h := workflowSourceHash([]byte(`workflow "x" {}`))
	if len(h) != 64 || strings.ContainsAny(h, "ghijklmnopqrstuvwxyz") {
		t.Errorf("hash = %q", h)
	}
	if h != workflowSourceHash([]byte(`workflow "x" {}`)) {
		t.Error("hash not deterministic")
	}
	if h == workflowSourceHash([]byte(`workflow "y" {}`)) {
		t.Error("hash insensitive to source")
	}
}

// TestStartLocalRunStateServer verifies the apply wiring end-to-end over a
// real socket: loopback bind, scoped run list, the events stream, and the
// stop verb cancelling the run context.
func TestStartLocalRunStateServer(t *testing.T) {
	t.Setenv("CRITERIA_HOME", t.TempDir())
	runID := "run-srv-1"

	// Seed the wired run: an events tee plus a live state file.
	if _, _, err := openRunEventsFile(runID); err != nil {
		t.Fatal(err)
	}
	st := &localRunState{
		PID:        os.Getpid(), // this test process is alive
		RunID:      runID,
		Workflow:   "wf-demo",
		StartedAt:  time.Now(),
		CriteriaID: "crit-1",
	}
	if err := writeLocalRunState(st); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(os.Getenv("CRITERIA_HOME"), "runs", runID, "events.ndjson"), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"schema_version":1,"seq":1,"run_id":"run-srv-1","payload_type":"RunStarted","payload":{"workflow_name":"wf-demo"}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	ctx, cancelRun := context.WithCancel(context.Background())
	url, stop, err := startLocalRunStateServer(newTestLogger(t), runID, 0, cancelRun)
	if err != nil {
		t.Fatalf("startLocalRunStateServer: %v", err)
	}
	defer stop()

	base := strings.TrimSuffix(url, "/")
	waitHealth(t, base)

	// Scoped list: only the wired run id is served.
	res, err := http.Get(base + "/runs")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var page struct {
		Runs []runstate.Run `json:"runs"`
	}
	if err := json.NewDecoder(res.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if len(page.Runs) != 1 || page.Runs[0].RunID != runID {
		t.Errorf("scoped runs = %+v, want only %s", page.Runs, runID)
	}

	// Other run ids are refused even when data exists on disk.
	other := filepath.Join(os.Getenv("CRITERIA_HOME"), "runs", "other-run")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "events.ndjson"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res2, err := http.Get(base + "/runs/other-run")
	if err != nil {
		t.Fatal(err)
	}
	res2.Body.Close()
	if res2.StatusCode != http.StatusNotFound {
		t.Errorf("unscoped run status = %d, want 404", res2.StatusCode)
	}

	// The events route serves the teed stream.
	res3, err := http.Get(base + "/runs/" + runID + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer res3.Body.Close()
	var events struct {
		Events []runstate.EventEnvelope `json:"events"`
	}
	if err := json.NewDecoder(res3.Body).Decode(&events); err != nil {
		t.Fatal(err)
	}
	if len(events.Events) != 1 || events.Events[0].Type != "RunStarted" {
		t.Errorf("events = %+v", events.Events)
	}

	// The stop verb cancels the run context.
	res4, err := http.Post(base+"/runs/"+runID+"/stop", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	res4.Body.Close()
	if res4.StatusCode != http.StatusOK {
		t.Errorf("stop status = %d, want 200", res4.StatusCode)
	}
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("stop verb did not cancel the run context")
	}
}

// TestAttachLocalRunStateServer exercises the apply-side wiring helper over a
// real socket: the server becomes healthy, the returned stop tears it down
// and cancels the engine context. Without the srvStop/self-capture fix the
// returned stop recursed into itself and overflowed the stack at run end.
func TestAttachLocalRunStateServer(t *testing.T) {
	t.Setenv("CRITERIA_HOME", t.TempDir())
	port := freePort(t)
	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	parent := context.Background()
	runCtx, stop := attachLocalRunStateServer(parent, newTestLogger(t), "run-attach-1", port)
	defer func() {
		// stop must stay idempotent-safe for double teardown paths.
		stop()
	}()
	if runCtx == parent {
		t.Fatal("attach returned the parent context; engine stop wiring would be lost")
	}
	if runCtx.Err() != nil {
		t.Fatalf("fresh run context already canceled: %v", runCtx.Err())
	}

	waitHealth(t, base)

	stop()
	select {
	case <-runCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("stop did not cancel the run context")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		res, err := http.Get(base + "/health")
		if err != nil {
			break
		}
		res.Body.Close()
		if time.Now().After(deadline) {
			t.Fatal("server still answering after stop")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestAttachLocalRunStateServer_BindFailure verifies the lifeline-not-a-gate
// rule: when the listener cannot bind, apply keeps the parent context and the
// returned stop is still a safe cancel.
func TestAttachLocalRunStateServer_BindFailure(t *testing.T) {
	t.Setenv("CRITERIA_HOME", t.TempDir())
	port := freePort(t)
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("occupy port: %v", err)
	}
	defer ln.Close()

	parent := context.Background()
	runCtx, stop := attachLocalRunStateServer(parent, newTestLogger(t), "run-attach-2", port)
	if runCtx != parent {
		t.Fatalf("bind failure must fall back to the parent context")
	}
	stop()
}

// TestServeUICmdFixedPort runs serve-ui on a pinned port and exercises the
// read surface over the real command path.
func TestServeUICmdFixedPort(t *testing.T) {
	home := t.TempDir()
	runID := "run-ui-2"
	dir := filepath.Join(home, "runs", runID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	events := `{"schema_version":1,"seq":1,"run_id":"run-ui-2","payload_type":"RunStarted","payload":{"workflow_name":"wf-ui"}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "events.ndjson"), []byte(events), 0o600); err != nil {
		t.Fatal(err)
	}

	port := freePort(t)
	cmd := NewServeUICmd()
	cmd.SetArgs([]string{"--home", home, "--port", fmt.Sprint(port)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd.SetContext(ctx)
	errCh := make(chan error, 1)
	go func() { errCh <- cmd.Execute() }()

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitHealth(t, base)

	res, err := http.Get(base + "/runs/" + runID)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var run runstate.Run
	if err := json.NewDecoder(res.Body).Decode(&run); err != nil || run.RunID != runID || run.Status != runstate.StatusFailed {
		t.Errorf("run decode: %v (%+v)", err, run)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("serve-ui run error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve-ui did not stop")
	}
}

// TestServeUICmdRefusesNonLoopback pins the locked bind rule at the command
// level.
func TestServeUICmdRefusesNonLoopback(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "runs", "run-bind")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events.ndjson"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := NewServeUICmd()
	cmd.SetArgs([]string{"--home", home, "--host", "0.0.0.0", "--port", fmt.Sprint(freePort(t))})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Errorf("non-loopback bind err = %v, want loopback refusal", err)
	}
}

// TestServeUICmdRefusesMissingHome documents the standalone UX: no state
// dir means there is nothing to serve.
func TestServeUICmdRefusesMissingHome(t *testing.T) {
	cmd := NewServeUICmd()
	cmd.SetArgs([]string{"--home", filepath.Join(t.TempDir(), "empty")})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "no run state directory") {
		t.Errorf("missing home err = %v", err)
	}
}
