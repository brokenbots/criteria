package cli

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/adapters/shell"
	"github.com/brokenbots/criteria/internal/engine"
	"github.com/brokenbots/criteria/internal/run"
	servertrans "github.com/brokenbots/criteria/internal/transport/server"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	"github.com/brokenbots/criteria/workflow"
)

// reattachTransport is the minimal server-client interface required by the
// crash-recovery functions. *servertrans.Client satisfies it. Defined here
// (not in the transport package) so tests can supply lightweight fakes without
// importing the production transport.
type reattachTransport interface {
	ReattachRun(ctx context.Context, runID, criteriaID string) (*pb.ReattachRunResponse, error)
	StartStreams(ctx context.Context, runID string) error
	Drain(ctx context.Context)
	ResumeCh() <-chan *pb.ResumeRun
	Publish(ctx context.Context, env *pb.Envelope)
}

// resumeInFlightRuns scans the local checkpoint directory and, for each
// in-flight run, calls ReattachRun on the server. Resumable runs are re-executed
// from the recorded step. Non-resumable runs have their checkpoint cleared.
//
// The clientOpts are used to build temporary clients for each resumed run.
// This function blocks until all resumable runs have completed (or failed).
func resumeInFlightRuns(ctx context.Context, log *slog.Logger, clientOpts *servertrans.Options) {
	checkpoints, err := ListStepCheckpoints()
	if err != nil {
		log.Warn("could not list step checkpoints; skipping crash recovery", "error", err)
		return
	}
	if len(checkpoints) == 0 {
		return
	}
	log.Info("found in-flight checkpoint(s); attempting crash recovery", "count", len(checkpoints))
	for _, cp := range checkpoints {
		resumeOneRun(ctx, log, cp, clientOpts)
	}
}

func resumeOneRun(ctx context.Context, log *slog.Logger, cp *StepCheckpoint, clientOpts *servertrans.Options) {
	log = log.With("run_id", cp.RunID, "step", cp.CurrentStep)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	rc, err := buildRecoveryClient(log, cp, clientOpts)
	if err != nil {
		return // buildRecoveryClient logged and cleared the checkpoint
	}
	defer rc.Close()

	resp, err := attemptReattach(ctx, log, rc, cp)
	if err != nil || resp == nil {
		return
	}

	graph, err := loadCheckpointWorkflow(ctx, log, cp)
	if err != nil {
		return
	}

	if err := checkIterationCursorValidity(graph, resp.VariableScope); err != nil {
		abandonCheckpoint(log, cp, "checkpoint step is no longer valid after workflow edit", err)
		return
	}

	if resp.Status == "paused" {
		resumePausedRun(ctx, log, rc, cp, graph, resp)
		return
	}
	resumeActiveRun(ctx, log, rc, cp, graph, resp)
}

// abandonCheckpoint logs a warning (with optional error) and removes the checkpoint.
func abandonCheckpoint(log *slog.Logger, cp *StepCheckpoint, reason string, err error) {
	if err != nil {
		log.Warn(reason, "error", err)
	} else {
		log.Warn(reason)
	}
	RemoveStepCheckpoint(cp.RunID)
}

// buildRecoveryClient validates checkpoint credentials, builds a temporary
// server client, and sets the persisted credentials on it. On any failure it
// abandons the checkpoint and returns a non-nil error so the caller can return.
func buildRecoveryClient(log *slog.Logger, cp *StepCheckpoint, clientOpts *servertrans.Options) (*servertrans.Client, error) {
	if cp.CriteriaID == "" || cp.Token == "" {
		abandonCheckpoint(log, cp, "checkpoint missing criteria credentials; clearing", nil)
		return nil, fmt.Errorf("missing credentials for run %q", cp.RunID)
	}
	// We do not Register (which would create a new criteria_id); instead we
	// re-use the original identity so ReattachRun ownership check passes.
	rc, err := servertrans.NewClient(cp.ServerURL, log, *clientOpts)
	if err != nil {
		abandonCheckpoint(log, cp, "cannot build recovery client; abandoning checkpoint", err)
		return nil, err
	}
	rc.SetCredentials(cp.CriteriaID, cp.Token)
	return rc, nil
}

// attemptReattach calls ReattachRun and checks CanResume. Returns nil, nil
// when the run is not resumable (checkpoint already cleared). Returns non-nil
// error when the RPC fails (checkpoint already cleared).
func attemptReattach(ctx context.Context, log *slog.Logger, rc reattachTransport, cp *StepCheckpoint) (*pb.ReattachRunResponse, error) {
	resp, err := rc.ReattachRun(ctx, cp.RunID, cp.CriteriaID)
	if err != nil {
		abandonCheckpoint(log, cp, "reattach RPC failed; abandoning checkpoint", err)
		return nil, err
	}
	if !resp.CanResume {
		log.Info("run not resumable (terminal or owned by another agent); clearing checkpoint",
			"status", resp.Status)
		RemoveStepCheckpoint(cp.RunID)
		return nil, nil
	}
	log.Info("resuming run after crash",
		"current_step", resp.CurrentStep,
		"last_attempt", resp.Attempt,
		"status", resp.Status)
	return resp, nil
}

