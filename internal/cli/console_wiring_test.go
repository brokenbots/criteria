package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/run"
)

// startFakeCastle serves the three castle.v1.ConsoleService procedures the
// loopback console proxies, recording every request.
func startFakeCastle(t *testing.T) (string, *[]string) {
	t.Helper()
	logs := &[]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*logs = append(*logs, fmt.Sprintf("%s %s %s", r.Method, r.URL.Path, body))
		if r.URL.Path == "/castle.v1.ConsoleService/Login" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"sessionToken":"kx","username":"ops"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, logs
}

// consolePost POSTs one Connect unary request to the loopback URL the
// started listener reported.
func consolePost(t *testing.T, base, proc, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/castle.v1.ConsoleService/"+proc, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("build %s: %v", proc, err)
	}
	req.Host = "127.0.0.1:0"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", proc, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// consoleBodyString drains a response body.
func consoleBodyString(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s body: %v", resp.Request.URL, err)
	}
	return string(b)
}

// TestLocalRunStateServerConsole pins the KB-328 wiring end to end over the
// real socket: with a paused local run, the started run-state server's
// console surface proxies castle Login to the configured control plane, and
// ResolveResume delivers the decision through the run's own control
// service — refused for a foreign run id, failed_precondition on a signal
// mismatch, accepted on the matching signal.
func TestLocalRunStateServerConsole(t *testing.T) {
	requireNoGoroutineLeak(t)
	home := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", home)

	castleURL, castleLog := startFakeCastle(t)
	wfPath := writeWorkflowFile(t, pauseResumeWorkflow)
	ctx := context.Background()
	_, graph, loader, err := compileForExecution(ctx, wfPath, discardLogger(), false, false)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	defer func() { _ = loader.Shutdown(context.WithoutCancel(ctx)) }()

	sink := &pauseTracker{Sink: &run.LocalSink{RunID: "console-run", Out: os.Stdout}}
	ctrl := newLocalRunControl("console-run", graph, sink, nil)
	base, stop, err := startLocalRunStateServer(ctx, newTestLogger(t), "console-run", "", ctrl, true, castleURL, nil)
	if err != nil {
		t.Fatalf("startLocalRunStateServer: %v", err)
	}
	defer stop()
	base = base[:len(base)-len("/runview/")] // http://127.0.0.1:PORT
	waitHealth(t, base)

	// Login is castle's: the fake plane saw the credentials and the real
	// session comes back through the loopback surface.
	resp := consolePost(t, base, "Login", `{"username":"ops","password":"pw"}`, map[string]string{"Authorization": "Bearer tok"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Login: %d %s", resp.StatusCode, consoleBodyString(t, resp))
	}
	if body := consoleBodyString(t, resp); body != `{"sessionToken":"kx","username":"ops"}` {
		t.Errorf("Login body = %q", body)
	}
	if len(*castleLog) != 1 {
		t.Fatalf("castle saw %d requests, want 1", len(*castleLog))
	}
	if want := "POST /castle.v1.ConsoleService/Login {\"username\":\"ops\",\"password\":\"pw\"}"; (*castleLog)[0] != want {
		t.Errorf("castle saw %q, want %q", (*castleLog)[0], want)
	}

	// ResolveResume with the pause not yet landed: the service layer's
	// landing window keeps the verb answerable once the pause lands.
	sink.OnRunPaused("gate", "signal", "resume")

	// A foreign run id keeps the honest not-found answer the control verbs
	// produce (checkRun), and mismatches keep the local vocabulary.
	resp = consolePost(t, base, "ResolveResume", `{"runId":"other-run","signal":"resume"}`, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("foreign run: %d %s", resp.StatusCode, consoleBodyString(t, resp))
	}
	var ce struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(consoleBodyString(t, resp)), &ce); err != nil {
		t.Fatalf("foreign run body: %v", err)
	}
	if ce.Code != "not_found" || ce.Message != `run "other-run" is not served by this control listener` {
		t.Errorf("foreign run = %+v", ce)
	}
	if len(*castleLog) != 1 {
		t.Errorf("resolve reached castle: %v", *castleLog)
	}

	resp = consolePost(t, base, "ResolveResume", `{"runId":"console-run","signal":"wrong"}`, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("mismatch: %d %s", resp.StatusCode, consoleBodyString(t, resp))
	}
	if err := json.Unmarshal([]byte(consoleBodyString(t, resp)), &ce); err != nil {
		t.Fatalf("mismatch body: %v", err)
	}
	// An undeclared signal has no pause target at all: the local control
	// vocabulary answers no_pending_signal (mismatch vocabulary is exercised
	// when another signal resolves but points elsewhere; the
	// runstate-side mapping is pinned by its scripted-resolver tests).
	if ce.Code != "failed_precondition" || ce.Message != "resolve rejected: the run has no pending signal to resolve" {
		t.Errorf("mismatch = %+v", ce)
	}

	// The matching signal delivers the decision through the run's own
	// control service: parked on the control bus for the engine's drain
	// loop and answered with issued_at.
	resp = consolePost(t, base, "ResolveResume", `{"runId":"console-run","signal":"resume","payload":{"outcome":"received"}}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resolve: %d %s", resp.StatusCode, consoleBodyString(t, resp))
	}
	var ro struct {
		IssuedAt string `json:"issuedAt"`
	}
	if err := json.Unmarshal([]byte(consoleBodyString(t, resp)), &ro); err != nil {
		t.Fatalf("resolve body: %v", err)
	}
	if issued, err := time.Parse(time.RFC3339, ro.IssuedAt); err != nil || time.Since(issued) > time.Minute {
		t.Errorf("issuedAt %q: %v", ro.IssuedAt, err)
	}
	select {
	case got := <-ctrl.resolveResumeChan():
		if got["outcome"] != "received" {
			t.Errorf("parked decision = %v", got)
		}
	default:
		t.Error("decision was not parked on the control bus")
	}
	if len(*castleLog) != 1 {
		t.Errorf("resolve reached castle after delivery: %v", *castleLog)
	}
}

// TestLocalRunStateServerConsoleDisabled pins the unconfigured-castle state
// through the real wiring: every login surface answers the disabled
// message and nothing is forwarded.
func TestLocalRunStateServerConsoleDisabled(t *testing.T) {
	requireNoGoroutineLeak(t)
	home := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", home)

	wfPath := writeWorkflowFile(t, pauseResumeWorkflow)
	ctx := context.Background()
	_, graph, loader, err := compileForExecution(ctx, wfPath, discardLogger(), false, false)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	defer func() { _ = loader.Shutdown(context.WithoutCancel(ctx)) }()

	sink := &pauseTracker{Sink: &run.LocalSink{RunID: "console-off", Out: os.Stdout}}
	ctrl := newLocalRunControl("console-off", graph, sink, nil)
	base, stop, err := startLocalRunStateServer(ctx, newTestLogger(t), "console-off", "", ctrl, true, "", nil)
	if err != nil {
		t.Fatalf("startLocalRunStateServer: %v", err)
	}
	defer stop()
	base = base[:len(base)-len("/runview/")]

	resp := consolePost(t, base, "Login", `{}`, nil)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("disabled Login: %d %s", resp.StatusCode, consoleBodyString(t, resp))
	}
	var ce struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(consoleBodyString(t, resp)), &ce); err != nil {
		t.Fatalf("disabled body: %v", err)
	}
	if ce.Code != "unimplemented" || ce.Message != "console login is disabled: no castle control plane is configured for this run-viewer (set --castle-addr or CRITERIA_CASTLE_ADDR to enable console login)" {
		t.Errorf("disabled Login = %+v", ce)
	}
}

// TestLocalControlResolverRejectsForeignRun pins the resolver method on its
// own: foreign ids get the checkRun NotFound, matching ids get the control
// vocabulary.
func TestLocalControlResolverRejectsForeignRun(t *testing.T) {
	requireNoGoroutineLeak(t)
	home := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", home)

	wfPath := writeWorkflowFile(t, pauseResumeWorkflow)
	ctx := context.Background()
	_, graph, loader, err := compileForExecution(ctx, wfPath, discardLogger(), false, false)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	defer func() { _ = loader.Shutdown(context.WithoutCancel(ctx)) }()

	sink := &pauseTracker{Sink: &run.LocalSink{RunID: "res-run", Out: os.Stdout}}
	ctrl := newLocalRunControl("res-run", graph, sink, nil)
	svc := &localControlService{ctrl: ctrl, runID: "res-run"}

	if _, _, err := svc.ResolveConsoleResume("other", "resume", nil); err == nil || err.Error() != `not_found: run "other" is not served by this control listener` {
		t.Errorf("foreign run = %v", err)
	}
	// A finished run answers run_not_paused immediately (no landing retry):
	// a run that exited can never pause.
	ctrl.markFinished()
	if accepted, reason, err := svc.ResolveConsoleResume("res-run", "resume", nil); err != nil || accepted || reason != "run_not_paused" {
		t.Errorf("unpaused run = (%v, %q, %v)", accepted, reason, err)
	}
	sink.OnRunPaused("gate", "signal", "resume")
	accepted, reason, err := svc.ResolveConsoleResume("res-run", "resume", map[string]string{"outcome": "received"})
	if err != nil || !accepted || reason != "ok" {
		t.Errorf("paused run = (%v, %q, %v)", accepted, reason, err)
	}
	select {
	case got := <-ctrl.resolveResumeChan():
		if got["outcome"] != "received" {
			t.Errorf("parked decision = %v", got)
		}
	default:
		t.Error("decision was not parked")
	}
}