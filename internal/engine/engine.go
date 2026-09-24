// Package engine drives a workflow's FSM. It is pure: dispatcher (adapter
// lookup) and event sink are injected so the engine can be tested without I/O.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/zclconf/go-cty/cty"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapter/environment/remote"
	"github.com/brokenbots/criteria/internal/adapter/environment/sandbox"
	"github.com/brokenbots/criteria/internal/adapter/secrets"
	"github.com/brokenbots/criteria/internal/adapterhost"
	engineruntime "github.com/brokenbots/criteria/internal/engine/runtime"
	"github.com/brokenbots/criteria/internal/runtime/state"
	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

// AdapterLifecycleEvent carries the controller-visible state needed to
// provision or release a remote adapter pod. Raw secrets must never appear in
// this payload with one deliberate exception (CRI-236): the per-scope accept
// token rides the already-authenticated provision/accept wire so operators do
// not need a criteria-side shared volume to read it from the token_ref file.
// TokenRef stays populated during the transition window so file-based
// operators keep working (removed operator-side in CRI-237).
type AdapterLifecycleEvent struct {
	RunID             string
	ScopeName         string // empty for the root scope
	ScopeInstanceID   string // unique UUID for this scope invocation
	AdapterName       string
	AdapterType       string // adapter implementation kind (e.g. "shell", "copilot")
	Digest            string // lockfile-pinned digest
	ShimListenAddress string
	TokenRef          string // path to the accept-token file; transition-window handoff
	Token             string // wire-delivered per-scope accept token; empty for released events
	Status            string // "provision_wanted" or "released"
	// Environment identity of the adapter session (CRI-233): the compiled
	// environment declaration's type + name from the workflow's environment
	// blocks. Additive fields; consumers must tolerate older events that
	// carry neither.
	EnvironmentType string // e.g. "remote"
	EnvironmentName string // declaration name, e.g. "prod"
}

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
	// OnAdapterLifecycleEvent is emitted when the engine wants a remote adapter
	// scope to be provisioned or released. It contains only controller-visible,
	// non-secret metadata including a token file reference, never the raw token.
	OnAdapterLifecycleEvent(event *AdapterLifecycleEvent)
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
	// OnAgentPromptInjected is emitted exactly once, at the moment an injected
	// agent prompt is delivered into the addressed step's live adapter session
	// (ADR-0006 D5). It is never emitted at receipt and never on delivery
	// failure; failures are recorded as structured logs instead.
	OnAgentPromptInjected(step, sessionID, prompt, caller string, deliveredAt time.Time)
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
	// secretOrigins records the provider OriginRef for each declared secret
	// variable so that adapter session snapshots can re-resolve values after
	// resume without persisting raw secrets.
	secretOrigins map[string]secrets.OriginRef
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
	// CRI-115: dataDir is the per-run data directory used for rotated remote
	// adapter accept-token files. Empty disables per-scope token rotation.
	dataDir string
	// CRI-169: pauseToolCallDrainTimeout is the bounded wait the drain-first
	// pause posture gives in-flight nested adapter tool calls before
	// canceling them. Zero means the SessionManager default (60s).
	pauseToolCallDrainTimeout time.Duration

	// WS17: liveSessions holds the active SessionManager while a run is in
	// progress, enabling Pause/Resume/Inspect from outside runLoop.
	liveSessions *adapterhost.SessionManager
	// livePrompts holds the active PromptRouter while a run is in progress.
	// Tests read it to assert router balance directly (CRI-259); production
	// code only routes through Deps.Prompts.
	livePrompts *PromptRouter
	mu          sync.RWMutex
	// workingDirAllowedRoots restricts environment working_directory values at
	// run start. A resolved path that is not under one of these roots (when any
	// are configured) or that contains ".." is rejected eagerly during adapter
	// verification, before any step runs. Empty means no additional root checks.
	workingDirAllowedRoots []string

	// sandboxProbeOverride, when non-nil, is propagated to the SessionManager so
	// tests can simulate a host with missing sandbox primitives.
	sandboxProbeOverride func() sandbox.Capabilities

	// localShimIsolation enables CRI-293 shim address isolation: when two or
	// more remote environments declare the same fixed listen_address, each
	// environment's shim binds its own auto-chosen loopback port instead of
	// colliding (local runs bind every shim in one process; server runs use
	// one adapter pod per environment and never collide). Set only by local
	// run entrypoints via WithLocalShimIsolation.
	localShimIsolation bool

	// CRI-304: adoptableRunDirs lists prior invocations' run data directories
	// whose surviving per-scope adapter instances may be adopted instead of
	// rotating fresh. The CLI populates it from persisted invocation-identity
	// markers for replays that follow a checkpoint-consuming resume; empty
	// leaves fresh rotation untouched.
	adoptableRunDirs []string

	// ADR-0006 (CRI-259): agentPromptCh is this run's injected-prompt channel,
	// fed by the CLI from the orchestrator's Control stream. promptOwnerID is
	// the run owner identity used by the delivery-side caller re-check (D4).
	// promptRunID is the run id AgentPrompt messages are addressed to; it
	// defaults to the snapshot run id when unset.
	// A nil channel disables the prompt path entirely.
	agentPromptCh <-chan *pb.AgentPrompt
	promptOwnerID string
	promptRunID   string
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
	sessions.SetLockfile(e.effectivePinSet())
	sessions.RedactionRegistry = secrets.NewRegistry()
	sessions.LifecycleSink = e.sink
	sessions.SetAllowedWorkingDirRoots(e.workingDirAllowedRoots)
	sessions.PauseToolCallDrainTimeout = e.pauseToolCallDrainTimeout
	if e.sandboxProbeOverride != nil {
		sessions.SetSandboxProbeOverride(e.sandboxProbeOverride)
	}
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

