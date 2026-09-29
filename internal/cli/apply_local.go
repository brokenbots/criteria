package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/engine"
	"github.com/brokenbots/criteria/workflow"
)

func runApplyLocal(
	ctx context.Context,
	opts applyOptions,
) error {
	log := opts.log
	if log == nil {
		log = newApplyLogger()
	}

	mode, err := resolveOutputMode(opts.output, os.Stdout)
	if err != nil {
		return err
	}
	jsonOut, cleanup, err := openNDJSONWriter(opts.eventsPath, mode)
	if err != nil {
		return err
	}
	defer cleanup()

	// CRI-202: enforce the retention janitor before this invocation starts.
	// Terminal runs' checkpoints are deleted and orphans swept; live or
	// stopped runs are exempt, so a resume identity match still finds its
	// checkpoint state intact.
	sweepOrphanCheckpointState(log)

	// CRI-125: identify this invocation before any fresh-run work so a
	// restarted runner resumes (or keeps failed) the original run instead of
	// forking a second run with a fresh run_id. A broken CLI variable input
	// fails fast: the invocation cannot faithfully re-enter the original run
	// with its (unreadable) variable inputs, so suppressing a fresh run and
	// resuming without the overrides would silently drop them.
	identity, suppressed, prepErr := prepareLocalRunIdentity(ctx, log, jsonOut, mode, opts.workflowPath, opts.varFiles, opts.varOverrides, localApprovalConfigFrom(&opts))
	if prepErr != nil {
		return prepErr
	}
	if suppressed {
		log.Info("in-flight run for this identity resumed; not starting a second run",
			"file", filepath.Base(opts.workflowPath))
		// CRI-125: a resumed run's outcome error was already returned via
		// prepErr above; reaching here means the resumed run completed
		// successfully, so the invocation must not start a second run.
		return nil
	}

	src, graph, loader, err := compileForExecution(ctx, opts.workflowPath, log, opts.warnsAsErrors, opts.allowUnsigned, opts.subworkflowRoots...)
	if err != nil {
		return err
	}
	// src (raw HCL bytes) is consumed only by server mode for signed payload
	// delivery; in local mode it is hashed into the run's workflow_hash so
	// the local run-state API can surface it (Run.workflowHash).
	workflowHash := workflowSourceHash(src)
	defer func() { _ = loader.Shutdown(context.WithoutCancel(ctx)) }()

	resolution, err := selectApprovalResolution(log, localApprovalConfigFrom(&opts), graph)
	if err != nil {
		return err
	}
	if err := ensureLocalModeSupported(graph); err != nil {
		return err
	}

	return executeFreshLocalRun(ctx, log, graph, loader, resolution, jsonOut, mode, opts, identity, workflowHash)
}

// executeFreshLocalRun runs a freshly-started local workflow to its terminal
// state, persisting the local run state and step checkpoints for crash
// recovery (CRI-125). Every local run is served by a loopback control
// listener (CRI-255) while apply executes — the runview API plus the Connect
// LocalControlService (PauseRun/ResumeRun/ResolveResume) — and, when the UI
// is enabled, the embedded run-viewer is served on the same server.
func executeFreshLocalRun(ctx context.Context, log *slog.Logger, graph *workflow.FSMGraph, loader adapterhost.Loader, resolution *approvalResolution, jsonOut io.Writer, mode outputMode, opts applyOptions, identity localRunIdentity, workflowHash string) error {
	runID := uuid.NewString()
	runEvents, closeRunEvents, err := openRunEventsFile(runID)
	if err != nil {
		return err
	}
	defer closeRunEvents()
	teeOut := io.MultiWriter(jsonOut, runEvents)

	var eng *engine.Engine // captured by getVisits closure below
	tracker, runSink := buildLocalRunSink(log, runID, opts.workflowPath, identity.fingerprint, teeOut, mode, graph, func() map[string]int {
		if eng != nil {
			return eng.VisitCounts()
		}
		return nil
	})

	log.Info("starting local run",
		"run_id", runID,
		"workflow", graph.Name,
		"file", filepath.Base(opts.workflowPath))

	state := newLocalRunState(runID, graph.Name, "")
	state.WorkflowHash = workflowHash
	_ = writeLocalRunState(state)
	// CRI-225: publish the resolved workflow origin as the run's metadata
	// admission record; local sources (nil origin) record nothing.
	publishRunMetadata(runID, opts.origin, log)
	defer removeLocalRunState(runID)
	defer RemoveStepCheckpoint(runID)

	eng, err = newLocalEngine(runID, graph, loader, runSink, opts, identity)
	if err != nil {
		return err
	}
	ctrl := newLocalRunControl(runID, graph, tracker, eng)

	// CRI-255: the loopback control listener is served for every local run —
	// it mounts the Connect LocalControlService (control surface) and, when
	// the UI is enabled, the run viewer. Its stop verb cancels the engine
	// context (the engine then emits a real terminal RunFailed event).
	runCtx, stopServer := attachLocalRunStateServer(ctx, log, runID, resolveRunListenAddr(&opts), ctrl, opts.ui)
	defer stopServer()
	defer ctrl.markFinished()
	if err := eng.Run(runCtx); err != nil {
		log.Error("local run failed", "run_id", runID, "error", err)
		return err
	}

	if err := finishFreshLocalRun(runCtx, log, loader, runSink, resolution, runID, opts, ctrl, eng); err != nil {
		return err
	}

	log.Info("local run completed", "run_id", runID)
	return nil
}

