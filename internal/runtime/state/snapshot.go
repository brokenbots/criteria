// Package state provides persistence helpers for Criteria runtime state,
// including session snapshots for pause/resume across host restarts.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/brokenbots/criteria/internal/adapterhost"
)

// ErrNoSnapshots reports that a snapshot directory holds no checkpoint
// files. Restore paths treat it as "never checkpointed" (a logged fresh
// start, not an error); nextSeq treats it as sequence 1.
var ErrNoSnapshots = errors.New("no snapshots found")

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

// ReadLatestSnapshot (the loud, digest-verifying variant) lives in
// checkpoint.go (CRI-202).

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
	if os.IsNotExist(err) || errors.Is(err, ErrNoSnapshots) {
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
		return 0, fmt.Errorf("no snapshots found in %s: %w", dir, ErrNoSnapshots)
	}
	return maxSeq, nil
}

func seqName(seq int, ext string) string {
	return fmt.Sprintf("%010d%s", seq, ext)
}