// livePromptRouter returns the PromptRouter of the running run, or nil
// outside a run. Tests use it to assert router balance directly (CRI-259);
// production code routes only through Deps.Prompts.
func (e *Engine) livePromptRouter() *PromptRouter {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.livePrompts
}

// setLiveRunState records the run's live session manager and prompt router;
// clearLiveRunState drops both when the run ends.
func (e *Engine) setLiveRunState(sessions *adapterhost.SessionManager, prompts *PromptRouter) {
	e.mu.Lock()
	e.liveSessions = sessions
	e.livePrompts = prompts
	e.mu.Unlock()
}

func (e *Engine) clearLiveRunState() {
	e.mu.Lock()
	e.liveSessions = nil
	e.livePrompts = nil
	e.mu.Unlock()
}

// effectivePinSet resolves the run's effective lockfile by one shared rule:
// the compiled graph's merged pin set wins when present, and the explicit
// WithLockfile value is the fallback. The merged pin set is built once at
// compile time from every workflow directory's .criteria.lock.hcl — including
// the fetched tree of a URL-sourced workflow, where WithLockfile is never set
// (CRI-263) — and is the single source of truth for the run; no workflow files
// are read here. Every consumer of pinned digests (session manager, remote
// shim verifier, remote lifecycle context) must resolve through this helper so
// the paths cannot disagree.
func (e *Engine) effectivePinSet() *lockfile.Lockfile {
	if e.graph != nil && e.graph.PinSet != nil {
		return e.graph.PinSet
	}
	return e.lockfile
}

// setLockfileOnSessions ensures the session manager has the effective pin set.
func (e *Engine) setLockfileOnSessions(sessions *adapterhost.SessionManager) {
	if lf := e.effectivePinSet(); lf != nil {
		sessions.SetLockfile(lf)
	}
}

// seedRunScope seeds variables, resolves declared secret data blocks, and
// returns the variable scope plus the populated DataStore. This helper keeps
// the fresh-run and resume-run setup paths identical (CRI-88).
func (e *Engine) seedRunScope(ctx context.Context, sink Sink, reg *secrets.Registry) (map[string]cty.Value, *DataStore, error) {
	vars, err := e.seedRunVars(ctx, sink, reg)
	if err != nil {
		return nil, nil, err
	}

	ds := NewDataStore(e.graph)
	if err := e.resolveSecretDataOrigins(ctx, ds, reg); err != nil {
		return nil, nil, err
	}
	vars = workflow.SeedDataSnapshot(vars, ds.Snapshot())
	return vars, ds, nil
}

