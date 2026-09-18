package cli

// M5.1 (CRI-229): workflow cache gc.
//
// criteria cache gc mirrors the OCI adapter cache's reachability model
// (internal/adapter/oci/gc.go): the workflow cache index is the reachability
// root, --older-than evicts stale entries, and per ADR-0005 D5/D11 a missing
// or corrupt index degrades safely — prune retains everything rather than
// guessing reachability.

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gcTestCacheRoot builds an isolated cache/workflows root for gc tests.
func gcTestCacheRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "cache", "workflows")
}

// writeGcTree materializes a fake workflow tree at rel (slash-separated)
// under cacheRoot containing the given file names.
func writeGcTree(t *testing.T, cacheRoot, rel string, files ...string) {
	t.Helper()
	dir := filepath.Join(cacheRoot, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(dir, 0o755))
	for _, name := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("body of "+name), 0o644))
	}
}

// writeGcIndex persists the given entries through the production writer.
func writeGcIndex(t *testing.T, cacheRoot string, entries []workflowCacheEntry) {
	t.Helper()
	index := workflowCacheIndex{Version: workflowCacheIndexVersion, Entries: entries}
	require.NoError(t, writeWorkflowCacheIndex(cacheRoot, &index))
}

// gcEntry builds an index entry for rel with fetchedAt ago.
func gcEntry(rel string, fetchedAt time.Time) workflowCacheEntry {
	return workflowCacheEntry{
		Kind:        "archive",
		Source:      "https://example.invalid/" + rel + ".tar.gz",
		ResolvedRef: "sha256:" + rel,
		CachePath:   rel,
		FetchedAt:   fetchedAt,
	}
}

func TestCacheGc_RemovesUnreachableRetainsReferenced(t *testing.T) {
	cacheRoot := gcTestCacheRoot(t)
	now := time.Now().UTC()
	writeGcIndex(t, cacheRoot, []workflowCacheEntry{gcEntry("sluga/v1", now)})
	writeGcTree(t, cacheRoot, "sluga/v1", "workflow.hcl")
	writeGcTree(t, cacheRoot, "slugb/v1", "workflow.hcl")
	// Nested cascade orphan under the referenced slug: the ancestor sluga is
	// protected, but the orphan version is not in the index.
	writeGcTree(t, cacheRoot, "sluga/subworkflows/orphan/v1", "workflow.hcl")

	result, err := gcWorkflowCache(cacheRoot, workflowCacheGcOptions{})
	require.NoError(t, err)

	assert.Equal(t, 2, result.RemovedTrees, "unreferenced slug and nested orphan are pruned")
	assert.Greater(t, result.FreedBytes, int64(0))
	assert.Empty(t, result.Warnings)

	// Referenced tree survives; unreferenced ones are gone.
	_, statErr := os.Stat(filepath.Join(cacheRoot, "sluga", "v1", "workflow.hcl"))
	require.NoError(t, statErr, "referenced tree must be retained")
	assert.NoDirExists(t, filepath.Join(cacheRoot, "slugb"))
	assert.NoDirExists(t, filepath.Join(cacheRoot, "sluga", "subworkflows"))

	index := readWorkflowCacheIndexForTest(t, cacheRoot)
	require.Len(t, index.Entries, 1)
	assert.Equal(t, "sluga/v1", index.Entries[0].CachePath)
}

func TestCacheGc_MissingIndexRetainsAll(t *testing.T) {
	cacheRoot := gcTestCacheRoot(t)
	writeGcTree(t, cacheRoot, "sluga/v1", "workflow.hcl")
	writeGcTree(t, cacheRoot, "slugb/v1", "workflow.hcl")

	result, err := gcWorkflowCache(cacheRoot, workflowCacheGcOptions{})
	require.NoError(t, err)

	assert.Equal(t, 0, result.RemovedTrees, "D11: no index means reachability cannot be established; retain")
	assert.Len(t, result.Warnings, 1)
	assert.Contains(t, result.Warnings[0], "not found")

	_, statErr := os.Stat(filepath.Join(cacheRoot, "sluga", "v1", "workflow.hcl"))
	require.NoError(t, statErr)
}

