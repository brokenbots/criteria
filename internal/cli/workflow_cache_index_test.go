package cli

// M5.1 (CRI-229): workflow cache index maintenance.
//
// The fetcher records every materialized tree — warm hits, rename-in
// success, and race fallbacks, including nested cascade entries — in
// cache/workflows/index.json under CRITERIA_HOME (ADR-0005 D5). Recording is
// best-effort bookkeeping: a corrupt index is left untouched (overwriting it
// would mark other cached trees unreachable) and never fails the fetch.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeWorkflowCacheIndexFile writes raw bytes as the index file, creating the
// cache root first.
func writeWorkflowCacheIndexFile(t *testing.T, cacheRoot string, raw []byte) {
	t.Helper()
	require.NoError(t, os.MkdirAll(cacheRoot, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(cacheRoot, "index.json"), raw, 0o600))
}

// readWorkflowCacheIndexForTest reads and decodes the on-disk index.
func readWorkflowCacheIndexForTest(t *testing.T, cacheRoot string) workflowCacheIndex {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(cacheRoot, "index.json"))
	require.NoError(t, err)
	var index workflowCacheIndex
	require.NoError(t, json.Unmarshal(raw, &index))
	return index
}

// findWorkflowCacheEntry returns the entry with the given slash-relative cache
// path.
func findWorkflowCacheEntry(t *testing.T, index workflowCacheIndex, cachePath string) workflowCacheEntry {
	t.Helper()
	for _, entry := range index.Entries {
		if entry.CachePath == cachePath {
			return entry
		}
	}
	t.Fatalf("index has no entry with cache_path %q; entries: %+v", cachePath, index.Entries)
	return workflowCacheEntry{}
}

func TestReadWorkflowCacheIndex_MissingFile(t *testing.T) {
	index, present, err := readWorkflowCacheIndex(filepath.Join(t.TempDir(), "cache", "workflows"))
	require.NoError(t, err)
	assert.False(t, present, "a missing index is not present")
	assert.Empty(t, index.Entries, "a missing index reads as empty")
	assert.Equal(t, workflowCacheIndexVersion, index.Version)
}

func TestUpsertWorkflowCacheEntry_SortsAndRefreshes(t *testing.T) {
	cacheRoot := filepath.Join(t.TempDir(), "cache", "workflows")
	first := workflowCacheEntry{
		Kind: "git", Source: "https://example.invalid/one.git", ResolvedRef: "sha-a",
		CachePath: "one/aaa", FetchedAt: time.Now().UTC().Add(-time.Hour),
	}
	second := workflowCacheEntry{
		Kind: "archive", Source: "https://example.invalid/two.tar.gz", ResolvedRef: "sha256:bbb",
		CachePath: "two/bbb", FetchedAt: time.Now().UTC(),
	}

	require.NoError(t, upsertWorkflowCacheEntry(cacheRoot, &second))
	require.NoError(t, upsertWorkflowCacheEntry(cacheRoot, &first))

	index := readWorkflowCacheIndexForTest(t, cacheRoot)
	require.Len(t, index.Entries, 2)
	assert.Equal(t, workflowCacheIndexVersion, index.Version)
	// Entries are sorted by cache path for deterministic output.
	assert.Equal(t, "one/aaa", index.Entries[0].CachePath)
	assert.Equal(t, "two/bbb", index.Entries[1].CachePath)

	// Re-upserting an identical row must not churn the file.
	before, err := os.ReadFile(filepath.Join(cacheRoot, "index.json"))
	require.NoError(t, err)
	require.NoError(t, upsertWorkflowCacheEntry(cacheRoot, &first))
	after, err := os.ReadFile(filepath.Join(cacheRoot, "index.json"))
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "identical re-upsert must not rewrite the index")

	// A changed row is refreshed in place.
	first.FetchedAt = time.Now().UTC()
	require.NoError(t, upsertWorkflowCacheEntry(cacheRoot, &first))
	index = readWorkflowCacheIndexForTest(t, cacheRoot)
	require.Len(t, index.Entries, 2)
	assert.True(t, findWorkflowCacheEntry(t, index, "one/aaa").FetchedAt.Equal(first.FetchedAt))
}

