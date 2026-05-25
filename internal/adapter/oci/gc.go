package oci

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// GCOptions controls which blobs the garbage collector removes.
type GCOptions struct {
	// MaxSize is the maximum total size (in bytes) of blobs kept under
	// blobs/sha256/. If the retained set exceeds this, refs are trimmed by
	// least-recently-used (mtime of manifest blob on disk) until the size
	// budget is satisfied. Zero means unlimited.
	MaxSize int64
	// OlderThan removes refs whose manifest blob mtime is older than this
	// duration from now. Zero means age is not a factor.
	OlderThan time.Duration
	// KeepReachable, when true, ensures that refs reachable from index.json
	// are never evicted even if they violate MaxSize or OlderThan constraints.
	// Unreachable orphan blobs are always removed regardless of this flag.
	KeepReachable bool
}

// GCResult reports what the garbage collector did.
type GCResult struct {
	// RemovedBlobs is the number of blob files deleted.
	RemovedBlobs int
	// FreedBytes is the total size freed.
	FreedBytes int64
	// Errors lists any non-fatal errors (e.g. inability to delete a single blob).
	Errors []error
}

// GC runs garbage collection on the layout according to opts.
//
// Algorithm:
//  1. Delete orphaned blobs (not reachable from index.json).
//  2. Unless KeepReachable, evict whole refs from index.json according to
//     OlderThan/MaxSize trimming (LRU by manifest blob mtime).
//  3. After eviction, atomically rewrite index.json without evicted refs, then
//     delete newly orphaned blobs.
func (l *Layout) GC(opts GCOptions) (GCResult, error) {
	release, err := l.Lock()
	if err != nil {
		return GCResult{}, err
	}
	defer release()

	var result GCResult

	// Phase 1: remove blobs not reachable from the current index.
	if err := l.gcDeleteOrphans(&result); err != nil {
		return result, err
	}

	// Phase 2: evict whole refs by OlderThan/MaxSize, then clean up.
	if !opts.KeepReachable && (opts.OlderThan > 0 || opts.MaxSize > 0) {
		if err := l.gcEvictRefs(opts, &result); err != nil {
			return result, err
		}
	}

	return result, nil
}

// gcDeleteOrphans deletes every blob in blobs/sha256/ that is not reachable
// from the current index.json.
func (l *Layout) gcDeleteOrphans(result *GCResult) error {
	reachable, err := l.reachableDigests()
	if err != nil {
		return err
	}
	return l.deleteUnreachableBlobs(reachable, result)
}

// gcEvictRefs identifies refs to evict according to opts, rewrites index.json,
// then runs a second orphan-deletion pass to clean up blobs from evicted refs.
func (l *Layout) gcEvictRefs(opts GCOptions, result *GCResult) error {
	ix, err := l.Index()
	if err != nil {
		return err
	}

	toEvict := l.selectEvictions(ix.Manifests, opts)
	if len(toEvict) == 0 {
		return nil
	}

	// Rewrite index.json without evicted refs.
	ix.Manifests = removeIndices(ix.Manifests, toEvict)
	if err := l.WriteIndex(ix); err != nil {
		return fmt.Errorf("oci: gc rewrite index: %w", err)
	}

	// Now clean up blobs that are no longer reachable.
	return l.gcDeleteOrphans(result)
}

// refMeta holds per-ref metadata for eviction decisions.
type refMeta struct {
	idx       int
	mtime     time.Time
	totalSize int64
}

// selectEvictions returns a set of descriptor indices to evict from manifests
// according to opts (OlderThan and/or MaxSize, LRU by manifest blob mtime).
func (l *Layout) selectEvictions(manifests []ocispec.Descriptor, opts GCOptions) map[int]bool {
	metas := make([]refMeta, 0, len(manifests))
	for i, m := range manifests {
		metas = append(metas, refMeta{
			idx:       i,
			mtime:     l.manifestMtime(m.Digest.String()),
			totalSize: l.refTotalSize(m.Digest.String()),
		})
	}

	evict := make(map[int]bool)

	if opts.OlderThan > 0 {
		cutoff := time.Now().Add(-opts.OlderThan)
		for _, m := range metas {
			if m.mtime.Before(cutoff) {
				evict[m.idx] = true
			}
		}
	}

	if opts.MaxSize > 0 {
		l.applyMaxSizeEvictions(metas, evict, opts.MaxSize)
	}

	return evict
}

