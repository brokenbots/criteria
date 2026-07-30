// Package engine drives a workflow's FSM. It is pure: dispatcher (adapter
// lookup) and event sink are injected so the engine can be tested without I/O.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/zclconf/go-cty/cty"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapter/environment/remote"
	"github.com/brokenbots/criteria/internal/adapter/secrets"
	"github.com/brokenbots/criteria/internal/adapterhost"
	engineruntime "github.com/brokenbots/criteria/internal/engine/runtime"
	"github.com/brokenbots/criteria/internal/runtime/state"
	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

// Sink receives engine-level events. Implementations (typically the server
// transport) are responsible for assigning sequence numbers, persisting, and
// streaming. The engine never blocks waiting for the sink. The interpreter
// loop invokes OnRunStarted/OnRunCompleted/OnRunFailed. stepNode invokes
// OnStepEntered/OnStepOutcome/OnStepTransition and StepEventSink.
//
// OnVariableSet and OnStepOutputCaptured were added in W04. This is an
// internal interface; only server and Local sinks implement it.
type Sink interface {
	OnRunStarted(workflowName, initialStep string)
	OnRunCompleted(finalState string, success bool)
	OnRunFailed(reason, step string)
	OnStepEntered(step, adapterName string, attempt int)
	OnStepOutcome(step, outcome string, duration time.Duration, err error)
	OnStepTransition(from, to, viaOutcome string)
	OnStepResumed(step string, attempt int, reason string)
	// OnVariableSet is emitted when a workflow variable value is established (W04).
	OnVariableSet(name, value, source string)
	// OnStepOutputCaptured is emitted after a step produces captured outputs (W04).
	OnStepOutputCaptured(step string, outputs map[string]string)
	// OnRunPaused is called when the engine pauses at a wait or approval node
	// (W05). node is the node name, mode is "duration"|"signal", signal is the
	// pending signal name (empty for duration mode).
	OnRunPaused(node, mode, signal string)
	// OnWaitEntered is emitted when the engine enters a wait node (W05).
	OnWaitEntered(node, mode, duration, signal string)
	// OnWaitResumed is emitted when a wait node resolves (W05). payload is nil
	// for duration-mode waits; carries the resume payload for signal-mode waits.
	OnWaitResumed(node, mode, signal string, payload map[string]string)
	// OnApprovalRequested is emitted when the engine enters an approval node (W05).
	OnApprovalRequested(node string, approvers []string, reason string)
	// OnApprovalDecision is emitted when an approval node resolves via Resume (W05).
	// decision is "approved" or "rejected". actor is audit metadata.
	OnApprovalDecision(node, decision, actor string, payload map[string]string)
	// OnBranchEvaluated is emitted when a branch node selects a transition arm (W06).
	// matchedArm is "arm[<index>]" or "default"; target is the transition target.
	// condition is the source text of the matched arm expression; empty for default.
	OnBranchEvaluated(node, matchedArm, target, condition string)
	// OnForEachEntered is emitted when a step begins iterating (for_each or count) (W07/W10).
	// count is the total number of items.
	OnForEachEntered(node string, count int)
	// OnStepIterationStarted is emitted at the start of each per-item iteration (W10).
	// Formerly OnForEachIteration (W07); renamed for step-level semantics.
	// index is zero-based; value is the string-rendered cty value of the current
	// item; anyFailed reports whether any prior iteration produced a failure outcome.
	OnStepIterationStarted(node string, index int, value string, anyFailed bool)
	// OnStepIterationCompleted is emitted when a step finishes all iterations (W10).
	// Formerly OnForEachOutcome (W07).
	// outcome is "all_succeeded" or "any_failed"; target is the transition target.
	OnStepIterationCompleted(node, outcome, target string)
	// OnStepIterationItem is emitted when the engine is about to execute the
	// step body for the next iteration item (W10). Formerly OnForEachStep (W08).
	// node is the step name, index is the zero-based iteration index.
	// step is reserved for workflow-type steps; empty for non-workflow steps.
	OnStepIterationItem(node string, index int, step string)
	// OnScopeIterCursorSet is emitted whenever the step iteration cursor stack
	// is created, advanced, or cleared (W07/W10). cursorJSON is the JSON-encoded
	// cursor stack; an empty string signals cursor cleared. The server stores
	// this verbatim without interpreting field names.
	OnScopeIterCursorSet(cursorJSON string)
	// OnAdapterLifecycle is emitted at adapter session lifecycle events (W12).
	// status is one of: "started", "exited", "crashed".
	// stepName is the step that owns the event; adapterName is the adapter
	// (e.g. "noop", "copilot"); detail is a one-line description (empty for
	// clean events).
	OnAdapterLifecycle(stepName, adapterName, status, detail string)
	// OnRunOutputs is emitted when a run reaches terminal state with declared outputs (W09).
	// outputs is a list of (name, value, declared_type) tuples in declaration order.
	// This method is called before OnRunCompleted.
	OnRunOutputs(outputs []map[string]string)
	// OnStepOutcomeDefaulted is emitted when a step produces an outcome not in
	// its declared set and the outcome "default" block is applied (W15).
	// original is the outcome name the adapter returned; mapped is "default".
	OnStepOutcomeDefaulted(step, original, mapped string)
	// OnStepOutcomeUnknown is emitted when a step produces an outcome not in its
	// declared set and no outcome "default" block is configured (W15).
	// This precedes a run failure.
	OnStepOutcomeUnknown(step, outcome string)
	// StepEventSink returns the per-step adapter sink (logs + adapter events).
	StepEventSink(step string) adapter.EventSink
}