// initAdapters verifies the graph and provisions scope-level adapter sessions.
// On failure it emits OnRunFailed and returns a wrapped error. The caller owns
// the returned teardown order.
func (e *Engine) initAdapters(ctx context.Context, sessions *adapterhost.SessionManager, sink Sink, vars map[string]cty.Value, failStep string) (Deps, []string, *remoteLifecycleContext, error) {
	// CRI-115: remote environments that enable per_scope_sessions rotate the
	// accept token per scope. Defer their adapter-info verification from
	// VerifyGraph to initScopeAdapters so the provisioning event and token
	// rotation happen before the shim blocks for the adapter phone-home.
	if e.graph != nil {
		if deferredRemote := deferredPerScopeRemoteAdapters(e.graph); len(deferredRemote) > 0 {
			sessions.SetDeferredRemoteAdapters(deferredRemote)
		}
	}

	if err := sessions.VerifyGraph(ctx, e.graph, vars); err != nil {
		sink.OnRunFailed(err.Error(), failStep)
		return Deps{}, nil, nil, err
	}

	lifecycle := newScopeLifecycleState(e.dataDir)
	lifecycle.adoptableRunDirs = e.adoptableRunDirs
	lifecycle.setRunID(e.runID)
	rlc := &remoteLifecycleContext{
		lockfile:       e.effectivePinSet(),
		scopeLifecycle: lifecycle,
	}
	deps := Deps{Sessions: sessions, Sink: sink}
	scopeOrder, err := initScopeAdapters(ctx, e.graph, deps, vars, e.workflowDir, "", e.secretOrigins, rlc)
	if err != nil {
		sink.OnRunFailed(err.Error(), failStep)
		return Deps{}, nil, nil, err
	}
	return deps, scopeOrder, rlc, nil
}

// Run executes the workflow until a terminal state is reached, the global
// step limit is exceeded, or ctx is cancelled.
//
// CRI-304: every init failure before runLoop (workflow version gate, variable
// seeding, remote shim startup, adapter provisioning) emits OnRunFailed so
// the control-plane run record reaches a terminal status; previously only
// runLoop errors and initAdapters did, leaving provisioning failures
// non-terminal (an observed crash-replay stayed "running" forever).
func (e *Engine) Run(ctx context.Context) error {
	sessions := adapterhost.NewSessionManager(e.loader)
	sessions.SetGraph(e.graph)
	sessions.Audit = e.auditWriter
	sessions.PauseToolCallDrainTimeout = e.pauseToolCallDrainTimeout
	if e.sandboxProbeOverride != nil {
		sessions.SetSandboxProbeOverride(e.sandboxProbeOverride)
	}
	e.setLockfileOnSessions(sessions)
	defer func() { _ = sessions.Shutdown(context.WithoutCancel(ctx)) }()

	// Create a per-run redaction registry and wire it into the session manager
	// and the engine sink so all secret values are masked before display or
	// persistence.
	redactionReg := secrets.NewRegistry()
	sessions.RedactionRegistry = redactionReg

	// Wrap the engine sink before any events are emitted.
	sink := NewRedactingSink(e.sink, redactionReg)
	sessions.LifecycleSink = sink
	sessions.SetAllowedWorkingDirRoots(e.workingDirAllowedRoots)

	// CRI-304: failRunInit emits OnRunFailed for a pre-runLoop init failure;
	// initAdapters emits its own, so it is deliberately excluded.
	failRunInit := func(err error) {
		sink.OnRunFailed(err.Error(), "")
	}

	if diags := workflow.CheckGraphCriteriaVersion(e.graph, nil); diags.HasErrors() {
		err := fmt.Errorf("%s", diags.Error())
		failRunInit(err)
		return err
	}

	// Seed variables and declared secret data blocks before adapter provisioning
	// so secret expressions can be evaluated against the run scope (WS13, CRI-88).
	vars, ds, err := e.seedRunScope(ctx, sink, redactionReg)
	if err != nil {
		failRunInit(err)
		return err
	}

	// WS20: if any environment is remote, start the phone-home shim before
	// provisioning adapters.
	if err := e.maybeStartRemoteShim(ctx, sessions); err != nil {
		failRunInit(err)
		return err
	}

	deps, scopeOrder, rlc, err := e.initAdapters(ctx, sessions, sink, vars, "")
	if err != nil {
		return err
	}
	defer func() { tearDownScopeAdapters(ctx, scopeOrder, deps, rlc) }()

	current := e.graph.InitialState
	sink.OnRunStarted(e.graph.Name, current)
	return e.runLoop(ctx, sessions, current, 1, vars, sink, ds, rlc)
}

