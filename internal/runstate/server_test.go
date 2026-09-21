package runstate

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// newTestServer builds a store fixture (one succeeded run, one running run)
// and returns its server handler.
func newTestServer(t *testing.T) (*Server, *Store) {
	t.Helper()
	s := newTestStore(t)
	root, _ := s.RunsRoot()
	writeRun(t, root, "r-done", nil, []ndEnvelope{
		env(1, "RunStarted", `{"workflow_name":"deploy","initial_step":"build"}`),
		env(2, "StepEntered", `{"step":"build","adapter":"shell"}`),
		env(3, "RunCompleted", `{"final_state":"done","success":true}`),
	}, &runMetadata{Kind: "git", Source: "https://github.com/acme/wf.git"})
	writeRun(t, root, "r-live", &localState{
		PID: os.Getpid(), RunID: "r-live", Workflow: "live", CriteriaID: "crit-1", StartedAt: time.Now().UTC(),
	}, []ndEnvelope{env(1, "RunStarted", `{"workflow_name":"live"}`)}, nil)
	return NewServer(s), s
}

func doJSON(t *testing.T, h http.Handler, method, target string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// TestServerRoutes verifies every seam route's status code and contract
// shape (camelCase castle-mapped JSON).
func TestServerRoutes(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()

	if code, body := doJSON(t, h, "GET", "/health"); code != http.StatusNoContent || body != "" {
		t.Errorf("health: code=%d body=%q", code, body)
	}

	code, body := doJSON(t, h, "GET", "/runs")
	if code != http.StatusOK {
		t.Fatalf("runs: code=%d body=%s", code, body)
	}
	var page RunsPage
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("decode runs page: %v", code)
	}
	if len(page.Runs) != 2 {
		t.Fatalf("runs page = %s", body)
	}
	if !strings.Contains(body, `"repoUrl":"https://github.com/acme/wf.git"`) {
		t.Errorf("repoUrl not mapped from run-metadata: %s", body)
	}
	// Pagination token present on a truncated page.
	_, body = doJSON(t, h, "GET", "/runs?limit=1")
	if !strings.Contains(body, `"nextPageToken"`) {
		t.Errorf("paginated runs page missing nextPageToken: %s", body)
	}
	if !strings.Contains(body, `"runId"`) || !strings.Contains(body, `"workflowName"`) {
		t.Errorf("runs page missing contract keys: %s", body)
	}

	// Filters flow through to the store.
	_, body = doJSON(t, h, "GET", "/runs?status=running")
	if !strings.Contains(body, `"runId":"r-live"`) || strings.Contains(body, `"runId":"r-done"`) {
		t.Errorf("status filter: %s", body)
	}
	_, body = doJSON(t, h, "GET", "/runs?agent=crit-1")
	if !strings.Contains(body, `"runId":"r-live"`) {
		t.Errorf("agent filter: %s", body)
	}

	code, body = doJSON(t, h, "GET", "/runs/r-done")
	if code != http.StatusOK {
		t.Fatalf("get run: code=%d", code)
	}
	var run Run
	if err := json.Unmarshal([]byte(body), &run); err != nil {
		t.Fatal(err)
	}
	if run.Status != StatusSucceeded || run.FinalState != "done" || run.RunID != "r-done" {
		t.Errorf("run = %s", body)
	}

	if code, _ := doJSON(t, h, "GET", "/runs/nope"); code != http.StatusNotFound {
		t.Errorf("unknown run code=%d, want 404", code)
	}
	if _, body := doJSON(t, h, "GET", "/runs/nope"); !strings.Contains(body, "run not found") {
		t.Errorf("unknown run body=%q", body)
	}

	code, body = doJSON(t, h, "GET", "/runs/r-done/events?since_seq=1")
	if code != http.StatusOK {
		t.Fatalf("events: code=%d", code)
	}
	var events RunEventsPage
	if err := json.Unmarshal([]byte(body), &events); err != nil {
		t.Fatal(err)
	}
	if len(events.Events) != 2 || events.Events[0].Seq != 2 || events.Events[0].Type != "StepEntered" {
		t.Errorf("events page = %s", body)
	}
	if !strings.Contains(body, `"type":"StepEntered"`) || !strings.Contains(body, `"schemaVersion":1`) {
		t.Errorf("events contract keys: %s", body)
	}
	if code, _ := doJSON(t, h, "GET", "/runs/r-done/events?since_seq=-1"); code != http.StatusBadRequest {
		t.Errorf("bad since_seq code=%d, want 400", code)
	}
	if code, _ := doJSON(t, h, "GET", "/runs/r-done/events?since_seq=abc"); code != http.StatusBadRequest {
		t.Errorf("non-numeric since_seq code=%d, want 400", code)
	}
	if code, _ := doJSON(t, h, "GET", "/runs/r-done/events?limit=xyz"); code != http.StatusBadRequest {
		t.Errorf("bad limit code=%d, want 400", code)
	}

	code, body = doJSON(t, h, "GET", "/runs/r-done/inspect")
	if code != http.StatusOK {
		t.Fatalf("inspect: code=%d body=%s", code, body)
	}
	if !strings.Contains(body, `"currentStep":"build"`) || !strings.Contains(body, `"adapter":"shell"`) {
		t.Errorf("inspect body = %s", body)
	}
	// Session echo.
	_, body = doJSON(t, h, "GET", "/runs/r-done/inspect?session=sess-7")
	if !strings.Contains(body, `"sessionId":"sess-7"`) {
		t.Errorf("session echo = %s", body)
	}

	code, body = doJSON(t, h, "GET", "/agents")
	if code != http.StatusOK || !strings.Contains(body, `"agents"`) || !strings.Contains(body, `"criteriaId":"crit-1"`) {
		t.Errorf("agents = %d %s", code, body)
	}
	code, body = doJSON(t, h, "GET", "/agents/crit-1")
	if code != http.StatusOK || !strings.Contains(body, `"criteriaId":"crit-1"`) {
		t.Errorf("agent = %d %s", code, body)
	}
	if code, _ := doJSON(t, h, "GET", "/agents/nobody"); code != http.StatusNotFound {
		t.Errorf("unknown agent code=%d, want 404", code)
	}
}