func TestCacheGc_CorruptIndexRetainsAll(t *testing.T) {
	cacheRoot := gcTestCacheRoot(t)
	corrupt := []byte(`{"version":1,"entries":[ broken`)
	writeWorkflowCacheIndexFile(t, cacheRoot, corrupt)
	writeGcTree(t, cacheRoot, "sluga/v1", "workflow.hcl")

	result, err := gcWorkflowCache(cacheRoot, workflowCacheGcOptions{})
	require.NoError(t, err, "corrupt index must degrade safely, not crash (ADR-0005 D5)")

	assert.Equal(t, 0, result.RemovedTrees, "corrupt index: retain all cached trees")
	assert.Len(t, result.Warnings, 1)
	assert.Contains(t, result.Warnings[0], "corrupt")

	_, statErr := os.Stat(filepath.Join(cacheRoot, "sluga", "v1", "workflow.hcl"))
	require.NoError(t, statErr)
	got, err := os.ReadFile(filepath.Join(cacheRoot, "index.json"))
	require.NoError(t, err)
	assert.Equal(t, string(corrupt), string(got), "corrupt index must be left untouched")
}

func TestCacheGc_OlderThanEvictsStale(t *testing.T) {
	cacheRoot := gcTestCacheRoot(t)
	now := time.Now().UTC()
	stale := now.Add(-48 * time.Hour)
	writeGcIndex(t, cacheRoot, []workflowCacheEntry{
		gcEntry("sluga/oldsha", stale),
		gcEntry("slugb/freshsha", now),
		// Zero fetched_at rows are kept conservatively: age unknown.
		{Kind: "git", CachePath: "slugc/zerosha", Source: "https://example.invalid/c.git", ResolvedRef: "sha-c"},
	})
	writeGcTree(t, cacheRoot, "sluga/oldsha", "workflow.hcl")
	writeGcTree(t, cacheRoot, "slugb/freshsha", "workflow.hcl")
	writeGcTree(t, cacheRoot, "slugc/zerosha", "workflow.hcl")

	result, err := gcWorkflowCache(cacheRoot, workflowCacheGcOptions{OlderThan: 24 * time.Hour})
	require.NoError(t, err)

	// Two removals: the stale version tree and the slug dir it emptied.
	assert.Equal(t, 2, result.RemovedTrees)
	assert.Equal(t, 1, result.DroppedEntries)
	assert.NoDirExists(t, filepath.Join(cacheRoot, "sluga"))
	assert.DirExists(t, filepath.Join(cacheRoot, "slugb", "freshsha"))
	assert.DirExists(t, filepath.Join(cacheRoot, "slugc", "zerosha"),
		"entries with unknown fetched_at must be retained")

	index := readWorkflowCacheIndexForTest(t, cacheRoot)
	require.Len(t, index.Entries, 2, "evicted entry must be persisted out of the index")
	assert.Equal(t, "slugb/freshsha", index.Entries[0].CachePath)
	assert.Equal(t, "slugc/zerosha", index.Entries[1].CachePath)
}

func TestCacheGc_DropsEntriesForMissingTrees(t *testing.T) {
	cacheRoot := gcTestCacheRoot(t)
	writeGcIndex(t, cacheRoot, []workflowCacheEntry{
		gcEntry("sluga/v1", time.Now().UTC()),
		gcEntry("gone/v1", time.Now().UTC()),
	})
	writeGcTree(t, cacheRoot, "sluga/v1", "workflow.hcl")

	result, err := gcWorkflowCache(cacheRoot, workflowCacheGcOptions{})
	require.NoError(t, err)

	assert.Equal(t, 1, result.DroppedEntries)
	index := readWorkflowCacheIndexForTest(t, cacheRoot)
	require.Len(t, index.Entries, 1)
	assert.Equal(t, "sluga/v1", index.Entries[0].CachePath)
}