// RunFrom resumes a workflow at startStep with the given initialAttempt
// number as the first attempt for that step (subsequent retries increment
// from there). It does NOT emit OnRunStarted (the run already started).
// If initialAttempt would already exceed max_step_retries, it emits
// OnRunFailed instead of attempting the step.
// Adapter sessions are provisioned fresh on each run (resumed or not),
// allowing the workflow to be resumed in a new process context.
//
// CRI-304: like Run, every init failure before runLoop emits OnRunFailed so
// a resumed run's record reaches a terminal status even when provisioning or
// shim startup fails.
func (e *Engine) RunFrom(ctx context.Context, startStep string, initialAttempt int) error {
	sessions := adapterhost.NewSessionManager(e.loader)
	sessions.SetGraph(e.graph)
	sessions.Audit = e.auditWriter
	sessions.PauseToolCallDrainTimeout = e.pauseToolCallDrainTimeout
	if e.sandboxProbeOverride != nil {
		sessions.SetSandboxProbeOverride(e.sandboxProbeOverride)
	}
	e.setLockfileOnSessions(sessions)
	defer func() { _ = sessions.Shutdown(context.WithoutCancel(ctx)) }()

	redactionReg := secrets.NewRegistry()
	sessions.RedactionRegistry = redactionReg

	sink := NewRedactingSink(e.sink, redactionReg)
	sessions.LifecycleSink = sink
	sessions.SetAllowedWorkingDirRoots(e.workingDirAllowedRoots)

	// CRI-304: failRunInit emits OnRunFailed for a pre-runLoop init failure;
	// initAdapters emits its own, so it is deliberately excluded.
	failRunInit := func(err error) {
		sink.OnRunFailed(err.Error(), startStep)
	}

	if diags := workflow.CheckGraphCriteriaVersion(e.graph, nil); diags.HasErrors() {
		err := fmt.Errorf("%s", diags.Error())
		failRunInit(err)
		return err
	}

	vars, ds, err := e.seedRunScope(ctx, sink, redactionReg)
	if err != nil {
		failRunInit(err)
		return err
	}

	// WS20: if any environment is remote, start the phone-home shim before
	// provisioning adapters.
	if err := e.maybeStartRemoteShim(ctx, sessions); err != nil {
		failRunInit(err)
		return err
	}

	deps, scopeOrder, rlc, err := e.initAdapters(ctx, sessions, sink, vars, startStep)
	if err != nil {
		return err
	}
	defer func() { tearDownScopeAdapters(ctx, scopeOrder, deps, rlc) }()

	if err := e.bootstrapSessionsForResume(ctx, sessions, startStep); err != nil {
		failRunInit(err)
		return err
	}
	return e.runLoop(ctx, sessions, startStep, initialAttempt, vars, sink, ds, rlc)
}