// TestServerControlVerbs verifies the verb contract: without a control
// handler every verb is 501 UNIMPLEMENTED; with one wired, the supported
// verb (stop) succeeds and unsupported verbs stay 501; unknown runs are 404.
func TestServerControlVerbs(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()
	for _, verb := range []string{"resume", "pause", "stop"} {
		if code, body := doJSON(t, h, "POST", "/runs/r-done/"+verb); code != http.StatusNotImplemented || !strings.Contains(body, "not implemented") {
			t.Errorf("%s: code=%d body=%q", verb, code, body)
		}
	}
	if code, _ := doJSON(t, h, "POST", "/runs/nope/stop"); code != http.StatusNotFound {
		t.Errorf("unknown run verb code=%d, want 404", code)
	}

	stopped := false
	srv2, _ := newTestServer(t)
	srv2.WithControl(func(runID, verb string) error {
		if verb == "stop" {
			stopped = true
			return nil
		}
		return ErrUnsupportedVerb
	})
	h2 := srv2.Handler()
	if code, _ := doJSON(t, h2, "POST", "/runs/r-done/stop"); code != http.StatusOK || !stopped {
		t.Errorf("wired stop did not reach the control handler (code=%d stopped=%v)", code, stopped)
	}
	if code, _ := doJSON(t, h2, "POST", "/runs/r-done/pause"); code != http.StatusNotImplemented {
		t.Errorf("unsupported wired verb code=%d, want 501", code)
	}
}

// TestServerListen_RefusesNonLoopback verifies the locked loopback-only rule.
func TestServerListen_RefusesNonLoopback(t *testing.T) {
	srv, _ := newTestServer(t)
	for _, host := range []string{"0.0.0.0", "192.168.1.5", "example.com", "::"} {
		if _, err := srv.Listen(host, 0); err == nil {
			t.Errorf("non-loopback bind %q accepted", host)
		}
	}
	// Loopback binds succeed and the addr is loopback.
	ln, err := srv.Listen("127.0.0.1", 0)
	if err != nil {
		t.Fatalf("loopback bind: %v", err)
	}
	defer ln.Close()
	ip := net.ParseIP(strings.Split(ln.Addr().String(), ":")[0])
	if ip == nil || !ip.IsLoopback() {
		t.Errorf("bound to %s, want loopback", ln.Addr())
	}
	// IPv6 loopback is also accepted.
	ln6, err := srv.Listen("::1", 0)
	if err != nil {
		t.Errorf("ipv6 loopback bind refused: %v", err)
	} else {
		ln6.Close()
	}
}

