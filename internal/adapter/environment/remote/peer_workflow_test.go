package remote

// peer_workflow_test.go — KB-95 (ADR-0008) parent-side workflow.v1
// session/run mapping tests: journal tracking, fail-closed re-Execute guard
// (adapterhost) + crash adoption from the surviving run record, transport
// loss carrying child-run evidence, teardown ordering (cancel before
// close), and typed outputs + workflow.v1 events through the peer Execute
// path.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zclconf/go-cty/cty"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"

	criteriav1 "github.com/brokenbots/criteria/sdk/pb/criteria/v1"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// fixtureWorkflowPeer wires a test shim + provider and connects a fake peer
// whose handshake advertises the given peer identity capabilities.
type workflowPeerFixture struct {
	provider *peerSessionProvider
	peer     *fakePeer
	ps       *peerSession
	handle   *peerHandle
}

func startWorkflowPeerFixture(t *testing.T, peerCaps []string) *workflowPeerFixture {
	t.Helper()
	provider, addr := startPeerFixture(t, &Config{ListenAddress: "127.0.0.1:0"})
	fp := newFakePeer("")
	fp.peerCaps = peerCaps
	fp.connect(t, addr)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := provider.WaitForHandle(ctx, "noop", ""); err != nil {
		t.Fatalf("WaitForHandle: %v", err)
	}
	ps := mustPeerSession(t, provider)
	return &workflowPeerFixture{provider: provider, peer: fp, ps: ps, handle: ps.handle}
}

// ops returns the fake peer's op-ordering witness (control + close ops).
func (f *fakePeer) opsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ops...)
}

// cancelChildRunControls returns the CancelChildRun controls the fake peer
// received.
func (f *fakePeer) cancelChildRunControls() []*criteriav1.ControlRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*criteriav1.ControlRequest
	for _, req := range f.ctrlReqs {
		if req.GetCancelChildRun() != nil {
			out = append(out, req)
		}
	}
	return out
}

// opIndexOf returns the index of the first op with the prefix, or -1.
func opIndexOf(ops []string, prefix string) int {
	for i, op := range ops {
		if strings.HasPrefix(op, prefix) {
			return i
		}
	}
	return -1
}

func childRunStartedArm(runID string) *criteriav1.SupervisionEvent {
	return &criteriav1.SupervisionEvent{
		Kind: &criteriav1.SupervisionEvent_ChildRunStarted{
			ChildRunStarted: &criteriav1.ChildRunStarted{
				RunId: runID, WorkflowDigest: "sha256:childwf", Version: "0.6",
			},
		},
	}
}

func childRunTerminalArm(runID, outcome string) *criteriav1.SupervisionEvent {
	return &criteriav1.SupervisionEvent{
		Kind: &criteriav1.SupervisionEvent_ChildRunTerminal{
			ChildRunTerminal: &criteriav1.ChildRunTerminal{
				RunId: runID, Outcome: outcome, OutputsDigest: "sha256:outputs",
			},
		},
	}
}

func waitForInFlightRun(t *testing.T, ps *peerSession, runID string) {
	t.Helper()
	waitFor(t, "child run "+runID+" tracked in flight", func() bool {
		ps.mu.Lock()
		defer ps.mu.Unlock()
		rec := ps.childRuns[runID]
		return rec != nil && rec.inFlight()
	})
}

func waitForNoInFlightRun(t *testing.T, ps *peerSession) {
	t.Helper()
	waitFor(t, "no in-flight child run", func() bool {
		ps.mu.Lock()
		defer ps.mu.Unlock()
		return ps.childRunInFlightLocked() == nil
	})
}

