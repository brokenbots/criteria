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

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// newTestServer builds a store fixture (one succeeded run, one running run)
// and returns its server handler.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	s := newTestStore(t)
	root, _ := s.RunsRoot()
	writeRun(t, root, "r-done", nil, []ndEnvelope{
		envPB(t, 1, "RunStarted", &pb.RunStarted{WorkflowName: "deploy", InitialStep: "build"}),
		envPB(t, 2, "StepEntered", &pb.StepEntered{Step: "build", Adapter: "shell"}),
		envPB(t, 3, "RunCompleted", &pb.RunCompleted{FinalState: "done", Success: true}),
	}, &runMetadata{Kind: "git", Source: "https://github.com/acme/wf.git"})
	writeRun(t, root, "r-live", &localState{
		PID: os.Getpid(), RunID: "r-live", Workflow: "live", CriteriaID: "crit-1", StartedAt: time.Now().UTC(),
	}, []ndEnvelope{envPB(t, 1, "RunStarted", &pb.RunStarted{WorkflowName: "live"})}, nil)
	return NewServer(s)
}

// doJSON issues a request against h as a loopback Host would (the server
// refuses other Hosts by design).
func doJSON(t *testing.T, h http.Handler, method, target string) (code int, body string) {
	t.Helper()
	req := httptest.NewRequest(method, target, http.NoBody)
	req.Host = "127.0.0.1:0"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// TestServerRoutes verifies every seam route's status code and contract
// shape under the canonical /runview/api mount (camelCase castle-mapped
// JSON, per the executable spec in localRunDataSource.ts).
func TestServerRoutes(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	if code, body := doJSON(t, h, "GET", "/runview/api/health"); code != http.StatusNoContent || body != "" {
		t.Errorf("health: code=%d body=%q", code, body)
	}

	code, body := doJSON(t, h, "GET", "/runview/api/runs")
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
	_, body = doJSON(t, h, "GET", "/runview/api/runs?limit=1")
	if !strings.Contains(body, `"nextPageToken"`) {
		t.Errorf("paginated runs page missing nextPageToken: %s", body)
	}
	if !strings.Contains(body, `"runId"`) || !strings.Contains(body, `"workflowName"`) {
		t.Errorf("runs page missing contract keys: %s", body)
	}

	// Filters flow through to the store.
	_, body = doJSON(t, h, "GET", "/runview/api/runs?status=running")
	if !strings.Contains(body, `"runId":"r-live"`) || strings.Contains(body, `"runId":"r-done"`) {
		t.Errorf("status filter: %s", body)
	}
	_, body = doJSON(t, h, "GET", "/runview/api/runs?agent=crit-1")
	if !strings.Contains(body, `"runId":"r-live"`) {
		t.Errorf("agent filter: %s", body)
	}

	code, body = doJSON(t, h, "GET", "/runview/api/runs/r-done")
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

	if code, _ := doJSON(t, h, "GET", "/runview/api/runs/nope"); code != http.StatusNotFound {
		t.Errorf("unknown run code=%d, want 404", code)
	}
	if _, body := doJSON(t, h, "GET", "/runview/api/runs/nope"); !strings.Contains(body, "run not found") {
		t.Errorf("unknown run body=%q", body)
	}

	code, body = doJSON(t, h, "GET", "/runview/api/runs/r-done/events?since_seq=1")
	if code != http.StatusOK {
		t.Fatalf("events: code=%d", code)
	}
	var events RunEventsPage
	if err := json.Unmarshal([]byte(body), &events); err != nil {
		t.Fatal(err)
	}
	// Contract shape: lastSeq is the run's highest seq; a non-full page has
	// nextSinceSeq null; types are the seam's camelCase vocabulary.
	if events.LastSeq != 3 || len(events.Events) != 2 || events.Events[0].Seq != 2 || events.Events[0].Type != "stepEntered" {
		t.Errorf("events page = %s", body)
	}
	if events.NextSinceSeq != nil {
		t.Errorf("non-full page nextSinceSeq = %v, want null", events.NextSinceSeq)
	}
	if !strings.Contains(body, `"type":"stepEntered"`) || !strings.Contains(body, `"schemaVersion":1`) {
		t.Errorf("events contract keys: %s", body)
	}
	if !strings.Contains(body, `"lastSeq":3`) || !strings.Contains(body, `"nextSinceSeq":null`) {
		t.Errorf("events page missing lastSeq/nextSinceSeq: %s", body)
	}
	if code, _ := doJSON(t, h, "GET", "/runview/api/runs/r-done/events?since_seq=-1"); code != http.StatusBadRequest {
		t.Errorf("bad since_seq code=%d, want 400", code)
	}
	if code, _ := doJSON(t, h, "GET", "/runview/api/runs/r-done/events?since_seq=abc"); code != http.StatusBadRequest {
		t.Errorf("non-numeric since_seq code=%d, want 400", code)
	}
	if code, _ := doJSON(t, h, "GET", "/runview/api/runs/r-done/events?limit=xyz"); code != http.StatusBadRequest {
		t.Errorf("bad limit code=%d, want 400", code)
	}

	code, body = doJSON(t, h, "GET", "/runview/api/runs/r-done/inspect")
	if code != http.StatusOK {
		t.Fatalf("inspect: code=%d body=%s", code, body)
	}
	if !strings.Contains(body, `"runId":"r-done"`) || !strings.Contains(body, `"currentStep":"build"`) || !strings.Contains(body, `"adapter":"shell"`) {
		t.Errorf("inspect body = %s", body)
	}
	// Session echo.
	_, body = doJSON(t, h, "GET", "/runview/api/runs/r-done/inspect?session=sess-7")
	if !strings.Contains(body, `"sessionId":"sess-7"`) {
		t.Errorf("session echo = %s", body)
	}

	// Agents: a bare JSON array (listAgents(): Agent[]) with the labels
	// field the castle type requires.
	code, body = doJSON(t, h, "GET", "/runview/api/agents")
	if code != http.StatusOK || !strings.HasPrefix(strings.TrimSpace(body), "[") {
		t.Errorf("agents = %d %s, want a bare JSON array", code, body)
	}
	var agents []Agent
	if err := json.Unmarshal([]byte(body), &agents); err != nil {
		t.Fatalf("agents body does not unmarshal into []Agent: %v (%s)", err, body)
	}
	if len(agents) != 1 || agents[0].CriteriaID != "crit-1" || agents[0].Labels == nil {
		t.Errorf("agents = %s", body)
	}
	code, body = doJSON(t, h, "GET", "/runview/api/agents/crit-1")
	if code != http.StatusOK || !strings.Contains(body, `"criteriaId":"crit-1"`) || !strings.Contains(body, `"labels":{`) {
		t.Errorf("agent = %d %s", code, body)
	}
	if code, _ := doJSON(t, h, "GET", "/runview/api/agents/nobody"); code != http.StatusNotFound {
		t.Errorf("unknown agent code=%d, want 404", code)
	}
}

// TestServerAPIMount pins R5: the API lives under /runview/api (the viewer
// bundle's default base) and unknown API paths answer JSON — never the
// viewer's HTML fallback, the DOA symptom this ticket removes.
func TestServerAPIMount(t *testing.T) {
	srv := newTestServer(t)
	viewer, err := NewViewer()
	if err != nil {
		t.Fatal(err)
	}
	srv.WithViewer(viewer)
	h := srv.Handler()

	code, body := doJSON(t, h, "GET", "/runview/api/runs")
	if code != http.StatusOK || !strings.Contains(body, `"runId"`) {
		t.Fatalf("/runview/api/runs = %d %s, want JSON", code, body)
	}
	if ct := strings.TrimSpace(contentTypeOf(t, h, "GET", "/runview/api/runs")); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("/runview/api/runs content-type = %q, want application/json", ct)
	}
	// Unknown API path: JSON 404, no HTML fallback.
	code, body = doJSON(t, h, "GET", "/runview/api/not-a-route")
	if code != http.StatusNotFound || strings.Contains(body, "<html") || !strings.Contains(body, `"error"`) {
		t.Errorf("unknown API path = %d %s, want a JSON 404", code, body)
	}
	// The viewer itself is under /runview/, and / redirects there.
	code, body = doJSON(t, h, "GET", "/runview/")
	if code != http.StatusOK || !strings.Contains(body, "<html") {
		t.Errorf("/runview/ = %d %q", code, body)
	}
	code, body = doJSON(t, h, "GET", "/")
	if code != http.StatusFound {
		t.Errorf("root = %d %q, want a redirect to /runview/", code, body)
	}
	if loc := locationOf(t, h, "GET", "/"); loc != "/runview/" {
		t.Errorf("root redirect location = %q, want /runview/", loc)
	}
	if code, _ := doJSON(t, h, "GET", "/runview/version.txt"); code != http.StatusOK {
		t.Errorf("/runview/version.txt code=%d", code)
	}
}