func TestUpsertWorkflowCacheEntry_RejectsUnsafeCachePaths(t *testing.T) {
	cacheRoot := filepath.Join(t.TempDir(), "cache", "workflows")
	cases := []string{
		"",
		".",
		"  ",
		"../outside",
		"ok/../../escape",
	}
	if runtime.GOOS != "windows" {
		// On Windows these strings parse differently ("/x" is not absolute,
		// backslashes are separators), so only assert them on unix.
		cases = append(cases, "/etc/passwd", `ok\..\windows-escape`)
	}
	for _, cachePath := range cases {
		t.Run(cachePath, func(t *testing.T) {
			err := upsertWorkflowCacheEntry(cacheRoot, &workflowCacheEntry{CachePath: cachePath})
			require.Error(t, err, "cache path %q must be rejected", cachePath)
			_, statErr := os.Stat(filepath.Join(cacheRoot, "index.json"))
			assert.True(t, os.IsNotExist(statErr), "rejected entries must not create an index")
		})
	}
}

func TestUpsertWorkflowCacheEntry_CorruptIndexPreserved(t *testing.T) {
	cacheRoot := filepath.Join(t.TempDir(), "cache", "workflows")
	corrupt := []byte(`{"version":1,"entries":[ broken`)
	writeWorkflowCacheIndexFile(t, cacheRoot, corrupt)

	err := upsertWorkflowCacheEntry(cacheRoot, &workflowCacheEntry{CachePath: "slug/v1"})
	require.Error(t, err)
	assert.ErrorIs(t, err, errWorkflowCacheIndexCorrupt)

	got, readErr := os.ReadFile(filepath.Join(cacheRoot, "index.json"))
	require.NoError(t, readErr)
	assert.Equal(t, string(corrupt), string(got),
		"degrade-safe (ADR-0005 D5): a corrupt index must not be overwritten")
}

func TestFetchGit_RecordsIndexEntry(t *testing.T) {
	fx := createWorkflowGitFixture(t)
	f := newTestFetcher(t, http.DefaultClient)
	source := "git::file://" + fx.path + "?ref=main"

	dir, _, err := f.Fetch(context.Background(), t.TempDir(), source)
	require.NoError(t, err)

	wantRel := slugify("file://"+fx.path) + "/" + fx.headSHA
	index := readWorkflowCacheIndexForTest(t, f.cacheRoot)
	entry := findWorkflowCacheEntry(t, index, wantRel)
	assert.Equal(t, "git", entry.Kind)
	assert.Equal(t, source, entry.Source)
	assert.Equal(t, fx.headSHA, entry.ResolvedRef)
	assert.False(t, entry.FetchedAt.IsZero(), "fetched_at must be recorded")

	abs := filepath.Join(f.cacheRoot, filepath.FromSlash(wantRel))
	assert.Equal(t, abs, dir, "cache_path must point at the fetched tree")
}

func TestFetchGit_WarmHitRefreshesFetchedAt(t *testing.T) {
	fx := createWorkflowGitFixture(t)
	f := newTestFetcher(t, http.DefaultClient)
	source := "git::file://" + fx.path + "?ref=main"
	wantRel := slugify("file://"+fx.path) + "/" + fx.headSHA

	_, _, err := f.Fetch(context.Background(), t.TempDir(), source)
	require.NoError(t, err)

	// Age the recorded entry so the warm hit has something to refresh.
	stale := time.Now().UTC().Add(-48 * time.Hour)
	index := readWorkflowCacheIndexForTest(t, f.cacheRoot)
	aged := findWorkflowCacheEntry(t, index, wantRel)
	aged.FetchedAt = stale
	require.NoError(t, upsertWorkflowCacheEntry(f.cacheRoot, &aged))

	// Second fetch takes the warm-hit path in fetchGit and re-records the
	// entry with the tree's mtime.
	_, _, err = f.Fetch(context.Background(), t.TempDir(), source)
	require.NoError(t, err)

	index = readWorkflowCacheIndexForTest(t, f.cacheRoot)
	refreshed := findWorkflowCacheEntry(t, index, wantRel)
	assert.True(t, refreshed.FetchedAt.After(stale),
		"warm hit must refresh fetched_at to the tree's mtime, got %v", refreshed.FetchedAt)
}

