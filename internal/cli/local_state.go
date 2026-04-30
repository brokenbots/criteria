package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type localRunState struct {
	PID       int       `json:"pid"`
	RunID     string    `json:"run_id"`
	Workflow  string    `json:"workflow"`
	ServerURL string    `json:"server_url"`
	StartedAt time.Time `json:"started_at"`
}

// StepCheckpoint is written to disk before each step is executed so that a
// restarted Criteria agent can resume from the last in-flight step.
type StepCheckpoint struct {
	RunID        string    `json:"run_id"`
	Workflow     string    `json:"workflow"`
	WorkflowPath string    `json:"workflow_path"`
	CurrentStep  string    `json:"current_step"`
	Attempt      int       `json:"attempt"`
	StartedAt    time.Time `json:"started_at"`
	ServerURL    string    `json:"server_url"`
	CriteriaID   string    `json:"criteria_id"`
	// Token is the bearer token for the criteria_id above. Stored so that
	// a restarted process can call ReattachRun and SubmitEvents without
	// re-registering (which would assign a new id and fail the ownership
	// check). The file is written with 0o600 permissions.
	Token string `json:"token"`
}

// stateDir returns the base directory for Criteria state files.
// It respects the CRITERIA_STATE_DIR environment variable; defaults to
// ~/.criteria. If the directory cannot be resolved or created, writes are
// soft-degraded (callers log the error and continue).
func stateDir() (string, error) {
	if d := os.Getenv("CRITERIA_STATE_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".criteria"), nil
}

func stateFilePath() (string, error) {
	d, err := stateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "criteria-state.json"), nil
}

func checkpointFilePath(runID string) (string, error) {
	d, err := stateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "runs", runID+".json"), nil
}

func writeLocalRunState(st *localRunState) error {
	p, err := stateFilePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

func readLocalRunState() (*localRunState, error) {
	p, err := stateFilePath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var st localRunState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("decode local run state: %w", err)
	}
	return &st, nil
}

func removeLocalRunState() {
	p, err := stateFilePath()
	if err != nil {
		return
	}
	_ = os.Remove(p)
}

// WriteStepCheckpoint persists the current step checkpoint for a run.
// On error it returns the error; callers should log and continue without
// crashing (soft-degrade if state dir is not writable).
func WriteStepCheckpoint(cp *StepCheckpoint) error {
	if cp == nil {
		return errors.New("checkpoint is nil")
	}
	if cp.RunID == "" {
		return errors.New("run_id required")
	}
	if cp.WorkflowPath != "" {
		f, err := os.Open(cp.WorkflowPath)
		if err != nil {
			return fmt.Errorf("workflow_path not accessible: %w", err)
		}
		_ = f.Close()
	}
	p, err := checkpointFilePath(cp.RunID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

// RemoveStepCheckpoint deletes the checkpoint file for a run.
func RemoveStepCheckpoint(runID string) {
	p, err := checkpointFilePath(runID)
	if err != nil {
		return
	}
	_ = os.Remove(p)
}

// approvalDecisionDir returns the directory path for persisted approval decisions
// for a run: <stateDir>/runs/<runID>/approvals/. The directory is not created
// by this function; callers that write files are responsible for MkdirAll.
func approvalDecisionDir(runID string) (string, error) {
	d, err := stateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "runs", runID, "approvals"), nil
}

// validateNodeName rejects node names that could escape the approvals directory
// via path traversal. HCL block labels are arbitrary strings, so we guard
// against names containing path separators (/ or \), "..", or a Windows volume
// prefix (e.g., "C:"). Both separators are checked regardless of OS so that
// decision files remain portable and safe on any platform.
func validateNodeName(nodeName string) error {
	if strings.ContainsRune(nodeName, '/') ||
		strings.ContainsRune(nodeName, '\\') ||
		strings.Contains(nodeName, "..") ||
		filepath.VolumeName(nodeName) != "" {
		return fmt.Errorf("node name %q contains invalid path characters", nodeName)
	}
	return nil
}

// ApprovalDecisionPath returns the path for a persisted decision for a specific
// node within a run: <stateDir>/runs/<runID>/approvals/<node>.json.
// This path is used for both read (reattach safety) and write (persistence).
func ApprovalDecisionPath(runID, nodeName string) (string, error) {
	if err := validateNodeName(nodeName); err != nil {
		return "", err
	}
	dir, err := approvalDecisionDir(runID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, nodeName+".json"), nil
}

// ApprovalRequestPath returns the path of the file-mode sentinel that the
// operator writes to provide a decision: <stateDir>/runs/<runID>/approval-<node>.json.
func ApprovalRequestPath(runID, nodeName string) (string, error) {
	if err := validateNodeName(nodeName); err != nil {
		return "", err
	}
	d, err := stateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "runs", runID, "approval-"+nodeName+".json"), nil
}

// ListStepCheckpoints returns all valid checkpoint files found in the runs
// subdirectory of the state dir. Corrupt or unreadable files are silently
// skipped (logged by the caller).
func ListStepCheckpoints() ([]*StepCheckpoint, error) {
	d, err := stateDir()
	if err != nil {
		return nil, err
	}
	runsDir := filepath.Join(d, "runs")
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]*StepCheckpoint, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		p := filepath.Join(runsDir, e.Name())
		b, readErr := os.ReadFile(p)
		if readErr != nil {
			continue // skip unreadable
		}
		var cp StepCheckpoint
		if jsonErr := json.Unmarshal(b, &cp); jsonErr != nil {
			continue // skip corrupt
		}
		if cp.RunID == "" {
			continue
		}
		out = append(out, &cp)
	}
	return out, nil
}
