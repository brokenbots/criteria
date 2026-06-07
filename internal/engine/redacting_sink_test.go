package engine

import (
	"errors"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapter/secrets"
)

// recordingSink captures every call for later assertion.
type recordingSink struct {
	onRunStartedArgs             []string
	onRunCompletedArgs           []string
	onRunFailedArgs              []string
	onStepEnteredArgs            []string
	onStepOutcomeArgs            []string
	onStepOutcomeErr             error
	onStepTransitionArgs         []string
	onStepResumedArgs            []string
	onVariableSetArgs            []string
	onStepOutputCapturedArgs     map[string]string
	onRunPausedArgs              []string
	onWaitEnteredArgs            []string
	onWaitResumedArgs            []string
	onWaitResumedPayload         map[string]string
	onApprovalRequestedArgs      []string
	onApprovalRequestedApprovers []string
	onApprovalDecisionArgs       []string
	onApprovalDecisionPayload    map[string]string
	onBranchEvaluatedArgs        []string
	onForEachEnteredNode         string
	onStepIterationStartedArgs   []string
	onStepIterationCompletedArgs []string
	onStepIterationItemArgs      []string
	onScopeIterCursorSetArg      string
	onAdapterLifecycleArgs       []string
	onRunOutputs                 []map[string]string
	onStepOutcomeDefaultedArgs   []string
	onStepOutcomeUnknownArgs     []string
	stepEventSinkStep            string
}

func (s *recordingSink) OnRunStarted(workflowName, initialStep string) {
	s.onRunStartedArgs = []string{workflowName, initialStep}
}
func (s *recordingSink) OnRunCompleted(finalState string, success bool) {
	s.onRunCompletedArgs = []string{finalState, boolStr(success)}
}
func (s *recordingSink) OnRunFailed(reason, step string) {
	s.onRunFailedArgs = []string{reason, step}
}
func (s *recordingSink) OnStepEntered(step, adapterName string, attempt int) {
	s.onStepEnteredArgs = []string{step, adapterName, intStr(attempt)}
}
func (s *recordingSink) OnStepOutcome(step, outcome string, duration time.Duration, err error) {
	s.onStepOutcomeArgs = []string{step, outcome, duration.String()}
	s.onStepOutcomeErr = err
}
func (s *recordingSink) OnStepTransition(from, to, viaOutcome string) {
	s.onStepTransitionArgs = []string{from, to, viaOutcome}
}
func (s *recordingSink) OnStepResumed(step string, attempt int, reason string) {
	s.onStepResumedArgs = []string{step, intStr(attempt), reason}
}
func (s *recordingSink) OnVariableSet(name, value, source string) {
	s.onVariableSetArgs = []string{name, value, source}
}
func (s *recordingSink) OnStepOutputCaptured(step string, outputs map[string]string) {
	s.onStepOutputCapturedArgs = outputs
}
func (s *recordingSink) OnRunPaused(node, mode, signal string) {
	s.onRunPausedArgs = []string{node, mode, signal}
}
func (s *recordingSink) OnWaitEntered(node, mode, duration, signal string) {
	s.onWaitEnteredArgs = []string{node, mode, duration, signal}
}
func (s *recordingSink) OnWaitResumed(node, mode, signal string, payload map[string]string) {
	s.onWaitResumedArgs = []string{node, mode, signal}
	s.onWaitResumedPayload = payload
}
func (s *recordingSink) OnApprovalRequested(node string, approvers []string, reason string) {
	s.onApprovalRequestedArgs = []string{node, reason}
	s.onApprovalRequestedApprovers = approvers
}
func (s *recordingSink) OnApprovalDecision(node, decision, actor string, payload map[string]string) {
	s.onApprovalDecisionArgs = []string{node, decision, actor}
	s.onApprovalDecisionPayload = payload
}
func (s *recordingSink) OnBranchEvaluated(node, matchedArm, target, condition string) {
	s.onBranchEvaluatedArgs = []string{node, matchedArm, target, condition}
}
func (s *recordingSink) OnForEachEntered(node string, count int) {
	s.onForEachEnteredNode = node
}
func (s *recordingSink) OnStepIterationStarted(node string, index int, value string, anyFailed bool) {
	s.onStepIterationStartedArgs = []string{node, intStr(index), value, boolStr(anyFailed)}
}
func (s *recordingSink) OnStepIterationCompleted(node, outcome, target string) {
	s.onStepIterationCompletedArgs = []string{node, outcome, target}
}
func (s *recordingSink) OnStepIterationItem(node string, index int, step string) {
	s.onStepIterationItemArgs = []string{node, intStr(index), step}
}
func (s *recordingSink) OnScopeIterCursorSet(cursorJSON string) {
	s.onScopeIterCursorSetArg = cursorJSON
}
func (s *recordingSink) OnAdapterLifecycle(stepName, adapterName, status, detail string) {
	s.onAdapterLifecycleArgs = []string{stepName, adapterName, status, detail}
}
func (s *recordingSink) OnRunOutputs(outputs []map[string]string) {
	s.onRunOutputs = outputs
}
func (s *recordingSink) OnStepOutcomeDefaulted(step, original, mapped string) {
	s.onStepOutcomeDefaultedArgs = []string{step, original, mapped}
}
func (s *recordingSink) OnStepOutcomeUnknown(step, outcome string) {
	s.onStepOutcomeUnknownArgs = []string{step, outcome}
}
func (s *recordingSink) StepEventSink(step string) adapter.EventSink {
	s.stepEventSinkStep = step
	return &fakeEventSink{}
}