// buildLocalRunSink wires the checkpoint function, pause tracker, and
// terminal-success sink for a fresh local run. getVisits captures the running
// engine's visit counts for checkpoint writes (W07).
func buildLocalRunSink(log *slog.Logger, runID, workflowPath, fingerprint string, jsonOut io.Writer, mode outputMode, graph *workflow.FSMGraph, getVisits func() map[string]int) (*pauseTracker, *terminalSuccessSink) {
	checkpointFn := buildLocalCheckpointFn(log, runID, graph.Name, workflowPath, fingerprint, getVisits)
	baseSink, local := buildLocalSink(runID, jsonOut, mode, graph.StepOrder(), checkpointFn, graph)
	// CRI-278: emit the once-per-run WorkflowGraphs event at the post-compile
	// seam, before the engine starts, so it takes the next seq on the ND-JSON
	// stream and lands at or before RunStarted.
	emitWorkflowGraphsLocal(log, local, graph)
	tracker := &pauseTracker{
		Sink: baseSink,
		PauseCheckpointFn: func(node string) {
			// Write a checkpoint pointing at the paused approval/signal-wait
			// node so that a crash while waiting can be recovered from the
			// right place.
			checkpointFn(node, 0)
		},
	}
	return tracker, &terminalSuccessSink{Sink: tracker}
}

// localRunEngineOptions returns the engine options every local `criteria
// apply` engine construction site must include: the workflow directory, run
// data directory, CRI-293 shim address isolation, and the CRI-202 checkpoint
// surface (snapshot base + run id, so adapter checkpoints persist under
// <home>/runs/<runID>/snapshots for later restore). Server engine
// construction sites must never include the shim isolation option: each
// environment's shim lives in its own adapter pod and must receive the
// declared address.
func localRunEngineOptions(workflowPath, dataDir, runID string) ([]engine.Option, error) {
	home, err := stateDir()
	if err != nil {
		return nil, fmt.Errorf("resolve criteria home for checkpoints: %w", err)
	}
	return []engine.Option{
		engine.WithWorkflowDir(workflowDirFromPath(workflowPath)),
		engine.WithDataDir(dataDir),
		engine.WithLocalShimIsolation(),
		engine.WithSnapshotBase(home),
		engine.WithRunID(runID),
	}, nil
}

// newLocalEngine constructs the engine for a fresh local run.
func newLocalEngine(runID string, graph *workflow.FSMGraph, loader adapterhost.Loader, runSink engine.Sink, opts applyOptions, identity localRunIdentity) (*engine.Engine, error) {
	auditPath, _ := auditLogPath(runID)
	auditWriter := adapterhost.NewFileAuditWriter(auditPath)
	dataDir, err := runDataDir(runID)
	if err != nil {
		return nil, err
	}
	engOpts, err := localRunEngineOptions(opts.workflowPath, dataDir, runID)
	if err != nil {
		return nil, err
	}
	engOpts = append(engOpts,
		engine.WithVarOverrides(identity.mergedVars),
		engine.WithAuditWriter(auditWriter))
	// CRI-304: record the invocation fingerprint and let the engine adopt
	// surviving per-scope instances from prior invocations of the same run.
	engOpts = append(engOpts, engineAdoptionOptions(dataDir, identity.fingerprint, runID)...)
	return engine.New(graph, loader, runSink, engOpts...), nil
}

