// Package cli implements the criteria CLI commands.
//
// This file implements the workflow cache index (ADR-0005 D5): a JSON file at
// <CRITERIA_HOME>/cache/workflows/index.json recording every fetched workflow
// tree. The fetcher maintains the index at materialization (rename-in) points.
// Per D5 the index is bookkeeping only — the pin lockfile stays the pin
// authority — and a corrupt or missing index degrades safely: fetch skips the
// update instead of overwriting (which would mark other cached trees
// unreachable) and prune retains all trees when reachability cannot be
// established.
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// workflowCacheIndexFileName is the index file inside the workflow cache root.
const workflowCacheIndexFileName = "index.json"

// workflowCacheIndexLockFileName guards concurrent index updates.
const workflowCacheIndexLockFileName = ".index.lock"

// workflowCacheIndexVersion is the on-disk schema version of the index.
const workflowCacheIndexVersion = 1

// errWorkflowCacheIndexCorrupt marks an index file that exists but cannot be
// parsed. Callers degrade safely rather than treating the cache as empty.
var errWorkflowCacheIndexCorrupt = errors.New("workflow cache index is corrupt")

// workflowCacheEntry records one fetched workflow tree. Field names mirror the
// snake_case JSON style of the run metadata file.
type workflowCacheEntry struct {
	Kind        string    `json:"kind"`
	Source      string    `json:"source"`
	ResolvedRef string    `json:"resolved_ref"`
	CachePath   string    `json:"cache_path"`
	FetchedAt   time.Time `json:"fetched_at"`
}

// workflowCacheIndex is the on-disk document.
type workflowCacheIndex struct {
	Version int                  `json:"version"`
	Entries []workflowCacheEntry `json:"entries"`
}

// workflowCacheIndexPath returns the absolute path of the index file under the
// workflow cache root.
func workflowCacheIndexPath(cacheRoot string) string {
	return filepath.Join(cacheRoot, workflowCacheIndexFileName)
}

// workflowCacheIndexLockPath returns the absolute path of the advisory lock
// file guarding index updates.
func workflowCacheIndexLockPath(cacheRoot string) string {
	return filepath.Join(cacheRoot, workflowCacheIndexLockFileName)
}

// readWorkflowCacheIndex loads the index, degrading safely. A missing file
// yields an empty index and present=false. A corrupt file yields the corrupt
// sentinel error: callers must not overwrite the file in that state, because a
// fresh index would mark every other cached tree unreachable.
func readWorkflowCacheIndex(cacheRoot string) (workflowCacheIndex, bool, error) {
	path := workflowCacheIndexPath(cacheRoot)
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return workflowCacheIndex{Version: workflowCacheIndexVersion}, false, nil
		}
		return workflowCacheIndex{}, false, fmt.Errorf("read workflow cache index: %w", err)
	}

	var index workflowCacheIndex
	if err := json.Unmarshal(raw, &index); err != nil {
		return workflowCacheIndex{}, false, fmt.Errorf("%w: %s", errWorkflowCacheIndexCorrupt, path)
	}
	if index.Version != workflowCacheIndexVersion {
		return workflowCacheIndex{}, false, fmt.Errorf("%w: unsupported schema version %d in %s",
			errWorkflowCacheIndexCorrupt, index.Version, path)
	}
	return index, true, nil
}

// upsertWorkflowCacheEntry atomically inserts or refreshes the entry for the
// given tree under the index lock. Identical rows are skipped to avoid
// rewriting the file on warm-cache hits. The returned error is bookkeeping
// only; fetch callers treat it as best-effort.
func upsertWorkflowCacheEntry(cacheRoot string, entry *workflowCacheEntry) error {
	unlock, err := lockWorkflowCacheIndex(cacheRoot)
	if err != nil {
		return err
	}
	defer unlock()
	return upsertWorkflowCacheEntryLocked(cacheRoot, entry)
}

// upsertWorkflowCacheEntryLocked is upsertWorkflowCacheEntry for callers that
// already hold the index lock — the fetcher's publish path holds it across
// rename-in and recording so a concurrent gc cannot sweep the tree in between.
func upsertWorkflowCacheEntryLocked(cacheRoot string, entry *workflowCacheEntry) error {
	if err := validWorkflowCacheRelPath(entry.CachePath); err != nil {
		return err
	}
	entry.CachePath = filepath.ToSlash(filepath.Clean(entry.CachePath))

	index, _, err := readWorkflowCacheIndex(cacheRoot)
	if err != nil {
		if errors.Is(err, errWorkflowCacheIndexCorrupt) {
			// Degrade-safe: leave the corrupt file untouched rather than
			// dropping every other cached tree's reachability record.
			return err
		}
		return err
	}

	updated := false
	for i := range index.Entries {
		if index.Entries[i].CachePath == entry.CachePath {
			if workflowCacheEntriesEqual(&index.Entries[i], entry) {
				return nil
			}
			index.Entries[i] = *entry
			updated = true
			break
		}
	}
	if !updated {
		index.Entries = append(index.Entries, *entry)
	}

	return writeWorkflowCacheIndex(cacheRoot, &index)
}

