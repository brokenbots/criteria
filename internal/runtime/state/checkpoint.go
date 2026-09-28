// Checkpoint persistence on the WS18 local snapshot substrate (CRI-202).
//
// The engine persists each adapter session's state blob at step boundaries
// under <base>/runs/<runID>/snapshots/<sessionID>/<seq>.{json,bin}. A step is
// only done once its checkpoint is durable, so restore paths read the latest
// complete checkpoint and every read failure that could hide state loss is
// loud: a truncated blob, a missing blob, or an interrupted write (blob
// without metadata) fails the run with a diagnostic naming the session and
// the schema tag instead of silently falling back to a fresh start.
package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/brokenbots/criteria/internal/adapterhost"
)

// CheckpointRunDir returns the directory holding a run's adapter checkpoints.
func CheckpointRunDir(base, runID string) string {
	return filepath.Join(base, "runs", runID, "snapshots")
}

// CheckpointStore scopes checkpoint persistence to one run's state home. The
// engine owns instances of it: saves ride the SessionManager.CheckpointSave
// callback, restore reads Latest, and the criteria-side janitor uses DeleteRun
// once a run reaches a terminal status (stopped/paused runs are exempt).
type CheckpointStore struct {
	base  string
	runID string
}

// NewCheckpointStore builds a checkpoint store rooted at base for runID.
func NewCheckpointStore(base, runID string) *CheckpointStore {
	return &CheckpointStore{base: base, runID: runID}
}

// Save persists a checkpoint for the named session and returns its sequence.
func (c *CheckpointStore) Save(sessionID string, snap *adapterhost.SessionSnapshot) (int, error) {
	return WriteSnapshot(SnapshotDir(c.base, c.runID, sessionID), snap)
}

// Latest reads the newest complete checkpoint for the named session,
// verifying the recorded digest against the blob bytes.
func (c *CheckpointStore) Latest(sessionID string) (*adapterhost.SessionSnapshot, error) {
	return ReadLatestSnapshot(SnapshotDir(c.base, c.runID, sessionID))
}

// Sessions lists the session IDs that hold checkpoints for this run.
func (c *CheckpointStore) Sessions() ([]string, error) {
	return ListSnapshotSessions(c.base, c.runID)
}

// DeleteRun removes every checkpoint this run persisted. The janitor calls it
// only for terminal runs: paused/stopped runs keep their checkpoints so their
// state survives the retention sweep.
func (c *CheckpointStore) DeleteRun() error {
	dir := CheckpointRunDir(c.base, c.runID)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("delete checkpoints for run %q: %w", c.runID, err)
	}
	return nil
}

// DeleteSession removes the checkpoints of one session of this run.
func (c *CheckpointStore) DeleteSession(sessionID string) error {
	dir := SnapshotDir(c.base, c.runID, sessionID)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("delete checkpoints for session %q of run %q: %w", sessionID, c.runID, err)
	}
	return nil
}

// seqFiles records which checkpoint files exist for a sequence number.
type seqFiles struct {
	jsonExists bool
	binExists  bool
}

// ReadLatestSnapshot finds the highest sequence number recorded in dir and
// reconstructs a SessionSnapshot from the corresponding .json + .bin files.
//
// Failures are loud and name the session (the directory basename) so a
// restore can surface "adapter X's checkpoint is unreadable" instead of
// silently starting fresh:
//   - the highest sequence has a blob but no metadata (a previous save was
//     interrupted between the two files);
//   - the highest sequence has metadata but no blob;
//   - the blob bytes do not hash to the recorded digest (truncated or
//     corrupted blob).
//
// Metadata without a recorded digest (pre-CRI-202 checkpoints) is returned
// unverified: there is nothing to compare against, and the schema tag on the
// metadata still gates the restore.
func ReadLatestSnapshot(dir string) (*adapterhost.SessionSnapshot, error) {
	session := filepath.Base(dir)
	files, err := scanCheckpointFiles(dir)
	if err != nil {
		return nil, fmt.Errorf("scan checkpoints for session %q: %w", session, err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no snapshots found in %s: %w", dir, ErrNoSnapshots)
	}
	maxSeq := -1
	for seq := range files {
		if seq > maxSeq {
			maxSeq = seq
		}
	}
	highest := files[maxSeq]
	if highest.binExists && !highest.jsonExists {
		return nil, fmt.Errorf("checkpoint for session %q is incomplete: state blob %d has no metadata (a previous save was interrupted); refusing to restore partial state", session, maxSeq)
	}
	if !highest.binExists && highest.jsonExists {
		return nil, fmt.Errorf("checkpoint for session %q is missing its state blob (seq %d); refusing to restore partial state", session, maxSeq)
	}

	snap, err := readCheckpointMetadata(dir, maxSeq)
	if err != nil {
		return nil, err
	}
	blob, err := os.ReadFile(filepath.Join(dir, seqName(maxSeq, ".bin")))
	if err != nil {
		return nil, fmt.Errorf("read checkpoint blob for session %q (schema %q): %w", session, snap.StateSchema, err)
	}
	if err := verifyStateDigest(session, snap.StateSchema, snap.StateDigest, blob); err != nil {
		return nil, err
	}
	snap.AdapterState = blob
	return snap, nil
}

// readCheckpointMetadata loads and unmarshals the .json metadata for seq.
func readCheckpointMetadata(dir string, seq int) (*adapterhost.SessionSnapshot, error) {
	session := filepath.Base(dir)
	data, err := os.ReadFile(filepath.Join(dir, seqName(seq, ".json")))
	if err != nil {
		return nil, fmt.Errorf("read checkpoint metadata for session %q: %w", session, err)
	}
	var snap adapterhost.SessionSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("unmarshal checkpoint metadata for session %q (schema tag in metadata unavailable): %w", session, err)
	}
	return &snap, nil
}

// verifyStateDigest checks blob bytes against a "sha256:<hex>" digest. An
// unsupported digest prefix means the checkpoint was written by a newer
// binary the current one cannot verify — refuse loudly rather than replay
// unverifiable bytes.
func verifyStateDigest(session, schema, digest string, blob []byte) error {
	if digest == "" {
		return nil
	}
	alg, hexSum, ok := strings.Cut(digest, ":")
	if !ok || alg != "sha256" {
		return fmt.Errorf("checkpoint for session %q (schema %q) carries unsupported state digest %q; older binaries cannot verify newer checkpoint digests and must refuse loudly", session, schema, digest)
	}
	sum := sha256.Sum256(blob)
	if got := hex.EncodeToString(sum[:]); got != hexSum {
		return fmt.Errorf("checkpoint blob for session %q (schema %q) is truncated or corrupted: recorded digest %s, computed sha256 %s", session, schema, digest, got)
	}
	return nil
}

// scanCheckpointFiles maps every sequence number in dir to the files it has.
func scanCheckpointFiles(dir string) (map[int]seqFiles, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make(map[int]seqFiles)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		var ext string
		switch {
		case strings.HasSuffix(name, ".json"):
			ext = ".json"
		case strings.HasSuffix(name, ".bin"):
			ext = ".bin"
		default:
			continue
		}
		seq, err := strconv.Atoi(strings.TrimSuffix(name, ext))
		if err != nil {
			continue
		}
		f := out[seq]
		if ext == ".json" {
			f.jsonExists = true
		} else {
			f.binExists = true
		}
		out[seq] = f
	}
	return out, nil
}