// loadCheckpointWorkflow re-parses and compiles the workflow recorded in cp.
// On failure it abandons the checkpoint and returns a non-nil error.
func loadCheckpointWorkflow(ctx context.Context, log *slog.Logger, cp *StepCheckpoint) (*workflow.FSMGraph, error) {
	graph, err := parseWorkflowFromPath(ctx, cp.WorkflowPath)
	if err != nil {
		abandonCheckpoint(log, cp, "cannot parse workflow for crashed run; abandoning", err)
		return nil, err
	}
	return graph, nil
}

// drainAndCleanup flushes pending server events then removes the checkpoint.
// context.WithoutCancel ensures the 5-second drain window is honoured even
// when ctx is already cancelled (e.g. after SIGTERM or a ctx.Done() select arm).
func drainAndCleanup(ctx context.Context, rc reattachTransport, cp *StepCheckpoint) {
	drainCtx, drainCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	rc.Drain(drainCtx)
	drainCancel()
	RemoveStepCheckpoint(cp.RunID)
}

// resumePausedRun re-enters a paused run using WithPendingSignal, then
// services further resume signals until the run reaches a terminal state.
func resumePausedRun(ctx context.Context, log *slog.Logger, rc reattachTransport, cp *StepCheckpoint, graph *workflow.FSMGraph, resp *pb.ReattachRunResponse) {
	if streamErr := rc.StartStreams(ctx, cp.RunID); streamErr != nil {
		abandonCheckpoint(log, cp, "failed to start server streams for paused run", streamErr)
		return
	}
	sink := &run.Sink{RunID: cp.RunID, Client: rc, Log: log, Ctx: ctx}
	loader := adapterhost.NewLoader()
	loader.RegisterBuiltin(shell.Name, adapterhost.BuiltinFactoryForAdapter(shell.New()))

	restoredVars, restoredIter, restoreErr := workflow.RestoreVarScope(resp.VariableScope, graph)
	if restoreErr != nil {
		log.Warn("could not restore variable scope after pause reattach; starting with defaults", "error", restoreErr)
	}
	eng := engine.New(graph, loader, sink,
		engine.WithResumedVars(restoredVars),
		engine.WithResumedIter(restoredIter),
		engine.WithResumedVisits(cp.Visits),
		engine.WithPendingSignal(resp.PendingSignal),
		engine.WithWorkflowDir(workflowDirFromPath(cp.WorkflowPath)),
		engine.WithLogger(log),
	)
	if runErr := eng.RunFrom(ctx, resp.CurrentStep, int(resp.Attempt)); runErr != nil {
		log.Error("paused run re-entry failed", "error", runErr)
		drainAndCleanup(ctx, rc, cp)
		return
	}
	serviceResumeSignals(ctx, log, rc, cp, graph, loader, sink, eng)
}

// serviceResumeSignals waits for and dispatches resume signals while the run
// remains paused, then drains and removes the checkpoint.
func serviceResumeSignals(ctx context.Context, log *slog.Logger, rc reattachTransport, cp *StepCheckpoint, graph *workflow.FSMGraph, loader adapterhost.Loader, sink *run.Sink, initialEng *engine.Engine) {
	eng := initialEng
	for sink.IsPaused() {
		log.Info("run remains paused after reattach; waiting for resume",
			"run_id", cp.RunID, "node", sink.PausedAt())
		var resumeMsg *pb.ResumeRun
		select {
		case <-ctx.Done():
			drainAndCleanup(ctx, rc, cp)
			return
		case resumeMsg = <-rc.ResumeCh():
		}
		if resumeMsg.RunId != cp.RunID {
			log.Warn("received resume for unexpected run", "expected", cp.RunID, "got", resumeMsg.RunId)
			continue
		}
		pausedNode := sink.PausedAt()
		sink.ClearPaused()
		resumedEng := engine.New(graph, loader, sink,
			engine.WithResumedVars(eng.VarScope()),
			engine.WithResumedVisits(eng.VisitCounts()),
			engine.WithResumePayload(resumeMsg.Payload),
			engine.WithWorkflowDir(workflowDirFromPath(cp.WorkflowPath)),
		)
		if runErr := resumedEng.RunFrom(ctx, pausedNode, 1); runErr != nil {
			log.Error("run failed after resume", "error", runErr)
			break
		}
		eng = resumedEng
	}
	drainAndCleanup(ctx, rc, cp)
}

