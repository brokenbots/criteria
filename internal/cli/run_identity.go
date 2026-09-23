package cli

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/brokenbots/criteria/internal/engine"
)

// runIdentityFingerprint computes a stable digest identifying one logical CLI
// invocation: the workflow being executed, the orchestrator it targets, and
// the variable inputs supplied on the command line.
//
// CRI-125: when a runner pod restarts mid-run, the restarted process must not
// silently start a second run (with a fresh run_id) against the same
// workflow. Crash-recovery checkpoints store this fingerprint, so a restart
// matches it and resumes — or keeps failed — the original run instead of
// forking a zombie run that breaks adapter state.
//
// The digest covers var-file contents rather than their paths: the operator
// materialises per-pod scratch paths (mktemp), so paths differ across
// restarts while the file bytes stay identical. Override strings are sorted
// so flag order does not affect identity. Raw secret values are never
// persisted anywhere — only the one-way digest is written to checkpoints.
//
// It returns "" when identity cannot be computed (unreadable var file, empty
// workflow path). "" never matches a checkpoint fingerprint, so callers fall
// back to the pre-CRI-125 behavior (start a fresh run) rather than
// suppressing.
func runIdentityFingerprint(workflowPath, serverURL string, varFiles, varOverrides []string) string {
	h := sha256.New()
	writeField := func(field string) error {
		var lenBuf [4]byte
		binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(field)))
		if _, err := h.Write(lenBuf[:]); err != nil {
			return err
		}
		_, err := h.Write([]byte(field))
		return err
	}
	fields := make([]string, 0, 4+len(varOverrides)+len(varFiles))
	fields = append(fields, "criteria-run-identity/v1")

	path := strings.TrimSpace(workflowPath)
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return ""
	}
	fields = append(fields, abs, strings.TrimRight(strings.TrimSpace(serverURL), "/"))

	sortedOverrides := make([]string, len(varOverrides))
	for i, v := range varOverrides {
		sortedOverrides[i] = strings.TrimSpace(v)
	}
	sort.Strings(sortedOverrides)
	fields = append(fields, fmt.Sprint(len(sortedOverrides)))
	fields = append(fields, sortedOverrides...)

	fields = append(fields, fmt.Sprint(len(varFiles)))
	for _, vf := range varFiles {
		raw, err := os.ReadFile(strings.TrimSpace(vf))
		if err != nil {
			return ""
		}
		fields = append(fields, string(raw))
	}

	for _, field := range fields {
		if err := writeField(field); err != nil {
			return ""
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// invocationIdentityMarkerName is the file persisted in every run data
// directory recording the fingerprint (CRI-125) of the invocation that
// constructed the run's engines.
const invocationIdentityMarkerName = "invocation-identity.json"

// invocationIdentityMarker is the on-disk shape of the per-run invocation
// fingerprint marker (CRI-304). Fresh replays that follow a checkpoint-
// consuming resume read these markers to find prior invocations of the same
// logical run whose surviving per-scope adapter instances can be adopted
// instead of wedging on fresh rotations.
type invocationIdentityMarker struct {
	Fingerprint string    `json:"fingerprint"`
	RecordedAt  time.Time `json:"recorded_at"`
}

func invocationIdentityMarkerPath(dataDir string) string {
	return filepath.Join(dataDir, invocationIdentityMarkerName)
}

// writeInvocationIdentityMarker persists the invocation fingerprint in the
// run's data directory. Runs without a fingerprint (legacy checkpoints, agent
// runs) record nothing: an empty fingerprint never matches anything, and the
// run's own directory is excluded from adoption scans regardless.
func writeInvocationIdentityMarker(dataDir, fingerprint string) error {
	if dataDir == "" || fingerprint == "" {
		return nil
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(invocationIdentityMarker{
		Fingerprint: fingerprint,
		RecordedAt:  time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	return os.WriteFile(invocationIdentityMarkerPath(dataDir), raw, 0o600)
}

// readInvocationIdentityMarker loads the marker persisted by a prior
// invocation of the run owning dataDir.
func readInvocationIdentityMarker(dataDir string) (invocationIdentityMarker, error) {
	raw, err := os.ReadFile(invocationIdentityMarkerPath(dataDir))
	if err != nil {
		return invocationIdentityMarker{}, err
	}
	var marker invocationIdentityMarker
	if err := json.Unmarshal(raw, &marker); err != nil {
		return invocationIdentityMarker{}, err
	}
	return marker, nil
}

// adoptablePriorRunDirs lists the run data directories of prior invocations
// of the same logical run (CRI-304): directories under <stateDir>/runs whose
// invocation-identity marker carries the fingerprint, excluding the caller's
// own run and any run with an in-flight crash-recovery checkpoint (those are
// owned by the CRI-125 resume sweep and must not be adopted from). The list
// is ordered newest marker first so a fresh replay prefers the most recent
// prior invocation's surviving instances. An empty fingerprint yields nil:
// identity is unavailable, so nothing is adoptable.
func adoptablePriorRunDirs(fingerprint, excludeRunID string) ([]string, error) {
	if fingerprint == "" {
		return nil, nil
	}
	d, err := stateDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(d, "runs"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	type markerCandidate struct {
		dir  string
		when time.Time
	}
	candidates := make([]markerCandidate, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == excludeRunID {
			continue
		}
		// A run with a live crash-recovery checkpoint is in-flight and owned
		// by the CRI-125 resume sweep; never adopt from it.
		cpPath, cpErr := checkpointFilePath(entry.Name())
		if cpErr != nil {
			continue
		}
		if _, statErr := os.Stat(cpPath); statErr == nil {
			continue
		}
		dataDir := filepath.Join(d, "runs", entry.Name())
		marker, markerErr := readInvocationIdentityMarker(dataDir)
		if markerErr != nil || marker.Fingerprint != fingerprint {
			continue
		}
		candidates = append(candidates, markerCandidate{dir: dataDir, when: marker.RecordedAt})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].when.After(candidates[j].when) })
	dirs := make([]string, len(candidates))
	for i, c := range candidates {
		dirs[i] = c.dir
	}
	return dirs, nil
}

// engineAdoptionOptions returns the CRI-304 engine options for a run engine:
// persist the invocation-identity marker in the run's data directory and list
// prior invocations of the same fingerprint whose surviving per-scope adapter
// instances are adoptable. Failures degrade to no adoption — the engine then
// rotates fresh exactly as before this change — with a warning so the
// regression stays diagnosable.
func engineAdoptionOptions(dataDir, fingerprint, runID string) []engine.Option {
	if err := writeInvocationIdentityMarker(dataDir, fingerprint); err != nil {
		slog.Warn("persisting invocation identity marker failed; cross-run adapter adoption disabled",
			"run_id", runID, "error", err)
	}
	dirs, err := adoptablePriorRunDirs(fingerprint, runID)
	if err != nil {
		slog.Warn("enumerating adoptable prior run directories failed; cross-run adapter adoption disabled",
			"run_id", runID, "error", err)
		return nil
	}
	if len(dirs) == 0 {
		return nil
	}
	return []engine.Option{engine.WithAdoptableRunDirs(dirs)}
}
