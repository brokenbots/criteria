package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zclconf/go-cty/cty"
	"pgregory.net/rapid"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// rapidStepSpec describes a generated step for runtime property testing.
type rapidStepSpec struct {
	name       string
	maxVisits  int
	hasRetry   bool
	retryTarget string
	nextTarget  string
	failTarget  string
}

// rapidWorkflowSpec describes a generated compilable workflow and its
// deterministic per-step outcome schedule.
type rapidWorkflowSpec struct {
	steps         []rapidStepSpec
	hasSwitch     bool
	switchCond    bool
	maxTotalSteps int
	// schedule maps stepName -> slice of outcomes returned on visit 0, 1, 2, ...
	schedule map[string][]string
}

func (w rapidWorkflowSpec) toHCL() string {
	var sb strings.Builder
	sb.WriteString("workflow {\n")
	sb.WriteString("  name          = \"rapid_runtime_test\"\n")
	sb.WriteString("  version       = \"1.0\"\n")
	sb.WriteString(fmt.Sprintf("  initial_state = %q\n", w.steps[0].name))
	sb.WriteString("  target_state  = \"done\"\n")
	sb.WriteString("  policy {\n")
	sb.WriteString(fmt.Sprintf("    max_total_steps  = %d\n", w.maxTotalSteps))
	sb.WriteString("    max_step_retries = 0\n")
	sb.WriteString("  }\n")
	sb.WriteString("}\n\n")
	sb.WriteString("adapter \"fake\" \"default\" {}\n\n")
	if w.hasSwitch {
		condLit := "false"
		if w.switchCond {
			condLit = "true"
		}
		sb.WriteString(fmt.Sprintf("variable \"branch_flag\" {\n  type    = bool\n  default = %s\n}\n\n", condLit))
	}

	for _, st := range w.steps {
		sb.WriteString(fmt.Sprintf("step %q {\n", st.name))
		sb.WriteString("  target = adapter.fake.default\n")
		if st.maxVisits > 0 {
			sb.WriteString(fmt.Sprintf("  max_visits = %d\n", st.maxVisits))
		}
		sb.WriteString(fmt.Sprintf("  outcome \"success\" {\n    next   = %s\n    output = { tag = %q }\n  }\n", st.nextTarget, st.name+"_ok"))
		if st.hasRetry {
			sb.WriteString(fmt.Sprintf("  outcome \"retry\" {\n    next   = %s\n    output = { tag = %q }\n  }\n", st.retryTarget, st.name+"_retry"))
		}
		sb.WriteString(fmt.Sprintf("  outcome \"failure\" {\n    next   = %s\n    output = { tag = %q }\n  }\n", st.failTarget, st.name+"_fail"))
		sb.WriteString("}\n\n")
	}

	if w.hasSwitch {
		n := len(w.steps)
		lastStep := fmt.Sprintf("step.step_%d", n-1)
		sb.WriteString("switch \"switch_0\" {\n")
		sb.WriteString(fmt.Sprintf("  match {\n    condition = var.branch_flag\n    next      = %s\n  }\n", lastStep))
		sb.WriteString("  default {\n    next = step.failed\n  }\n")
		sb.WriteString("}\n\n")
	}

	sb.WriteString("state \"done\" {\n  terminal = true\n  success  = true\n}\n\n")
	sb.WriteString("state \"failed\" {\n  terminal = true\n  success  = false\n}\n")
	return sb.String()
}