// finishFreshLocalRun handles post-engine work: resume cycles (boundary
// pauses and approval/signal node pauses) and the terminal-success failure
// translation. It runs for every local run: without a configured resumer the
// control listener's bus still resolves pauses (CRI-255), and a pause with
// no reachable surface fails the run loudly (CRI-256).
func finishFreshLocalRun(runCtx context.Context, log *slog.Logger, loader adapterhost.Loader, runSink *terminalSuccessSink, resolution *approvalResolution, runID string, opts applyOptions, ctrl *localRunControl, eng *engine.Engine) error {
	if err := drainLocalResumeCycles(runCtx, log, loader, runSink, resolution, runID, opts, ctrl, eng); err != nil {
		return err
	}
	return terminalFailureError(runSink)
}

// terminalFailureError composes the invocation error for a terminal run that
// completed with success=false. A rejected approval decision (CRI-256) carries
// its operator-supplied reason into the error so rejections are never silent.
func terminalFailureError(runSink *terminalSuccessSink) error {
	finalState, success, ok := runSink.TerminalSuccess()
	if !ok || success {
		return nil
	}
	if node, reason, rejected := runSink.Rejection(); rejected {
		if strings.TrimSpace(reason) != "" {
			return fmt.Errorf("run completed with terminal state %q (success=false); approval %q was rejected with reason: %s", finalState, node, reason)
		}
		return fmt.Errorf("run completed with terminal state %q (success=false); approval %q was rejected (no reason given)", finalState, node)
	}
	return fmt.Errorf("run completed with terminal state %q (success=false)", finalState)
}

// localRunIdentity carries the CLI variable inputs and the invocation
// fingerprint (CRI-125) needed by both the crash-recovery sweep and a fresh
// local run.
type localRunIdentity struct {
	mergedVars  map[string]cty.Value
	fingerprint string
}

// prepareLocalRunIdentity merges the invocation's CLI variables and computes
// the CRI-125 run identity fingerprint, then resumes any in-flight local
// runs whose checkpoint matches this identity. The boolean reports whether a
// matching run was consumed, in which case the caller must not start a
// second run; the error carries either a variable-source failure or the
// resumed run's outcome (nil on success). A variable-source error aborts the
// sweep: the invocation cannot faithfully re-enter the original run without
// its variable inputs.
func prepareLocalRunIdentity(ctx context.Context, log *slog.Logger, jsonOut io.Writer, mode outputMode, workflowPath string, varFiles, varOverrides []string, approvalCfg localApprovalConfig) (localRunIdentity, bool, error) {
	identity := localRunIdentity{}
	merged, err := mergeVarSources(varFiles, varOverrides)
	if err != nil {
		return identity, false, err
	}
	identity.mergedVars = merged
	identity.fingerprint = runIdentityFingerprint(workflowPath, "", varFiles, varOverrides)
	suppressed, outcomeErr := resumeLocalInFlightRuns(ctx, log, jsonOut, mode, identity.fingerprint, identity.mergedVars, approvalCfg)
	return identity, suppressed, outcomeErr
}

func resumeLocalInFlightRuns(ctx context.Context, log *slog.Logger, out io.Writer, mode outputMode, fingerprint string, mergedVars map[string]cty.Value, approvalCfg localApprovalConfig) (matched bool, outcome error) {
	checkpoints, err := ListStepCheckpoints()
	if err != nil {
		log.Warn("could not list step checkpoints; skipping local crash recovery", "error", err)
		return false, nil
	}
	for _, cp := range checkpoints {
		if strings.TrimSpace(cp.ServerURL) != "" {
			continue
		}
		// CRI-125: a consumed checkpoint whose fingerprint matches this
		// invocation means the original run was driven to its outcome here;
		// the caller must not start a second run for the same identity.
		// mergedVars only apply to fingerprint-matched checkpoints: the
		// checkpoint's state dir is shared across tickets, and another
		// ticket's vars must never leak into its crashed run.
		vars := map[string]cty.Value(nil)
		if fingerprint != "" && cp.Fingerprint == fingerprint {
			vars = mergedVars
		}
		consumed, cpOutcome := resumeOneLocalRun(ctx, log, cp, out, mode, vars, approvalCfg)
		if consumed && fingerprint != "" && cp.Fingerprint == fingerprint {
			matched = true
			if outcome == nil {
				outcome = cpOutcome
			}
		}
	}
	return matched, outcome
}

