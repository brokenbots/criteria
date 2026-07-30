package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// captureSink records every event that reaches the parent sink so subworkflow
// failure propagation can be asserted.
type captureSink struct {
	fakeSink

	mu           sync.Mutex
	lifecycle    []lifecycleEvent
	stepEntered  []stepEnteredEvent
	stepOutcomes []stepOutcomeEvent
	stepOutputs  []stepOutputEvent
}

type lifecycleEvent struct {
	stepName string
	adapter  string
	status   string
	detail   string
}

type stepEnteredEvent struct {
	step    string
	adapter string
	attempt int
}

type stepOutcomeEvent struct {
	step    string
	outcome string
	err     error
}

type stepOutputEvent struct {
	step    string
	outputs map[string]string
}

func (s *captureSink) OnAdapterLifecycle(stepName, adapter, status, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lifecycle = append(s.lifecycle, lifecycleEvent{stepName: stepName, adapter: adapter, status: status, detail: detail})
}

func (s *captureSink) OnStepEntered(step, adapter string, attempt int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stepEntered = append(s.stepEntered, stepEnteredEvent{step: step, adapter: adapter, attempt: attempt})
}

func (s *captureSink) OnStepOutcome(step, outcome string, duration time.Duration, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stepOutcomes = append(s.stepOutcomes, stepOutcomeEvent{step: step, outcome: outcome, err: err})
}

func (s *captureSink) OnStepOutputCaptured(step string, outputs map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stepOutputs = append(s.stepOutputs, stepOutputEvent{step: step, outputs: outputs})
}

// failingOpenAdapter fails during OpenSession with a deterministic message.
type failingOpenAdapter struct {
	fakeAdapter
	openErr error
}

func (p *failingOpenAdapter) OpenSession(context.Context, string, map[string]string, map[string]string) error {
	return p.openErr
}

// calleeBodyWithAdapterAndOutputs builds a callee FSMGraph that declares an
// adapter and exposes a declared output. The state immediately succeeds, so any
// error must come from adapter initialization.
func calleeBodyWithAdapterAndOutputs(adapterType string) *workflow.FSMGraph {
	instanceID := adapterType + ".default"
	return &workflow.FSMGraph{
		InitialState: "done",
		States:       map[string]*workflow.StateNode{"done": {Name: "done", Terminal: true, Success: true}},
		Variables:    map[string]*workflow.VariableNode{},
		Adapters:     map[string]*workflow.AdapterNode{instanceID: {Type: adapterType, Name: "default"}},
		AdapterOrder: []string{instanceID},
		Outputs: map[string]*workflow.OutputNode{
			"status": {Name: "status"},
		},
		OutputOrder: []string{"status"},
		Policy:      workflow.DefaultPolicy,
	}
}

