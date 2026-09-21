package runstate

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeRun materializes a fixture run dir: run-state.json (optional) plus an
// ND-JSON events file with the given envelopes.
func writeRun(t *testing.T, root, runID string, st *localState, lines []ndEnvelope, meta *runMetadata) string {
	t.Helper()
	dir := filepath.Join(root, runID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if st != nil {
		b, err := json.Marshal(st)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, stateFileName), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if len(lines) > 0 {
		f, err := os.OpenFile(filepath.Join(dir, eventsFileName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		for _, l := range lines {
			b, err := json.Marshal(l)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteString(string(b) + "\n"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if meta != nil {
		b, err := json.Marshal(meta)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, metadataFileName), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	root := t.TempDir()
	return NewStoreAt(root)
}

func env(seq int64, payloadType, payload string) ndEnvelope {
	return ndEnvelope{SchemaVersion: 1, Seq: seq, RunID: "r1", PayloadType: payloadType, Payload: json.RawMessage(payload)}
}

// TestGetRun_TerminalFromEvents verifies a completed run (run-state.json
// removed at completion, as local apply does) is servable from the events
// file alone with castle-mapped terminal fields.
func TestGetRun_TerminalFromEvents(t *testing.T) {
	s := newTestStore(t)
	root, _ := s.RunsRoot()
	writeRun(t, root, "r1", nil, []ndEnvelope{
		env(1, "RunStarted", `{"workflow_name":"deploy","initial_step":"build"}`),
		env(2, "StepEntered", `{"step":"build","adapter":"shell","attempt":1}`),
		env(3, "RunCompleted", `{"final_state":"done","success":true}`),
	}, &runMetadata{Kind: "git", Source: "https://github.com/acme/wf.git"})

	run, err := s.GetRun("r1")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.RunID != "r1" || run.WorkflowName != "deploy" {
		t.Errorf("run id/name = %q/%q", run.RunID, run.WorkflowName)
	}
	if run.Status != StatusSucceeded {
		t.Errorf("status = %q, want succeeded", run.Status)
	}
	if run.FinalState != "done" {
		t.Errorf("finalState = %q", run.FinalState)
	}
	if run.EndedAt == "" {
		t.Error("endedAt not derived from events file mtime")
	}
	if run.RepoURL != "https://github.com/acme/wf.git" {
		t.Errorf("repoUrl = %q", run.RepoURL)
	}
	if _, err := json.Marshal(run); err != nil {
		t.Fatalf("run not JSON-encodable: %v", err)
	}
}

// TestGetRun_RunningAndCrashed verifies the pid-alive status derivation:
// live pid → running; dead pid without a terminal event → failed.
func TestGetRun_RunningAndCrashed(t *testing.T) {
	s := newTestStore(t)
	root, _ := s.RunsRoot()

	// Running: this process is alive.
	writeRun(t, root, "alive", &localState{
		PID: os.Getpid(), RunID: "alive", Workflow: "wf", StartedAt: time.Now().UTC(),
	}, []ndEnvelope{env(1, "RunStarted", `{"workflow_name":"wf"}`)}, nil)
	// Crashed: reaped child pid — never alive again (pid reuse aside).
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn a child to fake a crashed pid: %v", err)
	}
	_ = cmd.Wait()
	writeRun(t, root, "crashed", &localState{
		PID: cmd.Process.Pid, RunID: "crashed", Workflow: "wf", StartedAt: time.Now().UTC(),
	}, nil, nil)

	running, err := s.GetRun("alive")
	if err != nil {
		t.Fatalf("GetRun(alive): %v", err)
	}
	if running.Status != StatusRunning {
		t.Errorf("status = %q, want running", running.Status)
	}
	if running.StartedAt == "" || running.WorkflowName != "wf" {
		t.Errorf("startedAt/name = %q/%q", running.StartedAt, running.WorkflowName)
	}

	crashed, err := s.GetRun("crashed")
	if err != nil {
		t.Fatalf("GetRun(crashed): %v", err)
	}
	if crashed.Status != StatusFailed {
		t.Errorf("status = %q, want failed for dead pid", crashed.Status)
	}
	if crashed.FailureReason == "" {
		t.Error("failureReason empty for crashed run")
	}
}

// TestGetRun_FailedTerminal maps a run.failed event to failed + failureReason.
func TestGetRun_Failed(t *testing.T) {
	s := newTestStore(t)
	root, _ := s.RunsRoot()
	writeRun(t, root, "f", &localState{PID: -1, RunID: "f", Workflow: "wf"}, []ndEnvelope{
		env(1, "RunStarted", `{"workflow_name":"wf"}`),
		env(2, "RunFailed", `{"reason":"adapter boom","step":"deploy"}`),
	}, nil)
	run, err := s.GetRun("f")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != StatusFailed || run.FailureReason != "adapter boom" {
		t.Errorf("status/reason = %q/%q", run.Status, run.FailureReason)
	}
}

// TestListRuns_FiltersAndPagination verifies agent/status filters, newest-first
// ordering, limit/cursor pagination and the nextPageToken contract.
func TestListRuns_FiltersAndPagination(t *testing.T) {
	s := newTestStore(t)
	root, _ := s.RunsRoot()
	// b is written last → newest first. a, b terminal succeeded; c running
	// with a criteria id.
	writeRun(t, root, "aaa", nil, []ndEnvelope{
		env(1, "RunStarted", `{"workflow_name":"a"}`),
		env(2, "RunCompleted", `{"final_state":"done","success":true}`),
	}, nil)
	time.Sleep(10 * time.Millisecond) // distinct mtimes
	writeRun(t, root, "bbb", nil, []ndEnvelope{
		env(1, "RunStarted", `{"workflow_name":"b"}`),
		env(2, "RunCompleted", `{"final_state":"done","success":false}`),
	}, nil)
	time.Sleep(10 * time.Millisecond)
	writeRun(t, root, "ccc", &localState{
		PID: os.Getpid(), RunID: "ccc", Workflow: "c", StartedAt: time.Now().UTC(), CriteriaID: "crit-9",
	}, []ndEnvelope{env(1, "RunStarted", `{"workflow_name":"c"}`)}, nil)

	// Newest first: ccc (running, newest mtime), bbb, aaa.
	page, err := s.ListRuns("", "", 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Runs) != 2 || page.Runs[0].RunID != "ccc" || page.Runs[1].RunID != "bbb" {
		t.Fatalf("page1 = %+v", page.Runs)
	}
	if page.NextPageToken != "bbb" {
		t.Errorf("nextPageToken = %q, want bbb", page.NextPageToken)
	}

	// Resume from the cursor.
	page2, err := s.ListRuns("", "", 2, page.NextPageToken)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2.Runs) != 1 || page2.Runs[0].RunID != "aaa" || page2.NextPageToken != "" {
		t.Errorf("page2 = %+v token=%q", page2.Runs, page2.NextPageToken)
	}

	// Status filter.
	onlyRunning, err := s.ListRuns("", StatusRunning, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(onlyRunning.Runs) != 1 || onlyRunning.Runs[0].RunID != "ccc" {
		t.Errorf("running filter = %+v", onlyRunning.Runs)
	}
	// Agent (criteria id) filter.
	byAgent, err := s.ListRuns("crit-9", "", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(byAgent.Runs) != 1 || byAgent.Runs[0].RunID != "ccc" {
		t.Errorf("agent filter = %+v", byAgent.Runs)
	}
	// Unknown cursor past the end → empty page, not an error.
	empty, err := s.ListRuns("", "", 0, "zzz")
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Runs) != 0 {
		t.Errorf("unknown cursor should return empty page, got %+v", empty.Runs)
	}
}

// TestEvents_PaginationAndSinceSeq verifies the seq cursor semantics:
// since_seq is exclusive, ordering ascending, token = last returned seq.
func TestEvents_PaginationAndSinceSeq(t *testing.T) {
	s := newTestStore(t)
	root, _ := s.RunsRoot()
	writeRun(t, root, "r1", nil, []ndEnvelope{
		env(1, "RunStarted", `{"workflow_name":"wf"}`),
		env(2, "StepEntered", `{"step":"a"}`),
		env(3, "StepOutcome", `{"step":"a","outcome":"ok"}`),
		env(4, "RunCompleted", `{"final_state":"done","success":true}`),
	}, nil)

	page, err := s.Events("r1", 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 2 || page.Events[0].Seq != 1 || page.Events[1].Seq != 2 {
		t.Fatalf("page1 = %+v", page.Events)
	}
	if page.NextPageToken != "2" {
		t.Errorf("token = %q, want 2", page.NextPageToken)
	}
	var since int64
	if _, err := fmt.Sscanf(page.NextPageToken, "%d", &since); err != nil {
		t.Fatal(err)
	}
	page2, err := s.Events("r1", since, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2.Events) != 2 || page2.Events[0].Seq != 3 || page2.NextPageToken != "" {
		t.Errorf("page2 = %+v token=%q", page2.Events, page2.NextPageToken)
	}
	// Contract shape: camelCase keys with the ND-JSON mapping.
	first := page.Events[0]
	if first.SchemaVersion != 1 || first.RunID != "r1" || first.Type != "RunStarted" {
		t.Errorf("envelope = %+v", first)
	}
	b, _ := json.Marshal(first)
	if !strings.Contains(string(b), `"type":"RunStarted"`) || !strings.Contains(string(b), `"seq":1`) {
		t.Errorf("wire shape = %s", b)
	}

	// Unknown run → ErrNotFound.
	if _, err := s.Events("nope", 0, 0); err == nil || !strings.Contains(err.Error(), "run not found") {
		t.Errorf("unknown run err = %v", err)
	}
}

// TestReadEvents_SkipsTornTail verifies a torn trailing line (the writer may
// hold the file open mid-append) is skipped, never fatal.
func TestReadEvents_SkipsTornTail(t *testing.T) {
	s := newTestStore(t)
	root, _ := s.RunsRoot()
	dir := filepath.Join(root, "torn")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"schema_version":1,"seq":1,"run_id":"torn","payload_type":"RunStarted","payload":{"workflow_name":"wf"}}` + "\n" +
		`{"schema_version":1,"seq":2,"run_id":"tor` // torn
	if err := os.WriteFile(filepath.Join(dir, eventsFileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	evs := s.readEvents("torn")
	if len(evs) != 1 || evs[0].Seq != 1 {
		t.Errorf("events = %+v, want the single complete line", evs)
	}
}

// TestRunDir_RefusesTraversal verifies the run id path guard.
func TestRunDir_RefusesTraversal(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"", ".", "..", "a/b", `a\b`} {
		if _, err := s.RunDir(id); err == nil {
			t.Errorf("RunDir(%q) accepted", id)
		}
	}
}