func genRapidWorkflowSpec() *rapid.Generator[rapidWorkflowSpec] {
	return rapid.Custom(func(t *rapid.T) rapidWorkflowSpec {
		n := rapid.IntRange(2, 6).Draw(t, "numSteps")
		hasSwitch := rapid.Bool().Draw(t, "hasSwitch")
		switchCond := rapid.Bool().Draw(t, "switchCond")

		steps := make([]rapidStepSpec, n)
		schedule := make(map[string][]string, n)

		for i := range n {
			name := fmt.Sprintf("step_%d", i)
			nextTarget := "step.done"
			if i < n-1 {
				if hasSwitch && i == n-2 {
					nextTarget = "step.switch_0"
				} else {
					nextTarget = fmt.Sprintf("step.step_%d", i+1)
				}
			}

			hasRetry := rapid.Bool().Draw(t, fmt.Sprintf("step_%d_hasRetry", i))
			retryTarget := ""
			if hasRetry {
				backIdx := rapid.IntRange(0, i).Draw(t, fmt.Sprintf("step_%d_backIdx", i))
				retryTarget = fmt.Sprintf("step.step_%d", backIdx)
			}

			maxVisits := rapid.IntRange(1, 5).Draw(t, fmt.Sprintf("step_%d_maxVisits", i))
			failChoices := []string{"step.failed", nextTarget}
			failTarget := rapid.SampledFrom(failChoices).Draw(t, fmt.Sprintf("step_%d_failTarget", i))

			steps[i] = rapidStepSpec{
				name:        name,
				maxVisits:   maxVisits,
				hasRetry:    hasRetry,
				retryTarget: retryTarget,
				nextTarget:  nextTarget,
				failTarget:  failTarget,
			}

			// Generate deterministic outcome schedule for up to 16 visits of this step.
			allowedOutcomes := []string{"success", "failure"}
			if hasRetry {
				allowedOutcomes = []string{"success", "retry", "failure"}
			}
			stepSched := make([]string, 16)
			for v := range stepSched {
				stepSched[v] = rapid.SampledFrom(allowedOutcomes).Draw(t, fmt.Sprintf("%s_visit_%d_outcome", name, v))
			}
			schedule[name] = stepSched
		}

		// Shortest path is n steps; allow budgets from n up to n+10 so some runs
		// complete normally while cyclic runs may hit max_total_steps or max_visits.
		extraBudget := rapid.IntRange(0, 10).Draw(t, "extraBudget")
		return rapidWorkflowSpec{
			steps:         steps,
			hasSwitch:     hasSwitch,
			switchCond:    switchCond,
			maxTotalSteps: n + extraBudget,
			schedule:      schedule,
		}
	})
}

// scheduledMockAdapter is a deterministic in-process adapter handle driven by
// the per-step visit schedule. It can optionally trigger a boundary pause on
// the active Engine when a target step execution count is reached.
type scheduledMockAdapter struct {
	mu          sync.Mutex
	schedule    map[string][]string
	callCounts  map[string]int
	totalCalls  int
	pauseAtCall int // 1-based global call index at which to call eng.RequestPause()
	eng         *Engine
}

func newScheduledMockAdapter(schedule map[string][]string, pauseAtCall int) *scheduledMockAdapter {
	return &scheduledMockAdapter{
		schedule:    schedule,
		callCounts:  make(map[string]int),
		pauseAtCall: pauseAtCall,
	}
}

func (a *scheduledMockAdapter) seedCallCounts(visits map[string]int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, v := range visits {
		a.callCounts[k] = v
	}
}

func (a *scheduledMockAdapter) Info(context.Context) (adapterhost.Info, error) {
	return adapterhost.Info{
		Name: "fake",
		AdapterInfo: workflow.AdapterInfo{
			OutputSchema: map[string]workflow.ConfigField{
				"visit": {CtyType: cty.String},
			},
		},
	}, nil
}

func (a *scheduledMockAdapter) OpenSession(context.Context, string, map[string]string, map[string]string) error {
	return nil
}

func (a *scheduledMockAdapter) Execute(_ context.Context, _ string, node *workflow.StepNode, _ adapter.EventSink, _ *v2.ExecutionRejection) (adapter.Result, error) {
	a.mu.Lock()
	visitIdx := a.callCounts[node.Name]
	a.callCounts[node.Name] = visitIdx + 1
	a.totalCalls++
	callNum := a.totalCalls
	eng := a.eng
	pauseAt := a.pauseAtCall

	outcome := "success"
	if sched, ok := a.schedule[node.Name]; ok && len(sched) > 0 {
		outcome = sched[visitIdx%len(sched)]
	}
	a.mu.Unlock()

	if pauseAt > 0 && callNum == pauseAt && eng != nil {
		eng.RequestPause()
	}

	return adapter.Result{
		Outcome: outcome,
		Outputs: map[string]cty.Value{
			"visit": cty.StringVal(fmt.Sprintf("%s#%d", node.Name, visitIdx+1)),
		},
	}, nil
}