type fakeEventSink struct{}

func (f *fakeEventSink) Log(stream string, chunk []byte) {}
func (f *fakeEventSink) Adapter(kind string, data any)   {}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
func intStr(i int) string {
	return string(rune('0' + i)) // simplistic, sufficient for small ints
}

func TestNewRedactingSink_NilRegistry(t *testing.T) {
	inner := &recordingSink{}
	got := NewRedactingSink(inner, nil)
	if got != inner {
		t.Fatal("expected nil registry to return inner sink unchanged")
	}
}

func TestRedactingSink_OnRunStarted(t *testing.T) {
	inner := &recordingSink{}
	reg := secrets.NewRegistry()
	reg.Register("secret123")
	sink := NewRedactingSink(inner, reg)

	sink.OnRunStarted("wf_secret123", "step_secret123")
	assertRedacted(t, inner.onRunStartedArgs, []string{"wf_[REDACTED]", "step_[REDACTED]"})
}

func TestRedactingSink_OnRunCompleted(t *testing.T) {
	inner := &recordingSink{}
	reg := secrets.NewRegistry()
	reg.Register("secret123")
	sink := NewRedactingSink(inner, reg)

	sink.OnRunCompleted("state_secret123", true)
	assertRedacted(t, inner.onRunCompletedArgs, []string{"state_[REDACTED]", "true"})
}

func TestRedactingSink_OnRunFailed(t *testing.T) {
	inner := &recordingSink{}
	reg := secrets.NewRegistry()
	reg.Register("secret123")
	sink := NewRedactingSink(inner, reg)

	sink.OnRunFailed("error_secret123", "step_secret123")
	assertRedacted(t, inner.onRunFailedArgs, []string{"error_[REDACTED]", "step_[REDACTED]"})
}

func TestRedactingSink_OnStepEntered(t *testing.T) {
	inner := &recordingSink{}
	reg := secrets.NewRegistry()
	reg.Register("secret123")
	sink := NewRedactingSink(inner, reg)

	sink.OnStepEntered("step_secret123", "adapter_secret123", 1)
	assertRedacted(t, inner.onStepEnteredArgs[:2], []string{"step_[REDACTED]", "adapter_[REDACTED]"})
}

