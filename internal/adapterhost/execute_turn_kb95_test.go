package adapterhost

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/workflow"

	"github.com/zclconf/go-cty/cty"
)

// kb95WorkflowStub is a minimal Handle implementing a workflow.v1
// criteria-as-adapter child (ADR-0008): same Handle surface as the other
// tool-call test fakes, plus optional SupervisedHandle classification
// delivery and an execution counter so tests can assert that a guarded
// caller never reached the child.
type kb95WorkflowStub struct {
	mu             sync.Mutex
	caps           []string
	execErr        error
	executes       int
	crashReason    string
	crashDelivered bool
}

func (s *kb95WorkflowStub) Info(_ context.Context) (Info, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	caps := s.caps
	if caps == nil {
		caps = []string{"execute"}
	}
	return Info{
		Capabilities: caps,
		AdapterInfo: workflow.AdapterInfo{
			InputSchema: map[string]workflow.ConfigField{
				"task": {Required: true},
			},
			OutputSchema: map[string]workflow.ConfigField{
				"report": {CtyType: cty.String},
			},
		},
	}, nil
}
func (s *kb95WorkflowStub) OpenSession(_ context.Context, _ string, _, _ map[string]string) error {
	return nil
}
func (s *kb95WorkflowStub) CloseSession(_ context.Context, _ string) error { return nil }
func (s *kb95WorkflowStub) Kill()                                          {}
func (s *kb95WorkflowStub) Pause(context.Context, string) error            { return nil }
func (s *kb95WorkflowStub) Resume(context.Context, string) error           { return nil }
func (s *kb95WorkflowStub) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}
func (s *kb95WorkflowStub) Snapshot(context.Context, string) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{}, nil
}
func (s *kb95WorkflowStub) Restore(context.Context, string, []byte, uint32) error { return nil }

func (s *kb95WorkflowStub) Execute(ctx context.Context, _ string, _ *workflow.StepNode, _ adapter.EventSink, _ *v2.ExecutionRejection) (adapter.Result, error) {
	s.mu.Lock()
	s.executes++
	s.mu.Unlock()
	if s.execErr != nil {
		return adapter.Result{Outcome: "failure"}, s.execErr
	}
	return adapter.Result{Outcome: "success", Outputs: map[string]cty.Value{"report": cty.StringVal("done")}}, nil
}

func (s *kb95WorkflowStub) executedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.executes
}

// SupervisionCrashReason implements SupervisedHandle: the peer supervision
// journal delivered the child classification verbatim (ADR-0007).
func (s *kb95WorkflowStub) SupervisionCrashReason() (string, bool) {
	return s.crashReason, s.crashDelivered
}

func newKB95Manager(t *testing.T, child *kb95WorkflowStub, mx *kb95WorkflowStub, plain *kb95WorkflowStub) *SessionManager {
	t.Helper()
	loader := NewLoaderWithDiscovery(func(string) (string, error) { return "", nil })
	loader.RegisterBuiltin("wfchild", func() Handle { return child })
	loader.RegisterBuiltin("wfmx", func() Handle { return mx })
	loader.RegisterBuiltin("plain", func() Handle { return plain })
	sm := NewSessionManager(loader)
	sm.SetGraph(&workflow.FSMGraph{
		Adapters: map[string]*workflow.AdapterNode{
			"wf.child":       {Type: "wfchild", Name: "child"},
			"wf.mx":          {Type: "wfmx", Name: "mx"},
			"plain.instance": {Type: "plain", Name: "instance"},
		},
	})
	return sm
}

func kb95Step(ref string) *workflow.StepNode {
	return &workflow.StepNode{
		Name:       "child",
		AdapterRef: ref,
		Outcomes:   map[string]*workflow.CompiledOutcome{"success": {Name: "success"}},
	}
}

