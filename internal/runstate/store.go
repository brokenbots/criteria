package runstate

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/brokenbots/criteria/internal/dirs"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// ErrNotFound is returned when a run id has no local state.
var ErrNotFound = errors.New("run not found")

const (
	stateFileName    = "run-state.json"
	eventsFileName   = "events.ndjson"
	metadataFileName = "run-metadata.json"
)

// Store reads run state (run-state.json, events.ndjson, run-metadata.json)
// under $CRITERIA_HOME/runs/<runID>/. It is read-only: nothing in this
// package writes to the run directories (CRI-279 locked rule: no writes
// beyond what the control verbs do).
type Store struct {
	// home resolves the criteria root. Overridable for tests.
	home func() (string, error)
	// scope, when non-empty, restricts the store to a single run id
	// (apply wires its run id). Set only through Scoped.
	scope string
}

// NewStore returns a Store rooted at the process criteria home.
func NewStore() *Store {
	return &Store{home: dirs.Home}
}

// NewStoreAt returns a Store rooted at an explicit directory (serve-ui --home).
func NewStoreAt(root string) *Store {
	return &Store{home: func() (string, error) { return root, nil }}
}

// RunsRoot returns <home>/runs.
func (s *Store) RunsRoot() (string, error) {
	d, err := s.home()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "runs"), nil
}

