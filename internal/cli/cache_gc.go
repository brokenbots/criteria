package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/brokenbots/criteria/internal/dirs"
)

// NewCacheCmd returns the `criteria cache` command group for managing the
// local workflow source cache (ADR-0005 D5).
func NewCacheCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cache",
		Short: "Manage the local workflow source cache",
	}
	cmd.AddCommand(newCacheGcCmd())
	return cmd
}

// newCacheGcCmd builds `criteria cache gc`, which removes stale or
// unreferenced workflow trees from the cache under CRITERIA_HOME. It follows
// the reachability model of the OCI adapter cache's index.json + prune
// (internal/adapter/oci/gc.go): the workflow cache index is the sole
// reachability root, so entries it records (per fetched tree: source,
// ref/digest, cache path, fetched_at) are retained and everything else under
// the cache root is pruned. The index is the reachability root because
// fetched trees are content-addressed (resolved git SHA / archive sha256
// digest): pruning an entry can only cost a re-fetch, never correctness, and
// the pin lockfile — not the index — remains the pin authority (D7). Per
// ADR-0005 D5 and D11 a missing or corrupt index degrades safely: prune
// retains all cached trees rather than guessing at reachability.
func newCacheGcCmd() *cobra.Command {
	var olderThan string

	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Remove stale or unreferenced workflow trees from the local cache",
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			return runCacheGc(olderThan, cmd.OutOrStdout())
		},
	}

	cmd.Flags().StringVar(&olderThan, "older-than", "", "Remove cached trees not fetched within this duration (e.g. 30d)")
	return cmd
}

func runCacheGc(olderThan string, out io.Writer) error {
	if out == nil {
		out = os.Stderr
	}
	var opts workflowCacheGcOptions
	if olderThan != "" {
		d, err := parseHumanDuration(olderThan)
		if err != nil {
			return fmt.Errorf("parse --older-than: %w", err)
		}
		opts.OlderThan = d
	}

	cacheRoot, err := dirs.CacheWorkflows()
	if err != nil {
		return err
	}

	result, err := gcWorkflowCache(cacheRoot, opts)
	if err != nil {
		return fmt.Errorf("gc workflow cache: %w", err)
	}

	fmt.Fprintf(out, "pruned %d workflow trees, freed %d bytes, dropped %d index entries\n",
		result.RemovedTrees, result.FreedBytes, result.DroppedEntries)
	for _, warning := range result.Warnings {
		fmt.Fprintf(out, "warning: %s\n", warning)
	}
	return nil
}

// workflowCacheGcOptions configures gcWorkflowCache.
type workflowCacheGcOptions struct {
	// OlderThan evicts entries whose fetched_at is older than this duration.
	// Zero disables age-based eviction.
	OlderThan time.Duration
}

// workflowCacheGcResult reports what gcWorkflowCache did.
type workflowCacheGcResult struct {
	RemovedTrees   int
	FreedBytes     int64
	DroppedEntries int
	Warnings       []string
}

// gcWorkflowCache prunes the workflow source cache under cacheRoot in three
// phases: age-based eviction (--older-than), deletion of trees unreachable
// from the index, and a final index rewrite dropping entries whose tree has
// vanished. The whole operation runs under the index lock so a concurrent
// fetch cannot rename a tree in while reachability is being computed.
func gcWorkflowCache(cacheRoot string, opts workflowCacheGcOptions) (workflowCacheGcResult, error) {
	var result workflowCacheGcResult

	if _, err := os.Stat(cacheRoot); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return result, nil
		}
		return result, fmt.Errorf("stat workflow cache root: %w", err)
	}

	unlock, err := lockWorkflowCacheIndex(cacheRoot)
	if err != nil {
		return result, err
	}
	defer unlock()

	index, present, err := readWorkflowCacheIndex(cacheRoot)
	if err != nil {
		if errors.Is(err, errWorkflowCacheIndexCorrupt) {
			result.Warnings = append(result.Warnings, "workflow cache index is corrupt; retaining all cached trees")
			return result, nil
		}
		return result, err
	}
	if !present && workflowCacheHasTrees(cacheRoot) {
		result.Warnings = append(result.Warnings, "workflow cache index not found; retaining all cached trees")
		return result, nil
	}

	evicted := false
	if opts.OlderThan > 0 {
		before := len(index.Entries)
		index.Entries = evictStaleWorkflowTrees(cacheRoot, index.Entries, time.Now().Add(-opts.OlderThan), &result)
		evicted = len(index.Entries) != before
	}
	if err := sweepUnreachableWorkflowTrees(cacheRoot, index.Entries, &result); err != nil {
		return result, err
	}
	if err := pruneWorkflowCacheIndex(cacheRoot, index.Entries, evicted, &result); err != nil {
		return result, err
	}
	return result, nil
}

// workflowCacheHasTrees reports whether the cache root holds any tree
// directories, distinguishing a genuinely empty cache from a missing index
// over live content.
func workflowCacheHasTrees(cacheRoot string) bool {
	entries, err := os.ReadDir(cacheRoot)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return true
		}
	}
	return false
}