func (a *scheduledMockAdapter) Permit(context.Context, string, string, bool, string) error {
	return nil
}
func (a *scheduledMockAdapter) CloseSession(context.Context, string) error { return nil }
func (a *scheduledMockAdapter) Kill()                                      {}
func (a *scheduledMockAdapter) Pause(context.Context, string) error        { return nil }
func (a *scheduledMockAdapter) Resume(context.Context, string) error       { return nil }
func (a *scheduledMockAdapter) Inspect(context.Context, string) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}
func (a *scheduledMockAdapter) Snapshot(context.Context, string) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{}, nil
}
func (a *scheduledMockAdapter) Restore(context.Context, string, []byte, uint32) error { return nil }

type recordedTransition struct {
	from       string
	to         string
	viaOutcome string
}

type recordedBranch struct {
	node       string
	matchedArm string
	target     string
}

type traceSink struct {
	fakeSink
	mu          sync.Mutex
	stepEntries []string
	outcomes    []string
	transitions []recordedTransition
	branches    []recordedBranch
	pausedNode  string
	pausedMode  string
	completed   bool
	finalState  string
	finalOK     bool
	failed      bool
	failReason  string
	failStep    string
}

func (s *traceSink) OnStepEntered(step, adapterName string, attempt int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fakeSink.OnStepEntered(step, adapterName, attempt)
	s.stepEntries = append(s.stepEntries, step)
}

func (s *traceSink) OnStepOutcome(step, outcome string, dur time.Duration, err error, comment string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fakeSink.OnStepOutcome(step, outcome, dur, err, comment)
	s.outcomes = append(s.outcomes, step+":"+outcome)
}

func (s *traceSink) OnStepTransition(from, to, viaOutcome string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fakeSink.OnStepTransition(from, to, viaOutcome)
	s.transitions = append(s.transitions, recordedTransition{from: from, to: to, viaOutcome: viaOutcome})
}

func (s *traceSink) OnBranchEvaluated(node, matchedArm, target, _ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.branches = append(s.branches, recordedBranch{node: node, matchedArm: matchedArm, target: target})
}

func (s *traceSink) OnRunPaused(node, mode, _ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pausedNode = node
	s.pausedMode = mode
}

func (s *traceSink) OnRunCompleted(finalState string, success bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fakeSink.OnRunCompleted(finalState, success)
	s.completed = true
	s.finalState = finalState
	s.finalOK = success
}

func (s *traceSink) OnRunFailed(reason, step string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fakeSink.OnRunFailed(reason, step)
	s.failed = true
	s.failReason = reason
	s.failStep = step
}

// persistedRunState models run-state.json for crash/pause-resume equivalence tests.
type persistedRunState struct {
	CurrentStep   string         `json:"current_step"`
	Attempt       int            `json:"attempt"`
	Visits        map[string]int `json:"visits"`
	VariableScope string         `json:"variable_scope"`
}

func compileGeneratedWorkflow(t *rapid.T, hclSrc string) *workflow.FSMGraph {
	spec, diags := workflow.Parse("rapid_runtime.hcl", []byte(hclSrc))
	if diags.HasErrors() {
		t.Fatalf("Parse failed: %s\nHCL:\n%s", diags.Error(), hclSrc)
	}
	g, cdiags := workflow.Compile(spec, nil)
	if cdiags.HasErrors() {
		t.Fatalf("Compile failed: %s\nHCL:\n%s", cdiags.Error(), hclSrc)
	}
	return g
}