// TestRunSubworkflow_AdapterInitFailure_PropagatesErrorToParent asserts that
// when a child workflow's adapter fails to initialize, the parent sink receives
// an OnAdapterLifecycle("init_failed", ...) event naming the parent step and
// failing adapter, and an OnStepOutcome(stepName, "failure", ...) event carrying
// the child's error.
func TestRunSubworkflow_AdapterInitFailure_PropagatesErrorToParent(t *testing.T) {
	const stepName = "call_child"
	const childName = "broken_child"
	const adapterType = "copilot"
	const instanceID = adapterType + ".default"
	const initErrMsg = "copilot adapter requires GITHUB_TOKEN secret"

	openErr := errors.New(initErrMsg)
	failingPlugin := &failingOpenAdapter{
		fakeAdapter: fakeAdapter{name: adapterType},
		openErr:     openErr,
	}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{adapterType: failingPlugin}}

	sink := &captureSink{}
	sessions := adapterhost.NewSessionManager(loader)
	t.Cleanup(func() { sessions.Shutdown(context.Background()) })
	deps := Deps{Sessions: sessions, Sink: sink}

	node := subworkflowNodeFor(childName, calleeBodyWithAdapterAndOutputs(adapterType))
	n := &stepNode{
		graph: &workflow.FSMGraph{
			Subworkflows: map[string]*workflow.SubworkflowNode{childName: node},
		},
		step: &workflow.StepNode{
			Name:           stepName,
			TargetKind:     workflow.StepTargetSubworkflow,
			SubworkflowRef: childName,
			Outcomes: map[string]*workflow.CompiledOutcome{
				"failure": {Name: "failure"},
			},
		},
	}
	parentSt := &RunState{
		Vars:        map[string]cty.Value{"var": cty.EmptyObjectVal},
		WorkflowDir: t.TempDir(),
	}

	next, err := n.evaluateSubworkflowStep(context.Background(), parentSt, deps)
	if err != nil {
		t.Fatalf("unexpected error from evaluateSubworkflowStep: %v", err)
	}
	if next != "" {
		t.Errorf("expected empty next state for mapped failure outcome, got %q", next)
	}

	sink.mu.Lock()
	lifecycle := append([]lifecycleEvent(nil), sink.lifecycle...)
	stepEntered := append([]stepEnteredEvent(nil), sink.stepEntered...)
	stepOutcomes := append([]stepOutcomeEvent(nil), sink.stepOutcomes...)
	sink.mu.Unlock()

	if len(stepEntered) == 0 {
		t.Fatal("expected OnStepEntered event for subworkflow step")
	}
	lastEntered := stepEntered[len(stepEntered)-1]
	if lastEntered.step != stepName {
		t.Errorf("OnStepEntered step = %q, want %q", lastEntered.step, stepName)
	}
	if lastEntered.adapter != "" {
		t.Errorf("OnStepEntered adapter = %q, want empty", lastEntered.adapter)
	}
	if lastEntered.attempt != 1 {
		t.Errorf("OnStepEntered attempt = %d, want 1", lastEntered.attempt)
	}

	var initFailedEvent *lifecycleEvent
	for i := range lifecycle {
		if lifecycle[i].status == "init_failed" {
			initFailedEvent = &lifecycle[i]
			break
		}
	}
	if initFailedEvent == nil {
		t.Fatalf("expected init_failed lifecycle event, got events: %v", lifecycle)
	}
	if initFailedEvent.stepName != stepName {
		t.Errorf("init_failed event stepName = %q, want %q", initFailedEvent.stepName, stepName)
	}
	if initFailedEvent.adapter != instanceID {
		t.Errorf("init_failed event adapter = %q, want %q", initFailedEvent.adapter, instanceID)
	}
	if !strings.Contains(initFailedEvent.detail, initErrMsg) {
		t.Errorf("init_failed event detail should contain adapter error, got: %v", initFailedEvent.detail)
	}

	if len(stepOutcomes) == 0 {
		t.Fatal("expected OnStepOutcome event for child failure")
	}
	lastOutcome := stepOutcomes[len(stepOutcomes)-1]
	if lastOutcome.step != stepName {
		t.Errorf("OnStepOutcome step = %q, want %q", lastOutcome.step, stepName)
	}
	if lastOutcome.outcome != "failure" {
		t.Errorf("OnStepOutcome outcome = %q, want %q", lastOutcome.outcome, "failure")
	}
	if lastOutcome.err == nil {
		t.Fatal("expected OnStepOutcome to carry the child error")
	}
	if !strings.Contains(lastOutcome.err.Error(), childName) || !strings.Contains(lastOutcome.err.Error(), initErrMsg) {
		t.Errorf("OnStepOutcome error should name child and adapter error, got: %v", lastOutcome.err)
	}
}

// TestRunSubworkflow_AdapterInitFailure_DefinesOutputsForParentOutcome asserts
// that a parent outcome expression reading subworkflow.status on the failure
// path resolves to null rather than raising "unsupported attribute".
func TestRunSubworkflow_AdapterInitFailure_DefinesOutputsForParentOutcome(t *testing.T) {
	const adapterType = "copilot"
	const initErrMsg = "missing secret"
	failingPlugin := &failingOpenAdapter{
		fakeAdapter: fakeAdapter{name: adapterType},
		openErr:     errors.New(initErrMsg),
	}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{adapterType: failingPlugin}}

	node := subworkflowNodeFor("broken_child", calleeBodyWithAdapterAndOutputs(adapterType))
	parentSt := &RunState{
		Vars:        map[string]cty.Value{"var": cty.EmptyObjectVal},
		WorkflowDir: t.TempDir(),
	}
	deps := depsWithLoader(t, loader)

	outputs, _, err := runSubworkflow(context.Background(), "call_child", node, parentSt, nil, deps)
	if err == nil {
		t.Fatal("expected error from failing child adapter init, got nil")
	}

	// Simulate the outcome projection a parent would perform: output = { status = subworkflow.status }.
	// With declared outputs defined as null, this must evaluate to a known null
	// value instead of a bare "unsupported attribute" diagnostic.
	projection := parseExpr(t, `{ status = subworkflow.status }`)
	projected, perr := evalOutcomeOutputProjection(projection, outputs, nil, parentSt)
	if perr != nil {
		t.Fatalf("expected output projection to succeed on failure path, got: %v", perr)
	}
	if status, ok := projected["status"]; !ok || !status.IsNull() {
		t.Errorf("expected projected 'status' to be null, got %v (ok=%v)", status, ok)
	}
}