func TestRedactingSink_OnStepOutcome(t *testing.T) {
	inner := &recordingSink{}
	reg := secrets.NewRegistry()
	reg.Register("secret123")
	sink := NewRedactingSink(inner, reg)

	err := errors.New("boom_secret123")
	sink.OnStepOutcome("step_secret123", "outcome_secret123", time.Second, err)
	assertRedacted(t, inner.onStepOutcomeArgs, []string{"step_[REDACTED]", "outcome_[REDACTED]", "1s"})
	if inner.onStepOutcomeErr == nil || inner.onStepOutcomeErr.Error() != "boom_[REDACTED]" {
		t.Fatalf("expected error redacted, got %v", inner.onStepOutcomeErr)
	}
}

func TestRedactingSink_OnStepOutcome_NilError(t *testing.T) {
	inner := &recordingSink{}
	reg := secrets.NewRegistry()
	reg.Register("secret123")
	sink := NewRedactingSink(inner, reg)

	sink.OnStepOutcome("step", "outcome", time.Second, nil)
	if inner.onStepOutcomeErr != nil {
		t.Fatalf("expected nil error forwarded, got %v", inner.onStepOutcomeErr)
	}
}

func TestRedactingSink_OnStepTransition(t *testing.T) {
	inner := &recordingSink{}
	reg := secrets.NewRegistry()
	reg.Register("secret123")
	sink := NewRedactingSink(inner, reg)

	sink.OnStepTransition("from_secret123", "to_secret123", "outcome_secret123")
	assertRedacted(t, inner.onStepTransitionArgs, []string{"from_[REDACTED]", "to_[REDACTED]", "outcome_[REDACTED]"})
}

func TestRedactingSink_OnStepResumed(t *testing.T) {
	inner := &recordingSink{}
	reg := secrets.NewRegistry()
	reg.Register("secret123")
	sink := NewRedactingSink(inner, reg)

	sink.OnStepResumed("step_secret123", 1, "reason_secret123")
	assertRedacted(t, []string{inner.onStepResumedArgs[0], inner.onStepResumedArgs[2]}, []string{"step_[REDACTED]", "reason_[REDACTED]"})
}

func TestRedactingSink_OnVariableSet(t *testing.T) {
	inner := &recordingSink{}
	reg := secrets.NewRegistry()
	reg.Register("secret123")
	sink := NewRedactingSink(inner, reg)

	sink.OnVariableSet("name_secret123", "value_secret123", "source_secret123")
	assertRedacted(t, inner.onVariableSetArgs, []string{"name_[REDACTED]", "value_[REDACTED]", "source_[REDACTED]"})
}

func TestRedactingSink_OnStepOutputCaptured(t *testing.T) {
	inner := &recordingSink{}
	reg := secrets.NewRegistry()
	reg.Register("secret123")
	sink := NewRedactingSink(inner, reg)

	sink.OnStepOutputCaptured("step_secret123", map[string]string{
		"key_secret123": "val_secret123",
	})
	if inner.onStepOutputCapturedArgs["key_[REDACTED]"] != "val_[REDACTED]" {
		t.Fatalf("expected map keys/values redacted, got %v", inner.onStepOutputCapturedArgs)
	}
}

func TestRedactingSink_OnRunPaused(t *testing.T) {
	inner := &recordingSink{}
	reg := secrets.NewRegistry()
	reg.Register("secret123")
	sink := NewRedactingSink(inner, reg)

	sink.OnRunPaused("node_secret123", "mode_secret123", "signal_secret123")
	assertRedacted(t, inner.onRunPausedArgs, []string{"node_[REDACTED]", "mode_[REDACTED]", "signal_[REDACTED]"})
}

func TestRedactingSink_OnWaitEntered(t *testing.T) {
	inner := &recordingSink{}
	reg := secrets.NewRegistry()
	reg.Register("secret123")
	sink := NewRedactingSink(inner, reg)

	sink.OnWaitEntered("n", "m", "dur_secret123", "sig_secret123")
	assertRedacted(t, inner.onWaitEnteredArgs[2:], []string{"dur_[REDACTED]", "sig_[REDACTED]"})
}

