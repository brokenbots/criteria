package adapterhost

import (
	"testing"

	"github.com/brokenbots/criteria/internal/adapter"
)

// TestKB57cRecoveredDenialKeepsSuccess: a policy denial the model recovers
// from (a later granted permission) must no longer flip a finalized "success"
// to "needs_review". KB-61 refire kb-61-1790827708: the coordinator's
// evidence compounds were denied then retried as granted split commands; the
// last-decision-wins semantics keep the verified success verdict, ending the
// deny-loop → needs_review → Review resurrection of the same tickets.
func TestKB57cRecoveredDenialKeepsSuccess(t *testing.T) {
	collector := &adapterEventCollector{}
	newSink := func() *executeCaptureSink {
		return &executeCaptureSink{sink: collector}
	}

	// deny then grant: recovered -> success survives
	s := newSink()
	s.emitDenied("r1", "shell", "no matching allow_tools entry")
	s.emitGranted("r2", "shell", "matched: shell:git log *")
	res := adapter.Result{Outcome: "success"}
	s.applyNeedsReviewOverride(&res)
	if res.Outcome != "success" {
		t.Fatalf("recovered denial overrode success -> %q; want success", res.Outcome)
	}

	// deny as the last decision: override to needs_review (unchanged legacy shape)
	s2 := newSink()
	s2.emitGranted("r3", "shell", "matched: shell:git log *")
	s2.emitDenied("r4", "shell", "no matching allow_tools entry")
	res2 := adapter.Result{Outcome: "success"}
	s2.applyNeedsReviewOverride(&res2)
	if res2.Outcome != "needs_review" {
		t.Fatalf("unrecovered denial did not override -> %q; want needs_review", res2.Outcome)
	}
}
