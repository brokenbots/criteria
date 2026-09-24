package engine

// prompt_live_test.go — end-to-end prompt-delivery tests against real
// out-of-process adapter binaries (CRI-259, ADR-0006 D1–D5, D9).
//
// TestEngineAgentPromptLiveDelivery runs the promptable testdata fixture
// (declares supports_prompt=true), injects an AgentPrompt while the step's
// Execute call is in flight, and asserts the host→adapter v2 call sequence
// gains exactly one Prompt call plus exactly one AgentPromptInjected event
// on the run's event stream between StepEntered and StepOutcome.
//
// TestEngineAgentPromptUnsupportedAdapter runs the same shape against the
// noop fixture (supports_prompt absent) and asserts the capability gate
// short-circuits: no injection event, typed UNSUPPORTED_ADAPTER delivery
// failure. The zero-Prompt-RPC-on-the-wire assertion itself lives at the
// session level in TestNoopAdapterPromptUnsupported (the gate returns
// ErrPromptUnsupportedAdapter without ever dispatching PromptMethodDesc —
// an attempted RPC would surface as a gRPC Unimplemented error instead).

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/brokenbots/criteria/internal/adapterhost"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

func buildPromptableAdapter(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve caller path")
	}
	moduleRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	binary := filepath.Join(t.TempDir(), "criteria-adapter-promptable")

	cmd := exec.Command("go", "build", "-o", binary, "./internal/adapter/conformance/testdata/promptable")
	cmd.Dir = moduleRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build promptable adapter: %v\n%s", err, string(out))
	}
	return binary
}

type promptInjectedRecord struct {
	step        string
	sessionID   string
	prompt      string
	caller      string
	deliveredAt time.Time
}

// promptInjectSink injects AgentPrompt messages into the run's channel once
// the injection trigger fires, and records prompt-injection events plus the
// StepEntered/StepOutcome marker order for the addressed step.
type promptInjectSink struct {
	fakeSink
	promptCh chan *pb.AgentPrompt
	runID    string
	owner    string

	// trigger, when set, gates injection; otherwise the sink waits settle.
	callLog string
	settle  time.Duration

	// iterationGate, when set, is invoked from OnStepIterationStarted so a
	// test can act on the run loop in the window between two iterations of
	// the same step (after the previous attempt loop's deferred endStep and
	// before the next iteration's beginExecute).
	iterationGate func(index int)
	// sendOnTransition, when set, is sent once on the first step
	// transition: the attempt loop's deferred endStep has run by then, so
	// the router sees a completed step. Pins the completed-step
	// NO_ACTIVE_SESSION branch.
	sendOnTransition   *pb.AgentPrompt
	transitionSendOnce sync.Once

	mu           sync.Mutex
	markers      []string
	injected     []promptInjectedRecord
	sendFailures int
}

func (s *promptInjectSink) OnStepEntered(step, _ string, _ int) {
	s.fakeSink.OnStepEntered(step, "", 0)
	s.mu.Lock()
	s.markers = append(s.markers, "entered:"+step)
	s.mu.Unlock()
}

func (s *promptInjectSink) OnStepOutcome(step, _ string, _ time.Duration, _ error) {
	s.mu.Lock()
	s.markers = append(s.markers, "outcome:"+step)
	s.mu.Unlock()
}

func (s *promptInjectSink) OnStepTransition(from, to, via string) {
	s.fakeSink.OnStepTransition(from, to, via)
	s.mu.Lock()
	msg := s.sendOnTransition
	s.mu.Unlock()
	if msg == nil {
		return
	}
	s.transitionSendOnce.Do(func() {
		select {
		case s.promptCh <- msg:
		case <-time.After(10 * time.Second):
			s.mu.Lock()
			s.sendFailures++
			s.mu.Unlock()
		}
	})
}

func (s *promptInjectSink) OnStepIterationStarted(step string, index int, item string, anyFailed bool) {
	s.fakeSink.OnStepIterationStarted(step, index, item, anyFailed)
	if s.iterationGate != nil {
		s.iterationGate(index)
	}
}

