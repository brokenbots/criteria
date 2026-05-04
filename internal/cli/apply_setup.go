package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/brokenbots/criteria/internal/adapters/shell"
	"github.com/brokenbots/criteria/internal/plugin"
	"github.com/brokenbots/criteria/workflow"
)

func newApplyLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func writeRunCheckpoint(log *slog.Logger, runID, graphName, workflowPath, serverURL, step string, attempt int, criteriaID, token string, visits map[string]int) {
	cp := &StepCheckpoint{
		RunID:        runID,
		Workflow:     graphName,
		WorkflowPath: workflowPath,
		CurrentStep:  step,
		Attempt:      attempt,
		StartedAt:    time.Now().UTC(),
		ServerURL:    serverURL,
		CriteriaID:   criteriaID,
		Token:        token,
		Visits:       visits,
	}
	if cpErr := WriteStepCheckpoint(cp); cpErr != nil {
		log.Warn("failed to write step checkpoint; crash recovery may not work", "error", cpErr)
	}
}

// buildLocalCheckpointFn returns a CheckpointFn that writes a fresh StepCheckpoint
// for crash-recovery persistence during an initial local run. getVisits, if non-nil,
// is called at each write to capture current per-step visit counts (W07). Mirrors the
// getVisits convention used by buildServerSink.
func buildLocalCheckpointFn(log *slog.Logger, runID, workflowName, workflowPath string, getVisits func() map[string]int) func(string, int) {
	return func(step string, attempt int) {
		cp := &StepCheckpoint{
			RunID:        runID,
			Workflow:     workflowName,
			WorkflowPath: workflowPath,
			CurrentStep:  step,
			Attempt:      attempt,
			StartedAt:    time.Now().UTC(),
		}
		if getVisits != nil {
			cp.Visits = getVisits()
		}
		if err := WriteStepCheckpoint(cp); err != nil {
			log.Warn("failed to write step checkpoint; crash recovery may not work", "error", err)
		}
	}
}

func newLocalRunState(runID, graphName, serverURL string) *localRunState {
	return &localRunState{
		PID:       os.Getpid(),
		RunID:     runID,
		Workflow:  graphName,
		ServerURL: serverURL,
		StartedAt: time.Now().UTC(),
	}
}

func compileForExecution(ctx context.Context, workflowPath string, log *slog.Logger, subworkflowRoots ...string) ([]byte, *workflow.FSMGraph, *plugin.DefaultLoader, error) {
	src, err := os.ReadFile(workflowPath)
	if err != nil {
		return nil, nil, nil, err
	}
	spec, diags := workflow.Parse(workflowPath, src)
	if diags.HasErrors() {
		return nil, nil, nil, fmt.Errorf("parse: %s", diags.Error())
	}

	loader := plugin.NewLoader()
	loader.RegisterBuiltin(shell.Name, plugin.BuiltinFactoryForAdapter(shell.New()))
	schemas := collectSchemas(ctx, loader, spec, log)

	resolver := &workflow.LocalSubWorkflowResolver{AllowedRoots: subworkflowRoots}
	graph, diags := workflow.CompileWithOpts(spec, schemas, workflow.CompileOpts{
		WorkflowDir:         filepath.Dir(workflowPath),
		SubWorkflowResolver: resolver,
		Schemas:             schemas,
	})
	if diags.HasErrors() {
		_ = loader.Shutdown(ctx)
		return nil, nil, nil, fmt.Errorf("compile: %s", diags.Error())
	}

	return src, graph, loader, nil
}
