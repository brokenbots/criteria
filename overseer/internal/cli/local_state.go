package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type localRunState struct {
	PID       int       `json:"pid"`
	RunID     string    `json:"run_id"`
	Workflow  string    `json:"workflow"`
	CastleURL string    `json:"castle_url"`
	StartedAt time.Time `json:"started_at"`
}

func stateFilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".overlord", "overseer-state.json"), nil
}

func writeLocalRunState(st *localRunState) error {
	p, err := stateFilePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
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