// Engine executes a single workflow run to a terminal state.
type Engine struct {
	graph               *workflow.FSMGraph
	loader              adapterhost.Loader
	sink                Sink
	subWorkflowResolver SubWorkflowResolver
	branchScheduler     BranchScheduler
	// resumedVars, when non-nil, overrides SeedVarsFromGraph at run start (W04).
	resumedVars map[string]cty.Value
	// resumedVisits, when non-nil, seeds RunState.Visits at run start (W07).
	// Used during crash-recovery reattach to restore per-step visit counts.
	resumedVisits map[string]int
	// varOverrides, when non-nil, overlays CLI-supplied typed variable values on top
	// of the graph variable defaults at run start.
	varOverrides map[string]cty.Value
	// resumedIterStack, when non-empty, seeds RunState.IterStack at run start
	// (W10). Used during crash-recovery reattach when a step iteration was active.
	resumedIterStack []workflow.IterCursor
	// pendingSignal, when non-empty, is placed into RunState at run start (W05).
	// Used during crash-recovery reattach when the run was paused mid-signal-wait.
	pendingSignal string
	// resumePayload, when non-nil, is placed into RunState at run start (W05).
	// Used when the orchestrator delivers a resume signal to a paused run.
	resumePayload map[string]string
	// lastVars captures the Vars map from RunState when execution pauses so
	// the caller can pass them to the resumed engine via WithResumedVars (W05).
	lastVars map[string]cty.Value
	// lastVisits captures the Visits map from RunState when execution stops so
	// the caller can pass them to a resumed engine via WithResumedVisits (W07).
	lastVisits map[string]int
	// liveRunState is set to the active RunState while runLoop is executing,
	// allowing VisitCounts() to return live values for mid-run checkpoints (W07).
	// Cleared by handleEvalError when the run ends.
	liveRunState *RunState
	// workflowDir is the directory containing the HCL workflow file. Passed to
	// RunState so that file() and fileexists() can resolve relative paths.
	workflowDir string
	// lockfile is the parsed adapter lockfile used for container-mode resolution.
	// If nil and workflowDir is set, the engine auto-reads it at run start.
	lockfile *lockfile.Lockfile
	// log is an optional structured logger for internal engine warnings.
	// Falls back to slog.Default() when nil.
	log *slog.Logger
	// auditWriter, when non-nil, is wired into the SessionManager so that
	// permission decisions are recorded to a file (WS16).
	auditWriter adapterhost.AuditWriter

	// WS18: snapshotBase is the base directory for persisting session
	// snapshots during Pause. When empty, snapshots are not persisted.
	snapshotBase string
	// WS18: runID namespaces snapshot files within snapshotBase.
	runID string

	// WS17: liveSessions holds the active SessionManager while a run is in
	// progress, enabling Pause/Resume/Inspect from outside runLoop.
	liveSessions *adapterhost.SessionManager
	mu           sync.RWMutex
}

func New(graph *workflow.FSMGraph, loader adapterhost.Loader, sink Sink, opts ...Option) *Engine {
	e := &Engine{graph: graph, loader: loader, sink: sink}
	for _, opt := range opts {
		if opt != nil {
			opt(e)
		}
	}
	return e
}