func (s *promptInjectSink) OnAgentPromptInjected(step, sessionID, prompt, caller string, deliveredAt time.Time) {
	s.mu.Lock()
	s.markers = append(s.markers, "injected")
	s.injected = append(s.injected, promptInjectedRecord{step, sessionID, prompt, caller, deliveredAt})
	s.mu.Unlock()
}

// armForStep launches a goroutine that waits for the delivery window to be
// open, then sends the provided prompts in order. The wait for the
// promptable fixture is the adapter's own call log (Execute in flight);
// otherwise a settle delay wide of the configured step delay is used.
func (s *promptInjectSink) armForStep(t *testing.T, prompts ...*pb.AgentPrompt) {
	t.Helper()
	armed := make(chan struct{})
	go func() {
		defer close(armed)
		if s.callLog != "" {
			deadline := time.Now().Add(5 * time.Second)
			for {
				b, err := os.ReadFile(s.callLog)
				if err == nil && strings.Contains(string(b), "execute\n") {
					break
				}
				if time.Now().After(deadline) {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		} else {
			time.Sleep(s.settle)
		}
		for _, msg := range prompts {
			select {
			case s.promptCh <- msg:
			case <-time.After(10 * time.Second):
				s.mu.Lock()
				s.sendFailures++
				s.mu.Unlock()
				return
			}
		}
	}()
	t.Cleanup(func() { <-armed })
}

func (s *promptInjectSink) promptInjectionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.injected)
}

func (s *promptInjectSink) firstInjection() (promptInjectedRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.injected) == 0 {
		return promptInjectedRecord{}, false
	}
	return s.injected[0], true
}

func (s *promptInjectSink) markerIndex(marker string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, m := range s.markers {
		if m == marker {
			return i
		}
	}
	return -1
}

func (s *promptInjectSink) sendFailureCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sendFailures
}

func promptLiveGraph(stepInput string) string {
	// Step "a" carries the live session; the trailing duration wait keeps the
	// run loop (and the prompt router) alive after "a" completes so the
	// completed-step NO_ACTIVE_SESSION probe is observable without a second
	// adapter Execute muddying the call log.
	return `
workflow {
  name = "prompt-live"
  version = "0.1"
  initial_state = "a"
  target_state  = "done"
}
step "a" {
  target = adapter.promptable.default
  input { ` + stepInput + ` }
  outcome "success" { next = wait.settle }
}
wait "settle" {
  duration = "1500ms"
  outcome "elapsed" { next = step.done }
}
state "done" { terminal = true }
adapter "promptable" "default" {}`
}