// checkIterationCursorValidity verifies that the active iteration cursor stack
// (if any) is still consistent with the newly-compiled graph. This guards
// against a checkpoint that was created while an iteration was in progress but
// the workflow was subsequently edited in a way that removed the iterating step.
//
// The check is based on the serialised iteration cursor stack from variableScope
// (the checkpoint's persisted state). This catches the real incompatibility case:
// a step that was previously iterating but was deleted by a workflow edit between
// crash and resume.
//
// Returns a non-nil error (suitable for abandonCheckpoint) when the check fails.
func checkIterationCursorValidity(graph *workflow.FSMGraph, variableScope string) error {
	// Discard the error: RestoreVarScope returns an empty stack on parse failure,
	// which the check below handles correctly. A broken scope is not an
	// iteration-cursor incompatibility.
	_, stack, _ := workflow.RestoreVarScope(variableScope, graph)
	if len(stack) == 0 {
		return nil // no active iteration cursor; nothing to verify
	}
	top := stack[len(stack)-1]
	if !top.InProgress {
		return nil
	}
	// Verify the step named by the cursor still exists in the graph.
	stepNode, ok := graph.Steps[top.StepName]
	if !ok {
		return fmt.Errorf("checkpoint iterating step %q no longer exists in the workflow", top.StepName)
	}
	_ = stepNode // reserved for future step-kind-specific resume validation
	return nil
}

// resumeActiveRun handles the normal (non-paused) resume path, including
// max_step_retries policy enforcement.
func resumeActiveRun(ctx context.Context, log *slog.Logger, rc reattachTransport, cp *StepCheckpoint, graph *workflow.FSMGraph, resp *pb.ReattachRunResponse) {
	nextAttempt := int(resp.Attempt) + 1
	maxAttempts := 1 + graph.Policy.MaxStepRetries
	if nextAttempt > maxAttempts {
		log.Warn("exceeded max_step_retries on resume; failing run",
			"next_attempt", nextAttempt, "max_attempts", maxAttempts)
		if streamErr := rc.StartStreams(ctx, cp.RunID); streamErr != nil {
			abandonCheckpoint(log, cp, "failed to start streams for failed resume", streamErr)
			return
		}
		sink := &run.Sink{RunID: cp.RunID, Client: rc, Log: log, Ctx: ctx}
		reason := fmt.Sprintf("exceeded max_step_retries on resume at step %q (attempt %d)", resp.CurrentStep, nextAttempt)
		sink.RunFailed(ctx, reason, resp.CurrentStep)
		drainAndCleanup(ctx, rc, cp)
		return
	}

	if streamErr := rc.StartStreams(ctx, cp.RunID); streamErr != nil {
		abandonCheckpoint(log, cp, "failed to start server streams for resumed run", streamErr)
		return
	}

	sink := &run.Sink{RunID: cp.RunID, Client: rc, Log: log, Ctx: ctx}
	sink.StepResumed(ctx, resp.CurrentStep, nextAttempt, "criteria_restart")
	loader := adapterhost.NewLoader()
	loader.RegisterBuiltin(shell.Name, adapterhost.BuiltinFactoryForAdapter(shell.New()))

	// Restore variable scope and iter cursor from the server (W04/W07).
	restoredVars, restoredIter, restoreErr := workflow.RestoreVarScope(resp.VariableScope, graph)
	if restoreErr != nil {
		log.Warn("could not restore variable scope; starting with defaults", "error", restoreErr)
	}

	eng := engine.New(graph, loader, sink,
		engine.WithResumedVars(restoredVars),
		engine.WithResumedIter(restoredIter),
		engine.WithResumedVisits(cp.Visits),
		engine.WithWorkflowDir(workflowDirFromPath(cp.WorkflowPath)),
		engine.WithLogger(log),
	)
	if runErr := eng.RunFrom(ctx, resp.CurrentStep, nextAttempt); runErr != nil {
		log.Error("resumed run failed", "error", runErr)
	} else {
		log.Info("resumed run completed")
	}
	drainAndCleanup(ctx, rc, cp)
}

func parseWorkflowFromPath(ctx context.Context, path string) (*workflow.FSMGraph, error) {
	if path == "" {
		return nil, fmt.Errorf("workflow path not recorded in checkpoint")
	}
	spec, diags := workflow.ParseFileOrDir(path)
	if diags.HasErrors() {
		return nil, fmt.Errorf("parse workflow:\n%w", newDiagsError(diags))
	}

	// Collect adapter schemas for compile-time validation.
	loader := adapterhost.NewLoader()
	loader.RegisterBuiltin(shell.Name, adapterhost.BuiltinFactoryForAdapter(shell.New()))
	schemas := collectSchemas(ctx, loader, spec, nil)
	_ = loader.Shutdown(ctx)

	graph, diags := workflow.CompileWithContext(ctx, spec, schemas, workflow.CompileOpts{
		WorkflowDir:         workflowDirFromPath(path),
		SubWorkflowResolver: &workflow.LocalSubWorkflowResolver{},
	})
	if diags.HasErrors() {
		return nil, fmt.Errorf("compile workflow:\n%w", newDiagsError(diags))
	}
	return graph, nil
}
