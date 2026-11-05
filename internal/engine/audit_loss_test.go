package engine

import (
	"context"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/brokenbots/criteria/internal/adapterhost"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// fakeAuditLossReporter is a configurable AuditLossReporter for report
// tests; its Write side accepts entries so it can be passed anywhere an
// AuditWriter is expected, and it reports a cumulative loss.
type fakeAuditLossReporter struct {
	dropped      int64
	attempted    int64
	lastError    string
	reportedFlag atomic.Bool
}

func (r *fakeAuditLossReporter) Write(e *adapterhost.DecisionLogEntry) {
	r.attempted++
}

// AuditLoss implements AuditLossReporter.
func (r *fakeAuditLossReporter) AuditLoss() (dropped, attempted int64, lastError string) {
	return r.dropped, r.attempted, r.lastError
}

// MarkLossReported implements AuditLossReporter.
func (r *fakeAuditLossReporter) MarkLossReported() bool {
	return r.reportedFlag.CompareAndSwap(false, true)
}

var _ adapterhost.AuditWriter = (*fakeAuditLossReporter)(nil)
var _ AuditLossReporter = (*fakeAuditLossReporter)(nil)

// TestReportAuditLoss pins the probe's contract: one event through the sink
// exactly when the writer reports drops, carrying the writer's snapshot,
// and nothing at all for nil plumbing, non-reporting writers, or a clean
// writer. A reported loss stays reported: a second probe cannot duplicate
// the event.
func TestReportAuditLoss(t *testing.T) {
	for _, tc := range []struct {
		name        string
		sink        Sink
		writer      adapterhost.AuditWriter
		dropped     int64
		attempted   int64
		lastError   string
		wantEvent   bool
		wantFlagSet bool
	}{
		{name: "nil sink ignores loss", sink: nil, writer: &fakeAuditLossReporter{dropped: 5}},
		{name: "nil writer ignores loss", sink: &fakeSink{}, writer: nil},
		{name: "non-reporting writer reports nothing", sink: &fakeSink{}, writer: &engineAuditCollector{}, dropped: 5},
		{name: "clean writer reports nothing", sink: &fakeSink{}, writer: &fakeAuditLossReporter{}},
		{
			name: "dropping writer reports once", sink: &fakeSink{},
			writer:  &fakeAuditLossReporter{dropped: 3, attempted: 7, lastError: "disk full"},
			dropped: 3, attempted: 7, lastError: "disk full", wantEvent: true, wantFlagSet: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reportAuditLoss(tc.sink, tc.writer, nil)

			var recorded []*pb.AuditLoss
			if fake, ok := tc.sink.(*fakeSink); ok {
				recorded = fake.auditLosses
			}
			if tc.wantEvent {
				if got := len(recorded); got != 1 {
					t.Fatalf("want exactly one AuditLoss event, got %d", got)
				}
				loss := recorded[0]
				if loss.DroppedEntries != tc.dropped || loss.AttemptedEntries != tc.attempted {
					t.Errorf("loss = %v, want dropped=%d attempted=%d", loss, tc.dropped, tc.attempted)
				}
				if loss.LastError != tc.lastError {
					t.Errorf("last error = %q, want %q", loss.LastError, tc.lastError)
				}
			} else if got := len(recorded); got != 0 {
				t.Fatalf("want no AuditLoss event, got %d", got)
			}
			if tc.wantFlagSet {
				if fake, ok := tc.writer.(*fakeAuditLossReporter); ok && !fake.reportedFlag.Load() {
					t.Error("a surfacing probe must claim the writer's once-flag")
				}
			}
		})
	}

	t.Run("second probe cannot duplicate the event", func(t *testing.T) {
		inner := &fakeSink{}
		writer := &fakeAuditLossReporter{dropped: 3, attempted: 7, lastError: "disk full"}
		reportAuditLoss(inner, writer, nil)
		reportAuditLoss(inner, writer, nil)
		if got := len(inner.auditLosses); got != 1 {
			t.Fatalf("want one event after repeated probes, got %d", got)
		}
	})
}

// opsRecordingSink records loss and terminal callbacks in one sequence so
// engine tests can assert the event order through the real chain.
type opsRecordingSink struct {
	fakeSink
	mu  sync.Mutex
	ops []string
}

func (s *opsRecordingSink) record(op string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ops = append(s.ops, op)
}

func (s *opsRecordingSink) OnRunCompleted(state string, ok bool) {
	s.record("terminal")
	s.fakeSink.OnRunCompleted(state, ok)
}

func (s *opsRecordingSink) OnRunFailed(reason, step string) {
	s.record("terminal")
	s.fakeSink.OnRunFailed(reason, step)
}

func (s *opsRecordingSink) OnAuditLoss(l *pb.AuditLoss) {
	s.record("loss")
	s.fakeSink.OnAuditLoss(l)
}