// runLoop is the shared execution loop. firstStepAttempt is the attempt index
// used for the initial step when resuming; subsequent steps start at attempt 1.
func (e *Engine) runLoop(ctx context.Context, sessions *adapterhost.SessionManager, current string, firstStepAttempt int, vars map[string]cty.Value, sink Sink, ds *DataStore, rlc *remoteLifecycleContext) error {
	st := &RunState{
		Current:                current,
		Vars:                   vars,
		PendingSignal:          e.pendingSignal,
		ResumePayload:          e.resumePayload,
		IterStack:              append([]workflow.IterCursor{}, e.resumedIterStack...),
		Visits:                 cloneVisits(e.resumedVisits),
		WorkflowDir:            e.workflowDir,
		DataStore:              ds,
		WorkflowName:           e.graph.Name,
		RemoteLifecycle:        rlc,
		CrashedCommentSessions: newCrashedSessionRefs(),
		// CRI-271: functional-step crash registry for re-open-before-follow-on.
		CrashedFunctionalSessions: newCrashedSessionRefs(),
		firstStep:                 true,
		firstStepAttempt:          firstStepAttempt,
	}
	prompts := NewPromptRouter(ctx, e.agentPromptCh, e.effectivePromptRunID(), e.promptOwnerID, e.graph, sessions, sink, e.logOrDefault())
	deps := e.buildDeps(sessions, sink, prompts)
	defer prompts.Stop()

	defer e.clearLiveRunState()
	e.setLiveRunState(sessions, prompts)

	e.liveRunState = st
	for {
		node, err := nodeFor(e.graph, st.Current)
		if err != nil {
			sink.OnRunFailed(err.Error(), st.Current)
			return err
		}
		next, err := node.Evaluate(ctx, st, deps)
		if err != nil {
			return e.handleEvalError(ctx, st, err, sink)
		}
		next, err = e.routeIteratingStep(st, next, sink)
		if err != nil {
			return e.handleEvalError(ctx, st, err, sink)
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
// runs it seeds from graph defaults, applies any CLI overrides, resolves secret
// variable provider origins, and emits OnVariableSet events.
func (e *Engine) seedRunVars(ctx context.Context, sink Sink, reg *secrets.Registry) (map[string]cty.Value, error) {
	if e.resumedVars != nil {
		return e.seedResumedVars(), nil
	}
	return e.seedFreshVars(ctx, sink, reg)
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

func (e *Engine) seedFreshVars(ctx context.Context, sink Sink, reg *secrets.Registry) (map[string]cty.Value, error) {
	vars := workflow.SeedVarsFromGraph(e.graph)
	vars["local"] = workflow.SeedLocalsFromGraph(e.graph)
	if len(e.varOverrides) > 0 {
		var err error
		vars, err = workflow.ApplyVarOverrides(e.graph, vars, e.varOverrides)
		if err != nil {
			return nil, err
		}
	}
	if reg != nil {
		var err error
		vars, err = e.resolveSecretVarOrigins(ctx, vars, reg)
		if err != nil {
			return nil, err
		}
	}
	e.emitVarSetEvents(vars, sink)
	return vars, nil
}

// resolveSecretVarOrigins walks declared secret variables and resolves any
// string value that looks like a provider reference (env:NAME, file:path,
// etc.) through the workflow's default environment provider stack. The
// resolved value replaces the provider-ref string in vars["var"] and is
// registered for redaction. The original OriginRef is stored in
// e.secretOrigins so that adapter session snapshots can re-resolve the value on
// resume without persisting the raw secret.
func (e *Engine) resolveSecretVarOrigins(ctx context.Context, vars map[string]cty.Value, reg *secrets.Registry) (map[string]cty.Value, error) {
	varObj := vars["var"]
	if varObj == cty.NilVal || !varObj.Type().IsObjectType() || len(e.graph.Variables) == 0 {
		return vars, nil
	}

	stack, err := secrets.StackFromEnvironment(getEnvironmentNode(e.graph, ""))
	if err != nil {
		return nil, fmt.Errorf("resolve secret variable origins: %w", err)
	}

	attrs := varObj.AsValueMap()
	resolved := make(map[string]cty.Value, len(attrs))
	origins := make(map[string]secrets.OriginRef, len(e.graph.Variables))
	for name, node := range e.graph.Variables {
		val, ok := attrs[name]
		if !ok {
			resolved[name] = cty.NullVal(node.Type)
			continue
		}
		if !node.Secret || val.IsNull() || !val.IsKnown() || val.Type() != cty.String {
			resolved[name] = val
			continue
		}
		raw := val.AsString()
		ref := secrets.ParseOriginRef(raw)
		var final string
		if ref.Kind == "literal" {
			final = ref.Ref
		} else {
			resolvedVal, resolveErr := stack.Resolve(ctx, ref)
			if resolveErr != nil {
				return nil, fmt.Errorf("variable %q (origin %s): %w", name, ref, resolveErr)
			}
			final = resolvedVal
		}
		resolved[name] = cty.StringVal(final)
		reg.Register(final)
		origins[name] = ref
	}

	out := make(map[string]cty.Value, len(vars))
	for k, v := range vars {
		out[k] = v
	}
	out["var"] = cty.ObjectVal(resolved)
	e.secretOrigins = origins
	return out, nil
}

// resolveSecretDataOrigins walks declared secret data blocks and resolves any
// initial string value that looks like a provider reference through the
// default environment provider stack. The resolved value is written into the
// run's DataStore and registered for redaction; the origin is stored in
// e.secretOrigins under the key "data.KIND.NAME" so that adapter session
// snapshots can re-resolve it on resume.
func (e *Engine) resolveSecretDataOrigins(ctx context.Context, ds *DataStore, reg *secrets.Registry) error {
	if len(e.graph.Data) == 0 {
		return nil
	}
	stack, err := secrets.StackFromEnvironment(getEnvironmentNode(e.graph, ""))
	if err != nil {
		return fmt.Errorf("resolve secret data origins: %w", err)
	}
	for kind, byName := range e.graph.Data {
		for name, node := range byName {
			if !node.Secret {
				continue
			}
			if err := e.resolveSecretDataValue(ctx, ds, reg, stack, kind, name, node.InitialValue); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *Engine) resolveSecretDataValue(ctx context.Context, ds *DataStore, reg *secrets.Registry, stack *secrets.Stack, kind, name string, val cty.Value) error {
	if val.IsNull() || !val.IsKnown() || val.Type() != cty.String {
		return nil
	}
	raw := val.AsString()
	ref := secrets.ParseOriginRef(raw)
	final := ref.Ref
	if ref.Kind != "literal" {
		resolvedVal, err := stack.Resolve(ctx, ref)
		if err != nil {
			return fmt.Errorf("data %q %q (origin %s): %w", kind, name, ref, err)
		}
		final = resolvedVal
	}
	if err := ds.Set(kind, name, cty.StringVal(final)); err != nil {
		return fmt.Errorf("data %q %q: %w", kind, name, err)
	}
	reg.Register(final)
	if e.secretOrigins == nil {
		e.secretOrigins = make(map[string]secrets.OriginRef)
	}
	e.secretOrigins["data."+kind+"."+name] = ref
	return nil
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
func (e *Engine) buildDeps(sessions *adapterhost.SessionManager, sink Sink, prompts *PromptRouter) Deps {
	return Deps{
		Sessions:            sessions,
		Loader:              e.loader,
		Sink:                sink,
		SubWorkflowResolver: e.subWorkflowResolver,
		BranchScheduler:     e.branchScheduler,
		Prompts:             prompts,
	}
}

// promptRunID returns the run id AgentPrompt messages are addressed to,
// defaulting to the snapshot run id.
func (e *Engine) effectivePromptRunID() string {
	if e.promptRunID != "" {
		return e.promptRunID
	}
	return e.runID
}

// logOrDefault returns the engine's structured logger, falling back to the
// slog default.
func (e *Engine) logOrDefault() *slog.Logger {
	if e.log != nil {
		return e.log
	}
	return slog.Default()
}

// advanceTo sets st.Current to next, moving the run forward to the next node.
func (e *Engine) advanceTo(st *RunState, next string) {
	st.Current = next
}

// handleEvalError dispatches errors from node.Evaluate. It handles ErrTerminal
// and ErrPaused specially; all other errors are propagated as run failures.
// CRI-271: when the run fails, the error class is logged explicitly so the
// engine log distinguishes an adapter session crash (a dead session, not a
// user action) from a host-initiated run cancellation.
func (e *Engine) handleEvalError(ctx context.Context, st *RunState, err error, sink Sink) error {
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
	// CRI-271: name the failure class before the generic failure event so
	// operators can tell an adapter session crash (the shim connection died
	// mid-turn) from a host-initiated cancellation (run timeout, user abort).
	var crash *adapterhost.SessionCrashError
	if errors.As(err, &crash) {
		slog.Warn("run failed by adapter session crash (not a run cancellation)",
			"step", st.Current, "session", crash.Session, "error", err)
	} else if ctxErr := ctx.Err(); ctxErr != nil {
		slog.Warn("run canceled", "step", st.Current, "reason", ctxErr.Error())
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
// With local shim isolation enabled (CRI-293), environments whose declared
// listen_address collides with another environment's bind their own
// auto-chosen loopback port instead.
func (e *Engine) maybeStartRemoteShim(ctx context.Context, sessions *adapterhost.SessionManager) error {
	if e.graph == nil || len(e.graph.Environments) == 0 {
		return nil
	}
	remoteEnvs := make(map[string]*workflow.EnvironmentNode, len(e.graph.Environments))
	for key, env := range e.graph.Environments {
		if env.Type == "remote" {
			remoteEnvs[key] = env
		}
	}
	if len(remoteEnvs) == 0 {
		return nil
	}

	verifier := &lockfileDigestVerifier{lockfile: e.effectivePinSet()}
	var isolated map[string]bool
	if e.localShimIsolation {
		isolated = isolatedShimEnvs(remoteEnvs)
	}

	for _, key := range slices.Sorted(maps.Keys(remoteEnvs)) {
		if err := e.startRemoteShimForEnv(ctx, key, remoteEnvs[key], sessions, verifier, isolated[key]); err != nil {
			return err
		}
	}
	return nil
}

// isolatedShimListenAddress is the address substituted for colliding
// environment listen_address declarations under local shim isolation: the OS
// picks a free loopback port per bind and Shim.ListenAddr reports the bound
// address for publication.
const isolatedShimListenAddress = "127.0.0.1:0"

// isolatedShimEnvs identifies remote environments whose declared
// listen_address collides with another environment's (CRI-293). Only fixed
// ports can collide: each bind on a port-0 address gets a distinct
// OS-assigned port, and non-addressable listen values (unix socket paths)
// keep today's bind-failure error path.
//
// Known limitation: grouping is by exact declared address string, so the
// same port declared with different host spellings across environments
// (e.g. "0.0.0.0:7778" vs "127.0.0.1:7778") is NOT detected as a collision
// and still fails locally with the bind error naming the address.
func isolatedShimEnvs(remoteEnvs map[string]*workflow.EnvironmentNode) map[string]bool {
	byAddress := make(map[string][]string, len(remoteEnvs))
	for key, env := range remoteEnvs {
		cfg, err := remote.ParseConfig(env.RawBody)
		if err != nil {
			continue // surfaces at shim start with the real error
		}
		if hasFixedPort(cfg.ListenAddress) {
			byAddress[cfg.ListenAddress] = append(byAddress[cfg.ListenAddress], key)
		}
	}
	isolated := make(map[string]bool)
	for _, keys := range byAddress {
		if len(keys) < 2 {
			continue
		}
		for _, key := range keys {
			isolated[key] = true
		}
	}
	return isolated
}

// hasFixedPort reports whether a listen address names a concrete TCP port,
// which is the only shape that can collide across binds on one host.
func hasFixedPort(listen string) bool {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	p, err := strconv.Atoi(port)
	return err == nil && p != 0
}

func (e *Engine) startRemoteShimForEnv(ctx context.Context, envKey string, env *workflow.EnvironmentNode, sessions *adapterhost.SessionManager, verifier remote.DigestVerifier, isolateListen bool) error {
	if env.Process != nil && !env.Process.IsWildcard() {
		return fmt.Errorf("remote environment %q does not support a process.exec allow-list; use process.exec = [\"*\"] to opt into unrestricted child execution or omit the process block", env.Name)
	}
	cfg, err := remote.ParseConfig(env.RawBody)
	if err != nil {
		return fmt.Errorf("remote environment %q: %w", env.Name, err)
	}
	if cfg.PerScopeSessions && e.dataDir == "" {
		return fmt.Errorf("remote environment %q: per_scope_sessions requires a run data directory (WithDataDir)", env.Name)
	}
	if isolateListen {
		slog.Info("isolating colliding remote environment shim",
			"environment", env.Name,
			"declared_listen_address", cfg.ListenAddress,
			"listen_address", isolatedShimListenAddress)
		cfg.ListenAddress = isolatedShimListenAddress
	}
	shim, err := remote.NewShim(cfg, verifier)
	if err != nil {
		return fmt.Errorf("remote environment %q: %w", env.Name, err)
	}
	shim.SetPerScopeSessions(cfg.PerScopeSessions)
	if err := shim.Start(ctx); err != nil {
		return fmt.Errorf("remote environment %q: %w", env.Name, err)
	}
	sessions.SetRemoteShimForEnv(envKey, shim)
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