// applyMaxSizeEvictions trims refs LRU until total reachable blob size ≤ maxSize.
func (l *Layout) applyMaxSizeEvictions(metas []refMeta, evict map[int]bool, maxSize int64) {
	var totalSize int64
	for _, m := range metas {
		if !evict[m.idx] {
			totalSize += m.totalSize
		}
	}
	if totalSize <= maxSize {
		return
	}

	// Sort surviving refs by mtime ascending (oldest = LRU first).
	surviving := make([]int, 0, len(metas))
	for _, m := range metas {
		if !evict[m.idx] {
			surviving = append(surviving, m.idx)
		}
	}
	sort.Slice(surviving, func(a, b int) bool {
		return metas[surviving[a]].mtime.Before(metas[surviving[b]].mtime)
	})

	for _, idx := range surviving {
		if totalSize <= maxSize {
			break
		}
		evict[idx] = true
		totalSize -= metas[idx].totalSize
	}
}

// deleteUnreachableBlobs deletes every file in blobs/sha256/ whose digest is
// not in the reachable set.
func (l *Layout) deleteUnreachableBlobs(reachable map[string]bool, result *GCResult) error {
	blobDir := filepath.Join(l.Root, "blobs", "sha256")
	entries, err := os.ReadDir(blobDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("oci: gc read blobs dir: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if reachable["sha256:"+entry.Name()] {
			continue
		}
		fi, err := entry.Info()
		if err != nil {
			result.Errors = append(result.Errors, err)
			continue
		}
		if removeErr := os.Remove(filepath.Join(blobDir, entry.Name())); removeErr != nil {
			result.Errors = append(result.Errors, removeErr)
			continue
		}
		result.RemovedBlobs++
		result.FreedBytes += fi.Size()
	}
	return nil
}

// manifestMtime returns the modification time of the manifest blob on disk.
// Returns the zero time if the file cannot be stat'd.
func (l *Layout) manifestMtime(d string) time.Time {
	fi, err := os.Stat(filepath.Join(l.Root, "blobs", "sha256", digestEncoded(d)))
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

// refTotalSize returns the total size (in bytes) of the manifest blob and all
// blobs it transitively references (layers + config).
func (l *Layout) refTotalSize(d string) int64 {
	set := make(map[string]bool)
	l.collectReachable(d, set)
	var total int64
	for digest := range set {
		fi, err := os.Stat(filepath.Join(l.Root, "blobs", "sha256", digestEncoded(digest)))
		if err == nil {
			total += fi.Size()
		}
	}
	return total
}

// removeIndices returns a new slice of descriptors with the given indices removed.
func removeIndices(descs []ocispec.Descriptor, remove map[int]bool) []ocispec.Descriptor {
	out := make([]ocispec.Descriptor, 0, len(descs)-len(remove))
	for i, d := range descs {
		if !remove[i] {
			out = append(out, d)
		}
	}
	return out
}

// reachableDigests returns the set of all digest strings (e.g. "sha256:abc…")
// that are reachable from index.json, including manifest blobs and any layers
// and config blobs they transitively reference.
func (l *Layout) reachableDigests() (map[string]bool, error) {
	ix, err := l.Index()
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool)
	for _, m := range ix.Manifests {
		l.collectReachable(m.Digest.String(), set)
	}
	return set, nil
}

// collectReachable adds d to the reachable set, then — if d is a manifest
// blob — parses it and recursively adds its config and layer digests.
func (l *Layout) collectReachable(d string, set map[string]bool) {
	if set[d] {
		return
	}
	set[d] = true

	// Attempt to parse the blob as an OCI manifest to include layers/config.
	// If it's not a manifest, the JSON unmarshal fails silently — that's fine.
	blobPath := filepath.Join(l.Root, "blobs", "sha256", digestEncoded(d))
	data, err := os.ReadFile(blobPath)
	if err != nil {
		return
	}

	var manifest ocispec.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil || len(manifest.Layers) == 0 {
		return
	}

	if manifest.Config.Digest != "" {
		set[manifest.Config.Digest.String()] = true
	}
	for _, layer := range manifest.Layers {
		set[layer.Digest.String()] = true
	}
}

// digestEncoded returns the hex-encoded portion of a "sha256:<hex>" string.
func digestEncoded(d string) string {
	const prefix = "sha256:"
	if len(d) > len(prefix) {
		return d[len(prefix):]
	}
	return d
}