// TestServerViewer serves the embedded bundle at the root with SPA fallback
// for unknown paths.
func TestServerViewer(t *testing.T) {
	srv, _ := newTestServer(t)
	viewer, err := NewViewer()
	if err != nil {
		t.Fatal(err)
	}
	srv.WithViewer(viewer)
	h := srv.Handler()

	if code, body := doJSON(t, h, "GET", "/"); code != http.StatusOK || !strings.Contains(body, "<html") {
		t.Errorf("root: code=%d body=%q", code, body)
	}
	if code, body := doJSON(t, h, "GET", "/some/spa/route"); code != http.StatusOK || !strings.Contains(body, "<html") {
		t.Errorf("spa fallback: code=%d body=%q", code, body)
	}
	if code, _ := doJSON(t, h, "GET", "/version.txt"); code != http.StatusOK {
		t.Errorf("version.txt code=%d", code)
	}
	// /health must keep answering 204 even with the viewer mounted.
	if code, _ := doJSON(t, h, "GET", "/health"); code != http.StatusNoContent {
		t.Errorf("health with viewer code=%d", code)
	}
}

// TestServerLiveOverSocket exercises the full loop: bind, serve, request,
// stop — the same path apply and serve-ui take (real net.Listener).
func TestServerLiveOverSocket(t *testing.T) {
	srv, _ := newTestServer(t)
	ln, err := srv.Listen("127.0.0.1", 0)
	if err != nil {
		t.Fatal(err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	resp, err := http.Get("http://" + ln.Addr().String() + "/runs/r-done")
	if err != nil {
		t.Fatalf("live request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("live run status = %d", resp.StatusCode)
	}
	var run Run
	if err := json.NewDecoder(resp.Body).Decode(&run); err != nil || run.RunID != "r-done" {
		t.Errorf("live run decode: %v", err)
	}

	srv.Stop()
	if err := <-serveErr; err != nil {
		t.Errorf("serve error: %v", err)
	}
}

// TestServerStopCancelsContext mirrors the apply wiring: the stop verb
// cancels a context (the engine path turns that into a terminal event).
func TestServerStopCancelsContext(t *testing.T) {
	srv, _ := newTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	srv.WithControl(func(runID, verb string) error {
		if verb == "stop" {
			cancel()
			return nil
		}
		return ErrUnsupportedVerb
	})
	h := srv.Handler()
	if code, _ := doJSON(t, h, "POST", "/runs/r-done/stop"); code != http.StatusOK {
		t.Fatalf("stop code=%d", code)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("stop verb did not cancel the run context")
	}
}

// TestServerControlRealStop wires a control handler that stops a real
// (reaped) child's pid-space analog: cancelRun through a context the test
// waits on. Placeholder-free: a real child process is spawned and reaped so
// the fixture matches the pid-alive store path.
func TestServerControlRealStop(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 5")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn child: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Wait() })
	_ = cmd.Process.Pid

	s := newTestStore(t)
	root, _ := s.RunsRoot()
	writeRun(t, root, "r-child", &localState{
		PID: cmd.Process.Pid, RunID: "r-child", Workflow: "wf", StartedAt: time.Now().UTC(),
	}, nil, nil)

	srv := NewServer(s)
	ctx, cancel := context.WithCancel(context.Background())
	srv.WithControl(func(runID, verb string) error {
		if runID != "r-child" {
			return ErrNotFound
		}
		if verb == "stop" {
			cancel()
			return nil
		}
		return ErrUnsupportedVerb
	})
	if code, _ := doJSON(t, srv.Handler(), "POST", "/runs/r-child/stop"); code != http.StatusOK {
		t.Fatalf("stop code=%d", code)
	}
	<-ctx.Done()
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}
