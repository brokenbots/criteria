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
	identity, suppressed := prepareLocalRunIdentity(ctx, log, jsonOut, mode, opts.workflowPath, opts.varFiles, opts.varOverrides)
	if suppressed {
		log.Info("in-flight run for this identity resumed; not starting a second run",
			"file", filepath.Base(opts.workflowPath))
		return nil
	}
	if identity.mergeErr != nil {
		return identity.mergeErr
	}

	src, graph, loader, err := compileForExecution(ctx, opts.workflowPath, log, opts.warnsAsErrors, opts.allowUnsigned, opts.subworkflowRoots...)
	if err != nil {
		return err
	}
	// src (raw HCL bytes) is consumed only by server mode for signed payload
	// delivery; local mode has no signing step, so src is intentionally
	// unused here.
	_ = src
	defer func() { _ = loader.Shutdown(context.WithoutCancel(ctx)) }()

	resumer, err := buildLocalResumer(log, opts.stdin)
	if err != nil {
		return err
	}
	if err := ensureLocalModeSupported(graph, resumer != nil); err != nil {
		return err
	}

	return executeFreshLocalRun(ctx, log, graph, loader, resumer, jsonOut, mode, opts, identity)
}

// executeFreshLocalRun runs a freshly-started local workflow to its terminal
// state, persisting the local run state and step checkpoints for crash
// recovery (CRI-125).
func executeFreshLocalRun(ctx context.Context, log *slog.Logger, graph *workflow.FSMGraph, loader adapterhost.Loader, resumer localresume.LocalResumer, jsonOut io.Writer, mode outputMode, opts applyOptions, identity localRunIdentity) error {
	runID := uuid.NewString()
	var eng *engine.Engine // captured by getVisits closure below
	tracker, runSink := buildLocalRunSink(log, runID, opts.workflowPath, identity.fingerprint, jsonOut, mode, graph, func() map[string]int {
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
	_ = writeLocalRunState(state)
	defer removeLocalRunState(runID)
	defer RemoveStepCheckpoint(runID)

	auditPath, _ := auditLogPath(runID)
	auditWriter := adapterhost.NewFileAuditWriter(auditPath)
	dataDir, err := runDataDir(runID)
	if err != nil {
		return err
	}
	eng = engine.New(graph, loader, runSink,
		engine.WithVarOverrides(identity.mergedVars),
		engine.WithWorkflowDir(workflowDirFromPath(opts.workflowPath)),
		engine.WithAuditWriter(auditWriter),
		engine.WithDataDir(dataDir),
	)
	if err := eng.Run(ctx); err != nil {
		log.Error("local run failed", "run_id", runID, "error", err)
		return err
	}

	if resumer != nil {
		if err := drainLocalResumeCycles(ctx, log, graph, loader, tracker, runSink, resumer, runID, opts, eng); err != nil {
			return err
		}
	}

	if finalState, success, ok := runSink.TerminalSuccess(); ok && !success {
		return fmt.Errorf("run completed with terminal state %q (success=false)", finalState)
	}

	log.Info("local run completed", "run_id", runID)
	return nil
}

// buildLocalRunSink wires the checkpoint function, pause tracker, and
// terminal-success sink for a fresh local run. getVisits captures the running
// engine's visit counts for checkpoint writes (W07).
func buildLocalRunSink(log *slog.Logger, runID, workflowPath, fingerprint string, jsonOut io.Writer, mode outputMode, graph *workflow.FSMGraph, getVisits func() map[string]int) (*pauseTracker, *terminalSuccessSink) {
	checkpointFn := buildLocalCheckpointFn(log, runID, graph.Name, workflowPath, fingerprint, getVisits)
	baseSink := buildLocalSink(runID, jsonOut, mode, graph.StepOrder(), checkpointFn, graph)
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

// localRunIdentity carries the CLI variable inputs and the invocation
// fingerprint (CRI-125) needed by both the crash-recovery sweep and a fresh
// local run.
type localRunIdentity struct {
	mergedVars  map[string]cty.Value
	fingerprint string
	mergeErr    error
}

// prepareLocalRunIdentity merges the invocation's CLI variables and computes
// the CRI-125 run identity fingerprint, then resumes any in-flight local
// runs whose checkpoint matches this identity. The boolean reports whether a
// matching run was consumed, in which case the caller must not start a
// second run. A variable-source error is returned in identity.mergeErr with
// the sweep skipped: the invocation cannot faithfully re-enter the original
// run without its variable inputs.
func prepareLocalRunIdentity(ctx context.Context, log *slog.Logger, jsonOut io.Writer, mode outputMode, workflowPath string, varFiles, varOverrides []string) (localRunIdentity, bool) {
	identity := localRunIdentity{}
	merged, mergeErr := mergeVarSources(varFiles, varOverrides)
	if mergeErr != nil {
		identity.mergeErr = mergeErr
		return identity, false
	}
	identity.mergedVars = merged
	identity.fingerprint = runIdentityFingerprint(workflowPath, "", varFiles, varOverrides)
	suppressed := resumeLocalInFlightRuns(ctx, log, jsonOut, mode, identity.fingerprint, identity.mergedVars)
	return identity, suppressed
}

func resumeLocalInFlightRuns(ctx context.Context, log *slog.Logger, out io.Writer, mode outputMode, fingerprint string, mergedVars map[string]cty.Value) bool {
	checkpoints, err := ListStepCheckpoints()
	if err != nil {
		log.Warn("could not list step checkpoints; skipping local crash recovery", "error", err)
		return false
	}
	matched := false
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
		if resumeOneLocalRun(ctx, log, cp, out, mode, vars) && fingerprint != "" && cp.Fingerprint == fingerprint {
			matched = true
		}
	}
	return matched
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
func resumeOneLocalRun(ctx context.Context, log *slog.Logger, cp *StepCheckpoint, out io.Writer, mode outputMode, mergedVars map[string]cty.Value) bool {
	graph, loader, resumer, ok := prepareReattach(ctx, log, cp)
	if !ok {
		return false
	}
	defer func() { _ = loader.Shutdown(context.WithoutCancel(ctx)) }()

	nextAttempt := cp.Attempt + 1
	maxAttempts := 1 + graph.Policy.MaxStepRetries
	if nextAttempt > maxAttempts {
		sink := buildLocalSink(cp.RunID, out, mode, graph.StepOrder(), nil, graph)
		reason := fmt.Sprintf("exceeded max_step_retries on resume at step %q (attempt %d)", cp.CurrentStep, nextAttempt)
		sink.OnRunFailed(reason, cp.CurrentStep)
		RemoveStepCheckpoint(cp.RunID)
		return true
	}

	opts, tracker, runSink, eng, engErr := buildReattachTrackerAndEngine(cp, log, graph, loader, out, mode, nextAttempt, mergedVars)
	if engErr != nil {
		log.Error("resumed local run failed to resolve run data dir", "run_id", cp.RunID, "error", engErr)
		RemoveStepCheckpoint(cp.RunID)
		return false
	}
	if runErr := eng.RunFrom(ctx, cp.CurrentStep, nextAttempt); runErr != nil {
		log.Error("resumed local run failed", "run_id", cp.RunID, "error", runErr)
		RemoveStepCheckpoint(cp.RunID)
		return true
	}
	if resumer != nil {
		if cycleErr := drainLocalResumeCycles(ctx, log, graph, loader, tracker, runSink, resumer, cp.RunID, opts, eng); cycleErr != nil {
			log.Error("resumed local run failed during approval", "run_id", cp.RunID, "error", cycleErr)
			RemoveStepCheckpoint(cp.RunID)
			return true
		}
	}
	log.Info("resumed local run completed", "run_id", cp.RunID)
	RemoveStepCheckpoint(cp.RunID)
	return true
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
	baseSink := buildLocalSink(cp.RunID, out, mode, graph.StepOrder(), checkpointFn, graph)
	tracker := &pauseTracker{
		Sink:              baseSink,
		PauseCheckpointFn: func(node string) { checkpointFn(node, 0) },
	}
	runSink := &terminalSuccessSink{Sink: tracker}
	tracker.OnStepResumed(cp.CurrentStep, nextAttempt, "criteria_restart")
	eng = engine.New(graph, loader, runSink,
		engine.WithVarOverrides(mergedVars),
		engine.WithWorkflowDir(workflowDirFromPath(cp.WorkflowPath)),
		engine.WithResumedVisits(cp.Visits),
		engine.WithDataDir(dataDir),
	)
	return opts, tracker, runSink, eng, nil
}