func TestRedactingSink_OnWaitResumed(t *testing.T) {
	inner := &recordingSink{}
	reg := secrets.NewRegistry()
	reg.Register("secret123")
	sink := NewRedactingSink(inner, reg)

	sink.OnWaitResumed("n", "m", "sig_secret123", map[string]string{"k": "v_secret123"})
	if inner.onWaitResumedPayload["k"] != "v_[REDACTED]" {
		t.Fatalf("expected payload redacted, got %v", inner.onWaitResumedPayload)
	}
}

func TestRedactingSink_OnApprovalRequested(t *testing.T) {
	inner := &recordingSink{}
	reg := secrets.NewRegistry()
	reg.Register("secret123")
	sink := NewRedactingSink(inner, reg)

	sink.OnApprovalRequested("node_secret123", []string{"alice_secret123", "bob"}, "reason_secret123")
	if len(inner.onApprovalRequestedApprovers) != 2 || inner.onApprovalRequestedApprovers[0] != "alice_[REDACTED]" {
		t.Fatalf("expected approvers redacted, got %v", inner.onApprovalRequestedApprovers)
	}
}

func TestRedactingSink_OnApprovalDecision(t *testing.T) {
	inner := &recordingSink{}
	reg := secrets.NewRegistry()
	reg.Register("secret123")
	sink := NewRedactingSink(inner, reg)

	sink.OnApprovalDecision("n_secret123", "d_secret123", "a_secret123", map[string]string{"k": "v_secret123"})
	assertRedacted(t, inner.onApprovalDecisionArgs, []string{"n_[REDACTED]", "d_[REDACTED]", "a_[REDACTED]"})
	if inner.onApprovalDecisionPayload["k"] != "v_[REDACTED]" {
		t.Fatalf("expected payload redacted, got %v", inner.onApprovalDecisionPayload)
	}
}

func TestRedactingSink_OnBranchEvaluated(t *testing.T) {
	inner := &recordingSink{}
	reg := secrets.NewRegistry()
	reg.Register("secret123")
	sink := NewRedactingSink(inner, reg)

	sink.OnBranchEvaluated("n", "arm_secret123", "t_secret123", "c_secret123")
	assertRedacted(t, inner.onBranchEvaluatedArgs[1:], []string{"arm_[REDACTED]", "t_[REDACTED]", "c_[REDACTED]"})
}

func TestRedactingSink_OnForEachEntered(t *testing.T) {
	inner := &recordingSink{}
	reg := secrets.NewRegistry()
	reg.Register("secret123")
	sink := NewRedactingSink(inner, reg)

	sink.OnForEachEntered("node_secret123", 3)
	if inner.onForEachEnteredNode != "node_[REDACTED]" {
		t.Fatalf("expected node redacted, got %s", inner.onForEachEnteredNode)
	}
}

func TestRedactingSink_OnStepIterationStarted(t *testing.T) {
	inner := &recordingSink{}
	reg := secrets.NewRegistry()
	reg.Register("secret123")
	sink := NewRedactingSink(inner, reg)

	sink.OnStepIterationStarted("node_secret123", 0, "val_secret123", false)
	assertRedacted(t, []string{inner.onStepIterationStartedArgs[0], inner.onStepIterationStartedArgs[2]}, []string{"node_[REDACTED]", "val_[REDACTED]"})
}

func TestRedactingSink_OnStepIterationCompleted(t *testing.T) {
	inner := &recordingSink{}
	reg := secrets.NewRegistry()
	reg.Register("secret123")
	sink := NewRedactingSink(inner, reg)

	sink.OnStepIterationCompleted("n_secret123", "o_secret123", "t_secret123")
	assertRedacted(t, inner.onStepIterationCompletedArgs, []string{"n_[REDACTED]", "o_[REDACTED]", "t_[REDACTED]"})
}