func TestCacheGc_NeverDeletesOutsideCacheRoot(t *testing.T) {
	base := t.TempDir()
	cacheRoot := filepath.Join(base, "cache", "workflows")
	decoy := filepath.Join(base, "decoy.txt")
	require.NoError(t, os.WriteFile(decoy, []byte("do not delete"), 0o644))

	writeGcIndex(t, cacheRoot, []workflowCacheEntry{
		gcEntry("../decoy.txt", time.Now().UTC()),
		{Kind: "archive", CachePath: "/etc", Source: "https://example.invalid/x", ResolvedRef: "x"},
		gcEntry("sluga/v1", time.Now().UTC()),
	})
	writeGcTree(t, cacheRoot, "sluga/v1", "workflow.hcl")

	result, err := gcWorkflowCache(cacheRoot, workflowCacheGcOptions{})
	require.NoError(t, err)

	_, statErr := os.Stat(decoy)
	require.NoError(t, statErr, "nothing outside the cache root may ever be deleted")

	index := readWorkflowCacheIndexForTest(t, cacheRoot)
	require.Len(t, index.Entries, 1)
	assert.Equal(t, "sluga/v1", index.Entries[0].CachePath)
	assert.Equal(t, 2, result.DroppedEntries, "path-escape and absolute entries are dropped")
}

func TestCacheGc_RemovesStrayFilesUnderRoot(t *testing.T) {
	cacheRoot := gcTestCacheRoot(t)
	writeGcIndex(t, cacheRoot, []workflowCacheEntry{gcEntry("sluga/v1", time.Now().UTC())})
	writeGcTree(t, cacheRoot, "sluga/v1", "workflow.hcl")
	require.NoError(t, os.WriteFile(filepath.Join(cacheRoot, ".fetch-123.tmp"), []byte("junk"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(cacheRoot, ".index.lock"), []byte(""), 0o640))

	result, err := gcWorkflowCache(cacheRoot, workflowCacheGcOptions{})
	require.NoError(t, err)

	assert.NoFileExists(t, filepath.Join(cacheRoot, ".fetch-123.tmp"), "stray temp files are removed")
	assert.FileExists(t, filepath.Join(cacheRoot, "index.json"))
	assert.FileExists(t, filepath.Join(cacheRoot, ".index.lock"))
	assert.Equal(t, 0, result.RemovedTrees)
}

func TestCacheGc_EmptyAndMissingCacheRoot(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does", "not", "exist")
	result, err := gcWorkflowCache(missing, workflowCacheGcOptions{})
	require.NoError(t, err)
	assert.Zero(t, result.RemovedTrees)
	assert.Zero(t, result.FreedBytes)
	assert.Zero(t, result.DroppedEntries)

	// An existing-but-empty cache root is a no-op with no warnings.
	empty := gcTestCacheRoot(t)
	require.NoError(t, os.MkdirAll(empty, 0o755))
	result, err = gcWorkflowCache(empty, workflowCacheGcOptions{})
	require.NoError(t, err)
	assert.Empty(t, result.Warnings)
}

func TestCacheGcCommand_RemovesUnreferenced(t *testing.T) {
	home := setWorkflowCacheHome(t)
	cacheRoot := filepath.Join(home, "cache", "workflows")
	writeGcIndex(t, cacheRoot, []workflowCacheEntry{gcEntry("sluga/v1", time.Now().UTC())})
	writeGcTree(t, cacheRoot, "sluga/v1", "workflow.hcl")
	writeGcTree(t, cacheRoot, "slugb/v1", "workflow.hcl")

	var buf bytes.Buffer
	cmd := NewCacheCmd()
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"gc"})
	require.NoError(t, cmd.Execute())

	assert.Contains(t, buf.String(), "pruned 1 workflow trees")
	assert.Contains(t, buf.String(), "dropped 0 index entries")
	assert.NoDirExists(t, filepath.Join(cacheRoot, "slugb"))
	assert.DirExists(t, filepath.Join(cacheRoot, "sluga", "v1"))
}

func TestCacheGcCommand_MissingIndexWarns(t *testing.T) {
	home := setWorkflowCacheHome(t)
	cacheRoot := filepath.Join(home, "cache", "workflows")
	writeGcTree(t, cacheRoot, "sluga/v1", "workflow.hcl")

	var buf bytes.Buffer
	cmd := NewCacheCmd()
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"gc"})
	require.NoError(t, cmd.Execute())

	assert.Contains(t, buf.String(), "warning: workflow cache index not found")
	assert.DirExists(t, filepath.Join(cacheRoot, "sluga", "v1"))
}

func TestCacheGcCommand_BadOlderThanErrors(t *testing.T) {
	setWorkflowCacheHome(t)

	var buf bytes.Buffer
	cmd := NewCacheCmd()
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"gc", "--older-than", "nonsense"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse --older-than")
}
