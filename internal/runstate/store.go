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

	"github.com/brokenbots/criteria/internal/dirs"
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
// newest-first. cursor is the last run id of the previous page (opaque to the
// client); the returned token is the next cursor, empty on the last page.
func (s *Store) ListRuns(agent, status string, limit int, cursor string) (*RunsPage, error) {
	ids, err := s.ListRunIDs()
	if err != nil {
		return nil, err
	}
	s.sortRunIDsNewestFirst(ids)
	if cursor != "" {
		// ids is newest-first (descending). Find the cursor, then resume
		// after it. A cursor this store no longer knows (aged out, removed)
		// has no stable resume point without duplicating earlier pages, so
		// the listing deterministically ends.
		idx := sort.Search(len(ids), func(i int) bool { return ids[i] <= cursor })
		if idx == len(ids) || ids[idx] != cursor {
			return &RunsPage{Runs: []Run{}}, nil
		}
		ids = ids[idx+1:]
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	page := &RunsPage{Runs: []Run{}}
	for _, id := range ids {
		if len(page.Runs) == limit {
			page.NextPageToken = page.Runs[len(page.Runs)-1].RunID
			return page, nil
		}
		r, err := s.GetRun(id)
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
	}
	return page, nil
}

// Events returns the run's events with seq > sinceSeq, in ascending seq
// order, capped at limit (default 500). The next-page token is the last
// returned event's seq (empty when the end of the stream was reached).
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
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	out := make([]EventEnvelope, 0, min(limit, len(all)))
	for _, ev := range all {
		if ev.Seq <= sinceSeq {
			continue
		}
		if len(out) == limit {
			last := out[len(out)-1].Seq
			return &RunEventsPage{Events: out, NextPageToken: strconv.FormatInt(last, 10)}, nil
		}
		out = append(out, ev)
	}
	return &RunEventsPage{Events: out}, nil
}

// readEvents parses the run's ND-JSON events file. Trailing partial lines
// (the writer may hold the file open mid-append) and malformed records are
// skipped, never fatal.
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
			Type:          env.PayloadType,
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
		switch ev.Type {
		case "RunStarted":
			if st != nil {
				continue // run-state.json is the better source
			}
			var p struct {
				WorkflowName string `json:"workflow_name"`
			}
			if json.Unmarshal(ev.Payload, &p) == nil && p.WorkflowName != "" {
				run.WorkflowName = p.WorkflowName
			}
		case "RunCompleted":
			var p struct {
				FinalState string `json:"final_state"`
				Success    bool   `json:"success"`
			}
			if json.Unmarshal(ev.Payload, &p) == nil {
				run.Status = StatusSucceeded
				run.FinalState = p.FinalState
				if !p.Success {
					run.Status = StatusFailed
				}
			}
		case "RunFailed":
			var p struct {
				Reason string `json:"reason"`
			}
			if json.Unmarshal(ev.Payload, &p) == nil {
				run.Status = StatusFailed
				run.FailureReason = p.Reason
			}
		}
	}
	switch {
	case run.Status != StatusRunning:
		run.EndedAt = fileMtimeRFC3339(dir, eventsFileName)
	case st != nil:
		if st.PID > 0 && !pidAlive(st.PID) {
			run.Status = StatusFailed
			run.FailureReason = "criteria process exited without reaching a terminal state"
			run.EndedAt = fileMtimeRFC3339(dir, eventsFileName)
			if run.EndedAt == "" {
				run.EndedAt = fileMtimeRFC3339(dir, stateFileName)
			}
		}
	default:
		// No run-state.json (removed at completion) and no terminal event:
		// the record is a truncated tail of an interrupted run.
		run.Status = StatusFailed
		run.FailureReason = "no terminal event recorded"
		run.EndedAt = fileMtimeRFC3339(dir, eventsFileName)
	}
	if src := readMetaSource(dir); src != "" {
		run.RepoURL = src
	}
	return run
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

// sortRunIDsNewestFirst orders run ids by their events file mtime, newest
// first, so the default list view mirrors the castle (most recent first).
func (s *Store) sortRunIDsNewestFirst(ids []string) {
	root, err := s.RunsRoot()
	if err != nil {
		sort.Strings(ids)
		return
	}
	mtime := make(map[string]time.Time, len(ids))
	for _, id := range ids {
		if info, err := os.Stat(filepath.Join(root, id, eventsFileName)); err == nil {
			mtime[id] = info.ModTime()
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		ti, tj := mtime[ids[i]], mtime[ids[j]]
		if ti.IsZero() || tj.IsZero() {
			if ti.IsZero() != tj.IsZero() {
				return tj.IsZero() // known mtimes first
			}
			return ids[i] < ids[j]
		}
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return ids[i] < ids[j]
	})
}

// Inspect materializes the castle-mapped RunInspection for a run. Local data
// derives a minimal but honest inspection: the most recent adapter session
// (from the last StepEntered event) and the run's last activity time. There
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
	insp := &RunInspection{PendingPermissions: 0}
	if session != "" {
		insp.SessionID = session
	}
	for _, ev := range events {
		switch ev.Type {
		case "StepEntered":
			var p struct {
				Step    string `json:"step"`
				Adapter string `json:"adapter"`
			}
			if json.Unmarshal(ev.Payload, &p) == nil {
				insp.CurrentStep = p.Step
				if p.Adapter != "" {
					insp.Adapter = p.Adapter
					if insp.SessionID == "" {
						// Local runs have one adapter session per step;
						// the session id is synthetic (run id + seq).
						insp.SessionID = fmt.Sprintf("%s/%d", runID, ev.Seq)
					}
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