// VarScope returns the variable scope captured when the last run paused.
// Returns nil if the engine has not yet paused. Used by the CLI pause/resume
// loop to carry variable state across a resume boundary (W05).
func (e *Engine) VarScope() map[string]cty.Value { return e.lastVars }

// VisitCounts returns the per-step visit counts. During an active run it
// returns the live values from the current RunState so that mid-run
// checkpoints capture the correct counts. After the run ends it returns the
// snapshot captured by handleEvalError. Returns nil if the engine has not yet
// run. Used by the CLI crash-recovery path to persist visit state across a
// resume boundary (W07).
func (e *Engine) VisitCounts() map[string]int {
	if e.liveRunState != nil {
		return e.liveRunState.Visits
	}
	return e.lastVisits
}

// Pause halts all open adapter sessions without losing state, snapshots each
// session, and persists the snapshots to disk (WS18). It is reentrant and
// idempotent.
func (e *Engine) Pause(ctx context.Context) error {
	e.mu.RLock()
	sessions := e.liveSessions
	e.mu.RUnlock()
	if sessions == nil {
		return errors.New("no active run to pause")
	}
	if err := sessions.PauseAll(ctx); err != nil {
		return err
	}
	if e.snapshotBase == "" || e.runID == "" {
		return nil
	}
	snaps, err := sessions.SnapshotAll(ctx)
	if err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	for name, snap := range snaps {
		dir := state.SnapshotDir(e.snapshotBase, e.runID, name)
		if _, err := state.WriteSnapshot(dir, snap); err != nil {
			return fmt.Errorf("persist snapshot for %q: %w", name, err)
		}
	}
	return nil
}

// Resume continues all paused adapter sessions. If the engine was restarted
// and liveSessions is nil, it reconstructs sessions from the latest persisted
// snapshots before resuming (WS18).
func (e *Engine) Resume(ctx context.Context) error {
	e.mu.RLock()
	sessions := e.liveSessions
	e.mu.RUnlock()
	if sessions == nil {
		if e.snapshotBase == "" || e.runID == "" {
			return errors.New("no active run to resume")
		}
		restored, err := e.restoreSessionsFromSnapshots(ctx)
		if err != nil {
			return err
		}
		sessions = restored
		e.mu.Lock()
		e.liveSessions = sessions
		e.mu.Unlock()
	}
	return sessions.ResumeAll(ctx)
}

func (e *Engine) restoreSessionsFromSnapshots(ctx context.Context) (*adapterhost.SessionManager, error) {
	sessions := adapterhost.NewSessionManager(e.loader)
	sessions.SetGraph(e.graph)
	sessions.SetLockfile(e.lockfile)
	sessions.RedactionRegistry = secrets.NewRegistry()
	if e.auditWriter != nil {
		sessions.Audit = e.auditWriter
	}
	if e.graph == nil {
		return nil, errors.New("cannot restore sessions: engine has no workflow graph")
	}
	ids, err := state.ListSnapshotSessions(e.snapshotBase, e.runID)
	if err != nil {
		return nil, fmt.Errorf("list snapshot sessions: %w", err)
	}
	for _, sid := range ids {
		dir := state.SnapshotDir(e.snapshotBase, e.runID, sid)
		snap, err := state.ReadLatestSnapshot(dir)
		if err != nil {
			return nil, fmt.Errorf("read snapshot for %q: %w", sid, err)
		}
		adapterNode := e.graph.Adapters[sid]
		if adapterNode == nil {
			return nil, fmt.Errorf("snapshot session %q not found in workflow graph", sid)
		}
		envNode := getEnvironmentNode(e.graph, adapterNode.Environment)
		_, err = sessions.Restore(ctx, sid, adapterNode.Type, adapterNode.OnCrash, adapterNode.Config, envNode, snap)
		if err != nil {
			return nil, fmt.Errorf("restore session %q: %w", sid, err)
		}
	}
	return sessions, nil
}

// InspectSession returns structured read-only state for a single session.
func (e *Engine) InspectSession(ctx context.Context, name string) (*v2.InspectResponse, error) {
	e.mu.RLock()
	sessions := e.liveSessions
	e.mu.RUnlock()
	if sessions == nil {
		return nil, errors.New("no active run to inspect")
	}
	return sessions.InspectSession(ctx, name)
}