func contentTypeOf(t *testing.T, h http.Handler, method, target string) string {
	t.Helper()
	req := httptest.NewRequest(method, target, http.NoBody)
	req.Host = "127.0.0.1:0"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Header().Get("Content-Type")
}

func locationOf(t *testing.T, h http.Handler, method, target string) string {
	t.Helper()
	req := httptest.NewRequest(method, target, http.NoBody)
	req.Host = "127.0.0.1:0"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Header().Get("Location")
}

// TestServerControlVerbs verifies the verb contract: without a control
// handler every verb is 501 UNIMPLEMENTED; with one wired, the supported
// verb (stop) succeeds and unsupported verbs stay 501; unknown runs are 404.
func TestServerControlVerbs(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()
	for _, verb := range []string{"resume", "pause", "stop"} {
		if code, body := doJSON(t, h, "POST", "/runview/api/runs/r-done/"+verb); code != http.StatusNotImplemented || !strings.Contains(body, "not implemented") {
			t.Errorf("%s: code=%d body=%q", verb, code, body)
		}
	}
	if code, _ := doJSON(t, h, "POST", "/runview/api/runs/nope/stop"); code != http.StatusNotFound {
		t.Errorf("unknown run verb code=%d, want 404", code)
	}

	stopped := false
	srv2 := newTestServer(t)
	srv2.WithControl(func(runID, verb string) error {
		if verb == "stop" {
			stopped = true
			return nil
		}
		return ErrUnsupportedVerb
	})
	h2 := srv2.Handler()
	if code, _ := doJSON(t, h2, "POST", "/runview/api/runs/r-done/stop"); code != http.StatusOK || !stopped {
		t.Errorf("wired stop did not reach the control handler (code=%d stopped=%v)", code, stopped)
	}
	if code, _ := doJSON(t, h2, "POST", "/runview/api/runs/r-done/pause"); code != http.StatusNotImplemented {
		t.Errorf("unsupported wired verb code=%d, want 501", code)
	}
}

