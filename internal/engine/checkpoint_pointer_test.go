package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/runtime/state"
	"github.com/brokenbots/criteria/workflow"
)

// ckPointerSink records the checkpoint pointer events the engine emits for
// durable checkpoint saves.
type ckPointerSink struct {
	fakeSink
	mu            sync.Mutex
	ptrs          []CheckpointPointerEvent
	onStepOutcome func(step string)
}

func (s *ckPointerSink) OnStepOutcome(step, outcome string, d time.Duration, err error) {
	s.fakeSink.OnStepOutcome(step, outcome, d, err)
	if s.onStepOutcome != nil {
		s.onStepOutcome(step)
	}
}

func (s *ckPointerSink) OnCheckpointPointer(ev *CheckpointPointerEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ptrs = append(s.ptrs, *ev)
}

func (s *ckPointerSink) pointers() []CheckpointPointerEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]CheckpointPointerEvent(nil), s.ptrs...)
}

// TestEngine_CheckpointPointerEmittedAtStepBoundary pins the CRI-203 advisory
// pointer at the emission source: the run canceled after step a's boundary —
// with step a's checkpoint durable — has served exactly one pointer, and its
// fields mirror the persisted snapshot on disk (no drift between what is
// emitted and the state table's representation).
func TestEngine_CheckpointPointerEmittedAtStepBoundary(t *testing.T) {
	tmp := t.TempDir()
	// Compile before ctx exists: compilation is not cancelable work, and
	// keeping it out of the run goroutine avoids racing t.* helpers.
	graph := twoStepGraph(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := &checkpointHandle{blockSecond: true}
	runDone := make(chan error, 1)
	sink := &ckPointerSink{onStepOutcome: func(step string) {
		// A step's checkpoint save happens at its boundary, before its
		// outcome event fires — cancel once step a is done so step b (whose
		// Execute blocks) never reaches a durable boundary.
		if step == "a" {
			cancel()
		}
	}}
	go func() {
		e := New(graph, &checkpointLoader{handle: h}, sink, WithRunID(testRunID),
			WithSnapshotBase(tmp))
		runDone <- e.Run(ctx)
	}()
	if err := <-runDone; err == nil {
		t.Fatal("expected canceled run to return an error")
	}

	ptrs := sink.pointers()
	if len(ptrs) != 1 {
		t.Fatalf("served %d checkpoint pointers; want 1 (step b was never durable)", len(ptrs))
	}
	p := ptrs[0]

	if p.RunID != testRunID {
		t.Errorf("runID = %q; want %q", p.RunID, testRunID)
	}
	if p.SessionID != "ck.default" {
		t.Errorf("sessionID = %q; want ck.default", p.SessionID)
	}
	if p.StateID != "ck.default/0000000001" {
		t.Errorf("stateID = %q; want ck.default/0000000001", p.StateID)
	}
	// Adapter identity resolves from the workflow graph: session keys are
	// graph adapter keys, so the pointer names the kind and the instance.
	if p.AdapterKind != "ck" || p.AdapterName != "default" {
		t.Errorf("adapter = %q/%q; want ck/default", p.AdapterKind, p.AdapterName)
	}

	// Cross-check against the persisted snapshot: the pointer is a rendered
	// view of the state row, not a parallel guess.
	snap, err := state.ReadLatestSnapshot(state.SnapshotDir(tmp, testRunID, "ck.default"))
	if err != nil {
		t.Fatalf("read snapshot for cross-check: %v", err)
	}
	if p.StateDigest != snap.StateDigest {
		t.Errorf("pointer digest %q != persisted %q", p.StateDigest, snap.StateDigest)
	}
	if p.StateSchema != snap.StateSchema {
		t.Errorf("pointer schema %q != persisted %q", p.StateSchema, snap.StateSchema)
	}
	if p.StateSize != int64(len(snap.AdapterState)) {
		t.Errorf("pointer size %d != persisted blob length %d", p.StateSize, len(snap.AdapterState))
	}

	if p.Granularity != string(workflow.StateGranularityPerStep) {
		t.Errorf("granularity = %q; want %q", p.Granularity, workflow.StateGranularityPerStep)
	}
}