// TestSession_WorkflowV1ExecuteTurnGate_KB95: the re-Execute guard (ADR-0008,
// KB-95). On a busy workflow.v1 session — i.e. its child run is in flight
// holding the execute turn — a second Execute fails closed with the typed
// ErrChildRunInFlight: never queued, turn never consumed. Everything else
// keeps the KB-155 behavior: plain sessions queue, multiplexable sessions
// (including workflow.v1 + concurrent_execute) skip the gate, and a free
// workflow.v1 session acquires normally.
func TestSession_WorkflowV1ExecuteTurnGate_KB95(t *testing.T) {
	child := &kb95WorkflowStub{caps: []string{"execute", "workflow.v1"}}
	mx := &kb95WorkflowStub{caps: []string{"execute", "workflow.v1", concurrentExecuteCapability}}
	plain := &kb95WorkflowStub{}
	sm := newKB95Manager(t, child, mx, plain)
	ctx := context.Background()
	for _, sess := range []struct{ name, adapter string }{
		{"wf.child", "wfchild"}, {"wf.mx", "wfmx"}, {"plain.instance", "plain"},
	} {
		if err := sm.Open(ctx, sess.name, sess.adapter, "", nil, nil); err != nil {
			t.Fatalf("Open %s: %v", sess.name, err)
		}
		defer func() { _ = sm.Close(context.Background(), sess.name) }()
	}

	// Plain regression (KB-155 unchanged): busy → queue → deadline.
	plainSess, err := sm.lookup("plain.instance")
	if err != nil {
		t.Fatalf("lookup plain: %v", err)
	}
	if queued, gateErr := sm.acquireExecuteTurn(ctx, plainSess); gateErr != nil || queued {
		t.Fatalf("plain first acquire: queued=%v err=%v", queued, gateErr)
	}
	plainCtx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	queued, gateErr := sm.acquireExecuteTurn(plainCtx, plainSess)
	if !errors.Is(gateErr, context.DeadlineExceeded) || !queued {
		t.Errorf("plain queued acquire: queued=%v err=%v, want DeadlineExceeded queued", queued, gateErr)
	}
	sm.releaseExecuteTurn(plainSess)

	// workflow.v1 busy → typed fail-closed error, never queued and never
	// queued-then-timed-out.
	wfSess, err := sm.lookup("wf.child")
	if err != nil {
		t.Fatalf("lookup wf: %v", err)
	}
	// The in-flight child run holds the turn (the parent's first Execute).
	if queued, gateErr := sm.acquireExecuteTurn(ctx, wfSess); gateErr != nil || queued {
		t.Fatalf("wf in-flight acquire: queued=%v err=%v", queued, gateErr)
	}
	againQueued, gateErr := sm.acquireExecuteTurn(ctx, wfSess)
	var guard *ErrChildRunInFlight
	if !errors.As(gateErr, &guard) || againQueued {
		t.Fatalf("guarded re-acquire: queued=%v err=%v, want *ErrChildRunInFlight not queued", againQueued, gateErr)
	}
	if guard.Session != "wf.child" {
		t.Errorf("guard.Session = %q, want wf.child", guard.Session)
	}
	// The failed guard caller took no turn: the in-flight holder releases
	// once and the next acquire is immediate.
	sm.releaseExecuteTurn(wfSess)
	if queued, gateErr := sm.acquireExecuteTurn(ctx, wfSess); gateErr != nil || queued {
		t.Errorf("acquire after in-flight release: queued=%v err=%v, want immediate (guard never consumed a turn)", queued, gateErr)
	}
	sm.releaseExecuteTurn(wfSess)

	// Free workflow.v1 session acquires normally (single child run at a time
	// is the contract, not zero).
	if queued, gateErr := sm.acquireExecuteTurn(ctx, wfSess); gateErr != nil || queued {
		t.Errorf("wf free acquire: queued=%v err=%v", queued, gateErr)
	}
	sm.releaseExecuteTurn(wfSess)

	// Multiplexable precedence (documented gate ordering): concurrent_execute
	// short-circuits before the workflow.v1 guard, so a multiplexable
	// workflow.v1 session keeps its multiplexed posture. The gate skips
	// entirely — two acquires overlap with no release.
	mxSess, err := sm.lookup("wf.mx")
	if err != nil {
		t.Fatalf("lookup mx: %v", err)
	}
	for i := 0; i < 2; i++ {
		if queued, gateErr := sm.acquireExecuteTurn(ctx, mxSess); gateErr != nil || queued {
			t.Errorf("mx acquire %d: queued=%v err=%v, want gate skipped", i+1, queued, gateErr)
		}
	}
}

