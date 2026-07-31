package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/diagutil"
	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/lockfile"
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

// workflowDirFromPath returns the workflow module directory for path.
// If path is a directory it is returned as-is; if it is a file, its parent
// directory is returned — all sibling .chcl and .hcl files form the same module.
func workflowDirFromPath(path string) string {
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return path
	}
	return filepath.Dir(path)
}

func compileForExecution(ctx context.Context, workflowPath string, log *slog.Logger, warnsAsErrors, allowUnsigned bool, subworkflowRoots ...string) ([]byte, *workflow.FSMGraph, *adapterhost.DefaultLoader, error) {
	spec, diags := workflow.ParseFileOrDir(workflowPath)
	if diags.HasErrors() {
		return nil, nil, nil, fmt.Errorf("parse errors:\n%w", newDiagsError(diags))
	}

	workflowDir := workflowDirFromPath(workflowPath)

	pinSet, err := verifyAndPullTreeAdapters(ctx, workflowDir, spec, allowUnsigned)
	if err != nil {
		return nil, nil, nil, err
	}

	loader := adapterhost.NewLoader()
	loader.SetDevBindings(devBindingPaths())
	schemas, schemaDiags := diagutil.CollectSchemas(ctx, loader, workflowDir, spec, log)

	resolver := &workflow.LocalSubWorkflowResolver{AllowedRoots: subworkflowRoots}
	graph, diags := workflow.CompileWithContext(ctx, spec, schemas, workflow.CompileOpts{
		WorkflowDir:         workflowDir,
		SubWorkflowResolver: resolver,
		Schemas:             schemas,
		PinSet:              pinSet,
	})
	if diags.HasErrors() {
		_ = loader.Shutdown(ctx)
		return nil, nil, nil, fmt.Errorf("compile errors:\n%w", newDiagsError(diags))
	}

	// Unverified-adapter warnings: with --warnings-as-errors, refuse to run a
	// graph that could fail mid-execution; otherwise log them and continue.
	schemaDiags = promoteWarnings(schemaDiags, warnsAsErrors)
	if err := newDiagsError(schemaDiags); err != nil {
		_ = loader.Shutdown(ctx)
		return nil, nil, nil, fmt.Errorf("compile errors:\n%w", err)
	}
	if log != nil {
		for _, d := range schemaDiags {
			log.Warn("adapter schema unverified", "summary", d.Summary, "detail", d.Detail)
		}
	}

	return spec.SourceBytes, graph, loader, nil
}

// verifyAndPullTreeAdapters resolves the merged, tree-wide pin set once, refuses
// to start if any adapter is unpinned, and auto-pulls the OCI artifacts so the
// digest-addressed binaries are present before compilation. The same
// in-memory pin set is returned for use by the compiler and engine.
func verifyAndPullTreeAdapters(ctx context.Context, workflowDir string, spec *workflow.Spec, allowUnsigned bool) (*lockfile.Lockfile, error) {
	pinSet, err := loadTreePinSet(ctx, workflowDir)
	if err != nil {
		return nil, fmt.Errorf("load workflow tree pin set: %w", err)
	}

	// Refuse to start if any adapter in the tree is unpinned. This must run
	// before auto-pull so a mutable tag can never be resolved for an unpinned
	// adapter.
	if unpinned, err := collectUnpinnedAdaptersWithPinSet(ctx, workflowDir, pinSet); err != nil {
		return nil, fmt.Errorf("lockfile coverage check failed: %w", err)
	} else if len(unpinned) > 0 {
		for _, e := range unpinned {
			fmt.Fprintf(os.Stderr, "error: %v\n", e)
		}
		return nil, fmt.Errorf("cannot start: %d unpinned adapter(s) in workflow tree; run `criteria adapter lock %s`", len(unpinned), workflowDir)
	}

	// Execution-time auto-pull: ensure OCI adapters are present, verified against
	// the resolved signing policy, and extracted before the run starts.
	if err := autoPullCompileAdapters(ctx, workflowDir, spec, pinSet, allowUnsigned); err != nil {
		return nil, err
	}

	return pinSet, nil
}