// setLockfileOnSessions ensures the session manager has the lockfile needed
// for container-mode adapter resolution. If the engine already has a lockfile
// it is used directly; otherwise if workflowDir is set the lockfile is read
// from the workflow directory.
func (e *Engine) setLockfileOnSessions(sessions *adapterhost.SessionManager) error {
	if e.lockfile != nil {
		sessions.SetLockfile(e.lockfile)
		return nil
	}
	if e.workflowDir == "" {
		return nil
	}
	lf, err := lockfile.ReadFromDir(e.workflowDir)
	if err != nil {
		return fmt.Errorf("read lockfile: %w", err)
	}
	sessions.SetLockfile(lf)
	return nil
}

// Run executes the workflow until a terminal state is reached, the global
// step limit is exceeded, or ctx is cancelled.
func (e *Engine) Run(ctx context.Context) error {
	sessions := adapterhost.NewSessionManager(e.loader)
	sessions.SetGraph(e.graph)
	sessions.Audit = e.auditWriter
	if err := e.setLockfileOnSessions(sessions); err != nil {
		return err
	}
	defer func() { _ = sessions.Shutdown(context.WithoutCancel(ctx)) }()

	// Create a per-run redaction registry and wire it into the session manager
	// and the engine sink so all secret values are masked before display or
	// persistence.
	redactionReg := secrets.NewRegistry()
	sessions.RedactionRegistry = redactionReg

	// Wrap the engine sink before any events are emitted.
	sink := NewRedactingSink(e.sink, redactionReg)

	// Seed variables before adapter provisioning so secret expressions can be
	// evaluated against the run scope (WS13).
	vars, err := e.seedRunVars(sink)
	if err != nil {
		return err
	}

	// WS20: if any environment is remote, start the phone-home shim before
	// provisioning adapters.
	if err := e.maybeStartRemoteShim(ctx, sessions); err != nil {
		return err
	}

	deps := Deps{
		Sessions: sessions,
		Sink:     sink,
	}

	// Provision adapter sessions at scope start (W12)
	scopeOrder, err := initScopeAdapters(ctx, e.graph, deps, vars, e.workflowDir, "")
	if err != nil {
		sink.OnRunFailed(err.Error(), e.graph.InitialState)
		return err
	}
	defer func() { tearDownScopeAdapters(ctx, scopeOrder, deps) }()

	current := e.graph.InitialState
	sink.OnRunStarted(e.graph.Name, current)
	return e.runLoop(ctx, sessions, current, 1, vars, sink)
}

// RunFrom resumes a workflow at startStep with the given initialAttempt
// number as the first attempt for that step (subsequent retries increment
// from there). It does NOT emit OnRunStarted (the run already started).
// If initialAttempt would already exceed max_step_retries, it emits
// OnRunFailed instead of attempting the step.
// Adapter sessions are provisioned fresh on each run (resumed or not),
// allowing the workflow to be resumed in a new process context.
func (e *Engine) RunFrom(ctx context.Context, startStep string, initialAttempt int) error {
	sessions := adapterhost.NewSessionManager(e.loader)
	sessions.SetGraph(e.graph)
	sessions.Audit = e.auditWriter
	if err := e.setLockfileOnSessions(sessions); err != nil {
		return err
	}
	defer func() { _ = sessions.Shutdown(context.WithoutCancel(ctx)) }()

	redactionReg := secrets.NewRegistry()
	sessions.RedactionRegistry = redactionReg

	sink := NewRedactingSink(e.sink, redactionReg)

	vars, err := e.seedRunVars(sink)
	if err != nil {
		return err
	}

	// WS20: if any environment is remote, start the phone-home shim before
	// provisioning adapters.
	if err := e.maybeStartRemoteShim(ctx, sessions); err != nil {
		return err
	}

	deps := Deps{
		Sessions: sessions,
		Sink:     sink,
	}

	// For resumed runs, provision adapter sessions at scope start (W12).
	// Sessions are always provisioned fresh, not restored from a prior run.
	scopeOrder, err := initScopeAdapters(ctx, e.graph, deps, vars, e.workflowDir, "")
	if err != nil {
		sink.OnRunFailed(err.Error(), startStep)
		return err
	}
	defer func() { tearDownScopeAdapters(ctx, scopeOrder, deps) }()

	if err := e.bootstrapSessionsForResume(ctx, sessions, startStep); err != nil {
		return err
	}
	return e.runLoop(ctx, sessions, startStep, initialAttempt, vars, sink)
}

