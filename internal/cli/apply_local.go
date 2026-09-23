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
	"github.com/brokenbots/criteria/internal/cli/localresume"
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

	// CRI-125: identify this invocation before any fresh-run work so a
	// restarted runner resumes (or keeps failed) the original run instead of
	// forking a second run with a fresh run_id. A broken CLI variable input
	// fails fast: the invocation cannot faithfully re-enter the original run
	// with its (unreadable) variable inputs, so suppressing a fresh run and
	// resuming without the overrides would silently drop them.
	identity, suppressed, prepErr := prepareLocalRunIdentity(ctx, log, jsonOut, mode, opts.workflowPath, opts.varFiles, opts.varOverrides)
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

	resumer, err := buildLocalResumer(log, opts.stdin)
	if err != nil {
		return err
	}
	if err := ensureLocalModeSupported(graph, resumer != nil); err != nil {
		return err
	}

	return executeFreshLocalRun(ctx, log, graph, loader, resumer, jsonOut, mode, opts, identity, workflowHash)
}

// executeFreshLocalRun runs a freshly-started local workflow to its terminal
// state, persisting the local run state and step checkpoints for crash
// recovery (CRI-125). When the UI is enabled, the run's events are also
// teed into <home>/runs/<runID>/events.ndjson and a loopback run-state
// server (scoped to this run) serves the run-viewer at a printed URL
// (CRI-279).
func executeFreshLocalRun(ctx context.Context, log *slog.Logger, graph *workflow.FSMGraph, loader adapterhost.Loader, resumer localresume.LocalResumer, jsonOut io.Writer, mode outputMode, opts applyOptions, identity localRunIdentity, workflowHash string) error {
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

	// CRI-279: serve the run-viewer on loopback for this run while apply
	// executes. The stop verb cancels the engine context (the engine then
	// emits a real terminal RunFailed event); pause/resume are UNIMPLEMENTED
	// until CRI-255 adds checkpoint-gated controls.
	runCtx := ctx
	var stopServer func()
	if opts.ui {
		runCtx, stopServer = attachLocalRunStateServer(ctx, log, runID, opts.uiPort)
		defer stopServer()
	}
	if err := eng.Run(runCtx); err != nil {
		log.Error("local run failed", "run_id", runID, "error", err)
		return err
	}

	if err := finishFreshLocalRun(runCtx, log, graph, loader, tracker, runSink, resumer, runID, opts, eng); err != nil {
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
// apply` engine construction site must include: the workflow directory and
// run data directory, plus the CRI-293 shim address isolation. A local run
// binds every referenced remote environment's shim in one process, so
// environments sharing a listen_address would collide on the second bind
// without per-environment port isolation. Server engine construction sites
// must never include that option: each environment's shim lives in its own
// adapter pod and must receive the declared address.
func localRunEngineOptions(workflowPath, dataDir string) []engine.Option {
	return []engine.Option{
		engine.WithWorkflowDir(workflowDirFromPath(workflowPath)),
		engine.WithDataDir(dataDir),
		engine.WithLocalShimIsolation(),
	}
}

// newLocalEngine constructs the engine for a fresh local run.
func newLocalEngine(runID string, graph *workflow.FSMGraph, loader adapterhost.Loader, runSink engine.Sink, opts applyOptions, identity localRunIdentity) (*engine.Engine, error) {
	auditPath, _ := auditLogPath(runID)
	auditWriter := adapterhost.NewFileAuditWriter(auditPath)
	dataDir, err := runDataDir(runID)
	if err != nil {
		return nil, err
	}
	engOpts := append(localRunEngineOptions(opts.workflowPath, dataDir),
		engine.WithVarOverrides(identity.mergedVars),
		engine.WithAuditWriter(auditWriter))
	// CRI-304: record the invocation fingerprint and let the engine adopt
	// surviving per-scope instances from prior invocations of the same run.
	engOpts = append(engOpts, engineAdoptionOptions(dataDir, identity.fingerprint, runID)...)
	return engine.New(graph, loader, runSink, engOpts...), nil
}

// finishFreshLocalRun handles post-engine work: resume cycles and the
// terminal-success failure translation.
func finishFreshLocalRun(runCtx context.Context, log *slog.Logger, graph *workflow.FSMGraph, loader adapterhost.Loader, tracker *pauseTracker, runSink *terminalSuccessSink, resumer localresume.LocalResumer, runID string, opts applyOptions, eng *engine.Engine) error {
	if resumer != nil {
		if err := drainLocalResumeCycles(runCtx, log, graph, loader, tracker, runSink, resumer, runID, opts, eng); err != nil {
			return err
		}
	}
	if finalState, success, ok := runSink.TerminalSuccess(); ok && !success {
		return fmt.Errorf("run completed with terminal state %q (success=false)", finalState)
	}
	return nil
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
func prepareLocalRunIdentity(ctx context.Context, log *slog.Logger, jsonOut io.Writer, mode outputMode, workflowPath string, varFiles, varOverrides []string) (localRunIdentity, bool, error) {
	identity := localRunIdentity{}
	merged, err := mergeVarSources(varFiles, varOverrides)
	if err != nil {
		return identity, false, err
	}
	identity.mergedVars = merged
	identity.fingerprint = runIdentityFingerprint(workflowPath, "", varFiles, varOverrides)
	suppressed, outcomeErr := resumeLocalInFlightRuns(ctx, log, jsonOut, mode, identity.fingerprint, identity.mergedVars)
	return identity, suppressed, outcomeErr
}

func resumeLocalInFlightRuns(ctx context.Context, log *slog.Logger, out io.Writer, mode outputMode, fingerprint string, mergedVars map[string]cty.Value) (matched bool, outcome error) {
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
		consumed, cpOutcome := resumeOneLocalRun(ctx, log, cp, out, mode, vars)
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
// constructs a local resumer. On failure it logs, clears the checkpoint,
// and returns zero values with false so the caller can skip the run.
func prepareReattach(ctx context.Context, log *slog.Logger, cp *StepCheckpoint) (*workflow.FSMGraph, adapterhost.Loader, localresume.LocalResumer, bool) {
	graph, err := parseWorkflowFromPath(ctx, cp.WorkflowPath)
	if err != nil {
		log.Warn("cannot parse workflow for crashed local run; abandoning", "run_id", cp.RunID, "error", err)
		RemoveStepCheckpoint(cp.RunID)
		return nil, nil, nil, false
	}
	resumer, resumerErr := buildLocalResumer(log, nil)
	if resumerErr != nil {
		log.Warn("local checkpoint: invalid CRITERIA_LOCAL_APPROVAL; clearing", "run_id", cp.RunID, "error", resumerErr)
		RemoveStepCheckpoint(cp.RunID)
		return nil, nil, nil, false
	}
	if err := ensureLocalModeSupported(graph, resumer != nil); err != nil {
		log.Warn("local checkpoint requires server; clearing", "run_id", cp.RunID, "error", err)
		RemoveStepCheckpoint(cp.RunID)
		return nil, nil, nil, false
	}
	loader := adapterhost.NewLoader()
	return graph, loader, resumer, true
}

// resumeOneLocalRun resumes a crashed local run from cp and returns whether
// the checkpoint's run was consumed: driven to a terminal outcome (success or
// a marked failure) by this process. It returns false only when the
// checkpoint was abandoned as unusable and no run outcome was recorded, so
// the caller may proceed with a fresh run.
func resumeOneLocalRun(ctx context.Context, log *slog.Logger, cp *StepCheckpoint, out io.Writer, mode outputMode, mergedVars map[string]cty.Value) (bool, error) {
	graph, loader, resumer, ok := prepareReattach(ctx, log, cp)
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
	var outcome error
	if runErr := eng.RunFrom(ctx, cp.CurrentStep, nextAttempt); runErr != nil {
		log.Error("resumed local run failed", "run_id", cp.RunID, "error", runErr)
		RemoveStepCheckpoint(cp.RunID)
		return true, runErr
	}
	if resumer != nil {
		if cycleErr := drainLocalResumeCycles(ctx, log, graph, loader, tracker, runSink, resumer, cp.RunID, opts, eng); cycleErr != nil {
			log.Error("resumed local run failed during approval", "run_id", cp.RunID, "error", cycleErr)
			RemoveStepCheckpoint(cp.RunID)
			return true, cycleErr
		}
	}
	log.Info("resumed local run completed", "run_id", cp.RunID)
	RemoveStepCheckpoint(cp.RunID)
	// CRI-125: a resumed run that reaches a terminal success=false state must
	// fail the invocation exactly like the fresh path does; otherwise the
	// suppression branch would mask the original run's failure with exit 0.
	if finalState, success, ok := runSink.TerminalSuccess(); ok && !success {
		outcome = fmt.Errorf("run completed with terminal state %q (success=false)", finalState)
	}
	return true, outcome
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
	reattachOpts := append(localRunEngineOptions(cp.WorkflowPath, dataDir),
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