func TestRedactingSink_OnStepIterationItem(t *testing.T) {
	inner := &recordingSink{}
	reg := secrets.NewRegistry()
	reg.Register("secret123")
	sink := NewRedactingSink(inner, reg)

	sink.OnStepIterationItem("n_secret123", 0, "s_secret123")
	assertRedacted(t, []string{inner.onStepIterationItemArgs[0], inner.onStepIterationItemArgs[2]}, []string{"n_[REDACTED]", "s_[REDACTED]"})
}

func TestRedactingSink_OnScopeIterCursorSet(t *testing.T) {
	inner := &recordingSink{}
	reg := secrets.NewRegistry()
	reg.Register("secret123")
	sink := NewRedactingSink(inner, reg)

	sink.OnScopeIterCursorSet("cursor_secret123")
	if inner.onScopeIterCursorSetArg != "cursor_[REDACTED]" {
		t.Fatalf("expected cursor redacted, got %s", inner.onScopeIterCursorSetArg)
	}
}

func TestRedactingSink_OnAdapterLifecycle(t *testing.T) {
	inner := &recordingSink{}
	reg := secrets.NewRegistry()
	reg.Register("secret123")
	sink := NewRedactingSink(inner, reg)

	sink.OnAdapterLifecycle("s", "a", "status_secret123", "detail_secret123")
	assertRedacted(t, inner.onAdapterLifecycleArgs[2:], []string{"status_[REDACTED]", "detail_[REDACTED]"})
}

func TestRedactingSink_OnRunOutputs(t *testing.T) {
	inner := &recordingSink{}
	reg := secrets.NewRegistry()
	reg.Register("secret123")
	sink := NewRedactingSink(inner, reg)

	sink.OnRunOutputs([]map[string]string{{"key_secret123": "val_secret123"}})
	if len(inner.onRunOutputs) != 1 || inner.onRunOutputs[0]["key_[REDACTED]"] != "val_[REDACTED]" {
		t.Fatalf("expected outputs redacted, got %v", inner.onRunOutputs)
	}
}

func TestRedactingSink_OnStepOutcomeDefaulted(t *testing.T) {
	inner := &recordingSink{}
	reg := secrets.NewRegistry()
	reg.Register("secret123")
	sink := NewRedactingSink(inner, reg)

	sink.OnStepOutcomeDefaulted("s_secret123", "o_secret123", "m_secret123")
	assertRedacted(t, inner.onStepOutcomeDefaultedArgs, []string{"s_[REDACTED]", "o_[REDACTED]", "m_[REDACTED]"})
}

func TestRedactingSink_OnStepOutcomeUnknown(t *testing.T) {
	inner := &recordingSink{}
	reg := secrets.NewRegistry()
	reg.Register("secret123")
	sink := NewRedactingSink(inner, reg)

	sink.OnStepOutcomeUnknown("s_secret123", "o_secret123")
	assertRedacted(t, inner.onStepOutcomeUnknownArgs, []string{"s_[REDACTED]", "o_[REDACTED]"})
}

func TestRedactingSink_StepEventSink(t *testing.T) {
	inner := &recordingSink{}
	reg := secrets.NewRegistry()
	sink := NewRedactingSink(inner, reg)

	got := sink.StepEventSink("step_secret123")
	if got == nil {
		t.Fatal("expected non-nil EventSink")
	}
	if _, ok := got.(*secrets.RedactingEventSink); !ok {
		t.Fatalf("expected *secrets.RedactingEventSink, got %T", got)
	}
	if inner.stepEventSinkStep != "step_secret123" {
		t.Fatalf("expected inner.StepEventSink called with raw step, got %s", inner.stepEventSinkStep)
	}
}

func assertRedacted(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("length mismatch: got %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("arg[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}