// runLoop is the shared execution loop. firstStepAttempt is the attempt index
// used for the initial step when resuming; subsequent steps start at attempt 1.
func (e *Engine) runLoop(ctx context.Context, sessions *adapterhost.SessionManager, current string, firstStepAttempt int, vars map[string]cty.Value, sink Sink) error {
	st := &RunState{
		Current:          current,
		Vars:             vars,
		PendingSignal:    e.pendingSignal,
		ResumePayload:    e.resumePayload,
		IterStack:        append([]workflow.IterCursor{}, e.resumedIterStack...),
		Visits:           cloneVisits(e.resumedVisits),
		WorkflowDir:      e.workflowDir,
		DataStore:        NewDataStore(e.graph),
		firstStep:        true,
		firstStepAttempt: firstStepAttempt,
	}
	deps := e.buildDeps(sessions, sink)

	e.mu.Lock()
	e.liveSessions = sessions
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.liveSessions = nil
		e.mu.Unlock()
	}()

	e.liveRunState = st
	for {
		node, err := nodeFor(e.graph, st.Current)
		if err != nil {
			sink.OnRunFailed(err.Error(), st.Current)
			return err
		}
		next, err := node.Evaluate(ctx, st, deps)
		if err != nil {
			return e.handleEvalError(st, err, sink)
		}
		next, err = e.routeIteratingStep(st, next, sink)
		if err != nil {
			return e.handleEvalError(st, err, sink)
		}
		if next == workflow.ReturnSentinel {
			e.handleReturnExit(st, sink)
			return nil
		}
		e.advanceTo(st, next)
	}
}

// routeIteratingStep handles post-step routing for steps with active iteration
// cursors (W10). Delegates to routeIteratingStepInGraph using the engine's
// own graph and sink. See routeIteratingStepInGraph for full semantics.
func (e *Engine) routeIteratingStep(st *RunState, next string, sink Sink) (string, error) {
	return routeIteratingStepInGraph(st, next, e.graph, sink)
}

// routeIteratingStepInGraph is the graph-agnostic iteration router called by
// both the engine's main loop and the workflow-body sub-loop. It checks
// whether the top cursor belongs to the current step and applies the
// appropriate iteration semantics:
//
//   - No active cursor → return next unchanged.
//   - More iterations remain → re-bind each.*, emit started event, re-enter step.
//   - All iterations done (or on_failure=abort after failure) → pop cursor,
//     emit completed event, return aggregate outcome target from graph.
func routeIteratingStepInGraph(st *RunState, next string, graph *workflow.FSMGraph, sink Sink) (string, error) {
	cur := st.TopCursor()
	if cur == nil || !cur.InProgress {
		return next, nil
	}

	// while-cursor lifecycle is managed entirely inside evaluateWhile (which
	// either re-enters the step or resolves the aggregate outcome directly).
	// Skip the for_each/count routing path for while cursors.
	if cur.IsWhile() {
		return next, nil
	}

	stepName := cur.StepName
	// Only intercept when the current node is the iterating step itself.
	// When the step has a workflow body (_continue comes from the body's
	// terminal state), next will be "_continue".
	if st.Current != stepName && next != "_continue" {
		return next, nil
	}

	// Record outcome for this iteration.
	outcomeIsSuccess := isSuccessOutcome(st.LastOutcome)
	if !outcomeIsSuccess {
		cur.AnyFailed = true
	}

	// Workflow-body early-exit: body reached a terminal state other than
	// "_continue" — stop the entire iteration immediately.
	if cur.EarlyExit {
		return finishIterationInGraph(st, stepName, graph, sink)
	}

	// on_failure=abort: stop after first failure.
	if cur.OnFailure == "abort" && !outcomeIsSuccess {
		return finishIterationInGraph(st, stepName, graph, sink)
	}

	cur.Index++
	cur.InProgress = false

	if cur.Index < cur.Total {
		return advanceIteration(st, cur, stepName, sink)
	}

	// All iterations done.
	return finishIterationInGraph(st, stepName, graph, sink)
}