// writeWorkflowCacheIndex persists the index atomically (temp file + rename in
// the cache root) so readers never observe a partial file. The caller must hold
// the index lock.
func writeWorkflowCacheIndex(cacheRoot string, index *workflowCacheIndex) error {
	if index.Version == 0 {
		index.Version = workflowCacheIndexVersion
	}
	sort.Slice(index.Entries, func(i, j int) bool {
		return index.Entries[i].CachePath < index.Entries[j].CachePath
	})

	raw, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return fmt.Errorf("encode workflow cache index: %w", err)
	}
	raw = append(raw, '\n')

	if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
		return fmt.Errorf("create workflow cache root: %w", err)
	}

	path := workflowCacheIndexPath(cacheRoot)
	tmp, err := os.CreateTemp(cacheRoot, ".index-*.tmp")
	if err != nil {
		return fmt.Errorf("create workflow cache index temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("write workflow cache index: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close workflow cache index temp file: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("chmod workflow cache index: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("publish workflow cache index: %w", err)
	}
	return nil
}

// workflowCacheEntriesEqual reports whether two entries record the same tree
// state (path-insensitive; the caller keys on cache path).
func workflowCacheEntriesEqual(a, b *workflowCacheEntry) bool {
	return a.Kind == b.Kind &&
		a.Source == b.Source &&
		a.ResolvedRef == b.ResolvedRef &&
		a.CachePath == b.CachePath &&
		a.FetchedAt.Equal(b.FetchedAt)
}

// isValidWorkflowCacheRelPath reports whether p is a clean, relative path
// that stays inside the workflow cache root.
func isValidWorkflowCacheRelPath(p string) bool {
	return validWorkflowCacheRelPath(p) == nil
}

// validWorkflowCacheRelPath validates that p is a clean, relative path that
// stays inside the workflow cache root. It is the guard that lets prune treat
// entry CachePath values as safe to join and delete.
func validWorkflowCacheRelPath(p string) error {
	trimmed := strings.TrimSpace(p)
	if trimmed == "" || trimmed == "." {
		return fmt.Errorf("workflow cache path %q is not a tree path", p)
	}
	cleaned := filepath.Clean(trimmed)
	if filepath.IsAbs(cleaned) {
		return fmt.Errorf("workflow cache path %q must be relative", p)
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) || strings.HasPrefix(cleaned, "../") {
		return fmt.Errorf("workflow cache path %q escapes the cache root", p)
	}
	if strings.ContainsRune(cleaned, '\\') {
		return fmt.Errorf("workflow cache path %q contains a path separator escape", p)
	}
	return nil
}

// workflowCacheRelPathToAbsolute joins a validated relative entry path back to
// the cache root. The caller must have validated the rel path first.
func workflowCacheRelPathToAbsolute(cacheRoot, rel string) string {
	return filepath.Join(cacheRoot, filepath.FromSlash(rel))
}

// workflowCacheRelPathCrossesSymlink reports whether any ancestor component of
// the validated rel path below cacheRoot is a symlink. Deletion through such a
// path — e.g. os.RemoveAll on an evicted entry — would act on the symlink's
// target outside the cache root, so callers must treat the entry as unsafe:
// retain it and never join it for deletion. The final component is not
// checked: os.Remove removes a symlinked leaf as a link, never through it,
// so a symlinked leaf stays inside the cache root either way. A missing
// ancestor component reports false (nothing to traverse); other stat errors
// surface so callers can retain conservatively.
func workflowCacheRelPathCrossesSymlink(cacheRoot, rel string) (bool, error) {
	cleaned := filepath.Clean(filepath.FromSlash(rel))
	if cleaned == "." {
		return false, nil
	}
	parts := strings.Split(cleaned, string(filepath.Separator))
	cur := cacheRoot
	for _, part := range parts[:len(parts)-1] {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return false, nil
			}
			return false, err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return true, nil
		}
	}
	return false, nil
}
