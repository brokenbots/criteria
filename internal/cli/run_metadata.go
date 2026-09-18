package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// RunMetadata is the run-scoped provenance record published at admission
// (ADR-0005 D6; the RunMetadata pattern from CRI-131): the resolved workflow
// origin a run executed, so provenance stays answerable after the run
// without re-deriving it from cache state. A run resolved from a remote
// source records one record at runs/<run_id>/run-metadata.json under the
// state dir; runs from local sources record nothing (there is no origin).
//
// The record is a JSON payload only: the wire (proto) event shape is
// unchanged. Every recorded field is credential-free: the source is stored
// through redactSourceForLog and the cache path derives from slugForSource.
// Expected-pin enforcement on the recorded ref is CRI-226's job.
type RunMetadata struct {
	// Kind is the resolved source form: "git" or "archive".
	Kind string `json:"kind,omitempty"`
	// Source is the workflow source with userinfo credentials redacted.
	Source string `json:"source,omitempty"`
	// ResolvedRef is the immutable resolved version: a git commit SHA for
	// git sources or the sha256:<digest> content digest for archive sources.
	ResolvedRef string `json:"resolved_ref,omitempty"`
	// CachePath is the local directory the workflow was materialized into.
	CachePath string `json:"cache_path,omitempty"`
	// FetchedAt is the UTC fetch timestamp of the cached tree.
	FetchedAt time.Time `json:"fetched_at"`
}

// runMetadataFromOrigin builds the admission record for a resolved origin,
// redacting the source so no credential reaches the recorded fields.
func runMetadataFromOrigin(origin *WorkflowOrigin) *RunMetadata {
	return &RunMetadata{
		Kind:        origin.Kind,
		Source:      redactSourceForLog(origin.Source),
		ResolvedRef: origin.ResolvedRef,
		CachePath:   origin.Path,
		FetchedAt:   origin.FetchedAt,
	}
}

// runMetadataFilePath returns runs/<run_id>/run-metadata.json under the
// state dir, mirroring the run-state layout.
func runMetadataFilePath(runID string) (string, error) {
	if runID == "" {
		return "", errors.New("run metadata requires a run id")
	}
	d, err := stateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "runs", runID, "run-metadata.json"), nil
}

// writeRunMetadata persists the run's provenance record as
// runs/<run_id>/run-metadata.json with state-file permissions (0600).
func writeRunMetadata(runID string, md *RunMetadata) error {
	path, err := runMetadataFilePath(runID)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(md, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create run metadata dir: %w", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("write run metadata: %w", err)
	}
	return nil
}

// publishRunMetadata records the resolved workflow origin as the run's
// RunMetadata admission record. Local sources have no origin and record
// nothing; a recording failure degrades to a warning because provenance
// recording must not fail the run it describes.
func publishRunMetadata(runID string, origin *WorkflowOrigin, log *slog.Logger) {
	if origin == nil {
		return
	}
	if err := writeRunMetadata(runID, runMetadataFromOrigin(origin)); err != nil {
		log.Warn("failed to record run metadata provenance", "run_id", runID, "error", err)
	}
}