// TestEngineAgentPromptLiveDelivery exercises the full criteria-side delivery
// seam against the promptable testdata fixture: control-channel dispatch,
// run plumbing, host capability gate (positive), adapter Prompt RPC, and the
// first-class AgentPromptInjected event (CRI-259 exit criteria 2, 3, 8).
func TestEngineAgentPromptLiveDelivery(t *testing.T) {
	adapterBin := buildPromptableAdapter(t)
	callLog := filepath.Join(t.TempDir(), "promptable-calls.log")
	t.Setenv("PROMPTABLE_CALL_LOG", callLog)

	loader := adapterhost.NewLoaderWithDiscovery(func(string) (string, error) {
		return adapterBin, nil
	})
	t.Cleanup(func() { _ = loader.Shutdown(context.Background()) })

	g := compile(t, promptLiveGraph(`delay_ms = "2500"`))

	runID := "run-prompt-live"
	ownerID := "agent-cri-259"
	promptCh := make(chan *pb.AgentPrompt, 8)
	// Prompt 4 is sent on the first step transition — after step "a"'s
	// attempt loop exited (the deferred endStep ran), during the settle
	// wait — so it pins the completed-step NO_ACTIVE_SESSION branch (no
	// hold, no retroactive injection).
	completed := &pb.AgentPrompt{
		RunId:            runID,
		Step:             "a",
		Prompt:           "after completion",
		CallerCriteriaId: ownerID,
		IssuedAt:         timestamppb.Now(),
	}
	sink := &promptInjectSink{promptCh: promptCh, runID: runID, owner: ownerID, callLog: callLog, sendOnTransition: completed}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelError}))

	// Prompt 1 is misaddressed (run-id mismatch → NOT_FOUND). Prompt 2 is the
	// live delivery. Prompt 3 is addressed to a step that never executes
	// (NO_ACTIVE_SESSION). Prompt 4 is sent after step "a" completed and is
	// the completed-step NO_ACTIVE_SESSION check (no retroactive injection).
	misaddressed := &pb.AgentPrompt{
		RunId:            "run-not-mine",
		Step:             "a",
		Prompt:           "wrong run",
		CallerCriteriaId: ownerID,
		IssuedAt:         timestamppb.Now(),
	}
	live := &pb.AgentPrompt{
		RunId:            runID,
		Step:             "a",
		Prompt:           "mid-turn nudge",
		CallerCriteriaId: ownerID,
		IssuedAt:         timestamppb.Now(),
	}
	strayStep := &pb.AgentPrompt{
		RunId:            runID,
		Step:             "never-executes",
		Prompt:           "no such step window",
		CallerCriteriaId: ownerID,
		IssuedAt:         timestamppb.Now(),
	}
	sink.armForStep(t, misaddressed, live, strayStep)

	e := NewTestEngine(g, loader, sink,
		WithRunID(runID),
		WithAgentPrompts(promptCh, ownerID, runID),
		WithLogger(logger),
	)
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if sink.terminal != "done" || !sink.terminalOK {
		t.Fatalf("terminal state: %s (ok=%v)", sink.terminal, sink.terminalOK)
	}
	if n := sink.sendFailureCount(); n != 0 {
		t.Fatalf("%d prompt send(s) timed out; router pump not consuming", n)
	}

	// Exit criterion 3: exactly one AgentPromptInjected envelope, populated
	// per D5 (step, session_id, prompt, caller, delivered_at).
	if n := sink.promptInjectionCount(); n != 1 {
		t.Fatalf("expected exactly 1 AgentPromptInjected, got %d", n)
	}
	rec, ok := sink.firstInjection()
	if !ok {
		t.Fatal("injection record missing")
	}
	if rec.step != "a" {
		t.Errorf("injected step = %q, want %q", rec.step, "a")
	}
	if rec.sessionID == "" {
		t.Error("injected session_id is empty")
	}
	if rec.prompt != "mid-turn nudge" {
		t.Errorf("injected prompt = %q, want %q", rec.prompt, "mid-turn nudge")
	}
	if rec.caller != ownerID {
		t.Errorf("injected caller = %q, want %q", rec.caller, ownerID)
	}
	if rec.deliveredAt.IsZero() {
		t.Error("injected delivered_at is zero")
	}

	// Exit criterion 8: the injection lands between StepEntered and
	// StepOutcome for the addressed step.
	entered, outcome := sink.markerIndex("entered:a"), sink.markerIndex("outcome:a")
	injected := sink.markerIndex("injected")
	if entered < 0 || outcome < 0 || injected < 0 {
		t.Fatalf("marker order incomplete: entered=%d injected=%d outcome=%d", entered, injected, outcome)
	}
	if !(entered < injected && injected < outcome) {
		t.Fatalf("injection not between StepEntered and StepOutcome: entered=%d injected=%d outcome=%d", entered, injected, outcome)
	}

	// Exit criterion 3 (extended S4 sequence): the host→adapter v2 call
	// sequence for the step gains exactly one Prompt call. The baseline
	// [Info, OpenSession, Execute, CloseSession] shape is preserved as an
	// ordered subsequence; extra Info calls from per-handle resolution are
	// tolerated, but the Prompt call itself must appear exactly once, after
	// the first Execute and before CloseSession.
	logged := readCallLog(t, callLog)
	promptCount := 0
	for _, m := range logged {
		if m == "prompt" {
			promptCount++
		}
	}
	if promptCount != 1 {
		t.Fatalf("call log has %d prompt calls, want exactly 1: %v", promptCount, logged)
	}
	want := []string{"info", "open_session", "execute", "prompt", "close_session"}
	idx := -1
	for _, m := range want {
		found := -1
		for i := idx + 1; i < len(logged); i++ {
			if logged[i] == m {
				found = i
				break
			}
		}
		if found < 0 {
			t.Fatalf("call log missing %q after index %d in order: %v", m, idx, logged)
		}
		idx = found
	}

	// Exit criterion 2: the misaddressed prompt was recorded as a typed
	// routing failure, never delivered (exactly one injected event above).
	// Exit criterion 5-adjacent R5: the stray-step prompt AND the
	// completed-step prompt (sent after the attempt loop exited) both resolve
	// NO_ACTIVE_SESSION at arrival — exactly two occurrences, neither held.
	if got := strings.Count(logBuf.String(), "class="+PromptFailNoActiveSession); got != 2 {
		t.Errorf("NO_ACTIVE_SESSION failure logs = %d, want 2 (stray step + completed step); log:\n%s", got, logBuf.String())
	}
	if got := strings.Count(logBuf.String(), "class="+PromptFailNotFound); got != 1 {
		t.Errorf("NOT_FOUND failure logs = %d, want 1; log:\n%s", got, logBuf.String())
	}
}