// RunDir returns the run's state directory. Run ids are UUIDs; anything that
// could escape the runs root is refused as not found.
func (s *Store) RunDir(runID string) (string, error) {
	if runID == "" || runID == "." || runID == ".." || strings.ContainsAny(runID, `/\`) {
		return "", ErrNotFound
	}
	if s.hasScope() && runID != s.scope {
		return "", ErrNotFound
	}
	root, err := s.RunsRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, runID), nil
}

// localState is the on-disk run-state.json record (subset; unknown fields in
// the file are ignored so older and newer writers interoperate).
type localState struct {
	PID          int       `json:"pid"`
	RunID        string    `json:"run_id"`
	Workflow     string    `json:"workflow"`
	ServerURL    string    `json:"server_url"`
	StartedAt    time.Time `json:"started_at"`
	Status       string    `json:"status,omitempty"`
	CriteriaID   string    `json:"criteria_id,omitempty"`
	WorkflowHash string    `json:"workflow_hash,omitempty"`
}

// runMetadata is the provenance record written at admission.
type runMetadata struct {
	Kind   string `json:"kind,omitempty"`
	Source string `json:"source,omitempty"`
}

// ndEnvelope is one line of the run's ND-JSON events file
// (internal/run LocalSink's envelope).
type ndEnvelope struct {
	SchemaVersion int             `json:"schema_version"`
	Seq           int64           `json:"seq"`
	RunID         string          `json:"run_id"`
	PayloadType   string          `json:"payload_type"`
	Payload       json.RawMessage `json:"payload"`
}

// ListRunIDs returns the run ids of every run directory that holds run data
// (an events file or a run-state.json). Non-run directories are skipped.
func (s *Store) ListRunIDs() ([]string, error) {
	root, err := s.RunsRoot()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]string, 0, len(entries))
	if s.hasScope() {
		if fileExists(filepath.Join(root, s.scope, eventsFileName)) ||
			fileExists(filepath.Join(root, s.scope, stateFileName)) {
			return []string{s.scope}, nil
		}
		return out, nil
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		hasEvents := fileExists(filepath.Join(dir, eventsFileName))
		hasState := fileExists(filepath.Join(dir, stateFileName))
		if !hasEvents && !hasState {
			continue // no run data owned here
		}
		out = append(out, e.Name())
	}
	return out, nil
}

// GetRun materializes the castle-mapped Run for runID. It returns an error
// wrapping ErrNotFound when the run has no local data.
func (s *Store) GetRun(runID string) (*Run, error) {
	dir, err := s.RunDir(runID)
	if err != nil {
		return nil, err
	}
	st := s.readState(runID)
	events := s.readEvents(runID)
	if st == nil && len(events) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, runID)
	}
	return deriveRun(dir, st, events), nil
}

// ListRuns returns the page of runs for GET /runs. Filters: agent (exact
// criteria id — empty matches all), status; limit and cursor paginate
// newest-first (events-file mtime, run id). The cursor is an opaque keyset
// token carrying the previous page's sort key — the (mtime, runID) of its
// last run — so pagination is monotonic with respect to the sort key and
// never drops the tail when run-id order differs from mtime order. A
// malformed or stale token deterministically ends the listing.
func (s *Store) ListRuns(agent, status string, limit int, cursor string) (*RunsPage, error) {
	if limit <= 0 {
		limit = 50
	} else if limit > 200 {
		limit = 200
	}
	entries, err := s.runEntriesNewestFirst()
	if err != nil {
		return nil, err
	}
	if cursor != "" {
		c, ok := decodeRunCursor(cursor)
		if !ok {
			return &RunsPage{Runs: []Run{}}, nil
		}
		entries = entriesAfter(entries, c)
	}
	page := &RunsPage{Runs: []Run{}}
	var last runEntry
	for _, e := range entries {
		if len(page.Runs) == limit {
			page.NextPageToken = runCursorToken(last)
			return page, nil
		}
		r, err := s.GetRun(e.id)
		if err != nil {
			continue // vanished or unreadable mid-list: skip
		}
		if agent != "" && r.CriteriaID != agent {
			continue
		}
		if status != "" && r.Status != status {
			continue
		}
		page.Runs = append(page.Runs, *r)
		last = e
	}
	return page, nil
}

// Events returns the run's events with seq > sinceSeq, in ascending seq
// order, capped at limit (default 500, max 1000 — above the cap the limit is
// clamped). The page shape follows the consumer contract (dataSource.ts
// RunEventsPage): lastSeq is the highest seq in the run at fetch time;
// nextSinceSeq is the continuation cursor for the next (newer) page — the
// last returned seq on a full page — or null when the page was not full.
// A page that is exactly the run's tail still carries nextSinceSeq, so the
// consumer's walk terminates with the empty probe it expects.
func (s *Store) Events(runID string, sinceSeq int64, limit int) (*RunEventsPage, error) {
	if _, err := s.RunDir(runID); err != nil {
		return nil, err
	}
	all := s.readEvents(runID)
	if len(all) == 0 {
		// Distinguish a run with no events yet from an unknown run.
		if _, err := s.GetRun(runID); err != nil {
			return nil, err
		}
	}
	if limit <= 0 {
		limit = 500
	} else if limit > 1000 {
		limit = 1000
	}
	var lastSeq int64
	if len(all) > 0 {
		lastSeq = all[len(all)-1].Seq
	}
	out := make([]EventEnvelope, 0, min(limit, len(all)))
	for _, ev := range all {
		if ev.Seq <= sinceSeq {
			continue
		}
		out = append(out, ev)
		if len(out) == limit {
			break
		}
	}
	page := &RunEventsPage{Events: out, LastSeq: lastSeq}
	if len(out) == limit {
		next := out[len(out)-1].Seq
		page.NextSinceSeq = &next
	}
	return page, nil
}

// readEvents parses the run's ND-JSON events file. Trailing partial lines
// (the writer may hold the file open mid-append) and malformed records are
// skipped, never fatal. The type is mapped onto the seam vocabulary
// (eventvocab.go) and the payload stays raw JSON verbatim.
func (s *Store) readEvents(runID string) []EventEnvelope {
	dir, err := s.RunDir(runID)
	if err != nil {
		return nil
	}
	f, err := os.Open(filepath.Join(dir, eventsFileName))
	if err != nil {
		return nil // absent (or unreadable): no events are servable
	}
	defer f.Close()
	var out []EventEnvelope
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var env ndEnvelope
		if json.Unmarshal([]byte(line), &env) != nil || env.PayloadType == "" {
			continue // torn or corrupt line
		}
		out = append(out, EventEnvelope{
			SchemaVersion: env.SchemaVersion,
			RunID:         env.RunID,
			Seq:           env.Seq,
			Type:          seamEventType(env.PayloadType),
			Payload:       env.Payload,
		})
	}
	return out
}

// readState loads run-state.json. Returns nil when absent or corrupt —
// terminal runs no longer carry the file (it is removed on completion), so
// absence is expected, not an error. Unreadable state degrades to nil: the
// events file remains servable.
func (s *Store) readState(runID string) *localState {
	dir, err := s.RunDir(runID)
	if err != nil {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		return nil
	}
	var st localState
	if json.Unmarshal(b, &st) != nil || st.RunID == "" {
		return nil
	}
	return &st
}

// readMetaSource returns the redacted workflow source URL from the run's
// metadata record, used as the Run.repoUrl castle mapping when present.
func readMetaSource(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, metadataFileName))
	if err != nil {
		return ""
	}
	var md runMetadata
	if json.Unmarshal(b, &md) != nil {
		return ""
	}
	return md.Source
}

// deriveRun builds the contract Run from the run's local files. Status
// derivation (the implementer's call, documented here):
//   - a terminal event (run.completed / run.failed) decides succeeded/failed,
//     with endedAt from the events file mtime and finalState/failureReason
//     from the event payload;
//   - otherwise run-state.json present and its pid alive → running;
//   - otherwise → failed ("criteria process exited without reaching a
//     terminal state"), i.e. a crash mid-run lands in the existing
//     crash-recovery reattach path and the viewer shows the run as failed.
func deriveRun(dir string, st *localState, events []EventEnvelope) *Run {
	run := &Run{Status: StatusRunning, RunID: filepath.Base(dir)}
	if st != nil {
		run.CriteriaID = st.CriteriaID
		run.WorkflowName = st.Workflow
		run.WorkflowHash = st.WorkflowHash
		if !st.StartedAt.IsZero() {
			run.StartedAt = st.StartedAt.UTC().Format(time.RFC3339)
		}
	}
	for _, ev := range events {
		applyEventToRun(run, st != nil, &ev)
	}
	finalizeRunStatus(dir, run, st)
	if src := readMetaSource(dir); src != "" {
		run.RepoURL = src
	}
	return run
}

// applyEventToRun folds a single NDJSON event into the derived run record.
// Payloads decode through the proto messages the producer serializes with
// protojson (camelCase keys; the parser also accepts the original snake_case
// field names) rather than ad-hoc JSON structs.
func applyEventToRun(run *Run, hasState bool, ev *EventEnvelope) {
	decode := func(msg proto.Message) bool {
		opts := protojson.UnmarshalOptions{DiscardUnknown: true}
		return opts.Unmarshal(ev.Payload, msg) == nil
	}
	switch ev.Type {
	case "runStarted":
		if hasState {
			return // run-state.json is the better source
		}
		var m pb.RunStarted
		if decode(&m) && m.WorkflowName != "" {
			run.WorkflowName = m.WorkflowName
		}
	case "runCompleted":
		var m pb.RunCompleted
		if decode(&m) {
			run.Status = StatusSucceeded
			run.FinalState = m.FinalState
			if !m.Success {
				run.Status = StatusFailed
			}
		}
	case "runFailed":
		var m pb.RunFailed
		if decode(&m) {
			run.Status = StatusFailed
			run.FailureReason = m.Reason
		}
	}
}

// finalizeRunStatus resolves the terminal-ended vs still-running question
// once the event stream has been folded in.
func finalizeRunStatus(dir string, run *Run, st *localState) {
	if run.Status != StatusRunning {
		run.EndedAt = fileMtimeRFC3339(dir, eventsFileName)
		return
	}
	if st == nil {
		// No run-state.json (removed at completion) and no terminal event:
		// the record is a truncated tail of an interrupted run.
		run.Status = StatusFailed
		run.FailureReason = "no terminal event recorded"
		run.EndedAt = fileMtimeRFC3339(dir, eventsFileName)
		return
	}
	if st.PID > 0 && !pidAlive(st.PID) {
		run.Status = StatusFailed
		run.FailureReason = "criteria process exited without reaching a terminal state"
		run.EndedAt = fileMtimeRFC3339(dir, eventsFileName)
		if run.EndedAt == "" {
			run.EndedAt = fileMtimeRFC3339(dir, stateFileName)
		}
	}
}

// pidAlive reports whether a process with pid is alive. A reused pid may
// false-positive; accepted (documented) for a best-effort local viewer.
func pidAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := p.Signal(syscall.Signal(0)); err != nil {
		// On Linux, EPERM means the process exists but is not ours to signal.
		return errors.Is(err, syscall.EPERM)
	}
	return true
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func fileMtimeRFC3339(dir, name string) string {
	info, err := os.Stat(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return info.ModTime().UTC().Format(time.RFC3339)
}

// runEntry is one run's sort key: the events-file mtime (zero when the run
// has no events file) and the run id tiebreak.
type runEntry struct {
	id    string
	mtime time.Time
}

// runEntriesNewestFirst lists every servable run entry ordered newest first:
// mtime descending, run id ascending as the tiebreak — a stable keyset order.
func (s *Store) runEntriesNewestFirst() ([]runEntry, error) {
	ids, err := s.ListRunIDs()
	if err != nil {
		return nil, err
	}
	root, err := s.RunsRoot()
	if err != nil {
		return nil, err
	}
	entries := make([]runEntry, 0, len(ids))
	for _, id := range ids {
		var mtime time.Time
		if info, err := os.Stat(filepath.Join(root, id, eventsFileName)); err == nil {
			mtime = info.ModTime()
		}
		entries = append(entries, runEntry{id: id, mtime: mtime})
	}
	sort.Slice(entries, func(i, j int) bool {
		ti, tj := entries[i].mtime, entries[j].mtime
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return entries[i].id < entries[j].id
	})
	return entries, nil
}

// entriesAfter keeps the entries strictly after cursor c in the newest-first
// order: strictly older mtime, or an equal mtime with a run id greater than
// the cursor's (the id tiebreak is ascending, so resuming after the cursor
// id means ids greater than it).
func entriesAfter(entries []runEntry, c runEntry) []runEntry {
	out := make([]runEntry, 0, len(entries))
	for _, e := range entries {
		if e.mtime.Before(c.mtime) || (e.mtime.Equal(c.mtime) && e.id > c.id) {
			out = append(out, e)
		}
	}
	return out
}

// runCursorToken encodes a run entry as an opaque pagination token carrying
// the sort key (mtime unix nanos | run id). It rides a URL query value; the
// '|' separator is percent-encoded by well-behaved clients and accepted raw
// by Go's query parser.
func runCursorToken(e runEntry) string {
	return strconv.FormatInt(e.mtime.UnixNano(), 10) + "|" + e.id
}

// decodeRunCursor parses an opaque cursor token. Tokens issued by this
// process round-trip; anything else (client-tampered, past-version) is
// reported as unusable.
func decodeRunCursor(token string) (runEntry, bool) {
	nanos, id, ok := strings.Cut(token, "|")
	if !ok {
		return runEntry{}, false
	}
	n, err := strconv.ParseInt(nanos, 10, 64)
	if err != nil || id == "" {
		return runEntry{}, false
	}
	return runEntry{id: id, mtime: time.Unix(0, n)}, true
}

// Inspect materializes the castle-mapped RunInspection for a run. Local data
// derives a minimal but honest inspection: the most recent adapter session
// (from the last stepEntered event) and the run's last activity time. There
// is no live session attachment locally; the session parameter (when set) is
// echoed so the viewer can address it.
func (s *Store) Inspect(runID, session string) (*RunInspection, error) {
	if _, err := s.GetRun(runID); err != nil {
		return nil, err
	}
	dir, err := s.RunDir(runID)
	if err != nil {
		return nil, err
	}
	events := s.readEvents(runID)
	insp := &RunInspection{RunID: runID, PendingPermissions: 0}
	if session != "" {
		insp.SessionID = session
	}
	for _, ev := range events {
		if ev.Type != "stepEntered" {
			continue
		}
		var m pb.StepEntered
		opts := protojson.UnmarshalOptions{DiscardUnknown: true}
		if opts.Unmarshal(ev.Payload, &m) == nil {
			insp.CurrentStep = m.Step
			if m.Adapter != "" {
				insp.Adapter = m.Adapter
				if insp.SessionID == "" {
					// Local runs have one adapter session per step;
					// the session id is synthetic (run id + seq).
					insp.SessionID = fmt.Sprintf("%s/%d", runID, ev.Seq)
				}
			}
		}
	}
	if at := fileMtimeRFC3339(dir, eventsFileName); at != "" {
		insp.LastActivityAt = at
	}
	return insp, nil
}

// Agents lists the criteria ids known to this state dir as stub Agent
// records (contract-acceptable stub; the full agent surface is the
// orchestrator-backed path).
func (s *Store) Agents() ([]Agent, error) {
	ids, err := s.ListRunIDs()
	if err != nil {
		return nil, err
	}
	seen := make(map[string]Agent)
	for _, id := range ids {
		st := s.readState(id)
		if st == nil || st.CriteriaID == "" {
			continue // no agent identity recorded locally
		}
		if _, ok := seen[st.CriteriaID]; !ok {
			seen[st.CriteriaID] = Agent{
				CriteriaID: st.CriteriaID,
				Name:       st.CriteriaID,
				Labels:     map[string]string{},
				Status:     "online",
				LastSeenAt: startedAtRFC3339(st),
			}
		}
	}
	out := make([]Agent, 0, len(seen))
	for _, a := range seen {
		out = append(out, a)
	}
	return out, nil
}

// Agent returns one stub Agent by criteria id.
func (s *Store) Agent(criteriaID string) (*Agent, bool) {
	ids, err := s.ListRunIDs()
	if err != nil {
		return nil, false
	}
	var last *localState
	for _, id := range ids {
		st := s.readState(id)
		if st == nil || st.CriteriaID != criteriaID {
			continue
		}
		if last == nil || st.StartedAt.After(last.StartedAt) {
			last = st
		}
	}
	if last == nil {
		return nil, false
	}
	return &Agent{
		CriteriaID: last.CriteriaID,
		Name:       last.CriteriaID,
		Labels:     map[string]string{},
		Status:     "online",
		LastSeenAt: startedAtRFC3339(last),
	}, true
}

func startedAtRFC3339(st *localState) string {
	if st == nil || st.StartedAt.IsZero() {
		return ""
	}
	return st.StartedAt.UTC().Format(time.RFC3339)
}

// Scoped returns a Store restricted to a single run id: the owning apply
// process wires its run id, so its server serves only the run it owns
// (CRI-279 locked rule). ListRunIDs reports just that run; RunDir refuses
// every other id. The standalone serve-ui command uses the unscoped store.
func (s *Store) Scoped(runID string) *Store {
	c := *s
	c.scope = runID
	return &c
}

// scope is the single-run restriction set by Scoped (empty = unrestricted).
// Not exported; set only through Scoped.
func (s *Store) hasScope() bool { return s.scope != "" }
