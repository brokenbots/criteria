package engine

import (
	"log/slog"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"

	"github.com/brokenbots/criteria/internal/adapterhost"
)

// AuditLossReporter reports cumulative permission-audit write loss for a
// run's audit writer. *adapterhost.FileAuditWriter implements it; other
// AuditWriter implementations (in-memory test fakes) do not and are never
// probed.
type AuditLossReporter interface {
	// AuditLoss reports how many audit entries were dropped, how many
	// writes were attempted, and the most recent write error ("" when the
	// writer never failed).
	AuditLoss() (dropped, attempted int64, lastError string)
}

// auditLossSink wraps a run's sink chain and emits a terminal AuditLoss
// event before the run's terminal event whenever the audit writer reports
// dropped entries. The permission path itself is never consulted beyond
// the atomic counter read, so wrapping can never make it block.
type auditLossSink struct {
	Sink
	reporter AuditLossReporter
	log      *slog.Logger
	reported bool
}

// NewAuditLossSink returns a sink that reports audit loss ahead of the run's
// terminal event. It returns inner unchanged when there is nothing to
// report through: inner nil, no audit writer, or an audit writer that does
// not implement AuditLossReporter.
func NewAuditLossSink(inner Sink, writer adapterhost.AuditWriter, log *slog.Logger) Sink {
	if inner == nil {
		return nil
	}
	reporter, ok := writer.(AuditLossReporter)
	if !ok {
		return inner
	}
	return &auditLossSink{Sink: inner, reporter: reporter, log: log}
}

// reportIfDropped emits the AuditLoss event once per run when the audit
// writer has dropped entries; a clean writer emits nothing.
func (s *auditLossSink) reportIfDropped() {
	if s.reported {
		return
	}
	dropped, attempted, lastError := s.reporter.AuditLoss()
	if dropped == 0 {
		return
	}
	s.reported = true
	if s.log != nil {
		s.log.Warn("permission audit entries were dropped during the run",
			"dropped", dropped, "attempted", attempted)
	}
	s.Sink.OnAuditLoss(&pb.AuditLoss{
		DroppedEntries:   dropped,
		AttemptedEntries: attempted,
		LastError:        lastError,
	})
}

func (s *auditLossSink) OnRunCompleted(finalState string, success bool) {
	s.reportIfDropped()
	s.Sink.OnRunCompleted(finalState, success)
}

func (s *auditLossSink) OnRunFailed(reason, step string) {
	s.reportIfDropped()
	s.Sink.OnRunFailed(reason, step)
}