// TestServerListen_RefusesNonLoopback verifies the locked loopback-only rule.
func TestServerListen_RefusesNonLoopback(t *testing.T) {
	srv := newTestServer(t)
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

// TestServerHostHeaderValidation pins R8: requests whose Host is not a
// loopback literal are refused with 403 (DNS-rebinding read protection),
// while loopback Hosts — IPv4, IPv6, and localhost, with a port — pass.
func TestServerHostHeaderValidation(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	for _, host := range []string{"evil.example", "attacker.dev", "127.0.0.2.example.com", "2130706433", "0x7f000001", "10.0.0.5"} {
		req := httptest.NewRequest("GET", "/runview/api/runs/r-done", http.NoBody)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("Host %q = %d, want 403", host, rec.Code)
		}
	}
	// Loopback Hosts keep working (with and without a port).
	for _, host := range []string{"127.0.0.1", "127.0.0.1:8080", "localhost", "localhost:0", "[::1]", "[::1]:8080"} {
		req := httptest.NewRequest("GET", "/runview/api/health", http.NoBody)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Errorf("Host %q = %d, want 204", host, rec.Code)
		}
	}
	// An empty Host is refused too.
	req := httptest.NewRequest("GET", "/runview/api/health", http.NoBody)
	req.Host = ""
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("empty Host = %d, want 403", rec.Code)
	}
}

