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

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// writeRun materializes a fixture run dir: run-state.json (optional) plus an
// ND-JSON events file with the given envelopes.
func writeRun(t *testing.T, root, runID string, st *localState, lines []ndEnvelope, meta *runMetadata) {
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
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	root := t.TempDir()
	return NewStoreAt(root)
}

// envPB builds an ND-JSON envelope exactly the way the producer renders one:
// the payload is protojson.Marshal of the pb message (camelCase keys — the
// format internal/run's LocalSink writes), never hand-written JSON. RunID is
// not validated against the fixture dir name, so a fixed value keeps the
// helpers simple.
func envPB(t *testing.T, seq int64, payloadType string, msg proto.Message) ndEnvelope {
	t.Helper()
	b, err := protojson.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal %s payload: %v", payloadType, err)
	}
	return ndEnvelope{SchemaVersion: 1, Seq: seq, RunID: "r1", PayloadType: payloadType, Payload: b}
}

// TestGetRun_TerminalFromEvents verifies a completed run (run-state.json
// removed at completion, as local apply does) is servable from the events
// file alone, with workflowName and finalState folded from the producer's
// actual protojson payload keys.
func TestGetRun_TerminalFromEvents(t *testing.T) {
	s := newTestStore(t)
	root, _ := s.RunsRoot()
	writeRun(t, root, "r1", nil, []ndEnvelope{
		envPB(t, 1, "RunStarted", &pb.RunStarted{WorkflowName: "deploy", InitialStep: "build"}),
		envPB(t, 2, "StepEntered", &pb.StepEntered{Step: "build", Adapter: "shell", Attempt: 1}),
		envPB(t, 3, "RunCompleted", &pb.RunCompleted{FinalState: "done", Success: true}),
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

// TestGetRun_ProtojsonKeysAreTheProducerFormat pins R1 at the wire level: the
// producer writes protojson output (camelCase keys, success omitted when
// false), so a completed run must surface workflowName and finalState even
// when run-state.json is gone. The served events must also carry the seam's
// camelCase type vocabulary, with the terminal type in the consumer's
// terminal set.
func TestGetRun_ProtojsonKeysAreTheProducerFormat(t *testing.T) {
	s := newTestStore(t)
	root, _ := s.RunsRoot()
	writeRun(t, root, "camel-run", nil, []ndEnvelope{
		envPB(t, 1, "RunStarted", &pb.RunStarted{WorkflowName: "camel-wf", InitialStep: "build"}),
		envPB(t, 2, "RunCompleted", &pb.RunCompleted{FinalState: "deployed", Success: true}),
	}, nil)
	writeRun(t, root, "camel-fail", nil, []ndEnvelope{
		envPB(t, 1, "RunStarted", &pb.RunStarted{WorkflowName: "camel-wf"}),
		// protojson omits zero fields: success=false is absent on the wire.
		envPB(t, 2, "RunCompleted", &pb.RunCompleted{FinalState: "failed"}),
	}, nil)

	run, err := s.GetRun("camel-run")
	if err != nil {
		t.Fatal(err)
	}
	if run.WorkflowName != "camel-wf" {
		t.Errorf("workflowName = %q, want non-empty from the protojson payload", run.WorkflowName)
	}
	if run.FinalState != "deployed" || run.Status != StatusSucceeded {
		t.Errorf("finalState/status = %q/%q", run.FinalState, run.Status)
	}

	failed, err := s.GetRun("camel-fail")
	if err != nil {
		t.Fatal(err)
	}
	if failed.FinalState != "failed" || failed.Status != StatusFailed {
		t.Errorf("omitted success=false must read as failed: finalState=%q status=%q", failed.FinalState, failed.Status)
	}

	// The served event types are the seam vocabulary; the terminal one is a
	// member of the consumer's terminal set.
	for _, id := range []string{"camel-run", "camel-fail"} {
		page, err := s.Events(id, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		sawTerminal := false
		for _, ev := range page.Events {
			if ev.Type == "RunStarted" || ev.Type == "RunCompleted" {
				t.Errorf("type %q is the raw payload_type, not the seam vocabulary", ev.Type)
			}
			if TerminalSeamEventTypes[ev.Type] {
				sawTerminal = true
			}
		}
		if !sawTerminal {
			t.Errorf("run %s never served a terminal type (types in the terminal set)", id)
		}
	}
}

// TestEvents_StreamTerminationWalk mirrors the consumer's openRunStream walk:
// poll ascending pages (since_seq exclusive; a full page is a continuation)
// until an event whose type is in the terminal set arrives, then stop. A run
// whose stream never yields a terminal type would spin forever — this is the
// regression the PascalCase vocabulary caused.
func TestEvents_StreamTerminationWalk(t *testing.T) {
	s := newTestStore(t)
	root, _ := s.RunsRoot()
	lines := []ndEnvelope{envPB(t, 1, "RunStarted", &pb.RunStarted{WorkflowName: "wf"})}
	for i := int64(2); i <= 5; i++ {
		lines = append(lines, envPB(t, i, "StepLog", &pb.StepLog{Step: "build", Stream: pb.LogStream_LOG_STREAM_STDOUT, Chunk: "x"}))
	}
	lines = append(lines, envPB(t, 6, "RunCompleted", &pb.RunCompleted{FinalState: "done", Success: true}))
	writeRun(t, root, "walk", nil, lines, nil)

	const pageLimit = 2
	since := int64(0)
	polls := 0
	terminated := false
	for {
		page, err := s.Events("walk", since, pageLimit)
		if err != nil {
			t.Fatalf("walk fetch: %v", err)
		}
		for _, ev := range page.Events {
			if TerminalSeamEventTypes[ev.Type] {
				terminated = true
				break
			}
			since = ev.Seq
		}
		if terminated || len(page.Events) < pageLimit {
			// A non-full page means no continuation: the walk ends either way.
			break
		}
		polls++
		if polls > 10 {
			t.Fatal("event stream walk never terminated on a terminal type")
		}
	}
	if !terminated {
		t.Error("walk ended without observing a terminal event type")
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
	}, []ndEnvelope{envPB(t, 1, "RunStarted", &pb.RunStarted{WorkflowName: "wf"})}, nil)
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

// TestGetRun_Failed maps a runFailed event to failed + failureReason.
func TestGetRun_Failed(t *testing.T) {
	s := newTestStore(t)
	root, _ := s.RunsRoot()
	writeRun(t, root, "f", &localState{PID: -1, RunID: "f", Workflow: "wf"}, []ndEnvelope{
		envPB(t, 1, "RunStarted", &pb.RunStarted{WorkflowName: "wf"}),
		envPB(t, 2, "RunFailed", &pb.RunFailed{Reason: "adapter boom", Step: "deploy"}),
	}, nil)
	run, err := s.GetRun("f")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != StatusFailed || run.FailureReason != "adapter boom" {
		t.Errorf("status/reason = %q/%q", run.Status, run.FailureReason)
	}
}

// TestListRuns_FiltersAndPagination verifies agent/status filters,
// newest-first ordering, limit/keyset-cursor pagination and the opaque token
// contract.
func TestListRuns_FiltersAndPagination(t *testing.T) {
	s := newTestStore(t)
	root, _ := s.RunsRoot()
	// b is written last → newest first. a, b terminal succeeded; c running
	// with a criteria id.
	writeRun(t, root, "aaa", nil, []ndEnvelope{
		envPB(t, 1, "RunStarted", &pb.RunStarted{WorkflowName: "a"}),
		envPB(t, 2, "RunCompleted", &pb.RunCompleted{FinalState: "done", Success: true}),
	}, nil)
	time.Sleep(10 * time.Millisecond) // distinct mtimes
	writeRun(t, root, "bbb", nil, []ndEnvelope{
		envPB(t, 1, "RunStarted", &pb.RunStarted{WorkflowName: "b"}),
		envPB(t, 2, "RunCompleted", &pb.RunCompleted{FinalState: "done", Success: false}),
	}, nil)
	time.Sleep(10 * time.Millisecond)
	writeRun(t, root, "ccc", &localState{
		PID: os.Getpid(), RunID: "ccc", Workflow: "c", StartedAt: time.Now().UTC(), CriteriaID: "crit-9",
	}, []ndEnvelope{envPB(t, 1, "RunStarted", &pb.RunStarted{WorkflowName: "c"})}, nil)

	// Newest first: ccc (running, newest mtime), bbb, aaa.
	page, err := s.ListRuns("", "", 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Runs) != 2 || page.Runs[0].RunID != "ccc" || page.Runs[1].RunID != "bbb" {
		t.Fatalf("page1 = %+v", page.Runs)
	}
	if page.NextPageToken == "" || page.NextPageToken == "bbb" {
		t.Errorf("nextPageToken = %q, want the opaque keyset token of bbb", page.NextPageToken)
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
	// A cursor this store never issued → deterministic empty page, not an error.
	empty, err := s.ListRuns("", "", 0, "bogus")
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Runs) != 0 {
		t.Errorf("malformed cursor should return empty page, got %+v", empty.Runs)
	}
}

// TestListRuns_KeysetWalkUnorderedIDs is the R7 regression: run ids whose
// mtime order is NOT lexicographic must walk to the end with every run
// appearing exactly once — the previous cursor (a bare run id compared
// against mtime-sorted ids) dropped the tail whenever the two orders
// disagreed.
func TestListRuns_KeysetWalkUnorderedIDs(t *testing.T) {
	s := newTestStore(t)
	root, _ := s.RunsRoot()
	// Write in mtime order aaaa,dddd,bbbb,cccc: mtime order (newest first)
	// is cccc,bbbb,dddd,aaaa — deliberately different from id order.
	for _, id := range []string{"aaaa", "dddd", "bbbb", "cccc"} {
		writeRun(t, root, id, nil, []ndEnvelope{
			envPB(t, 1, "RunStarted", &pb.RunStarted{WorkflowName: id}),
			envPB(t, 2, "RunCompleted", &pb.RunCompleted{FinalState: "done", Success: true}),
		}, nil)
		time.Sleep(10 * time.Millisecond)
	}

	seen := map[string]int{}
	since := ""
	for pageNum := 0; pageNum < 10; pageNum++ {
		page, err := s.ListRuns("", "", 1, since)
		if err != nil {
			t.Fatalf("page %d: %v", pageNum, err)
		}
		if len(page.Runs) == 0 {
			break
		}
		for _, r := range page.Runs {
			seen[r.RunID]++
		}
		if page.NextPageToken == "" {
			break
		}
		since = page.NextPageToken
	}
	if len(seen) != 4 {
		t.Fatalf("walk saw %d of 4 runs: %v", len(seen), seen)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("run %s appeared %d times", id, n)
		}
	}
}

// TestEvents_PaginationAndSinceSeq verifies the seq cursor semantics:
// since_seq is exclusive, ordering ascending, and the page carries
// lastSeq/nextSinceSeq — the continuation cursor is set exactly on full
// pages, including an exact-multiple tail (the consumer's empty-probe walk).
func TestEvents_PaginationAndSinceSeq(t *testing.T) {
	s := newTestStore(t)
	root, _ := s.RunsRoot()
	writeRun(t, root, "r1", nil, []ndEnvelope{
		envPB(t, 1, "RunStarted", &pb.RunStarted{WorkflowName: "wf"}),
		envPB(t, 2, "StepEntered", &pb.StepEntered{Step: "a"}),
		envPB(t, 3, "StepOutcome", &pb.StepOutcome{Step: "a", Outcome: "ok", DurationMs: 100}),
		envPB(t, 4, "RunCompleted", &pb.RunCompleted{FinalState: "done", Success: true}),
	}, nil)

	page, err := s.Events("r1", 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 2 || page.Events[0].Seq != 1 || page.Events[1].Seq != 2 {
		t.Fatalf("page1 = %+v", page.Events)
	}
	if page.LastSeq != 4 {
		t.Errorf("lastSeq = %d, want the run's highest seq (4)", page.LastSeq)
	}
	if page.NextSinceSeq == nil || *page.NextSinceSeq != 2 {
		t.Errorf("nextSinceSeq = %v, want 2 (full page)", page.NextSinceSeq)
	}
	page2, err := s.Events("r1", *page.NextSinceSeq, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2.Events) != 2 || page2.Events[0].Seq != 3 {
		t.Errorf("page2 = %+v", page2.Events)
	}
	if page2.NextSinceSeq == nil || *page2.NextSinceSeq != 4 {
		t.Errorf("exact-multiple tail page must still carry the probe cursor, got %v", page2.NextSinceSeq)
	}
	// Empty probe: no continuation, lastSeq still the run's highest seq.
	probe, err := s.Events("r1", *page2.NextSinceSeq, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(probe.Events) != 0 || probe.LastSeq != 4 || probe.NextSinceSeq != nil {
		t.Errorf("empty probe = %+v, want {events:[],lastSeq:4,nextSinceSeq:null}", probe)
	}
	// Contract shape: camelCase seam type with the producer's envelope keys.
	first := page.Events[0]
	if first.SchemaVersion != 1 || first.RunID != "r1" || first.Type != "runStarted" {
		t.Errorf("envelope = %+v", first)
	}
	b, _ := json.Marshal(first)
	if !strings.Contains(string(b), `"type":"runStarted"`) || !strings.Contains(string(b), `"seq":1`) {
		t.Errorf("wire shape = %s", b)
	}

	// Unknown run → ErrNotFound.
	if _, err := s.Events("nope", 0, 0); err == nil || !strings.Contains(err.Error(), "run not found") {
		t.Errorf("unknown run err = %v", err)
	}
}

// TestEvents_LimitClamping pins the clamp rule: an over-cap limit clamps to
// the max (1000) instead of silently falling back to the default.
func TestEvents_LimitClamping(t *testing.T) {
	s := newTestStore(t)
	root, _ := s.RunsRoot()
	lines := make([]ndEnvelope, 0, 1001)
	for i := int64(1); i <= 1001; i++ {
		lines = append(lines, envPB(t, i, "StepLog", &pb.StepLog{Step: "a", Chunk: "x"}))
	}
	writeRun(t, root, "clamp", nil, lines, nil)

	page, err := s.Events("clamp", 0, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1000 {
		t.Errorf("over-cap limit served %d events, want the 1000 max", len(page.Events))
	}
	if page.NextSinceSeq == nil || *page.NextSinceSeq != 1000 {
		t.Errorf("full page must carry the continuation cursor, got %v", page.NextSinceSeq)
	}
	// Default (0) stays 500.
	def, err := s.Events("clamp", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(def.Events) != 500 {
		t.Errorf("default limit served %d events, want 500", len(def.Events))
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
	complete := envPB(t, 1, "RunStarted", &pb.RunStarted{WorkflowName: "wf"})
	line, err := json.Marshal(complete)
	if err != nil {
		t.Fatal(err)
	}
	body := string(line) + "\n" + `{"schema_version":1,"seq":2,"run_id":"tor` // torn
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

// TestInspect_Shape pins the inspection shape: runId present,
// currentStep/adapter folded from the stepEntered payload.
func TestInspect_Shape(t *testing.T) {
	s := newTestStore(t)
	root, _ := s.RunsRoot()
	writeRun(t, root, "insp", nil, []ndEnvelope{
		envPB(t, 1, "RunStarted", &pb.RunStarted{WorkflowName: "wf"}),
		envPB(t, 2, "StepEntered", &pb.StepEntered{Step: "build", Adapter: "shell", Attempt: 1}),
	}, nil)
	insp, err := s.Inspect("insp", "sess-7")
	if err != nil {
		t.Fatal(err)
	}
	if insp.RunID != "insp" {
		t.Errorf("runId = %q, want insp", insp.RunID)
	}
	if insp.SessionID != "sess-7" || insp.CurrentStep != "build" || insp.Adapter != "shell" {
		t.Errorf("inspection = %+v", insp)
	}
	if _, err := json.Marshal(insp); err != nil {
		t.Fatalf("inspection not JSON-encodable: %v", err)
	}
	// Without a session, the synthetic id carries the run id and the seq of
	// the most recent stepEntered event (seq 2 here).
	insp2, err := s.Inspect("insp", "")
	if err != nil {
		t.Fatal(err)
	}
	if insp2.SessionID != fmt.Sprintf("insp/%d", 2) {
		t.Errorf("synthetic session id = %q", insp2.SessionID)
	}
}

// TestAgents_LabelsAndShape pins the Agent stub shape: labels always present
// (empty object for the stub) and the record round-trips the castle type.
func TestAgents_LabelsAndShape(t *testing.T) {
	s := newTestStore(t)
	root, _ := s.RunsRoot()
	writeRun(t, root, "a1", &localState{
		PID: os.Getpid(), RunID: "a1", Workflow: "wf", CriteriaID: "crit-1", StartedAt: time.Now().UTC(),
	}, nil, nil)

	agents, err := s.Agents()
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 {
		t.Fatalf("agents = %+v", agents)
	}
	if agents[0].Labels == nil {
		t.Error("agent labels must be a non-nil object (the consumer type requires labels)")
	}
	b, err := json.Marshal(agents)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"labels":{`) {
		t.Errorf("agents wire shape = %s", b)
	}
	agent, ok := s.Agent("crit-1")
	if !ok || agent.Labels == nil {
		t.Errorf("Agent(crit-1) = %+v", agent)
	}
}
