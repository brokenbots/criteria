// Package state provides persistence helpers for Criteria runtime state,
// including session snapshots for pause/resume across host restarts.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/brokenbots/criteria/internal/adapterhost"
)

// SnapshotDir returns the directory for a given run/session snapshot sequence.
func SnapshotDir(base, runID, sessionID string) string {
	return filepath.Join(base, "runs", runID, "snapshots", sessionID)
}

// WriteSnapshot persists a session snapshot as <seq>.bin (opaque adapter state)
// and <seq>.json (SessionSnapshot metadata). The sequence number is returned.
func WriteSnapshot(dir string, snap *adapterhost.SessionSnapshot) (seq int, err error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return 0, fmt.Errorf("mkdir snapshot dir: %w", err)
	}
	seq, err = nextSeq(dir)
	if err != nil {
		return 0, fmt.Errorf("determine next sequence: %w", err)
	}

	binPath := filepath.Join(dir, seqName(seq, ".bin"))
	if err := os.WriteFile(binPath, snap.AdapterState, 0o600); err != nil {
		return 0, fmt.Errorf("write snapshot blob: %w", err)
	}

	meta := *snap
	meta.AdapterState = nil // stored separately in .bin
	jsonPath := filepath.Join(dir, seqName(seq, ".json"))
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return 0, fmt.Errorf("marshal snapshot metadata: %w", err)
	}
	if err := os.WriteFile(jsonPath, data, 0o600); err != nil {
		return 0, fmt.Errorf("write snapshot metadata: %w", err)
	}
	return seq, nil
}

// ReadLatestSnapshot finds the highest sequence number in dir and reconstructs
// a SessionSnapshot from the corresponding .json + .bin files.
func ReadLatestSnapshot(dir string) (*adapterhost.SessionSnapshot, error) {
	seq, err := latestSeq(dir)
	if err != nil {
		return nil, err
	}
	jsonPath := filepath.Join(dir, seqName(seq, ".json"))
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		return nil, fmt.Errorf("read snapshot metadata: %w", err)
	}
	var snap adapterhost.SessionSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("unmarshal snapshot metadata: %w", err)
	}
	binPath := filepath.Join(dir, seqName(seq, ".bin"))
	blob, err := os.ReadFile(binPath)
	if err != nil {
		return nil, fmt.Errorf("read snapshot blob: %w", err)
	}
	snap.AdapterState = blob
	return &snap, nil
}

// ListSnapshotSessions returns all session IDs under the run's snapshots root.
func ListSnapshotSessions(base, runID string) ([]string, error) {
	dir := filepath.Join(base, "runs", runID, "snapshots")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

func nextSeq(dir string) (int, error) {
	seq, err := latestSeq(dir)
	if os.IsNotExist(err) {
		return 1, nil
	}
	if err != nil && (strings.Contains(err.Error(), "no snapshots found")) {
		return 1, nil
	}
	if err != nil {
		return 0, err
	}
	return seq + 1, nil
}

func latestSeq(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var maxSeq int = -1
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSuffix(name, ".json"))
		if err != nil {
			continue
		}
		if n > maxSeq {
			maxSeq = n
		}
	}
	if maxSeq < 0 {
		return 0, fmt.Errorf("no snapshots found in %s", dir)
	}
	return maxSeq, nil
}

func seqName(seq int, ext string) string {
	return fmt.Sprintf("%010d%s", seq, ext)
}
