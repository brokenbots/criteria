package engine

// cri271_repro_test.go — regression tests for CRI-271: a live adapter session
// dying mid-run (shim Canceled / plugin stdio EOF after long LLM streaming
// turns) must not silently tear down the run's bookkeeping.
//
// The incident: the copilot adapter's session died mid-turn with
// `rpc error: code = Canceled desc = grpc: the client connection is closing`.
// Under the default on_crash=fail policy the session stayed registered but
// dead, so the follow-on bookkeeping steps (comment_handler_failed,
// set_review_state) replayed the crash error on the corpse and the run died
// without completing its state writes. The engine now re-opens a crashed
// session before a follow-on step executes on it, so the step runs on a live
// session; the crash itself is diagnosed with a named reason and the idle
// window since the adapter's last event.

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// cri271CrashErr is the exact crash signature from the CRI-271 report
// (runs 17fc5860 / f5eb0f32, copilot.developer, session shim Canceled).
const cri271CrashErr = "rpc error: code = Canceled desc = grpc: the client connection is closing"

// cri271Workflow mirrors the incident tail: develop loses its session
// mid-turn, routes via its declared failure outcome to the bookkeeping steps
// (comment_handler_failed, set_review_state), then reaches the terminal
// awaiting_human state.
const cri271Workflow = `
workflow {
  name = "cri271"
  version = "0.1"
  initial_state = "develop"
  target_state  = "awaiting_human"
}
step "develop" {
  target = adapter.fake
  outcome "success" { next = state.awaiting_human }
  outcome "failure" { next = step.comment_handler_failed }
}
step "comment_handler_failed" {
  target = adapter.fake
  outcome "success" { next = step.set_review_state }
}
step "set_review_state" {
  target = adapter.fake
  outcome "success" { next = state.awaiting_human }
}
state "awaiting_human" {
  terminal = true
  success  = true
}`

// cri271Adapter models an adapter whose session dies mid-turn (CRI-271): the
// crashing step's first Execute loses the shim connection, and every Execute
// on the dead session replays the same transport error until the session is
// re-opened (a new OpenSession call stands for the fresh shim connection).
type cri271Adapter struct {
	*fakeAdapter
	crashStep string

	mu       sync.Mutex
	opens    []string
	executes []string
	live     bool
}

func newCri271Adapter(crashStep string) *cri271Adapter {
	return &cri271Adapter{fakeAdapter: &fakeAdapter{name: "fake"}, crashStep: crashStep}
}

func (a *cri271Adapter) OpenSession(_ context.Context, name string, _, _ map[string]string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.opens = append(a.opens, name)
	a.live = true
	return nil
}

func (a *cri271Adapter) Execute(_ context.Context, name string, step *workflow.StepNode, _ adapter.EventSink) (adapter.Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	stepName := ""
	if step != nil {
		stepName = step.Name
	}
	a.executes = append(a.executes, stepName)
	if !a.live {
		// The session is dead; every Execute on it replays the transport
		// error, exactly as the production shim did.
		return adapter.Result{}, errors.New(cri271CrashErr)
	}
	if stepName == a.crashStep {
		// The turn dies mid-flight: the shim connection drops.
		a.live = false
		return adapter.Result{}, errors.New(cri271CrashErr)
	}
	return adapter.Result{Outcome: "success"}, nil
}

func (a *cri271Adapter) callLog() (opens, executes []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.opens...), append([]string(nil), a.executes...)
}

// cri271Sink extends fakeSink with a per-step adapter event recorder so tests
// can assert the diagnosable session.crash event adapterhost emits.
type cri271Sink struct {
	*fakeSink

	mu     sync.Mutex
	events []cri271Event
}

type cri271Event struct {
	kind string
	data map[string]any
}

func (s *cri271Sink) StepEventSink(string) adapter.EventSink {
	return &cri271EventRecorder{parent: s}
}

type cri271EventRecorder struct {
	parent *cri271Sink
}

func (r *cri271EventRecorder) Log(string, []byte) {}
func (r *cri271EventRecorder) Adapter(kind string, data any) {
	payload, _ := data.(map[string]any)
	r.parent.mu.Lock()
	defer r.parent.mu.Unlock()
	r.parent.events = append(r.parent.events, cri271Event{kind: kind, data: payload})
}

func (s *cri271Sink) recorded(kind string) (map[string]any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, evt := range s.events {
		if evt.kind == kind {
			return evt.data, true
		}
	}
	return nil, false
}

