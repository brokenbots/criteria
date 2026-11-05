package engine

import (
	"log/slog"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"

	"github.com/brokenbots/criteria/internal/adapterhost"
)

// AuditLossReporter reports cumulative permission-audit write loss for a
// run's audit writer. *adapterhost.FileAuditWriter implements it; other
// AuditWriter implementations do not and are never probed.
type AuditLossReporter interface {
	// AuditLoss reports how many audit entries were dropped, how many
	// writes were attempted, and the most recent write error ("" when the
	// writer never failed).
	AuditLoss() (dropped, attempted int64, lastError string)
	// MarkLossReported atomically claims the writer's loss for one
	// surfaced run record, returning true exactly when the caller flips
	// it. The once-guard lives on the writer so nested run segments that
	// share one writer emit the loss event once, not once per segment or
	// wrapper.
	MarkLossReported() bool
}

// reportAuditLoss surfaces a run's permission-audit write loss in the run
// record: one AuditLoss event through the sink chain when the writer
// reports dropped entries. Adapters keep writing permission-audit entries
// during the run's teardown phase, after the terminal event, so this runs
// in a defer declared before every lifecycle defer (last to execute) and
// probes the writer's final cumulative counters. It never blocks on the
// writer: the probe is atomic-counter reads and the event emission rides
// the run's existing sink plumbing.
func reportAuditLoss(sink Sink, writer adapterhost.AuditWriter, log *slog.Logger) {
	if sink == nil || writer == nil {
		return
	}
	reporter, ok := writer.(AuditLossReporter)
	if !ok {
		return
	}
	dropped, attempted, lastError := reporter.AuditLoss()
	if dropped == 0 {
		return
	}
	if !reporter.MarkLossReported() {
		return
	}
	if log != nil {
		log.Warn("permission audit entries were dropped during the run",
			"dropped", dropped, "attempted", attempted, "last_audit_error", lastError)
	}
	sink.OnAuditLoss(&pb.AuditLoss{
		DroppedEntries:   dropped,
		AttemptedEntries: attempted,
		LastError:        lastError,
	})
}