// TestSession_WorkflowV1GuardIntegration_KB95: through the manager's public
// Execute, a re-Execute on a workflow.v1 session while its child run is in
// flight returns the typed guard error and the child adapter saw zero
// executions — never queued, never double-run.
func TestSession_WorkflowV1GuardIntegration_KB95(t *testing.T) {
	child := &kb95WorkflowStub{caps: []string{"execute", "workflow.v1"}}
	sm := newKB95Manager(t, child, &kb95WorkflowStub{}, &kb95WorkflowStub{})
	ctx := context.Background()
	if err := sm.Open(ctx, "wf.child", "wfchild", "", nil, nil); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = sm.Close(context.Background(), "wf.child") }()

	// Hold the turn the way the in-flight child run's Execute holds it.
	sess, err := sm.lookup("wf.child")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if _, gateErr := sm.acquireExecuteTurn(ctx, sess); gateErr != nil {
		t.Fatalf("acquire: %v", gateErr)
	}

	_, execErr := sm.Execute(ctx, "wf.child", kb95Step("wf.child"), &adapterEventCollector{}, nil)
	var guard *ErrChildRunInFlight
	if !errors.As(execErr, &guard) {
		t.Fatalf("Execute err = %v, want *ErrChildRunInFlight", execErr)
	}
	if got := child.executedCount(); got != 0 {
		t.Errorf("child executed %d times during guard rejection, want 0", got)
	}
	sm.releaseExecuteTurn(sess)

	// After the child run settles (turn released), Execute works normally.
	res, execErr := sm.Execute(ctx, "wf.child", kb95Step("wf.child"), &adapterEventCollector{}, nil)
	if execErr != nil {
		t.Fatalf("Execute after settle: %v", execErr)
	}
	if res.Outcome != "success" {
		t.Errorf("outcome = %q, want success", res.Outcome)
	}
}

// TestSession_WorkflowV1CrashAdoption_KB95: a workflow.v1 Execute that dies
// on the phone-home transport fails with the child's CrashClassified
// evidence (ADR-0007 wire fact delivered through SupervisedHandle, KB-95
// adoption semantics) — a typed SessionCrashError carrying
// CrashReasonChildRunLost, surfaced verbatim on the session.crash sink event,
// not a raw transport-error string.
func TestSession_WorkflowV1CrashAdoption_KB95(t *testing.T) {
	child := &kb95WorkflowStub{
		caps:           []string{"execute", "workflow.v1"},
		execErr:        status.Error(codes.Unavailable, "connection lost"),
		crashReason:    CrashReasonChildRunLost,
		crashDelivered: true,
	}
	sm := newKB95Manager(t, child, &kb95WorkflowStub{}, &kb95WorkflowStub{})
	ctx := context.Background()
	if err := sm.Open(ctx, "wf.child", "wfchild", "", nil, nil); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = sm.Close(context.Background(), "wf.child") }()

	sink := &adapterEventCollector{}
	_, execErr := sm.Execute(ctx, "wf.child", kb95Step("wf.child"), sink, nil)
	var crash *SessionCrashError
	if !errors.As(execErr, &crash) {
		t.Fatalf("Execute err = %v, want *SessionCrashError", execErr)
	}
	if crash.Session != "wf.child" {
		t.Errorf("crash.Session = %q, want wf.child", crash.Session)
	}
	// The underlying error keeps the child evidence for the engine's message.
	if !strings.Contains(crash.Error(), "connection lost") {
		t.Errorf("crash error = %q, want it to carry the child evidence", crash.Error())
	}

	evt, ok := sink.first("session.crash")
	if !ok {
		t.Fatal("no session.crash event")
	}
	if got, _ := evt["crash_reason"].(string); got != CrashReasonChildRunLost {
		t.Errorf("crash_reason = %q, want %q", got, CrashReasonChildRunLost)
	}
}