// advanceIteration re-binds each.* variables and events for the next iteration.
// It updates the cursor key, marks InProgress, emits scope/cursor events, and
// returns the step name so the engine re-enters the same step.
func advanceIteration(st *RunState, cur *workflow.IterCursor, stepName string, sink Sink) (string, error) {
	item := cur.Items[cur.Index]
	var key cty.Value
	if cur.Index < len(cur.Keys) {
		key = cur.Keys[cur.Index]
	} else {
		key = cty.StringVal(fmt.Sprintf("%d", cur.Index))
	}
	cur.Key = key
	cur.InProgress = true
	st.Vars = workflow.WithEachBinding(st.Vars, &workflow.EachBinding{
		Value: item,
		Key:   key,
		Index: cur.Index,
		Total: cur.Total,
		First: cur.Index == 0,
		Last:  cur.Index == cur.Total-1,
		Prev:  cur.Prev,
	})
	if curJSON, err := workflow.SerializeIterCursor(cur); err == nil {
		sink.OnScopeIterCursorSet(curJSON)
	}
	sink.OnStepIterationStarted(stepName, cur.Index, workflow.CtyValueToString(item), cur.AnyFailed)
	return stepName, nil // re-enter the same step
}

// finishIterationInGraph closes out an iteration loop: pops the cursor, clears
// each.* bindings, emits OnStepIterationCompleted, and returns the aggregate
// outcome target looked up from graph. When the aggregate outcome routes via
// next = step.return and declares an output expression, the expression is
// evaluated and the result stored in st.ReturnOutputs before the sentinel is
// returned — matching the single-step return path in applyOutcome.
func finishIterationInGraph(st *RunState, stepName string, graph *workflow.FSMGraph, sink Sink) (string, error) {
	cur := st.PopCursor()
	st.Vars = workflow.ClearEachBinding(st.Vars)
	sink.OnScopeIterCursorSet("") // cursor cleared

	step, ok := graph.Steps[stepName]
	if !ok {
		return stepName, nil
	}

	aggregateOutcome := "all_succeeded"
	if cur.AnyFailed && cur.OnFailure != "ignore" {
		aggregateOutcome = "any_failed"
	}

	co, ok := step.Outcomes[aggregateOutcome]
	if !ok {
		// Fall back to all_succeeded (required by compile; missing any_failed is
		// a compile warning, not an error).
		co = step.Outcomes["all_succeeded"]
	}

	sink.OnStepIterationCompleted(stepName, aggregateOutcome, co.Next)

	// Evaluate output projection for the aggregate outcome. This is used by both
	// the return path (st.ReturnOutputs) and any write blocks declared on the
	// aggregate outcome. Evaluated once and shared between both paths.
	var aggregateProjectedCty map[string]cty.Value
	if co.OutputExpr != nil {
		projected, err := evalOutcomeOutputProjection(co.OutputExpr, nil, nil, st)
		if err != nil {
			return "", fmt.Errorf("step %q aggregate outcome %q: output projection: %w", stepName, aggregateOutcome, err)
		}
		aggregateProjectedCty = projected
		if co.Next == workflow.ReturnSentinel {
			st.ReturnOutputs = projected
		}
	}

	// Apply write blocks for the aggregate outcome if declared.
	if len(co.Writes) > 0 && st.DataStore != nil {
		if err := applyDataWrites(stepName, aggregateOutcome, co.Writes, aggregateProjectedCty, nil, nil, st, sink); err != nil {
			return "", err
		}
	}

	return co.Next, nil
}

// seedRunVars returns the restored scope unchanged for resumed runs. For fresh
// runs it seeds from graph defaults, applies any CLI overrides, and emits
// OnVariableSet events.
func (e *Engine) seedRunVars(sink Sink) (map[string]cty.Value, error) {
	if e.resumedVars != nil {
		return e.seedResumedVars(), nil
	}
	return e.seedFreshVars(sink)
}

func (e *Engine) seedResumedVars() map[string]cty.Value {
	// Locals are compile-time constants that are never persisted in the
	// scope snapshot. Always reseed them from the current graph so that
	// resumed runs have the same local.* bindings as fresh runs.
	resumed := make(map[string]cty.Value, len(e.resumedVars)+1)
	for k, v := range e.resumedVars {
		resumed[k] = v
	}
	resumed["local"] = workflow.SeedLocalsFromGraph(e.graph)
	return resumed
}