// poisonedAuditWriter returns a real FileAuditWriter whose every setup
// attempt fails: a blocker file sits where the audit path's parent
// directory would be created.
func poisonedAuditWriter(t *testing.T) *adapterhost.FileAuditWriter {
	t.Helper()
	blocker := t.TempDir() + "/blocker"
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return adapterhost.NewFileAuditWriter(blocker + "/audit.log")
}

// TestRun_SurfacesAuditLossFromRealPermissionPath is the acceptance check
// for audit-loss surfacing: a full engine run whose adapter permission
// decisions normally audit-write (the caller's step-level tools grant plus
// the callee's own allowed permission request) against a poisoned audit
// file still completes successfully — the permission path is never
// blocked — and the run's record carries exactly one AuditLoss event with
// the final dropped count, including any teardown-phase audit writes that
// land after the terminal event.
func TestRun_SurfacesAuditLossFromRealPermissionPath(t *testing.T) {
	audit := poisonedAuditWriter(t)

	g := compileFile(t, adapterToolCallLoopFixture)

	inner := &opsRecordingSink{}
	var sink Sink = inner
	callee := &loopCalleeAdapter{rec: &nestedEngineRecorder{}}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"caller": &loopCallerAdapter{},
		"callee": callee,
	}}

	if err := New(g, loader, sink, WithAuditWriter(audit)).Run(context.Background()); err != nil {
		t.Fatalf("run must not fail when the audit file is unavailable: %v", err)
	}
	if !inner.terminalOK || inner.failure != "" {
		t.Fatalf("run terminal=%q ok=%v failure=%q; want a successful completion", inner.terminal, inner.terminalOK, inner.failure)
	}

	// Every audit attempt the run made — including any teardown-phase
	// writes the engine emits after the terminal event — must be counted
	// as a drop, and the surfaced event must carry those final counts.
	dropped, attempted, lastErr := audit.AuditLoss()
	if dropped < 2 {
		t.Fatalf("the fixture's permission decisions must have been accounted as drops, got %d (attempted %d)", dropped, attempted)
	}
	if dropped != attempted {
		t.Fatalf("every attempt must drop while the path is unwritable: dropped=%d attempted=%d", dropped, attempted)
	}
	if lastErr == "" {
		t.Fatal("expected the loss snapshot to carry the failing setup error")
	}

	if got := len(inner.auditLosses); got != 1 {
		t.Fatalf("want exactly one AuditLoss event in the run's record, got %d", got)
	}
	loss := inner.auditLosses[0]
	if loss.DroppedEntries != dropped || loss.AttemptedEntries != attempted {
		t.Errorf("loss = %v, want dropped=%d attempted=%d", loss, dropped, attempted)
	}
	if loss.LastError == "" {
		t.Error("the run record must carry the audit writer's last error")
	}

	// The record shows the run outcome first and the loss verdict last:
	// the loss probe runs after the whole lifecycle unwind, so it also
	// sees the teardown-phase audit writes.
	if got := len(inner.ops); got < 2 || inner.ops[got-1] != "loss" {
		t.Fatalf("want the loss event last in the record (after the terminal), ops=%v", inner.ops)
	}
}

// TestRun_CleanAuditProducesNoLossEvent proves the loss event is strictly
// loss-driven: the same permission-heavy fixture against a healthy audit
// file completes with zero loss events and every decision on disk.
func TestRun_CleanAuditProducesNoLossEvent(t *testing.T) {
	auditPath := t.TempDir() + "/audit.log"
	audit := adapterhost.NewFileAuditWriter(auditPath)

	g := compileFile(t, adapterToolCallLoopFixture)

	inner := &opsRecordingSink{}
	var sink Sink = inner
	callee := &loopCalleeAdapter{rec: &nestedEngineRecorder{}}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{
		"caller": &loopCallerAdapter{},
		"callee": callee,
	}}

	if err := New(g, loader, sink, WithAuditWriter(audit)).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := len(inner.auditLosses); got != 0 {
		t.Fatalf("want no AuditLoss event for a healthy audit file, got %d", got)
	}
	dropped, attempted, _ := audit.AuditLoss()
	if dropped != 0 || attempted < 2 {
		t.Fatalf("healthy writer: dropped=%d attempted=%d, want zero drops and at least the two permission decisions", dropped, attempted)
	}
	if lines := countAuditLines(t, auditPath); int64(lines) != attempted {
		t.Fatalf("audit file holds %d lines against %d attempted entries", lines, attempted)
	}
}

// countAuditLines counts JSON lines in the audit file at path; a missing
// file counts as zero.
func countAuditLines(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read audit file %s: %v", path, err)
	}
	return strings.Count(string(b), "\n")
}