// TestRapid_RuntimeTerminationAndTransitionValidity verifies that for any
// valid compiled workflow and any adapter outcome schedule:
//  1. Execution deterministically terminates within policy.max_total_steps and
//     per-step max_visits bounds.
//  2. Every emitted step and switch transition event corresponds to a declared
//     edge in the compiled FSMGraph.
func TestRapid_RuntimeTerminationAndTransitionValidity(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		wfSpec := genRapidWorkflowSpec().Draw(t, "workflow")
		hclSrc := wfSpec.toHCL()
		g := compileGeneratedWorkflow(t, hclSrc)

		mockAdapter := newScheduledMockAdapter(wfSpec.schedule, 0)
		loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": mockAdapter}}
		sink := &traceSink{}
		eng := New(g, loader, sink)
		mockAdapter.eng = eng

		runErr := eng.Run(context.Background())

		// 1. Termination & Budget Invariants.
		if !sink.completed && !sink.failed {
			t.Fatalf("expected run to terminate in either completed or failed state (err=%v)", runErr)
		}
		if len(sink.stepEntries) > g.Policy.MaxTotalSteps {
			t.Fatalf("executed %d steps, exceeding policy.max_total_steps (%d)", len(sink.stepEntries), g.Policy.MaxTotalSteps)
		}

		visits := eng.VisitCounts()
		for stepName, count := range visits {
			stepNode, ok := g.Steps[stepName]
			if !ok {
				t.Fatalf("visit recorded for undeclared step %q", stepName)
			}
			if stepNode.MaxVisits > 0 && count > stepNode.MaxVisits {
				t.Fatalf("step %q visited %d times, exceeding max_visits (%d)", stepName, count, stepNode.MaxVisits)
			}
		}

		if sink.completed {
			if runErr != nil {
				t.Fatalf("completed run returned non-nil error: %v", runErr)
			}
			stNode, ok := g.States[sink.finalState]
			if !ok || !stNode.Terminal {
				t.Fatalf("run completed at non-terminal or undeclared state %q", sink.finalState)
			}
		} else {
			if runErr == nil {
				t.Fatalf("failed run returned nil error (reason=%q)", sink.failReason)
			}
			if !strings.Contains(sink.failReason, "policy.max_total_steps exceeded") &&
				!strings.Contains(sink.failReason, "exceeded max_visits") {
				t.Fatalf("unexpected run failure reason: %s", sink.failReason)
			}
		}

		// 2. Transition Validity Invariant: every emitted transition must match
		// a declared outcome edge in the compiled FSMGraph.
		for i, tr := range sink.transitions {
			stepNode, ok := g.Steps[tr.from]
			if !ok {
				t.Fatalf("transition[%d] from undeclared step %q", i, tr.from)
			}
			co, ok := stepNode.Outcomes[tr.viaOutcome]
			if !ok && stepNode.DefaultOutcome != nil && stepNode.DefaultOutcome.Name == tr.viaOutcome {
				co = stepNode.DefaultOutcome
				ok = true
			}
			if !ok {
				t.Fatalf("transition[%d] from step %q via undeclared outcome %q", i, tr.from, tr.viaOutcome)
			}
			if co.Next != tr.to {
				t.Fatalf("transition[%d] from step %q via %q went to %q, but graph edge targets %q", i, tr.from, tr.viaOutcome, tr.to, co.Next)
			}
			// If followed by another step entry (or switch), verify continuity.
			if _, isState := g.States[tr.to]; isState {
				if i != len(sink.transitions)-1 {
					t.Fatalf("transition[%d] entered state %q before final transition", i, tr.to)
				}
			}
		}

		for i, br := range sink.branches {
			swNode, ok := g.Switches[br.node]
			if !ok {
				t.Fatalf("branch[%d] from undeclared switch %q", i, br.node)
			}
			validTarget := false
			if br.matchedArm == "default" {
				validTarget = swNode.DefaultNext == br.target
			} else {
				for idx, arm := range swNode.Conditions {
					if fmt.Sprintf("match[%d]", idx) == br.matchedArm && arm.Next == br.target {
						validTarget = true
						break
					}
				}
			}
			if !validTarget {
				t.Fatalf("branch[%d] on switch %q matched %q -> %q, which is not a declared switch edge", i, br.node, br.matchedArm, br.target)
			}
		}
	})
}