// TestEngineAgentPromptUnsupportedAdapter runs the capability-gate negative
// path end-to-end against the noop testdata fixture: the adapter does not
// declare supports_prompt, so delivery fails with UNSUPPORTED_ADAPTER and no
// AgentPromptInjected event is emitted (CRI-259 exit criterion 4).
func TestEngineAgentPromptUnsupportedAdapter(t *testing.T) {
	adapterBin := buildNoopAdapter(t)
	loader := adapterhost.NewLoaderWithDiscovery(func(string) (string, error) {
		return adapterBin, nil
	})
	t.Cleanup(func() { _ = loader.Shutdown(context.Background()) })

	g := compile(t, `
workflow {
  name = "prompt-noop"
  version = "0.1"
  initial_state = "a"
  target_state  = "done"
}
step "a" {
  target = adapter.noop.default
  input { delay_ms = "2500" }
  outcome "success" { next = step.done }
}
state "done" { terminal = true }
adapter "noop" "default" {}`)

	runID := "run-prompt-noop"
	ownerID := "agent-cri-259"
	promptCh := make(chan *pb.AgentPrompt, 8)
	sink := &promptInjectSink{promptCh: promptCh, runID: runID, owner: ownerID, settle: 150 * time.Millisecond}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelError}))

	sink.armForStep(t, &pb.AgentPrompt{
		RunId:            runID,
		Step:             "a",
		Prompt:           "should be gated",
		CallerCriteriaId: ownerID,
		IssuedAt:         timestamppb.Now(),
	})

	e := NewTestEngine(g, loader, sink,
		WithRunID(runID),
		WithAgentPrompts(promptCh, ownerID, runID),
		WithLogger(logger),
	)
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if sink.terminal != "done" || !sink.terminalOK {
		t.Fatalf("terminal state: %s (ok=%v)", sink.terminal, sink.terminalOK)
	}
	if n := sink.sendFailureCount(); n != 0 {
		t.Fatalf("%d prompt send(s) timed out", n)
	}
	if n := sink.promptInjectionCount(); n != 0 {
		t.Fatalf("AgentPromptInjected emitted for non-prompt-capable adapter (%d events)", n)
	}
	if !strings.Contains(logBuf.String(), "class=UNSUPPORTED_ADAPTER") {
		t.Errorf("capability gate failure log missing typed class UNSUPPORTED_ADAPTER; log:\n%s", logBuf.String())
	}
}

func readCallLog(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read call log: %v", err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}
