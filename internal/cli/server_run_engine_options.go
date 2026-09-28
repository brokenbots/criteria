package cli

import (
	"fmt"

	"github.com/brokenbots/criteria/internal/engine"
)

// serverRunEngineOptions returns the engine options every server-mode engine
// construction site must include: the workflow directory, run data directory,
// and the CRI-202 checkpoint surface (snapshot base + run id, so adapter
// checkpoints persist under <home>/runs/<runID>/snapshots for later restore
// on the next pause/resume cycle or a resumed engine construction). This
// mirrors localRunEngineOptions minus the CRI-293 shim isolation, which stays
// local-only: each environment's shim lives in its own adapter pod and must
// receive the declared address. The resolved run data directory is returned
// alongside the options for callers that still need it (e.g.
// engineAdoptionOptions).
func serverRunEngineOptions(runID, workflowPath string) (string, []engine.Option, error) {
	home, err := stateDir()
	if err != nil {
		return "", nil, fmt.Errorf("resolve criteria home for checkpoints: %w", err)
	}
	dataDir, err := runDataDir(runID)
	if err != nil {
		return "", nil, err
	}
	return dataDir, []engine.Option{
		engine.WithWorkflowDir(workflowDirFromPath(workflowPath)),
		engine.WithDataDir(dataDir),
		engine.WithSnapshotBase(home),
		engine.WithRunID(runID),
	}, nil
}