// TestRapid_CrashResumeEquivalence proves that interrupting a workflow run at
// an arbitrary step, persisting state to run-state.json, and resuming via
// Engine.RunFrom produces the exact same execution trace, terminal outcome,
// and budget/visit accounting as an uninterrupted baseline run.
func TestRapid_CrashResumeEquivalence(t *testing.T) {
	stateDir := t.TempDir()

	rapid.Check(t, func(t *rapid.T) {
		wfSpec := genRapidWorkflowSpec().Draw(t, "workflow")
		hclSrc := wfSpec.toHCL()
		g := compileGeneratedWorkflow(t, hclSrc)

		// 1. Run uninterrupted baseline.
		baseAdapter := newScheduledMockAdapter(wfSpec.schedule, 0)
		baseLoader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": baseAdapter}}
		baseSink := &traceSink{}
		baseEng := New(g, baseLoader, baseSink)
		baseAdapter.eng = baseEng

		baseErr := baseEng.Run(context.Background())
		baseVisits := maps.Clone(baseEng.VisitCounts())
		totalExecuted := len(baseSink.stepEntries)
		if totalExecuted == 0 {
			t.Fatalf("baseline run executed 0 steps")
		}

		// Choose an arbitrary step boundary k in [1, totalExecuted] at which to
		// interrupt the run, persist run-state.json, and resume.
		pauseAt := rapid.IntRange(1, totalExecuted).Draw(t, "pauseAtStep")

		// 2. Run until step pauseAt completes and boundary pause lands.
		part1Adapter := newScheduledMockAdapter(wfSpec.schedule, pauseAt)
		part1Loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": part1Adapter}}
		part1Sink := &traceSink{}
		part1Eng := New(g, part1Loader, part1Sink)
		part1Adapter.eng = part1Eng

		part1Err := part1Eng.Run(context.Background())

		// If the run already terminated (completed at a terminal state or failed
		// at a budget/visit limit) on or before the boundary pause could land,
		// part1 must already match the baseline run identically.
		if part1Sink.pausedNode == "" {
			if (part1Err != nil) != (baseErr != nil) {
				t.Fatalf("unpaused part1 err=%v != baseErr=%v", part1Err, baseErr)
			}
			if !slices.Equal(part1Sink.stepEntries, baseSink.stepEntries) {
				t.Fatalf("step entries mismatch: got %v, want %v", part1Sink.stepEntries, baseSink.stepEntries)
			}
			if !maps.Equal(part1Eng.VisitCounts(), baseVisits) {
				t.Fatalf("visits mismatch: got %v, want %v", part1Eng.VisitCounts(), baseVisits)
			}
			return
		}

		if part1Err != nil {
			t.Fatalf("paused part1 run returned unexpected error: %v", part1Err)
		}

		// 3. Persist run-state.json to disk and read it back.
		scopeJSON, err := workflow.SerializeVarScope(part1Eng.VarScope())
		if err != nil {
			t.Fatalf("SerializeVarScope: %v", err)
		}
		runStateFile := filepath.Join(stateDir, "run-state.json")
		persisted := persistedRunState{
			CurrentStep:   part1Sink.pausedNode,
			Attempt:       1,
			Visits:        maps.Clone(part1Eng.VisitCounts()),
			VariableScope: scopeJSON,
		}
		rawBytes, err := json.Marshal(persisted)
		if err != nil {
			t.Fatalf("json.Marshal(run-state.json): %v", err)
		}
		if err := os.WriteFile(runStateFile, rawBytes, 0o600); err != nil {
			t.Fatalf("WriteFile(run-state.json): %v", err)
		}

		loadedBytes, err := os.ReadFile(runStateFile)
		if err != nil {
			t.Fatalf("ReadFile(run-state.json): %v", err)
		}
		var loaded persistedRunState
		if err := json.Unmarshal(loadedBytes, &loaded); err != nil {
			t.Fatalf("json.Unmarshal(run-state.json): %v", err)
		}

		restoredVars, restoredIter, err := workflow.RestoreVarScope(loaded.VariableScope, g)
		if err != nil {
			t.Fatalf("RestoreVarScope: %v", err)
		}

		// 4. Resume from loaded run-state.json on a fresh Engine.
		part2Adapter := newScheduledMockAdapter(wfSpec.schedule, 0)
		part2Adapter.seedCallCounts(loaded.Visits)
		part2Loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": part2Adapter}}
		part2Sink := &traceSink{}
		part2Eng := New(
			g,
			part2Loader,
			part2Sink,
			WithResumedVisits(loaded.Visits),
			WithResumedVars(restoredVars),
			WithResumedIter(restoredIter),
		)
		part2Adapter.eng = part2Eng

		part2Err := part2Eng.RunFrom(context.Background(), loaded.CurrentStep, loaded.Attempt)

		// 5. Assert Resume Equivalence against uninterrupted baseline.
		combinedSteps := append(slices.Clone(part1Sink.stepEntries), part2Sink.stepEntries...)
		if !slices.Equal(combinedSteps, baseSink.stepEntries) {
			t.Fatalf("resumed step trace %v != baseline %v (paused at %q after %d steps)",
				combinedSteps, baseSink.stepEntries, loaded.CurrentStep, pauseAt)
		}

		combinedOutcomes := append(slices.Clone(part1Sink.outcomes), part2Sink.outcomes...)
		if !slices.Equal(combinedOutcomes, baseSink.outcomes) {
			t.Fatalf("resumed outcomes %v != baseline %v", combinedOutcomes, baseSink.outcomes)
		}

		combinedTransitions := append(slices.Clone(part1Sink.transitions), part2Sink.transitions...)
		if !slices.Equal(combinedTransitions, baseSink.transitions) {
			t.Fatalf("resumed transitions %v != baseline %v", combinedTransitions, baseSink.transitions)
		}

		if part2Sink.completed != baseSink.completed ||
			part2Sink.finalState != baseSink.finalState ||
			part2Sink.finalOK != baseSink.finalOK {
			t.Fatalf("resumed completion (completed=%v state=%q ok=%v) != baseline (completed=%v state=%q ok=%v)",
				part2Sink.completed, part2Sink.finalState, part2Sink.finalOK,
				baseSink.completed, baseSink.finalState, baseSink.finalOK)
		}

		if part2Sink.failed != baseSink.failed || part2Sink.failReason != baseSink.failReason {
			t.Fatalf("resumed failure (failed=%v reason=%q err=%v) != baseline (failed=%v reason=%q err=%v)",
				part2Sink.failed, part2Sink.failReason, part2Err,
				baseSink.failed, baseSink.failReason, baseErr)
		}

		finalVisits := part2Eng.VisitCounts()
		if !maps.Equal(finalVisits, baseVisits) {
			t.Fatalf("resumed visit counts %v != baseline %v", finalVisits, baseVisits)
		}
	})
}