// prepareReattach validates the checkpoint, builds an adapter loader, and
// selects the pause-resolution posture from the CRI-256 inputs. On failure it
// logs, clears the checkpoint, and returns false so the caller can skip the
// run.
func prepareReattach(ctx context.Context, log *slog.Logger, cp *StepCheckpoint, approvalCfg localApprovalConfig) (*workflow.FSMGraph, adapterhost.Loader, *approvalResolution, bool) {
	graph, err := parseWorkflowFromPath(ctx, cp.WorkflowPath)
	if err != nil {
		log.Warn("cannot parse workflow for crashed local run; abandoning", "run_id", cp.RunID, "error", err)
		RemoveStepCheckpoint(cp.RunID)
		return nil, nil, nil, false
	}
	resolution, resErr := selectApprovalResolution(log, approvalCfg, graph)
	if resErr != nil {
		log.Warn("local checkpoint: approval resolution is unavailable; clearing", "run_id", cp.RunID, "error", resErr)
		RemoveStepCheckpoint(cp.RunID)
		return nil, nil, nil, false
	}
	if err := ensureLocalModeSupported(graph); err != nil {
		log.Warn("local checkpoint requires server; clearing", "run_id", cp.RunID, "error", err)
		RemoveStepCheckpoint(cp.RunID)
		return nil, nil, nil, false
	}
	loader := adapterhost.NewLoader()
	return graph, loader, resolution, true
}

// resumeOneLocalRun resumes a crashed local run from cp and returns whether
// the checkpoint's run was consumed: driven to a terminal outcome (success or
// a marked failure) by this process. It returns false only when the
// checkpoint was abandoned as unusable and no run outcome was recorded, so
// the caller may proceed with a fresh run.
func resumeOneLocalRun(ctx context.Context, log *slog.Logger, cp *StepCheckpoint, out io.Writer, mode outputMode, mergedVars map[string]cty.Value, approvalCfg localApprovalConfig) (bool, error) {
	graph, loader, resolution, ok := prepareReattach(ctx, log, cp, approvalCfg)
	if !ok {
		return false, nil
	}
	defer func() { _ = loader.Shutdown(context.WithoutCancel(ctx)) }()

	// CRI-304: the interrupted step restarts at attempt 1 with a fresh retry
	// budget. The pre-crash attempt produced no outcome, so counting it
	// against max_step_retries made mid-step crash recovery useless for any
	// workflow with max_step_retries <= 1. The persisted max_visits counts
	// (restored via WithResumedVisits) still bound total attempts across
	// resumes, so a crash-loop cannot amplify into unbounded work.
	// cp.Attempt is the pre-crash attempt; it stays out of the budget
	// decision and is surfaced for diagnostics only.
	nextAttempt := 1
	log.Info("resuming interrupted step with fresh attempt budget",
		"run_id", cp.RunID, "step", cp.CurrentStep, "last_attempt", cp.Attempt, "resumed_attempt", nextAttempt)

	opts, tracker, runSink, eng, engErr := buildReattachTrackerAndEngine(cp, log, graph, loader, out, mode, nextAttempt, mergedVars)
	if engErr != nil {
		log.Error("resumed local run failed to resolve run data dir", "run_id", cp.RunID, "error", engErr)
		RemoveStepCheckpoint(cp.RunID)
		return false, nil
	}
	// CRI-255: the reattach run is served by its own control listener (the
	// crashed process's listener is gone): the fresh attachment publishes a
	// renewed control.json for the same run id.
	ctrl := newLocalRunControl(cp.RunID, graph, tracker, eng)
	runCtx, stopServer := attachLocalRunStateServer(ctx, log, cp.RunID, resolveRunListenAddr(&opts), ctrl, opts.ui)
	defer stopServer()
	defer ctrl.markFinished()
	if runErr := eng.RunFrom(runCtx, cp.CurrentStep, nextAttempt); runErr != nil {
		log.Error("resumed local run failed", "run_id", cp.RunID, "error", runErr)
		RemoveStepCheckpoint(cp.RunID)
		return true, runErr
	}
	if cycleErr := drainLocalResumeCycles(runCtx, log, loader, runSink, resolution, cp.RunID, opts, ctrl, eng); cycleErr != nil {
		log.Error("resumed local run failed during approval", "run_id", cp.RunID, "error", cycleErr)
		RemoveStepCheckpoint(cp.RunID)
		return true, cycleErr
	}
	log.Info("resumed local run completed", "run_id", cp.RunID)
	RemoveStepCheckpoint(cp.RunID)
	// CRI-125: a resumed run that reaches a terminal success=false state must
	// fail the invocation exactly like the fresh path does; otherwise the
	// suppression branch would mask the original run's failure with exit 0.
	return true, terminalFailureError(runSink)
}

