package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/brokenbots/overlord/overseer/internal/adapters/shell"
	"github.com/brokenbots/overlord/overseer/internal/engine"
	"github.com/brokenbots/overlord/overseer/internal/plugin"
	"github.com/brokenbots/overlord/overseer/internal/run"
	castletrans "github.com/brokenbots/overlord/overseer/internal/transport/castle"
	pb "github.com/brokenbots/overlord/shared/pb/overlord/v1"
	"github.com/brokenbots/overlord/workflow"
)

// resumeInFlightRuns scans the local checkpoint directory and, for each
// in-flight run, calls ReattachRun on Castle. Resumable runs are re-executed
// from the recorded step. Non-resumable runs have their checkpoint cleared.
//
// The clientOpts are used to build temporary clients for each resumed run.
// This function blocks until all resumable runs have completed (or failed).
func resumeInFlightRuns(ctx context.Context, log *slog.Logger, clientOpts castletrans.Options) {
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

func resumeOneRun(ctx context.Context, log *slog.Logger, cp *StepCheckpoint, clientOpts castletrans.Options) {
	log = log.With("run_id", cp.RunID, "step", cp.CurrentStep)
	resumeCtx, resumeCancel := context.WithCancel(ctx)
	defer resumeCancel()

	if cp.OverseerID == "" || cp.Token == "" {
		log.Warn("checkpoint missing overseer credentials; clearing", "run_id", cp.RunID)
		RemoveStepCheckpoint(cp.RunID)
		return
	}

	// Build a temporary client for this resumed run using the persisted
	// credentials. We do not Register (which would create a new overseer_id);
	// instead we re-use the original identity so ReattachRun ownership check passes.
	recoverClient, err := castletrans.NewClient(cp.CastleURL, log, clientOpts)
	if err != nil {
		log.Warn("cannot build recovery client; abandoning checkpoint", "error", err)
		RemoveStepCheckpoint(cp.RunID)
		return
	}
	defer recoverClient.Close()
	recoverClient.SetCredentials(cp.OverseerID, cp.Token)

	resp, err := recoverClient.ReattachRun(resumeCtx, cp.RunID, cp.OverseerID)
	if err != nil {
		log.Warn("reattach RPC failed; abandoning checkpoint", "error", err)
		RemoveStepCheckpoint(cp.RunID)
		return
	}
	if !resp.CanResume {
		log.Info("run not resumable (terminal or owned by another overseer); clearing checkpoint",
			"status", resp.Status)
		RemoveStepCheckpoint(cp.RunID)
		return
	}

	log.Info("resuming run after crash",
		"current_step", resp.CurrentStep,
		"last_attempt", resp.Attempt,
		"status", resp.Status)

	// Re-parse the workflow from the checkpoint path.
	graph, parseErr := parseWorkflowFromPath(cp.WorkflowPath)
	if parseErr != nil {
		log.Warn("cannot parse workflow for crashed run; abandoning", "error", parseErr)
		RemoveStepCheckpoint(cp.RunID)
		return
	}

	// If the run was paused when the Overseer crashed, re-enter the wait/approval
	// node with WithPendingSignal so it immediately re-issues ErrPaused and waits
	// for the real resume signal (W05).
	if resp.Status == "paused" {
		if streamErr := recoverClient.StartStreams(resumeCtx, cp.RunID); streamErr != nil {
			log.Warn("failed to start Castle streams for paused run", "error", streamErr)
			RemoveStepCheckpoint(cp.RunID)
			return
		}

		sink := &run.Sink{
			RunID:  cp.RunID,
			Client: recoverClient,
			Log:    log,
		}

		loader := plugin.NewLoader()
		loader.RegisterBuiltin(shell.Name, plugin.BuiltinFactoryForAdapter(shell.New()))

		restoredVars, restoredIter, restoreErr := workflow.RestoreVarScope(resp.VariableScope, graph)
		if restoreErr != nil {
			log.Warn("could not restore variable scope after pause reattach; starting with defaults", "error", restoreErr)
		}

		eng := engine.New(graph, loader, sink,
			engine.WithResumedVars(restoredVars),
			engine.WithResumedIter(restoredIter),
			engine.WithPendingSignal(resp.PendingSignal),
		)
		if runErr := eng.RunFrom(resumeCtx, resp.CurrentStep, int(resp.Attempt)); runErr != nil {
			log.Error("paused run re-entry failed", "error", runErr)
			drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
			recoverClient.Drain(drainCtx)
			drainCancel()
			RemoveStepCheckpoint(cp.RunID)
			return
		}

		// If still paused after re-entry, wait for resume signal from Castle.
		for sink.IsPaused() {
			log.Info("run remains paused after reattach; waiting for resume",
				"run_id", cp.RunID, "node", sink.PausedAt())
			var resumeMsg *pb.ResumeRun
			select {
			case <-resumeCtx.Done():
				drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
				recoverClient.Drain(drainCtx)
				drainCancel()
				return
			case resumeMsg = <-recoverClient.ResumeCh():
			}
			if resumeMsg.RunId != cp.RunID {
				log.Warn("received resume for unexpected run", "expected", cp.RunID, "got", resumeMsg.RunId)
				continue
			}
			pausedNode := sink.PausedAt()
			sink.ClearPaused()
			resumedEng := engine.New(graph, loader, sink,
				engine.WithResumedVars(eng.VarScope()),
				engine.WithResumePayload(resumeMsg.Payload),
			)
			if runErr := resumedEng.RunFrom(resumeCtx, pausedNode, 1); runErr != nil {
				log.Error("run failed after resume", "error", runErr)
				break
			}
			eng = resumedEng
		}

		drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
		recoverClient.Drain(drainCtx)
		drainCancel()
		RemoveStepCheckpoint(cp.RunID)
		return
	}

	nextAttempt := int(resp.Attempt) + 1

	// Check max_step_retries: if resuming would exceed policy, fail the run.
	maxAttempts := 1 + graph.Policy.MaxStepRetries
	if nextAttempt > maxAttempts {
		log.Warn("exceeded max_step_retries on resume; failing run",
			"next_attempt", nextAttempt, "max_attempts", maxAttempts)
		if streamErr := recoverClient.StartStreams(resumeCtx, cp.RunID); streamErr != nil {
			log.Warn("failed to start streams for failed resume", "error", streamErr)
			RemoveStepCheckpoint(cp.RunID)
			return
		}
		sink := &run.Sink{
			RunID:  cp.RunID,
			Client: recoverClient,
			Log:    log,
		}
		reason := fmt.Sprintf("exceeded max_step_retries on resume at step %q (attempt %d)", resp.CurrentStep, nextAttempt)
		sink.OnRunFailed(reason, resp.CurrentStep)
		// Use a background context so terminal-event flush still runs even when
		// the run context has already been cancelled (e.g. SIGTERM).
		drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
		recoverClient.Drain(drainCtx)
		drainCancel()
		RemoveStepCheckpoint(cp.RunID)
		return
	}

	if streamErr := recoverClient.StartStreams(resumeCtx, cp.RunID); streamErr != nil {
		log.Warn("failed to start Castle streams for resumed run", "error", streamErr)
		RemoveStepCheckpoint(cp.RunID)
		return
	}

	sink := &run.Sink{
		RunID:  cp.RunID,
		Client: recoverClient,
		Log:    log,
	}

	// Emit StepResumed before the engine re-enters the step.
	sink.OnStepResumed(resp.CurrentStep, nextAttempt, "overseer_restart")

	loader := plugin.NewLoader()
	loader.RegisterBuiltin(shell.Name, plugin.BuiltinFactoryForAdapter(shell.New()))

	// Restore the variable scope from Castle so expressions referencing
	// prior step outputs are evaluated correctly after crash recovery (W04).
	// Also restore the iter cursor if a for_each was active at crash time (W07).
	restoredVars, restoredIter, restoreErr := workflow.RestoreVarScope(resp.VariableScope, graph)
	if restoreErr != nil {
		log.Warn("could not restore variable scope; starting with defaults", "error", restoreErr)
	}

	eng := engine.New(graph, loader, sink,
		engine.WithResumedVars(restoredVars),
		engine.WithResumedIter(restoredIter),
	)
	if runErr := eng.RunFrom(resumeCtx, resp.CurrentStep, nextAttempt); runErr != nil {
		log.Error("resumed run failed", "error", runErr)
	} else {
		log.Info("resumed run completed")
	}
	// Use a background context so the flush is independent of run cancellation.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
	recoverClient.Drain(drainCtx)
	drainCancel()
	RemoveStepCheckpoint(cp.RunID)
}

func parseWorkflowFromPath(path string) (*workflow.FSMGraph, error) {
	if path == "" {
		return nil, fmt.Errorf("workflow path not recorded in checkpoint")
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read workflow %q: %w", path, err)
	}
	spec, diags := workflow.Parse(path, src)
	if diags.HasErrors() {
		return nil, fmt.Errorf("parse workflow: %s", diags.Error())
	}

	// Collect adapter schemas for compile-time validation.
	ctx := context.Background()
	loader := plugin.NewLoader()
	loader.RegisterBuiltin(shell.Name, plugin.BuiltinFactoryForAdapter(shell.New()))
	schemas := collectSchemas(ctx, loader, spec, nil)
	_ = loader.Shutdown(ctx)

	graph, diags := workflow.Compile(spec, schemas)
	if diags.HasErrors() {
		return nil, fmt.Errorf("compile workflow: %s", diags.Error())
	}
	return graph, nil
}