func (e *Engine) seedFreshVars(sink Sink) (map[string]cty.Value, error) {
	vars := workflow.SeedVarsFromGraph(e.graph)
	vars["local"] = workflow.SeedLocalsFromGraph(e.graph)
	if len(e.varOverrides) > 0 {
		var err error
		vars, err = workflow.ApplyVarOverrides(e.graph, vars, e.varOverrides)
		if err != nil {
			return nil, err
		}
	}
	e.emitVarSetEvents(vars, sink)
	return vars, nil
}

func (e *Engine) emitVarSetEvents(vars map[string]cty.Value, sink Sink) {
	varObj := vars["var"]
	for name, node := range e.graph.Variables {
		var source string
		if _, ok := e.varOverrides[name]; ok {
			source = "override"
		} else if node.Default != cty.NilVal {
			source = "default"
		} else {
			continue
		}
		// Read the value back from the run scope so the event matches what
		// downstream expressions actually observe.
		val := e.varValueFromScope(varObj, name, node)
		display := "(sensitive)"
		if !node.Secret {
			display = workflow.CtyValueForDisplay(val)
		}
		sink.OnVariableSet(name, display, source)
	}
}

func (e *Engine) varValueFromScope(varObj cty.Value, name string, node *workflow.VariableNode) cty.Value {
	if varObj != cty.NilVal && varObj.Type().IsObjectType() && varObj.Type().HasAttribute(name) {
		return varObj.GetAttr(name)
	}
	// By the time emitVarSetEvents runs, SeedVarsFromGraph/ApplyVarOverrides
	// have guaranteed that every declared variable exists as an attribute in
	// vars["var"], so this fallback is defensive.
	return node.Default
}

// buildDeps constructs the Deps bundle injected into each node's Evaluate call.
func (e *Engine) buildDeps(sessions *adapterhost.SessionManager, sink Sink) Deps {
	return Deps{
		Sessions:            sessions,
		Loader:              e.loader,
		Sink:                sink,
		SubWorkflowResolver: e.subWorkflowResolver,
		BranchScheduler:     e.branchScheduler,
	}
}

// advanceTo sets st.Current to next, moving the run forward to the next node.
func (e *Engine) advanceTo(st *RunState, next string) {
	st.Current = next
}

// handleEvalError dispatches errors from node.Evaluate. It handles ErrTerminal
// and ErrPaused specially; all other errors are propagated as run failures.
func (e *Engine) handleEvalError(st *RunState, err error, sink Sink) error {
	// Capture the visit state and clear the live pointer so VisitCounts()
	// returns a stable snapshot after the run ends (W07).
	e.liveRunState = nil
	e.lastVisits = st.Visits
	if errors.Is(err, engineruntime.ErrTerminal) {
		state, ok := e.graph.States[st.Current]
		if !ok {
			missing := fmt.Errorf("terminal node %q is not a state", st.Current)
			sink.OnRunFailed(missing.Error(), st.Current)
			return missing
		}
		// Evaluate outputs at terminal state (W09).
		outputs, outErr := evalRunOutputs(e.graph, st)
		if outErr != nil {
			// Output evaluation failed; emit error and fail the run.
			sink.OnRunFailed(outErr.Error(), st.Current)
			return outErr
		}
		// Emit outputs before run.completed if present.
		if len(outputs) > 0 {
			sink.OnRunOutputs(outputs)
		}
		sink.OnRunCompleted(state.Name, state.Success)
		return nil
	}
	if errors.Is(err, engineruntime.ErrPaused) {
		// The node has already set st.PendingSignal and emitted WaitEntered/
		// ApprovalRequested. Notify the sink so it can update run status and
		// then yield control back to the orchestrator.
		mode := "signal"
		if wait, ok := e.graph.Waits[st.Current]; ok && wait.Duration > 0 {
			mode = "duration"
		}
		e.lastVars = st.Vars
		sink.OnRunPaused(st.Current, mode, st.PendingSignal)
		return nil
	}
	sink.OnRunFailed(err.Error(), st.Current)
	return err
}