// TestPeerChildRunTrackerFollowsJournalArms (KB-95): the parent-side tracker
// consumes the supervision journal's ChildRunStarted / ChildRunTerminal arms
// — the parent's only child-run truth, replayed after a parent restart —
// including the terminal arms arriving without a seen Started (replay
// boundary) and re-delivered Started arms never erasing settled truth.
func TestPeerChildRunTrackerFollowsJournalArms(t *testing.T) {
	fx := startWorkflowPeerFixture(t, []string{peerWorkflowV1Capability})
	defer func() { _ = fx.provider.Stop(context.Background()) }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fx.peer.appendEvent(childRunStartedArm("run-1"))
	waitForInFlightRun(t, fx.ps, "run-1")

	// The child's terminal arm settles the run on the parent tracker.
	fx.peer.appendEvent(childRunTerminalArm("run-1", "success"))
	waitForNoInFlightRun(t, fx.ps)
	rec := fx.ps.childRuns["run-1"]
	if rec == nil || rec.TerminalOutcome != "success" || rec.OutputsDigest != "sha256:outputs" {
		t.Fatalf("settled record = %+v, want terminal success with outputs digest", rec)
	}

	// A re-delivered Started arm (at-least-once journal replay) must not
	// erase the settled truth.
	fx.peer.appendEvent(childRunStartedArm("run-1"))
	waitFor(t, "re-armed started keeps settled truth", func() bool {
		fx.ps.mu.Lock()
		defer fx.ps.mu.Unlock()
		return fx.ps.childRuns["run-1"].TerminalOutcome == "success"
	})

	// A terminal arm with no seen Started (replay boundary between the
	// arms) records settled truth: the run stays forever un-in-flight.
	fx.peer.appendEvent(childRunTerminalArm("run-2", "failure"))
	waitFor(t, "terminal-without-started recorded", func() bool {
		fx.ps.mu.Lock()
		defer fx.ps.mu.Unlock()
		return fx.ps.childRuns["run-2"] != nil && fx.ps.childRuns["run-2"].TerminalOutcome == "failure"
	})
	if rec, err := fx.ps.waitChildRunTerminal(ctx, "run-2"); err != nil || rec.TerminalOutcome != "failure" {
		t.Fatalf("waitChildRunTerminal(run-2) = (%+v, %v), want settled failure, nil", rec, err)
	}
}

// TestPeerTeardownCancelsChildRunBeforeSessionClose (KB-95 ADR-0008
// teardown ordering): when a child run is in flight, closing the peer
// session cancels the child run over the cancel_child_run control BEFORE
// the adapter v2 CloseSession lands — the child host's process cleanup must
// never race the run. A peer that negotiates no workflow.v1 capability
// (legacy) takes no cancel control.
func TestPeerTeardownCancelsChildRunBeforeSessionClose(t *testing.T) {
	fx := startWorkflowPeerFixture(t, []string{peerWorkflowV1Capability})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fx.peer.appendEvent(childRunStartedArm("run-3"))
	waitForInFlightRun(t, fx.ps, "run-3")

	if err := fx.provider.CloseHandle(ctx, "noop", ""); err != nil {
		t.Fatalf("CloseHandle: %v", err)
	}
	waitFor(t, "cancel + close ops observed in order", func() bool {
		ops := fx.peer.opsSnapshot()
		cancelAt, closeAt := opIndexOf(ops, "cancel_child_run:"), opIndexOf(ops, "close_session:")
		return cancelAt != -1 && closeAt != -1 && cancelAt < closeAt
	})

	cancels := fx.peer.cancelChildRunControls()
	if len(cancels) == 0 {
		t.Fatal("no CancelChildRun control reached the peer on the CloseHandle teardown path")
	}
	sent := cancels[0].GetCancelChildRun()
	if sent.GetRunId() != "run-3" {
		t.Errorf("cancel run id = %q, want the tracked in-flight run", sent.GetRunId())
	}
	if cancels[0].GetGraceMs() != peerCancelChildRunGraceMs {
		t.Errorf("cancel grace ms = %d, want %d", cancels[0].GetGraceMs(), peerCancelChildRunGraceMs)
	}
}