// cri271NewLoader registers the adapter under both the bare type name and the
// dotted reference the compiled graph uses (same shape as cri130NewLoader).
func cri271NewLoader(p adapterhost.Handle) *fakeLoader {
	return &fakeLoader{adapters: map[string]adapterhost.Handle{
		"fake":         p,
		"fake.default": p,
	}}
}

// TestCRI271_FunctionalCrashReopensSessionForBookkeeping is the CRI-271
// regression test: the develop step's session dies mid-turn, the run routes
// via its declared failure outcome, and the follow-on bookkeeping steps
// (comment_handler_failed, set_review_state) execute on the re-opened live
// session so the run's bookkeeping completes and the run terminates at
// awaiting_human instead of dying silently.
func TestCRI271_FunctionalCrashReopensSessionForBookkeeping(t *testing.T) {
	g := compile(t, cri271Workflow)
	p := newCri271Adapter("develop")
	sink := &cri271Sink{fakeSink: &fakeSink{}}
	if err := NewTestEngine(g, cri271NewLoader(p), sink).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.terminal != "awaiting_human" || sink.terminalOK {
		t.Errorf("terminal state %q success=%v; want awaiting_human/false (CRI-274: the crashed develop step is a real failure; reopened bookkeeping completes but the run must not report success)", sink.terminal, sink.terminalOK)
	}

	opens, executes := p.callLog()
	// Exactly one re-open (the second OpenSession) sits between the crashed
	// develop turn and the bookkeeping steps; no extra sessions were spawned.
	wantOpens := []string{"fake.default", "fake.default"}
	if len(opens) != len(wantOpens) {
		t.Fatalf("OpenSession calls: %v; want %v", opens, wantOpens)
	}
	for i, want := range wantOpens {
		if opens[i] != want {
			t.Errorf("OpenSession calls[%d] = %q; want %q", i, opens[i], want)
		}
	}
	wantExecutes := []string{"develop", "comment_handler_failed", "set_review_state"}
	if len(executes) != len(wantExecutes) {
		t.Fatalf("Execute calls: %v; want %v", executes, wantExecutes)
	}
	for i, want := range wantExecutes {
		if executes[i] != want {
			t.Errorf("Execute calls[%d] = %q; want %q", i, executes[i], want)
		}
	}

	// The crash is diagnosable: the session.crash event names the session and
	// the classified crash reason (the shim connection drop), and carries the
	// idle window since the adapter's last event.
	data, ok := sink.recorded("session.crash")
	if !ok {
		t.Fatal("expected a session.crash event")
	}
	if data["session"] != "fake.default" {
		t.Errorf("session.crash session=%v; want fake.default", data["session"])
	}
	if reason, _ := data["crash_reason"].(string); reason == "" {
		t.Error("expected a named crash_reason on the session.crash event")
	} else if reason != "gRPC client transport closed (adapter or shim closed the connection)" {
		t.Errorf("crash_reason=%q; want the named shim-connection-drop cause", reason)
	}
	if idle, ok := data["idle_since_last_event"].(string); !ok || idle == "" {
		t.Errorf("expected idle_since_last_event on the session.crash event, got %v", data["idle_since_last_event"])
	}
}

// TestCRI271_ReopenDisabledKeepsPreCRI271Behavior pins the kill switch: with
// CRITERIA_SESSION_CRASH_REOPEN disabled, a crashed session stays dead, the
// comment step is still suppressed as best-effort (CRI-130), and the run
// fails when the follow-on state write replays the crash error — the exact
// pre-CRI-271 behavior the incident reported.
func TestCRI271_ReopenDisabledKeepsPreCRI271Behavior(t *testing.T) {
	t.Setenv("CRITERIA_SESSION_CRASH_REOPEN", "0")
	g := compile(t, cri271Workflow)
	p := newCri271Adapter("develop")
	sink := &fakeSink{}
	if err := NewTestEngine(g, cri271NewLoader(p), sink).Run(context.Background()); err == nil {
		t.Fatal("expected the run to fail with re-open disabled")
	}
	if sink.failure == "" {
		t.Error("expected OnRunFailed with re-open disabled")
	}
	opens, executes := p.callLog()
	// Only the initial open: no session was re-opened.
	if len(opens) != 1 {
		t.Errorf("OpenSession calls: %v; want exactly the initial open", opens)
	}
	// The bookkeeping steps were still attempted on the dead session before
	// the run failed: the comment step was suppressed (CRI-130), the state
	// write was not.
	wantExecutes := []string{"develop", "comment_handler_failed", "set_review_state"}
	if len(executes) != len(wantExecutes) {
		t.Fatalf("Execute calls: %v; want %v", executes, wantExecutes)
	}
}