// evictStaleWorkflowTrees removes trees whose recorded fetched_at predates
// cutoff and returns the surviving entries. Entries with a zero fetched_at are
// kept conservatively: their age is unknown, so eviction cannot be justified.
func evictStaleWorkflowTrees(cacheRoot string, entries []workflowCacheEntry, cutoff time.Time, result *workflowCacheGcResult) []workflowCacheEntry {
	survivors := make([]workflowCacheEntry, 0, len(entries))
	for _, entry := range entries {
		stale := !entry.FetchedAt.IsZero() && entry.FetchedAt.Before(cutoff)
		if !stale {
			survivors = append(survivors, entry)
			continue
		}
		if !isValidWorkflowCacheRelPath(entry.CachePath) {
			result.DroppedEntries++
			continue
		}
		treePath := workflowCacheRelPathToAbsolute(cacheRoot, entry.CachePath)
		size := dirTreeSize(treePath)
		if err := os.RemoveAll(treePath); err != nil {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("remove stale tree %s: %v", entry.CachePath, err))
			survivors = append(survivors, entry)
			continue
		}
		result.RemovedTrees++
		result.FreedBytes += size
		result.DroppedEntries++
	}
	return survivors
}

// sweepUnreachableWorkflowTrees walks the cache root and deletes every
// directory not protected by the index (entry paths plus all their ancestor
// directories). Unreachable directories are deleted whole and skipped; stray
// files directly under the cache root — leftover temp files — are removed,
// while the index and its lock file stay. The walk never follows symlinks, so
// removal stays inside cacheRoot.
func sweepUnreachableWorkflowTrees(cacheRoot string, entries []workflowCacheEntry, result *workflowCacheGcResult) error {
	protected := protectedWorkflowPaths(entries)

	return filepath.WalkDir(cacheRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == cacheRoot {
			return nil
		}
		if !d.IsDir() {
			return removeStrayWorkflowCacheFile(cacheRoot, path, d, result)
		}

		rel, err := filepath.Rel(cacheRoot, path)
		if err != nil {
			return err
		}
		if protected[filepath.ToSlash(rel)] {
			return nil
		}
		if !isValidWorkflowCacheRelPath(rel) {
			// Unreachable by construction (below cacheRoot); leave alone.
			return nil
		}
		size := dirTreeSize(path)
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove unreachable workflow cache tree %s: %w", filepath.ToSlash(rel), err)
		}
		result.RemovedTrees++
		result.FreedBytes += size
		return fs.SkipDir
	})
}

// removeStrayWorkflowCacheFile removes leftover files directly under the cache
// root, except the index and its lock file. Files deeper inside the cache sit
// within trees the walk either protects or deletes whole, so they stay.
func removeStrayWorkflowCacheFile(cacheRoot, path string, d fs.DirEntry, result *workflowCacheGcResult) error {
	if filepath.Dir(path) != cacheRoot {
		return nil
	}
	switch d.Name() {
	case workflowCacheIndexFileName, workflowCacheIndexLockFileName:
		return nil
	}
	if info, err := d.Info(); err == nil && info.Mode().IsRegular() {
		result.FreedBytes += info.Size()
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stray workflow cache file %s: %w", d.Name(), err)
	}
	return nil
}

// protectedWorkflowPaths maps every entry cache path plus all of its ancestor
// directories (and the cache root itself) to true. Ancestors must be protected
// so a nested cascade tree — cache/workflows/<root>/subworkflows/<child>/<v> —
// is reachable through its top-level slug even when the root has no direct
// entry of its own.
func protectedWorkflowPaths(entries []workflowCacheEntry) map[string]bool {
	protected := make(map[string]bool)
	protected["."] = true
	for _, entry := range entries {
		if !isValidWorkflowCacheRelPath(entry.CachePath) {
			continue
		}
		rel := filepath.ToSlash(filepath.Clean(entry.CachePath))
		protected[rel] = true
		for dir := filepath.ToSlash(filepath.Dir(rel)); dir != "." && dir != "/" && dir != ""; dir = filepath.ToSlash(filepath.Dir(dir)) {
			protected[dir] = true
		}
	}
	return protected
}

// pruneWorkflowCacheIndex rewrites the index dropping entries whose tree no
// longer exists on disk or whose cache path is invalid. Nothing is written
// when every entry survives and eviction already persisted its changes.
func pruneWorkflowCacheIndex(cacheRoot string, entries []workflowCacheEntry, force bool, result *workflowCacheGcResult) error {
	survivors := make([]workflowCacheEntry, 0, len(entries))
	dropped := 0
	for _, entry := range entries {
		if !isValidWorkflowCacheRelPath(entry.CachePath) {
			dropped++
			continue
		}
		_, err := os.Stat(workflowCacheRelPathToAbsolute(cacheRoot, entry.CachePath))
		switch {
		case err == nil:
			survivors = append(survivors, entry)
		case errors.Is(err, os.ErrNotExist):
			dropped++
		default:
			// Stat failed for an unknown reason; keep the entry
			// conservatively rather than losing reachability.
			survivors = append(survivors, entry)
		}
	}
	if dropped == 0 && !force {
		return nil
	}
	result.DroppedEntries += dropped

	index := workflowCacheIndex{Version: workflowCacheIndexVersion, Entries: survivors}
	return writeWorkflowCacheIndex(cacheRoot, &index)
}

// dirTreeSize returns the total size of regular files under path, or 0 when
// the walk fails (the tree is being deleted either way).
func dirTreeSize(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}