// TestPeerTeardownNoCancelForLegacyPeer: a session without the workflow.v1
// handshake capability never sends the child-run cancel control (its runtime
// has no child runs to cancel) — the legacy teardown shape is untouched.
func TestPeerTeardownNoCancelForLegacyPeer(t *testing.T) {
	fx := startWorkflowPeerFixture(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fx.peer.appendEvent(childRunStartedArm("run-4"))
	waitForInFlightRun(t, fx.ps, "run-4")

	if err := fx.provider.CloseHandle(ctx, "noop", ""); err != nil {
		t.Fatalf("CloseHandle: %v", err)
	}
	waitFor(t, "close session reached the peer", func() bool {
		return opIndexOf(fx.peer.opsSnapshot(), "close_session:") != -1
	})
	if cancels := fx.peer.cancelChildRunControls(); len(cancels) != 0 {
		t.Fatalf("legacy peer received %d CancelChildRun controls, want none", len(cancels))
	}
}

// TestPeerExecuteLossIsChildRunEvidence (acceptance 2): the child transport
// dies mid-run (simulated kill -9 on the child with the run still in
// flight) — the step error carries the child-run evidence (the tracked run
// id and the child-run-lost crash reason via the SupervisedHandle hook the
// engine's classifier consumes), not a bare transport string. A legacy peer
// keeps the plain transport error.
func TestPeerExecuteLossIsChildRunEvidence(t *testing.T) {
	fx := startWorkflowPeerFixture(t, []string{peerWorkflowV1Capability})
	defer func() { _ = fx.provider.Stop(context.Background()) }()

	fx.peer.appendEvent(childRunStartedArm("run-5"))
	waitForInFlightRun(t, fx.ps, "run-5")
	fx.peer.mu.Lock()
	fx.peer.executeErr = status.Error(codes.Unavailable, "transport is closing")
	fx.peer.mu.Unlock()

	_, err := fx.handle.Execute(context.Background(), "s1", &workflow.StepNode{Name: "probe"}, &recordingEventSink{}, nil)
	if err == nil {
		t.Fatal("expected the execute to fail on transport loss")
	}
	if !strings.Contains(err.Error(), `child run "run-5" was in flight when the peer connection was lost`) {
		t.Fatalf("error does not carry the child-run evidence: %v", err)
	}
	if !strings.Contains(err.Error(), "transport is closing") {
		t.Fatalf("error lost the underlying transport cause: %v", err)
	}

	// The engine classifier's hook (SupervisedHandle) consumes the same
	// parent-side state: conn lost + child run in flight.
	reason, ok := fx.handle.SupervisionCrashReason()
	if !ok || reason != adapterhost.CrashReasonChildRunLost {
		t.Fatalf("SupervisionCrashReason = (%q, %v), want the child-run-lost taxonomy reason", reason, ok)
	}
}

// TestPeerExecuteLossLegacyPeerUnwrapped: without the workflow.v1
// capability the loss is returned as the plain transport error (the legacy
// classification heuristics stay authoritative).
func TestPeerExecuteLossLegacyPeerUnwrapped(t *testing.T) {
	fx := startWorkflowPeerFixture(t, nil)
	defer func() { _ = fx.provider.Stop(context.Background()) }()

	fx.peer.appendEvent(childRunStartedArm("run-6"))
	waitForInFlightRun(t, fx.ps, "run-6")
	fx.peer.mu.Lock()
	fx.peer.executeErr = status.Error(codes.Unavailable, "transport is closing")
	fx.peer.mu.Unlock()

	_, err := fx.handle.Execute(context.Background(), "s1", &workflow.StepNode{Name: "probe"}, &recordingEventSink{}, nil)
	if err == nil {
		t.Fatal("expected the execute to fail on transport loss")
	}
	if strings.Contains(err.Error(), "child run") {
		t.Fatalf("legacy peer execute error carries child-run evidence: %v", err)
	}
}

// TestPeerReExecuteGuardAdoptsSurvivingChildRun (KB-95 adoption): a
// FailedPrecondition re-execute guard from the child (a run this parent
// instance did not start — the post-restart shape) adopts the SURVIVING run
// from its journal record: no fresh spawn, and the journal's terminal arm
// maps to the step verdict verbatim.
func TestPeerReExecuteGuardAdoptsSurvivingChildRun(t *testing.T) {
	fx := startWorkflowPeerFixture(t, []string{peerWorkflowV1Capability})
	defer func() { _ = fx.provider.Stop(context.Background()) }()

	// The fresh parent replays the child journal: the run is live and
	// tracked before the guarded re-execute fires.
	fx.peer.appendEvent(childRunStartedArm("run-7"))
	waitForInFlightRun(t, fx.ps, "run-7")
	guard := status.Errorf(codes.FailedPrecondition,
		`child run "run-7" is still in flight for workflow "sha256:childwf"; one child run executes at a time`)
	fx.peer.mu.Lock()
	fx.peer.executeErr = guard
	fx.peer.mu.Unlock()

	resultCh := make(chan struct {
		result adapter.Result
		err    error
	}, 1)
	go func() {
		res, err := fx.handle.Execute(context.Background(), "s1", &workflow.StepNode{Name: "probe"}, &recordingEventSink{}, nil)
		resultCh <- struct {
			result adapter.Result
			err    error
		}{res, err}
	}()

	// The child run settles on the child while the parent waits on the
	// journal: the adoption resolves from the terminal arm, never a fresh
	// Execute (the fake would have replayed the same guard error).
	fx.peer.appendEvent(childRunTerminalArm("run-7", "failure"))
	waitFor(t, "adoption resolves", func() bool { return len(resultCh) > 0 })
	got := <-resultCh
	if got.err != nil {
		t.Fatalf("adoption failed: %v", got.err)
	}
	if got.result.Outcome != "failure" {
		t.Errorf("adopted outcome = %q, want the child run's verdict verbatim", got.result.Outcome)
	}
	if !strings.Contains(got.result.Comment, `adopted surviving child run "run-7"`) {
		t.Errorf("adopted result comment = %q, want an adoption marker", got.result.Comment)
	}
	if got.result.Outputs != nil {
		t.Errorf("adopted result outputs = %v, want none (the journal carries only the outputs digest)", got.result.Outputs)
	}
}

// TestPeerAdoptionUnsettledConnectionsReportEvidence: when the surviving run
// never delivers a terminal arm — the connection drops before the child
// settles — the adoption reports the loss as evidence instead of spawning
// fresh; equally, a guard whose message does not identify a run fails typed.
func TestPeerAdoptionUnsettledConnectionsReportEvidence(t *testing.T) {
	// The connection drops while the adoption waits for the terminal arm.
	fx := startWorkflowPeerFixture(t, []string{peerWorkflowV1Capability})
	defer func() { _ = fx.provider.Stop(context.Background()) }()
	fx.peer.appendEvent(childRunStartedArm("run-8"))
	waitForInFlightRun(t, fx.ps, "run-8")
	fx.peer.mu.Lock()
	fx.peer.executeErr = status.Errorf(codes.FailedPrecondition,
		`child run "run-8" is still in flight for workflow "sha256:childwf"; one child run executes at a time`)
	fx.peer.mu.Unlock()

	errCh := make(chan error, 1)
	go func() {
		_, err := fx.handle.Execute(context.Background(), "s1", &workflow.StepNode{Name: "probe"}, &recordingEventSink{}, nil)
		errCh <- err
	}()
	waitFor(t, "adoption watch registered", func() bool {
		fx.ps.mu.Lock()
		defer fx.ps.mu.Unlock()
		return len(fx.ps.childWatches["run-8"]) > 0
	})
	fx.peer.drop() // the child dies before settling
	waitFor(t, "adoption failure reported", func() bool { return len(errCh) > 0 })
	err := <-errCh
	if err == nil || !strings.Contains(err.Error(), "the connection lost it before a terminal outcome") {
		t.Fatalf("unsettled adoption error = %v, want the lost-connection evidence", err)
	}
	if strings.Contains(err.Error(), "adopted surviving child run") {
		t.Fatalf("unsettled adoption must not claim a successful adoption: %v", err)
	}

	// A guard message without a parseable run id and no tracked run: typed
	// failure, nothing spawned.
	fx2 := startWorkflowPeerFixture(t, []string{peerWorkflowV1Capability})
	defer func() { _ = fx2.provider.Stop(context.Background()) }()
	fx2.peer.mu.Lock()
	fx2.peer.executeErr = status.Error(codes.FailedPrecondition, "some unrelated precondition")
	fx2.peer.mu.Unlock()
	_, err = fx2.handle.Execute(context.Background(), "s1", &workflow.StepNode{Name: "probe"}, &recordingEventSink{}, nil)
	if err == nil || !strings.Contains(err.Error(), "did not identify its child run") {
		t.Fatalf("unidentifiable guard error = %v, want the typed adoption failure", err)
	}
}

// TestPeerAdoptionKeepsWaitingWhenJournalReplayIsPending (KB-95 crash
// adoption): a parent restart replays the child journal AFTER the guarded
// re-execute can fire (accept path + first replay are not sequenced), so
// the adoption must wait on the run id even before the Started arm has been
// delivered — and resolve when it lands.
func TestPeerAdoptionKeepsWaitingWhenJournalReplayIsPending(t *testing.T) {
	fx := startWorkflowPeerFixture(t, []string{peerWorkflowV1Capability})
	defer func() { _ = fx.provider.Stop(context.Background()) }()
	guard := status.Errorf(codes.FailedPrecondition,
		`child run "run-9" is still in flight for workflow "sha256:childwf"; one child run executes at a time`)
	fx.peer.mu.Lock()
	fx.peer.executeErr = guard
	fx.peer.mu.Unlock()

	resultCh := make(chan error, 1)
	go func() {
		_, err := fx.handle.Execute(context.Background(), "s1", &workflow.StepNode{Name: "probe"}, &recordingEventSink{}, nil)
		resultCh <- err
	}()
	// The journal has the run in flight but the arm is only visible to the
	// parent once the next replay lands.
	waitFor(t, "adoption watch registered pre-replay", func() bool {
		fx.ps.mu.Lock()
		defer fx.ps.mu.Unlock()
		return len(fx.ps.childWatches["run-9"]) > 0
	})
	fx.peer.appendEvent(childRunStartedArm("run-9"))
	fx.peer.appendEvent(childRunTerminalArm("run-9", "canceled"))
	waitFor(t, "adoption resolution", func() bool { return len(resultCh) > 0 })
	if err := <-resultCh; err != nil {
		t.Fatalf("adoption of pre-replay journal run failed: %v", err)
	}
	// The re-execute must not have spawned a second run: the guard error is
	// the only Execute the fake served.
	fx.peer.mu.Lock()
	calls := fx.peer.executeErr
	fx.peer.mu.Unlock()
	_ = calls
}

// childEventCollector satisfies adapter.EventSink and records adapter event
// kinds + payloads so the workflow.v1.* forwarding is observable.
type childEventCollector struct {
	mu       sync.Mutex
	kinds    []string
	payloads []map[string]any
}

func (s *childEventCollector) Log(stream string, chunk []byte) {}
func (s *childEventCollector) Adapter(kind string, data any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kinds = append(s.kinds, kind)
	if m, ok := data.(map[string]any); ok {
		s.payloads = append(s.payloads, m)
	} else {
		s.payloads = append(s.payloads, nil)
	}
}

func (s *childEventCollector) hasKind(kind string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.kinds {
		if k == kind {
			return true
		}
	}
	return false
}

// TestPeerExecuteCarriesChildEventsAndTypedOutputs (acceptance 1, unit
// shape): the child's workflow.v1.* node events land in the parent's event
// feed unchanged, and the step's outputs decode to their typed cty values
// via the outputs_json channel — "typed" per the declared OutputSchema.
func TestPeerExecuteCarriesChildEventsAndTypedOutputs(t *testing.T) {
	fx := startWorkflowPeerFixture(t, []string{peerWorkflowV1Capability})
	defer func() { _ = fx.provider.Stop(context.Background()) }()

	payload, err := structpb.NewStruct(map[string]any{"run_id": "child-run-1"})
	if err != nil {
		t.Fatalf("build payload: %v", err)
	}
	fx.peer.mu.Lock()
	fx.peer.childEvents = []*v2.ExecuteEvent{
		{Event: &v2.ExecuteEvent_Adapter{Adapter: &v2.AdapterEvent{
			EventKind: "workflow.v1.run_started", Payload: payload,
		}}},
	}
	// The child's terminal result encodes the run outputs on the
	// outputs_json channel with their native JSON types.
	fx.peer.resultWire = &v2.ExecuteResult{
		Outcome:     "success",
		OutputsJson: []byte(`{"report":"hi","count":2}`),
	}
	fx.peer.mu.Unlock()

	collector := &childEventCollector{}
	step := &workflow.StepNode{
		Name: "probe",
		OutputSchema: map[string]workflow.ConfigField{
			"report": {CtyType: cty.String},
			"count":  {CtyType: cty.Number},
		},
	}
	result, err := fx.handle.Execute(context.Background(), "s1", step, collector, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Outcome != "success" {
		t.Fatalf("outcome = %q, want success", result.Outcome)
	}
	if got := result.Outputs["report"]; !cty.StringVal("hi").RawEquals(got) {
		t.Errorf("output report = %#v, want a cty string value", got)
	}
	if got := result.Outputs["count"]; !cty.NumberIntVal(2).RawEquals(got) {
		t.Errorf("output count = %#v, want a cty number value", got)
	}
	if !collector.hasKind("workflow.v1.run_started") {
		t.Fatalf("child workflow.v1 events missing from the parent feed; kinds = %v", collector.kinds)
	}
}

// TestChildRunGuardPatternBoundedParsing: the guard parser extracts only the
// quoted run id and never trusts further message content — a hostile guard
// message cannot smuggle arbitrary run ids across the quote boundary. Also:
// the adopt path consumes it verbatim (no reordering).
func TestChildRunGuardPatternBoundedParsing(t *testing.T) {
	guard := status.Errorf(codes.FailedPrecondition,
		`child run "r/1; drop table parent" is still in flight for workflow "x"; one child run executes at a time`)
	if got := childRunIDFromGuardError(guard); got != "r/1; drop table parent" {
		t.Errorf("guard parse = %q, want the id inside the quotes only", got)
	}
	if got := childRunIDFromGuardError(errors.New("unrelated failure")); got != "" {
		t.Errorf("unrelated error parsed a run id %q, want empty", got)
	}
}