func TestFetchArchive_RecordsIndexEntry(t *testing.T) {
	fx := createWorkflowArchiveFixture(t)
	f := newTestFetcher(t, http.DefaultClient)

	dir, _, err := f.Fetch(context.Background(), t.TempDir(), fx.tarGzURL)
	require.NoError(t, err)

	wantRel := slugify(fx.tarGzURL) + "/" + fx.tarGzRef
	index := readWorkflowCacheIndexForTest(t, f.cacheRoot)
	entry := findWorkflowCacheEntry(t, index, wantRel)
	assert.Equal(t, "archive", entry.Kind)
	assert.Equal(t, fx.tarGzURL, entry.Source)
	assert.Equal(t, fx.tarGzRef, entry.ResolvedRef)
	assert.False(t, entry.FetchedAt.IsZero(), "fetched_at must be recorded")

	abs := f.cacheRoot
	assert.Equal(t, filepath.Join(abs, filepath.FromSlash(wantRel)), dir)
}

func TestFetchArchive_CredentialsNotInIndex(t *testing.T) {
	fx := createWorkflowArchiveFixture(t)
	f := newTestFetcher(t, http.DefaultClient)
	hostPort := strings.TrimPrefix(fx.tarGzURL, "http://")
	source := "http://" + testUserPass + "@" + hostPort

	_, _, err := f.Fetch(context.Background(), t.TempDir(), source)
	require.NoError(t, err)

	raw, readErr := os.ReadFile(filepath.Join(f.cacheRoot, "index.json"))
	require.NoError(t, readErr)
	assert.NotContains(t, string(raw), testUserPass,
		"credentials must not be persisted in the index source field")
	assert.NotContains(t, string(raw), "user_pass",
		"slugified credentials must not be persisted either")

	index := readWorkflowCacheIndexForTest(t, f.cacheRoot)
	wantRel := slugify("http://redacted@"+hostPort) + "/" + fx.tarGzRef
	entry := findWorkflowCacheEntry(t, index, wantRel)
	assert.Equal(t, "http://redacted@"+hostPort, entry.Source,
		"the index records the redacted source form, mirroring run metadata")
}

func TestFetch_CorruptIndexDegradesSafely(t *testing.T) {
	fx := createWorkflowGitFixture(t)
	f := newTestFetcher(t, http.DefaultClient)
	corrupt := []byte(`{"version":1,"entries":[ {broken`)
	writeWorkflowCacheIndexFile(t, f.cacheRoot, corrupt)

	dir, _, err := f.Fetch(context.Background(), t.TempDir(), "git::file://"+fx.path+"?ref=main")
	require.NoError(t, err, "fetch must not fail when the index is corrupt (ADR-0005 D5)")

	requireResolvedWorkflow(t, dir)
	got, readErr := os.ReadFile(filepath.Join(f.cacheRoot, "index.json"))
	require.NoError(t, readErr)
	assert.Equal(t, string(corrupt), string(got),
		"degrade-safe: fetch must skip the update and leave the corrupt index untouched")
}

