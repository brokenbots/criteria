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

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/adapters/shell"
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

	resumeLocalInFlightRuns(ctx, log, jsonOut, mode)

	src, graph, loader, err := compileForExecution(ctx, opts.workflowPath, log, opts.subworkflowRoots...)
	if err != nil {
		return err
	}
	defer func() { _ = loader.Shutdown(context.WithoutCancel(ctx)) }()

	resumer, err := buildLocalResumer(log, opts.stdin)
	if err != nil {
		return err
	}
	if err := ensureLocalModeSupported(graph, resumer != nil); err != nil {
		return err
	}

	runID := uuid.NewString()
	var eng *engine.Engine // captured by getVisits closure below
	checkpointFn := buildLocalCheckpointFn(log, runID, graph.Name, opts.workflowPath, func() map[string]int {
		if eng != nil {
			return eng.VisitCounts()
		}
		return nil
	})
	baseSink := buildLocalSink(runID, jsonOut, mode, graph.StepOrder(), checkpointFn, graph)
	tracker := &pauseTracker{
		Sink: baseSink,
		PauseCheckpointFn: func(node string) {
			// Write a checkpoint pointing at the paused approval/signal-wait node so
			// that a crash while waiting can be recovered from the right place.
			checkpointFn(node, 0)
		},
	}

	log.Info("starting local run",
		"run_id", runID,
		"workflow", graph.Name,
		"file", filepath.Base(opts.workflowPath))

	state := &localRunState{
		PID:       os.Getpid(),
		RunID:     runID,
		Workflow:  graph.Name,
		ServerURL: "",
		StartedAt: time.Now().UTC(),
	}
	_ = writeLocalRunState(state)
	defer removeLocalRunState()
	defer RemoveStepCheckpoint(runID)

	// src (raw HCL bytes) is consumed only by server mode for signed payload delivery;
	// local mode has no signing step, so src is intentionally unused here.
	_ = src
	eng = engine.New(graph, loader, tracker,
		engine.WithVarOverrides(parseVarOverrides(opts.varOverrides)),
		engine.WithWorkflowDir(workflowDirFromPath(opts.workflowPath)),
	)
	if err := eng.Run(ctx); err != nil {
		log.Error("local run failed", "run_id", runID, "error", err)
		return err
	}

	if resumer != nil {
		if err := drainLocalResumeCycles(ctx, log, graph, loader, tracker, resumer, runID, opts, eng); err != nil {
			return err
		}
	}

	log.Info("local run completed", "run_id", runID)
	return nil
}

func resumeLocalInFlightRuns(ctx context.Context, log *slog.Logger, out io.Writer, mode outputMode) {
	checkpoints, err := ListStepCheckpoints()
	if err != nil {
		log.Warn("could not list step checkpoints; skipping local crash recovery", "error", err)
		return
	}
	if len(checkpoints) == 0 {
		return
	}
	for _, cp := range checkpoints {
		if strings.TrimSpace(cp.ServerURL) != "" {
			continue
		}
		resumeOneLocalRun(ctx, log, cp, out, mode)
	}
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
	loader.RegisterBuiltin(shell.Name, adapterhost.BuiltinFactoryForAdapter(shell.New()))
	return graph, loader, resumer, true
}

func resumeOneLocalRun(ctx context.Context, log *slog.Logger, cp *StepCheckpoint, out io.Writer, mode outputMode) {
	graph, loader, resumer, ok := prepareReattach(ctx, log, cp)
	if !ok {
		return
	}
	defer func() { _ = loader.Shutdown(context.WithoutCancel(ctx)) }()

	nextAttempt := cp.Attempt + 1
	maxAttempts := 1 + graph.Policy.MaxStepRetries
	if nextAttempt > maxAttempts {
		sink := buildLocalSink(cp.RunID, out, mode, graph.StepOrder(), nil, graph)
		reason := fmt.Sprintf("exceeded max_step_retries on resume at step %q (attempt %d)", cp.CurrentStep, nextAttempt)
		sink.OnRunFailed(reason, cp.CurrentStep)
		RemoveStepCheckpoint(cp.RunID)
		return
	}

	opts, tracker, eng := buildReattachTrackerAndEngine(cp, log, graph, loader, out, mode, nextAttempt)
	if runErr := eng.RunFrom(ctx, cp.CurrentStep, nextAttempt); runErr != nil {
		log.Error("resumed local run failed", "run_id", cp.RunID, "error", runErr)
		RemoveStepCheckpoint(cp.RunID)
		return
	}
	if resumer != nil {
		if cycleErr := drainLocalResumeCycles(ctx, log, graph, loader, tracker, resumer, cp.RunID, opts, eng); cycleErr != nil {
			log.Error("resumed local run failed during approval", "run_id", cp.RunID, "error", cycleErr)
			RemoveStepCheckpoint(cp.RunID)
			return
		}
	}
	log.Info("resumed local run completed", "run_id", cp.RunID)
	RemoveStepCheckpoint(cp.RunID)
}

// buildReattachTrackerAndEngine wires the checkpoint sink, pause tracker, and
// engine for a crash-reattach run. The checkpointFn closure captures eng so
// that each checkpoint write includes the current visit counts (W07).
func buildReattachTrackerAndEngine(cp *StepCheckpoint, log *slog.Logger, graph *workflow.FSMGraph, loader adapterhost.Loader, out io.Writer, mode outputMode, nextAttempt int) (applyOptions, *pauseTracker, *engine.Engine) {
	opts := applyOptions{workflowPath: cp.WorkflowPath}
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
	tracker.OnStepResumed(cp.CurrentStep, nextAttempt, "criteria_restart")
	eng = engine.New(graph, loader, tracker,
		engine.WithWorkflowDir(workflowDirFromPath(cp.WorkflowPath)),
		engine.WithResumedVisits(cp.Visits),
	)
	return opts, tracker, eng
}