// TestServerViewer serves the embedded bundle under /runview/ with SPA
// fallback for unknown app paths.
func TestServerViewer(t *testing.T) {
	srv := newTestServer(t)
	viewer, err := NewViewer()
	if err != nil {
		t.Fatal(err)
	}
	srv.WithViewer(viewer)
	h := srv.Handler()

	if code, body := doJSON(t, h, "GET", "/runview/some/spa/route"); code != http.StatusOK || !strings.Contains(body, "<html") {
		t.Errorf("spa fallback: code=%d body=%q", code, body)
	}
	// /runview/api/health must keep answering 204 even with the viewer mounted.
	if code, _ := doJSON(t, h, "GET", "/runview/api/health"); code != http.StatusNoContent {
		t.Errorf("health with viewer code=%d", code)
	}
}

// TestServerLiveOverSocket exercises the full loop: bind, serve, request,
// stop — the same path apply and serve-ui take (real net.Listener).
func TestServerLiveOverSocket(t *testing.T) {
	srv := newTestServer(t)
	ln, err := srv.Listen("127.0.0.1", 0)
	if err != nil {
		t.Fatal(err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	resp, err := http.Get("http://" + ln.Addr().String() + "/runview/api/runs/r-done")
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
	srv := newTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	srv.WithControl(func(runID, verb string) error {
		if verb == "stop" {
			cancel()
			return nil
		}
		return ErrUnsupportedVerb
	})
	h := srv.Handler()
	if code, _ := doJSON(t, h, "POST", "/runview/api/runs/r-done/stop"); code != http.StatusOK {
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
	if code, _ := doJSON(t, srv.Handler(), "POST", "/runview/api/runs/r-child/stop"); code != http.StatusOK {
		t.Fatalf("stop code=%d", code)
	}
	<-ctx.Done()
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

// TestServerScopedStoreIsolation pins the apply-owned run rule: a Scoped
// store's server 404s every other run id.
func TestServerScopedStoreIsolation(t *testing.T) {
	s := newTestStore(t)
	root, _ := s.RunsRoot()
	writeRun(t, root, "mine", nil, []ndEnvelope{
		envPB(t, 1, "RunStarted", &pb.RunStarted{WorkflowName: "mine"}),
	}, nil)
	writeRun(t, root, "theirs", nil, []ndEnvelope{
		envPB(t, 1, "RunStarted", &pb.RunStarted{WorkflowName: "theirs"}),
	}, nil)

	h := NewServer(s.Scoped("mine")).Handler()
	if code, body := doJSON(t, h, "GET", "/runview/api/runs/mine"); code != http.StatusOK || !strings.Contains(body, `"runId":"mine"`) {
		t.Errorf("owned run = %d %s", code, body)
	}
	if code, _ := doJSON(t, h, "GET", "/runview/api/runs/theirs"); code != http.StatusNotFound {
		t.Errorf("foreign run code=%d, want 404", code)
	}
	if code, body := doJSON(t, h, "GET", "/runview/api/runs"); strings.Contains(body, "theirs") {
		t.Errorf("scoped list leaked other runs: %d %s", code, body)
	}
}

// TestLoopbackHostOnlyUnit covers the host predicate directly (IPv6 forms,
// case handling).
func TestLoopbackHostPredicate(t *testing.T) {
	yes := []string{"localhost", "LOCALHOST", "127.0.0.1", "::1", "127.8.8.8"}
	no := []string{"", "example.com", "127.0.0.1.evil.com", "::2", "fe80::1", "0.0.0.0"}
	for _, h := range yes {
		if !isLoopbackHost(h) {
			t.Errorf("isLoopbackHost(%q) = false, want true", h)
		}
	}
	for _, h := range no {
		if isLoopbackHost(h) {
			t.Errorf("isLoopbackHost(%q) = true, want false", h)
		}
	}
}