func TestCompile_RemoteCascade_IndexesNestedEntries(t *testing.T) {
	setWorkflowCacheHome(t)
	cacheRoot := cascadeCacheRoot(t)
	child := createPinGitFixture(t, pinCalleeWorkflowHCL)
	parent := createPinGitFixture(t, cascadeWorkflowHCL("cascade_parent", gitFileSource(child), ""))

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "workflow.hcl"),
		[]byte(cascadeWorkflowHCL("cascade_root", gitFileSource(parent), "")), 0o644))

	_, _, err := parseCompileForCli(context.Background(), root, nil, false, false)
	require.NoError(t, err)

	index := readWorkflowCacheIndexForTest(t, cacheRoot)
	require.Len(t, index.Entries, 2, "both cascade levels must be indexed")

	parentEntry := findWorkflowCacheEntry(t, index,
		parentSlugFor(t, parent.path)+"/"+parent.headSHA)
	assert.Equal(t, "git", parentEntry.Kind)
	assert.Equal(t, parent.headSHA, parentEntry.ResolvedRef)

	nestedRel := parentSlugFor(t, parent.path) + "/subworkflows/" + parentSlugFor(t, child.path) + "/" + child.headSHA
	childEntry := findWorkflowCacheEntry(t, index, nestedRel)
	assert.Equal(t, "git", childEntry.Kind)
	assert.Equal(t, child.headSHA, childEntry.ResolvedRef)
	assert.False(t, childEntry.FetchedAt.IsZero())
}

func TestRecordWorkflowCacheEntry_SkipsSymlinkedAncestor(t *testing.T) {
	f := newTestFetcher(t, http.DefaultClient)
	require.NoError(t, os.MkdirAll(f.cacheRoot, 0o755))
	outside := filepath.Join(t.TempDir(), "outside")
	require.NoError(t, os.MkdirAll(outside, 0o755))
	require.NoError(t, os.Symlink(outside, filepath.Join(f.cacheRoot, "evil")))

	// A tree path whose ancestor component is a symlink must never be
	// indexed: gc joins entry paths for deletion, and a symlinked ancestor
	// could make that act outside the cache root.
	f.recordWorkflowCacheEntry("git", "https://example.invalid/repo.git", "abc123",
		filepath.Join(f.cacheRoot, "evil", "b"), time.Now().UTC())

	_, err := os.Stat(filepath.Join(f.cacheRoot, workflowCacheIndexFileName))
	require.ErrorIs(t, err, os.ErrNotExist, "index must stay absent when the only candidate entry is unsafe")
}

func TestPublishWorkflowCacheTree_ExcludesConcurrentSweep(t *testing.T) {
	f := newTestFetcher(t, http.DefaultClient)
	require.NoError(t, os.MkdirAll(f.cacheRoot, 0o755))

	srcTree := filepath.Join(t.TempDir(), "prepared")
	require.NoError(t, os.MkdirAll(srcTree, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(srcTree, "workflow.hcl"), []byte("workflow {}"), 0o644))
	treeDir := filepath.Join(f.cacheRoot, "sluga", "abc123")
	// The fetcher MkdirAll's the slug dir before publishing.
	require.NoError(t, os.MkdirAll(filepath.Dir(treeDir), 0o755))

	// A simulated gc holding the index lock must block rename-in: publish
	// cannot land the tree or record it while the sweep is running, or the
	// sweep could delete the tree before its entry exists.
	unlock, err := lockWorkflowCacheIndex(f.cacheRoot)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() {
		done <- f.publishWorkflowCacheTree("git", "https://example.invalid/repo.git", "abc123", srcTree, treeDir)
	}()
	time.Sleep(200 * time.Millisecond)
	assert.NoDirExists(t, treeDir, "rename-in must wait for the index lock")
	unlock()

	require.NoError(t, <-done)
	_, statErr := os.Stat(treeDir)
	require.NoError(t, statErr, "the tree must be published once the lock is released")
	index := readWorkflowCacheIndexForTest(t, f.cacheRoot)
	entry := findWorkflowCacheEntry(t, index, "sluga/abc123")
	assert.Equal(t, "git", entry.Kind)
	assert.False(t, entry.FetchedAt.IsZero())
}