// handleReturnExit handles top-level runs that exit via next = step.return.
// The projected outputs in st.ReturnOutputs are emitted as OnRunOutputs
// (if non-empty) and the run is completed successfully with no named final state.
func (e *Engine) handleReturnExit(st *RunState, sink Sink) {
	e.liveRunState = nil
	e.lastVisits = st.Visits

	if len(st.ReturnOutputs) > 0 {
		outputs := formatReturnOutputs(st.ReturnOutputs)
		if len(outputs) > 0 {
			sink.OnRunOutputs(outputs)
		}
	}
	sink.OnRunCompleted("", true)
}

// formatReturnOutputs converts the ReturnOutputs cty.Value map to the
// []map[string]string tuple format expected by OnRunOutputs.
func formatReturnOutputs(returnOutputs map[string]cty.Value) []map[string]string {
	if len(returnOutputs) == 0 {
		return nil
	}
	out := make([]map[string]string, 0, len(returnOutputs))
	for name, val := range returnOutputs {
		rendered, err := renderCtyValue(val)
		if err != nil {
			rendered = fmt.Sprintf("%v", val)
		}
		out = append(out, map[string]string{
			"name":          name,
			"value":         rendered,
			"declared_type": "",
		})
	}
	return out
}

func cloneVisits(v map[string]int) map[string]int {
	if v == nil {
		return nil
	}
	out := make(map[string]int, len(v))
	for k, c := range v {
		out[k] = c
	}
	return out
}

func (e *Engine) bootstrapSessionsForResume(ctx context.Context, sessions *adapterhost.SessionManager, startStep string) error {
	// Sessions are process-local and do not survive adapter restarts.
	// With automatic lifecycle management (W12), adapters are provisioned at scope start.
	// Crash recovery no longer needs to replay lifecycle steps since there are no longer
	// any explicit lifecycle="open"/"close" steps. This function is kept for compatibility
	// but does nothing.
	return nil
}

// maybeStartRemoteShim checks whether the workflow references any remote
// environments. If so, it parses each remote env config, builds a shim, and
// starts listening for inbound adapter connections before adapter provisioning.
func (e *Engine) maybeStartRemoteShim(ctx context.Context, sessions *adapterhost.SessionManager) error {
	if e.graph == nil || len(e.graph.Environments) == 0 {
		return nil
	}
	var remoteEnvs []*workflow.EnvironmentNode
	for _, env := range e.graph.Environments {
		if env.Type == "remote" {
			remoteEnvs = append(remoteEnvs, env)
		}
	}
	if len(remoteEnvs) == 0 {
		return nil
	}

	lf := e.lockfile
	verifier := &lockfileDigestVerifier{lockfile: lf}

	for _, env := range remoteEnvs {
		cfg, err := remote.ParseConfig(env.RawBody)
		if err != nil {
			return fmt.Errorf("remote environment %q: %w", env.Name, err)
		}
		shim, err := remote.NewShim(cfg, verifier)
		if err != nil {
			return fmt.Errorf("remote environment %q: %w", env.Name, err)
		}
		if err := shim.Start(ctx); err != nil {
			return fmt.Errorf("remote environment %q: %w", env.Name, err)
		}
		sessions.SetRemoteShim(shim)
	}
	return nil
}

// lockfileDigestVerifier implements remote.DigestVerifier using the workflow
// lockfile to validate adapter digests.
//
// For remote adapters the digest is the runtime trust anchor: the artifact's
// signature is verified at pull/lock time and during the apply auto-pull
// (internal/cli.verifyAgainstPin, which builds a verify policy from the
// lockfile-pinned signer via policyForPin and confirms it with
// assertSignerMatchesPin). A remote adapter presents only its type and digest
// over mTLS — no signature material is available here — so this verifier
// enforces the pinned digest, which binds the connection to the exact verified
// bytes recorded in the lockfile.
type lockfileDigestVerifier struct {
	lockfile *lockfile.Lockfile
}

func (v *lockfileDigestVerifier) Verify(adapterType, digest string) error {
	if v.lockfile == nil {
		return fmt.Errorf("no lockfile available")
	}
	for i := range v.lockfile.Adapters {
		a := &v.lockfile.Adapters[i]
		if a.Type == adapterType {
			if a.ResolvedDigest == digest {
				return nil
			}
			return fmt.Errorf("digest mismatch for adapter %q: got %q, want %q", adapterType, digest, a.ResolvedDigest)
		}
	}
	return fmt.Errorf("adapter %q not found in lockfile", adapterType)
}

// ErrCancelled is returned when the run context is cancelled mid-step.
var ErrCancelled = errors.New("run cancelled")
