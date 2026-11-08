package engine

// node_workflow.go — sub-workflow body execution helper for type="workflow"
// steps (W10). The body is an independently compiled FSMGraph with a synthetic
// "_continue" terminal state. The engine runs the body in a nested loop until
// it reaches a terminal state. If that terminal state is "_continue", the
// caller treats it as a normal iteration-advance; any other terminal state is
// an early-exit and signals the iteration to stop.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/zclconf/go-cty/cty"

	engineruntime "github.com/brokenbots/criteria/internal/engine/runtime"
	"github.com/brokenbots/criteria/workflow"
)

// checkRequiredVars returns an error if any required body variable (no default)
// lacks a binding in parentInput, or if its binding is null. This is the
// runtime complement to the compile-time check in compileWorkflowStep.
func checkRequiredVars(body *workflow.FSMGraph, parentInput cty.Value) error {
	hasInput := parentInput != cty.NilVal && parentInput.IsKnown() && !parentInput.IsNull() && parentInput.Type().IsObjectType()
	var missing []string
	for name, node := range body.Variables {
		if !node.IsRequired() {
			continue
		}
		supplied := hasInput && parentInput.Type().HasAttribute(name) && !parentInput.GetAttr(name).IsNull()
		if !supplied {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("workflow body missing required input(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

// runWorkflowBody executes the sub-workflow body synchronously in a nested
// engine loop. It returns the terminal state name, any return-projected outputs
// (non-nil when the body exited via "return" sentinel), and the child's final
// vars when the body reaches a terminal state, or an error on fatal conditions.
//
//   - body is the compiled FSMGraph of the sub-workflow body.
//   - bodyEntry is the initial state name for the body run.
//   - childVars is the pre-seeded child scope built by seedChildVarsFromBindings.
//   - workflowDir is forwarded for file() resolution in eval contexts.
//   - deps carries the same session manager and event sink as the outer loop.
//   - parallelCeiling and parallelSem carry the inherited parallel_max ceiling
//     from any enclosing parallel step (zero/nil if none).
//
// When the body reaches "_continue" the caller should treat the iteration as
// successfully completed and advance the cursor. Any other terminal state is
// an early-exit from the iteration; the caller should forward that outcome.
// When terminal == "return", returnOutputs carries the projected output values;
// the caller should skip normal output evaluation.
//
// The returned child vars represent the body's final execution scope and are
// used by the caller to evaluate output{} block expressions.
func runWorkflowBody(ctx context.Context, body *workflow.FSMGraph, bodyEntry string, childVars map[string]cty.Value, workflowDir string, deps Deps, rlc *remoteLifecycleContext, scopeName string, parallelCeiling int, parallelSem chan struct{}, parallelSemCache map[parallelSemKey]chan struct{}, parallelSemMu *sync.Mutex, crashedSessions *crashedSessionRefs, ancestors ...string) (terminal string, returnOutputs, finalVars map[string]cty.Value, err error) {
	bodyEntry, err = resolveBodyEntry(body, bodyEntry)
	if err != nil {
		return "", nil, nil, err
	}

	if diags := workflow.CheckGraphCriteriaVersion(body, ancestors); diags.HasErrors() {
		return "", nil, nil, fmt.Errorf("%s", diags.Error())
	}

	bodyOrder, childSt, err := startWorkflowBody(ctx, body, bodyEntry, childVars, workflowDir, deps, rlc, scopeName, parallelCeiling, parallelSem, parallelSemCache, parallelSemMu, crashedSessions, ancestors, rlc.detachedRecords())
	if err != nil {
		return "", nil, nil, err
	}
	// Body entry runs the declared-fate hook inside initScopeAdapters: a
	// body re-declaring an adapter instance with continuation = "fresh"
	// releases the engagement an ancestor still owns for that instance
	// (root-scope record or ledger record) BEFORE provisioning its own
	// conversation, so the re-entry never inherits the outer conversation.
	//
	// Body exit then disposes each body-owned instance per its DECLARED
	// session fate (ADR-0014 D3): an instance the body declares
	// continuation = "fresh" is released at exit — close + unregister +
	// tombstone, FOR THAT INSTANCE ONLY. That is the declaration-guarded,
	// selective return of the release the churn kill removed from body
	// exits; the universal release that manufactured the teardown→rotate→
	// re-dial handshake-race class stays dead. Every other instance keeps
	// the churn-kill detach: the live
	// engagement survives the exit for a re-entry (or the
	// run's final teardown) to adopt.
	defer func() { exitBodyScopeAdapters(ctx, body, scopeName, bodyOrder, deps, rlc) }()

	return runWorkflowBodyLoop(ctx, body, childSt, deps)
}

// exitBodyScopeAdapters partitions the body's adapter order by the BODY
// graph's declared session fate and disposes each partition: fresh-declared
// instances release (releaseScopeAdapters), everything else keeps the
// churn-kill detach path. The partition only sees the body's own records —
// a re-declared PARENT instance's per-scope record lives under the parent's
// scope name and is never body-owned; the declared-fate entry hook
// (releaseForeignScopeSessionsForFresh) released that inherited engagement
// when the body entered, so the exit release stays body-owned too.
func exitBodyScopeAdapters(ctx context.Context, body *workflow.FSMGraph, scopeName string, order []string, deps Deps, rlc *remoteLifecycleContext) {
	var freshOrder, detachOrder []string
	for _, adapterID := range order {
		if ad := body.Adapters[adapterID]; ad != nil && ad.Continuation == workflow.ContinuationFresh {
			freshOrder = append(freshOrder, adapterID)
			continue
		}
		detachOrder = append(detachOrder, adapterID)
	}
	// Fates partition into disjoint instance sets, and each helper releases
	// LIFO within its own set, so the two exit phases cannot re-order the
	// other's instances.
	releaseScopeAdapters(ctx, scopeName, freshOrder, deps, rlc)
	detachBodyScopeAdapters(ctx, scopeName, detachOrder, deps, rlc)
}
func resolveBodyEntry(body *workflow.FSMGraph, bodyEntry string) (string, error) {
	if bodyEntry == "" {
		bodyEntry = body.InitialState
	}
	if bodyEntry == "" {
		return "", fmt.Errorf("workflow body has no initial state")
	}
	return bodyEntry, nil
}

func startWorkflowBody(ctx context.Context, body *workflow.FSMGraph, bodyEntry string, childVars map[string]cty.Value, workflowDir string, deps Deps, rlc *remoteLifecycleContext, scopeName string, parallelCeiling int, parallelSem chan struct{}, parallelSemCache map[parallelSemKey]chan struct{}, parallelSemMu *sync.Mutex, crashedSessions *crashedSessionRefs, ancestors []string, detached map[string]*adapterLifecycleRecord) ([]string, *RunState, error) {
	// Subworkflow secret origins are not independently tracked today; the
	// child scope already carries resolved values from the parent. Passing nil
	// means adapter session snapshots for child-scope secrets fall back to an
	// untracked literal origin. This is acceptable for bodies that do not pause
	// mid-execution with active adapter sessions.
	bodyOrder, err := initScopeAdapters(ctx, body, deps, childVars, workflowDir, scopeName, nil, rlc)
	if err != nil {
		return nil, nil, fmt.Errorf("workflow body init adapters: %w", err)
	}

	if parallelSemCache == nil {
		parallelSemCache = make(map[parallelSemKey]chan struct{})
	}
	if parallelSemMu == nil {
		parallelSemMu = &sync.Mutex{}
	}

	childSt := &RunState{
		Current:          bodyEntry,
		Vars:             childVars,
		WorkflowDir:      workflowDir,
		PendingSignal:    "",
		ResumePayload:    nil,
		DataStore:        NewDataStore(body),
		RemoteLifecycle:  rlc,
		firstStep:        false,
		WorkflowName:     body.Name,
		Ancestors:        ancestors,
		ParallelCeiling:  parallelCeiling,
		ParallelSem:      parallelSem,
		ParallelSemCache: parallelSemCache,
		ParallelSemMu:    parallelSemMu,
		// The crashed-session set is shared by reference so a crash
		// recorded anywhere in the run is re-opened by follow-on steps
		// wherever they execute — parallel iteration states and subworkflow
		// bodies included.
		CrashedSessions: crashedSessions,
	}
	// Re-entry reuse: hand the detached records back to a body that
	// continues the same engagements. Re-adopting each record into this
	// scope's lifecycle means (a) phase-A's maybeRotateRemoteScope finds
	// the persisted instance record for the SAME scope instance via
	// tryReuseScopeInstance — no rotation, token stays live, NO re-dial and
	// therefore NO handshake race — and (b) this body's exit re-detaches
	// the record (keeping the engagement alive) instead of releasing it.
	// A nil/empty detached map is a body whose scope owns fresh sessions
	// (first entry, or a body re-declaring a PARENT adapter — the parent's
	// records never enter the detachment ledger — and sibling bodies'
	// records have their own scope names).
	//
	// adoptDetached re-checks ledger membership because this snapshot may
	// be stale by now: a declared-fresh body entry released its record for
	// this very instance out of the ledger while initScopeAdapters ran.
	if rlc != nil && len(detached) > 0 {
		for _, rec := range detached {
			if rec != nil && rec.scopeName == scopeName {
				rlc.adoptDetached(rec)
			}
		}
	}
	return bodyOrder, childSt, nil
}

func runWorkflowBodyLoop(ctx context.Context, body *workflow.FSMGraph, childSt *RunState, deps Deps) (terminal string, returnOutputs, finalVars map[string]cty.Value, err error) {
	for {
		node, err := nodeFor(body, childSt.Current)
		if err != nil {
			return "", nil, nil, fmt.Errorf("workflow body: %w", err)
		}
		next, err := node.Evaluate(ctx, childSt, deps)
		if err != nil {
			if errors.Is(err, engineruntime.ErrTerminal) {
				if childSt.DataStore != nil {
					childSt.Vars = workflow.SeedDataSnapshot(childSt.Vars, childSt.DataStore.Snapshot())
				}
				return childSt.Current, nil, childSt.Vars, nil
			}
			return "", nil, nil, fmt.Errorf("workflow body step %q: %w", childSt.Current, err)
		}
		// Apply iteration routing for any for_each/count steps inside the body.
		next, err = routeIteratingStepInGraph(childSt, next, body, deps.Sink)
		if err != nil {
			return "", nil, nil, fmt.Errorf("workflow body step %q: iteration: %w", childSt.Current, err)
		}
		if next == workflow.ReturnSentinel {
			return workflow.ReturnSentinel, childSt.ReturnOutputs, childSt.Vars, nil
		}
		childSt.Current = next
	}
}