// TestRapid_StateMachineMultiPauseResume uses rapid's stateful testing to drive
// a workflow through multiple sequential pause -> persist run-state.json ->
// reload -> RunFrom cycles, verifying that arbitrary multi-interruption
// schedules produce the exact same result as an uninterrupted run.
func TestRapid_StateMachineMultiPauseResume(t *testing.T) {
	stateDir := t.TempDir()

	rapid.Check(t, func(t *rapid.T) {
		wfSpec := genRapidWorkflowSpec().Draw(t, "workflow")
		hclSrc := wfSpec.toHCL()
		g := compileGeneratedWorkflow(t, hclSrc)

		// Compute baseline reference run.
		baseAdapter := newScheduledMockAdapter(wfSpec.schedule, 0)
		baseLoader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": baseAdapter}}
		baseSink := &traceSink{}
		baseEng := New(g, baseLoader, baseSink)
		baseAdapter.eng = baseEng
		_ = baseEng.Run(context.Background())

		// State machine state tracked across rapid actions.
		var (
			currentNode   = g.InitialState
			started       = false
			terminated    = false
			visits        = make(map[string]int)
			scopeJSON     string
			allSteps      []string
			allTrans      []recordedTransition
			finalDone     bool
			finalState    string
			finalOK       bool
			finalFailed   bool
			finalFailMsg  string
			runStateFile  = filepath.Join(stateDir, "sm-run-state.json")
		)

		t.Repeat(map[string]func(*rapid.T){
			"runSliceAndPersist": func(t *rapid.T) {
				if terminated {
					t.Skip("workflow already terminated")
				}
				stepsToRun := rapid.IntRange(1, 3).Draw(t, "stepsToRun")

				adapterInst := newScheduledMockAdapter(wfSpec.schedule, stepsToRun)
				adapterInst.seedCallCounts(visits)
				loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"fake": adapterInst}}
				sink := &traceSink{}

				var opts []Option
				if started {
					restoredVars, restoredIter, err := workflow.RestoreVarScope(scopeJSON, g)
					if err != nil {
						t.Fatalf("RestoreVarScope: %v", err)
					}
					opts = append(opts,
						WithResumedVisits(visits),
						WithResumedVars(restoredVars),
						WithResumedIter(restoredIter),
					)
				}

				eng := New(g, loader, sink, opts...)
				adapterInst.eng = eng

				if !started {
					started = true
					_ = eng.Run(context.Background())
				} else {
					_ = eng.RunFrom(context.Background(), currentNode, 1)
				}

				allSteps = append(allSteps, sink.stepEntries...)
				allTrans = append(allTrans, sink.transitions...)
				visits = maps.Clone(eng.VisitCounts())

				if sink.pausedNode != "" {
					currentNode = sink.pausedNode
					var err error
					scopeJSON, err = workflow.SerializeVarScope(eng.VarScope())
					if err != nil {
						t.Fatalf("SerializeVarScope: %v", err)
					}
					// Persist and reload from run-state.json on every slice boundary.
					raw, err := json.Marshal(persistedRunState{
						CurrentStep:   currentNode,
						Attempt:       1,
						Visits:        visits,
						VariableScope: scopeJSON,
					})
					if err != nil {
						t.Fatalf("Marshal: %v", err)
					}
					if err := os.WriteFile(runStateFile, raw, 0o600); err != nil {
						t.Fatalf("WriteFile: %v", err)
					}
					loadedRaw, err := os.ReadFile(runStateFile)
					if err != nil {
						t.Fatalf("ReadFile: %v", err)
					}
					var loaded persistedRunState
					if err := json.Unmarshal(loadedRaw, &loaded); err != nil {
						t.Fatalf("Unmarshal: %v", err)
					}
					currentNode = loaded.CurrentStep
					visits = loaded.Visits
					scopeJSON = loaded.VariableScope
				} else {
					terminated = true
					finalDone = sink.completed
					finalState = sink.finalState
					finalOK = sink.finalOK
					finalFailed = sink.failed
					finalFailMsg = sink.failReason
				}
			},
			"verifyInvariants": func(t *rapid.T) {
				// Prefix of executed steps must always match the baseline run's prefix.
				if len(allSteps) > len(baseSink.stepEntries) {
					t.Fatalf("executed %d steps, exceeding baseline %d", len(allSteps), len(baseSink.stepEntries))
				}
				if !slices.Equal(allSteps, baseSink.stepEntries[:len(allSteps)]) {
					t.Fatalf("step prefix mismatch: got %v, want %v", allSteps, baseSink.stepEntries[:len(allSteps)])
				}
				if terminated {
					if !slices.Equal(allSteps, baseSink.stepEntries) {
						t.Fatalf("final steps %v != baseline %v", allSteps, baseSink.stepEntries)
					}
					if !slices.Equal(allTrans, baseSink.transitions) {
						t.Fatalf("final transitions %v != baseline %v", allTrans, baseSink.transitions)
					}
					if finalDone != baseSink.completed || finalState != baseSink.finalState || finalOK != baseSink.finalOK {
						t.Fatalf("final completion mismatch: got (%v, %q, %v), want (%v, %q, %v)",
							finalDone, finalState, finalOK, baseSink.completed, baseSink.finalState, baseSink.finalOK)
					}
					if finalFailed != baseSink.failed || finalFailMsg != baseSink.failReason {
						t.Fatalf("final failure mismatch: got (%v, %q), want (%v, %q)",
							finalFailed, finalFailMsg, baseSink.failed, baseSink.failReason)
					}
					if !maps.Equal(visits, baseEng.VisitCounts()) {
						t.Fatalf("final visits %v != baseline %v", visits, baseEng.VisitCounts())
					}
				}
			},
		})
	})
}