// buildReattachTrackerAndEngine wires the checkpoint sink, pause tracker, and
// engine for a crash-reattach run. The checkpointFn closure captures eng so
// that each checkpoint write includes the current visit counts (W07).
// mergedVars, when non-empty, applies the current invocation's CLI variable
// overrides to the resumed engine (CRI-125): local checkpoints do not persist
// a variable scope, so without this a fingerprint-matched resumed run would
// silently lose the caller's --var/--var-file inputs.
func buildReattachTrackerAndEngine(cp *StepCheckpoint, log *slog.Logger, graph *workflow.FSMGraph, loader adapterhost.Loader, out io.Writer, mode outputMode, nextAttempt int, mergedVars map[string]cty.Value) (applyOptions, *pauseTracker, *terminalSuccessSink, *engine.Engine, error) {
	opts := applyOptions{workflowPath: cp.WorkflowPath}
	dataDir, err := runDataDir(cp.RunID)
	if err != nil {
		return opts, nil, nil, nil, err
	}
	var eng *engine.Engine // captured by checkpointFn; assigned below before any callbacks fire
	checkpointFn := func(step string, attempt int) {
		next := *cp
		next.CurrentStep = step
		next.Attempt = attempt
		next.StartedAt = time.Now().UTC()
		if eng != nil {
			next.Visits = eng.VisitCounts()
		}
		if cpErr := WriteStepCheckpoint(&next); cpErr != nil {
			log.Warn("failed to update local checkpoint", "run_id", cp.RunID, "error", cpErr)
		}
	}
	baseSink, _ := buildLocalSink(cp.RunID, out, mode, graph.StepOrder(), checkpointFn, graph)
	tracker := &pauseTracker{
		Sink:              baseSink,
		PauseCheckpointFn: func(node string) { checkpointFn(node, 0) },
	}
	runSink := &terminalSuccessSink{Sink: tracker}
	tracker.OnStepResumed(cp.CurrentStep, nextAttempt, "criteria_restart")
	reattachOpts, err := localRunEngineOptions(cp.WorkflowPath, dataDir, cp.RunID)
	if err != nil {
		return opts, nil, nil, nil, err
	}
	reattachOpts = append(reattachOpts,
		engine.WithVarOverrides(mergedVars),
		engine.WithResumedVisits(cp.Visits))
	// CRI-293: this is a local crash-reattach engine; it binds the remote
	// environment shims in-process just like the fresh-run and resume-cycle
	// engines, so it needs the same shared listen_address isolation (carried
	// by localRunEngineOptions).
	// CRI-304: record the checkpoint's invocation fingerprint and let the
	// resumed engine adopt surviving per-scope instances from other prior
	// invocations of the same run.
	reattachOpts = append(reattachOpts, engineAdoptionOptions(dataDir, cp.Fingerprint, cp.RunID)...)
	eng = engine.New(graph, loader, runSink, reattachOpts...)
	return opts, tracker, runSink, eng, nil
}
