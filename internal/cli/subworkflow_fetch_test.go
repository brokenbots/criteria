package cli

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/brokenbots/criteria/workflow/lockfile"
)

// ---------------------------------------------------------------------------
// Archive extraction (go-getter decompressors behind the pre-scan wrapper)
// ---------------------------------------------------------------------------

func writeArchiveFile(t *testing.T, data []byte, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, data, 0o644))
	return path
}

func buildTarGz(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tr := tar.NewWriter(gz)
	for name, body := range entries {
		writeTarEntry(t, tr, name, body)
	}
	require.NoError(t, tr.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

func buildZip(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range entries {
		writeZipEntry(t, zw, name, body)
	}
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

// TestExtractArchive_TarGz_RejectPathTraversal verifies that a tar.gz archive
// containing an entry whose name escapes the destination directory is rejected
// before any entry is extracted, with the pre-go-getter error text.
func TestExtractArchive_TarGz_RejectPathTraversal(t *testing.T) {
	tmp := t.TempDir()
	dst := filepath.Join(tmp, "extract")
	data := buildTarGz(t, map[string][]byte{
		"workflow.hcl":  []byte("workflow {}"),
		"../escape.txt": []byte("escaped"),
	})
	archivePath := writeArchiveFile(t, data, "wf.tar.gz")

	err := extractArchive("wf.tar.gz", archivePath, dst)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes destination directory")

	_, statErr := os.Stat(dst)
	assert.True(t, os.IsNotExist(statErr), "traversal archive must be rejected before extraction")

	outside := filepath.Join(tmp, "escape.txt")
	_, statErr = os.Stat(outside)
	assert.True(t, os.IsNotExist(statErr), "traversing entry must not write outside dst")
}

// TestExtractArchive_TarGz_RejectAbsolutePath verifies that absolute tar.gz
// archive entry names are rejected.
func TestExtractArchive_TarGz_RejectAbsolutePath(t *testing.T) {
	tmp := t.TempDir()
	dst := filepath.Join(tmp, "extract")
	data := buildTarGz(t, map[string][]byte{
		"/etc/passwd": []byte("root"),
	})
	archivePath := writeArchiveFile(t, data, "wf.tar.gz")

	err := extractArchive("wf.tar.gz", archivePath, dst)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absolute path")
	_, statErr := os.Stat(dst)
	assert.True(t, os.IsNotExist(statErr))
}

// TestExtractArchive_Zip_RejectPathTraversal verifies that a zip archive
// containing an entry whose name escapes the destination directory is rejected
// before any entry is extracted, with the pre-go-getter error text.
func TestExtractArchive_Zip_RejectPathTraversal(t *testing.T) {
	tmp := t.TempDir()
	dst := filepath.Join(tmp, "extract")
	data := buildZip(t, map[string][]byte{
		"workflow.hcl":  []byte("workflow {}"),
		"../escape.txt": []byte("escaped"),
	})
	archivePath := writeArchiveFile(t, data, "wf.zip")

	err := extractArchive("wf.zip", archivePath, dst)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes destination directory")

	outside := filepath.Join(tmp, "escape.txt")
	_, statErr := os.Stat(outside)
	assert.True(t, os.IsNotExist(statErr), "traversing entry must not write outside dst")
}

// TestExtractArchive_Zip_RejectAbsolutePath verifies that absolute zip archive
// entry names are rejected.
func TestExtractArchive_Zip_RejectAbsolutePath(t *testing.T) {
	tmp := t.TempDir()
	dst := filepath.Join(tmp, "extract")
	data := buildZip(t, map[string][]byte{
		"/etc/passwd": []byte("root"),
	})
	archivePath := writeArchiveFile(t, data, "wf.zip")

	err := extractArchive("wf.zip", archivePath, dst)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absolute path")
	_, statErr := os.Stat(dst)
	assert.True(t, os.IsNotExist(statErr))
}

// TestExtractArchive_TarGz_DeeplyNestedEntry verifies that a tar.gz archive
// with a deeply nested file extracts correctly.
func TestExtractArchive_TarGz_DeeplyNestedEntry(t *testing.T) {
	tmp := t.TempDir()
	dst := filepath.Join(tmp, "extract")
	data := buildTarGz(t, map[string][]byte{
		"a/b/c/deep.txt": []byte("deep value"),
	})
	archivePath := writeArchiveFile(t, data, "wf.tar.gz")

	require.NoError(t, extractArchive("wf.tar.gz", archivePath, dst))

	got, err := os.ReadFile(filepath.Join(dst, "a", "b", "c", "deep.txt"))
	require.NoError(t, err)
	assert.Equal(t, "deep value", string(got))
}

// TestExtractArchive_TgzSuffix verifies the .tgz suffix routes to the tar.gz
// decompressor.
func TestExtractArchive_TgzSuffix(t *testing.T) {
	tmp := t.TempDir()
	dst := filepath.Join(tmp, "extract")
	data := buildTarGz(t, map[string][]byte{
		"nested/workflow.hcl": []byte("workflow {}"),
	})
	archivePath := writeArchiveFile(t, data, "wf.tgz")

	require.NoError(t, extractArchive("wf.tgz", archivePath, dst))
	got, err := os.ReadFile(filepath.Join(dst, "nested", "workflow.hcl"))
	require.NoError(t, err)
	assert.Equal(t, "workflow {}", string(got))
}

// TestExtractArchive_Zip_DeeplyNestedEntry verifies that a zip archive with a
// deeply nested file extracts correctly.
func TestExtractArchive_Zip_DeeplyNestedEntry(t *testing.T) {
	tmp := t.TempDir()
	dst := filepath.Join(tmp, "extract")
	data := buildZip(t, map[string][]byte{
		"a/b/c/deep.txt": []byte("deep value"),
	})
	archivePath := writeArchiveFile(t, data, "wf.zip")

	require.NoError(t, extractArchive("wf.zip", archivePath, dst))

	got, err := os.ReadFile(filepath.Join(dst, "a", "b", "c", "deep.txt"))
	require.NoError(t, err)
	assert.Equal(t, "deep value", string(got))
}

// TestExtractArchive_UnsupportedFormat verifies the suffix dispatch rejects
// unknown archive formats.
func TestExtractArchive_UnsupportedFormat(t *testing.T) {
	tmp := t.TempDir()
	dst := filepath.Join(tmp, "extract")
	archivePath := writeArchiveFile(t, []byte("not an archive"), "wf.bin")

	err := extractArchive("wf.bin", archivePath, dst)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unsupported archive format for "wf.bin"`)
}

func writeTarEntry(t *testing.T, tw *tar.Writer, name string, body []byte) {
	t.Helper()
	h := &tar.Header{
		Name:     name,
		Mode:     0o644,
		Size:     int64(len(body)),
		Typeflag: tar.TypeReg,
	}
	require.NoError(t, tw.WriteHeader(h))
	_, err := tw.Write(body)
	require.NoError(t, err)
}

func writeZipEntry(t *testing.T, zw *zip.Writer, name string, body []byte) {
	t.Helper()
	w, err := zw.CreateHeader(&zip.FileHeader{
		Name:   name,
		Method: zip.Store,
	})
	require.NoError(t, err)
	_, err = io.Copy(w, bytes.NewReader(body))
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// End-to-end fetcher tests
// ---------------------------------------------------------------------------

// newTestFetcher returns a fetcher rooted at a private cache so tests never
// touch the shared criteria cache.
func newTestFetcher(t *testing.T, httpClient *http.Client) *defaultWorkflowFetcher {
	t.Helper()
	return &defaultWorkflowFetcher{
		cacheRoot: filepath.Join(t.TempDir(), "cache", "workflows"),
		http:      httpClient,
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

// runGitBare runs a git command against a bare repository, overriding the
// safe.bareRepository=explicit default of newer git versions.
func runGitBare(t *testing.T, dir string, args ...string) string {
	return runGit(t, dir, append([]string{"-c", "safe.bareRepository=all"}, args...)...)
}

type gitFixture struct {
	path    string // bare repo path usable as file:///<path> or scp-style source
	headSHA string // default branch commit
	tagSHA  string // lightweight tag v1 commit
}

// createGitFixture builds a bare repository with a main branch, a workflow
// file, and a lightweight v1 tag.
func createGitFixture(t *testing.T) gitFixture {
	t.Helper()
	src := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.MkdirAll(src, 0o755))
	runGit(t, src, "init", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(src, "workflow.hcl"), []byte("workflow \"child\" {}"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(src, "nested"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "nested", "step.hcl"), []byte("step \"one\" {}"), 0o644))
	runGit(t, src, "add", ".")
	runGit(t, src, "-c", "user.email=test@example.com", "-c", "user.name=test", "commit", "-m", "initial")

	bare := filepath.Join(t.TempDir(), "fixture.git")
	runGit(t, src, "clone", "--bare", "--quiet", src, bare)
	runGitBare(t, bare, "tag", "v1", "main")

	return gitFixture{
		path:    bare,
		headSHA: runGitBare(t, bare, "rev-parse", "main"),
		tagSHA:  runGitBare(t, bare, "rev-parse", "v1"),
	}
}

// installFakeSSH replaces the ssh transport with a script that runs the
// requested git command locally, emulating an ssh server that serves the
// fixture path.
func installFakeSSH(t *testing.T) {
	t.Helper()
	script := filepath.Join(t.TempDir(), "fake-ssh")
	content := "#!/bin/sh\n# Fake ssh transport: run the requested git command locally.\nlast=\"\"\nfor arg in \"$@\"; do\n\tlast=\"$arg\"\ndone\nexec sh -c \"$last\"\n"
	require.NoError(t, os.WriteFile(script, []byte(content), 0o755))
	t.Setenv("GIT_SSH_COMMAND", script)
}

func requireGitTree(t *testing.T, dir string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(dir, "workflow.hcl"))
	require.NoError(t, err)
	assert.Equal(t, "workflow \"child\" {}", string(got))
	nested, err := os.ReadFile(filepath.Join(dir, "nested", "step.hcl"))
	require.NoError(t, err)
	assert.Equal(t, "step \"one\" {}", string(nested))
}

func lockedRefEqual(t *testing.T, got *lockfile.LockedWorkflowRef, source, resolved, kind string) {
	t.Helper()
	require.NotNil(t, got)
	assert.Equal(t, &lockfile.LockedWorkflowRef{
		Name:        "",
		Source:      source,
		ResolvedRef: resolved,
		Kind:        kind,
	}, got)
}

// TestFetchGit_RefQueryForm covers the "?ref=" source form with a branch, a
// tag, and HEAD, asserting materialized contents, LockedWorkflowRef values,
// and the cache/workflows/<slug>/<version> layout.
func TestFetchGit_RefQueryForm(t *testing.T) {
	fx := createGitFixture(t)
	cases := []struct {
		name     string
		source   string
		expected string
	}{
		{"branch", "git::file://" + fx.path + "?ref=main", fx.headSHA},
		{"tag", "git::file://" + fx.path + "?ref=v1", fx.tagSHA},
		{"head", "git::file://" + fx.path + "?ref=HEAD", fx.headSHA},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newTestFetcher(t, http.DefaultClient)
			dir, locked, err := f.Fetch(context.Background(), t.TempDir(), tc.source)
			require.NoError(t, err)

			requireGitTree(t, dir)
			lockedRefEqual(t, locked, tc.source, tc.expected, "git")

			wantDir := filepath.Join(f.cacheRoot, slugify("file://"+fx.path), tc.expected)
			assert.Equal(t, wantDir, dir, "cache layout must be cache/workflows/<slug>/<version>")
			info, statErr := os.Stat(dir)
			require.NoError(t, statErr)
			assert.True(t, info.IsDir())
		})
	}
}

// TestFetchGit_ForcePrefixHead covers the bare "git::" form without a ref,
// which resolves HEAD.
func TestFetchGit_ForcePrefixHead(t *testing.T) {
	fx := createGitFixture(t)
	f := newTestFetcher(t, http.DefaultClient)
	source := "git::file://" + fx.path

	dir, locked, err := f.Fetch(context.Background(), t.TempDir(), source)
	require.NoError(t, err)

	requireGitTree(t, dir)
	lockedRefEqual(t, locked, source, fx.headSHA, "git")
}

// TestFetchGit_ExplicitSHARef covers a "?ref=<commit>" form, which must not
// require an ls-remote round trip.
func TestFetchGit_ExplicitSHARef(t *testing.T) {
	fx := createGitFixture(t)
	f := newTestFetcher(t, http.DefaultClient)
	source := "git::file://" + fx.path + "?ref=" + fx.tagSHA

	dir, locked, err := f.Fetch(context.Background(), t.TempDir(), source)
	require.NoError(t, err)

	requireGitTree(t, dir)
	lockedRefEqual(t, locked, source, fx.tagSHA, "git")
}

// TestFetchGit_CachedHit verifies a second fetch of the same source returns
// the already-materialized cache entry.
func TestFetchGit_CachedHit(t *testing.T) {
	fx := createGitFixture(t)
	f := newTestFetcher(t, http.DefaultClient)
	source := "git::file://" + fx.path + "?ref=main"

	first, _, err := f.Fetch(context.Background(), t.TempDir(), source)
	require.NoError(t, err)
	second, locked2, err := f.Fetch(context.Background(), t.TempDir(), source)
	require.NoError(t, err)

	assert.Equal(t, first, second)
	lockedRefEqual(t, locked2, source, fx.headSHA, "git")
	requireGitTree(t, second)
}

// TestFetchGit_SshForm covers the ssh:// form (with the .git suffix) end to
// end through a fake ssh transport, including the clone performed by go-getter.
func TestFetchGit_SshForm(t *testing.T) {
	fx := createGitFixture(t)
	installFakeSSH(t)
	f := newTestFetcher(t, http.DefaultClient)
	source := "ssh://git@127.0.0.1" + fx.path + "?ref=main"

	dir, locked, err := f.Fetch(context.Background(), t.TempDir(), source)
	require.NoError(t, err)

	requireGitTree(t, dir)
	lockedRefEqual(t, locked, source, fx.headSHA, "git")
}

// TestFetchGit_ScpStyleForm covers the scp-style "git@host:path" form with the
// git:: prefix, which go-getter canonicalizes through its git detector.
func TestFetchGit_ScpStyleForm(t *testing.T) {
	fx := createGitFixture(t)
	installFakeSSH(t)
	f := newTestFetcher(t, http.DefaultClient)
	source := "git::git@127.0.0.1:" + fx.path + "?ref=main"

	dir, locked, err := f.Fetch(context.Background(), t.TempDir(), source)
	require.NoError(t, err)

	requireGitTree(t, dir)
	lockedRefEqual(t, locked, source, fx.headSHA, "git")
}

// TestFetchGit_ScpStyleBareForm covers the bare scp-style "git@host:path" form
// without the git:: prefix. url.Parse rejects it, so Fetch must route it to
// the git getter through the parse-failure branch instead of failing with an
// unsupported-scheme or local-resolution error.
func TestFetchGit_ScpStyleBareForm(t *testing.T) {
	fx := createGitFixture(t)
	installFakeSSH(t)
	f := newTestFetcher(t, http.DefaultClient)
	source := "git@127.0.0.1:" + fx.path + "?ref=main"

	dir, locked, err := f.Fetch(context.Background(), t.TempDir(), source)
	require.NoError(t, err)

	requireGitTree(t, dir)
	lockedRefEqual(t, locked, source, fx.headSHA, "git")
}

// TestFetchGit_GitURLPatternForm covers the ".git" recognition branch of
// looksLikeGitURL: an https URL ending in .git is classified as a git source
// by the lock resolver, and the fetcher routes git-looking https URLs to the
// git getter unless their path ends in an archive suffix (an archive-suffix
// path always wins), so a plain ssh form remains the observable ssh contract.
func TestFetchGit_GitURLPatternForm(t *testing.T) {
	assert.True(t, looksLikeGitURL("https://example.com/org/repo.git"))
	assert.True(t, looksLikeGitURL("git@github.com:org/repo.git"))
	assert.True(t, looksLikeGitURL("git://example.com/org/repo.git"))
	assert.False(t, looksLikeGitURL("https://example.com/release.tar.gz"))
}

// TestRoutesToGit pins the fetcher's git-vs-archive classification. The git
// URL pattern must not capture archive URLs: an http(s) URL whose path ends
// in a supported archive suffix routes to the archive fetcher even when it
// matches a git pattern (a ".git" segment earlier in the path, or a
// github.com/gitlab.com host), while ".git"-suffixed paths, the
// git::/git:///ssh:// forms, and the scheme-less scp-style form keep routing
// to the git getter. scp-style sources are rejected by url.Parse, so the
// classifier is exercised with a scheme-less placeholder URL for them.
func TestRoutesToGit(t *testing.T) {
	cases := []struct {
		name   string
		source string
		want   bool
	}{
		{"https-git-suffix", "https://host.example/org/repo.git?ref=main", true},
		{"git-scheme", "git://host.example/org/repo.git", true},
		{"ssh-scheme", "ssh://git@host.example/org/repo.git", true},
		{"scp-style-git-form", "git@host.example:org/repo.git?ref=main", true},
		{"scp-style-git-form-with-port", "git@github.com:org/repo.git", true},
		{"git-force-prefix", "git::https://host.example/org/repo.git?ref=main", true},
		{"git-force-prefix-archive-suffix", "git::https://host.example/flow.tar.gz", true},
		{"github-host-repo", "https://github.com/org/repo?ref=main", true},
		{"gitlab-host-repo", "https://gitlab.com/org/repo.git", true},
		{"plain-targz-archive", "https://host.example/flow.tar.gz", false},
		{"zip-archive", "http://host.example/flow.zip", false},
		{"tgz-archive", "https://host.example/flow.tgz", false},
		{"archive-with-query", "https://host.example/flow.tar.gz?download=1", false},
		// The regression boundary: git-pattern-matching archive URLs stay
		// archives (previously misrouted to the git fetcher).
		{"dot-git-earlier-in-path", "http://host.example/v1.git/flow.tar.gz?download=1", false},
		{"github-release-asset", "https://github.com/org/repo/releases/download/v1/workflows.tar.gz", false},
		{"github-repo-archive", "https://github.com/org/repo/archive/v1.tar.gz", false},
		{"gitlab-repo-archive", "https://gitlab.com/org/repo/-/archive/v1/repo-v1.zip", false},
		{"non-git-scheme", "ftp://host.example/repo.git", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.source)
			if err != nil {
				// Only scp-style git forms are unparseable; the fetcher
				// routes them to the git getter from the source string alone
				// (Fetch's parse-failure branch), so the classifier is pinned
				// against a placeholder URL carrying no scheme.
				require.True(t, looksLikeGitURL(tc.source), "unparseable case %q must be a git form", tc.source)
				u = &url.URL{}
			}
			assert.Equal(t, tc.want, routesToGit(tc.source, u))
		})
	}
}

// TestFetchArchive_TarGz covers the http(s) archive form end to end: the
// LockedWorkflowRef carries the sha256 content digest, the archive is
// extracted under cache/workflows/<slug>/<digest>, and a second fetch returns
// the cached entry.
func TestFetchArchive_TarGz(t *testing.T) {
	body := buildTarGz(t, map[string][]byte{
		"workflow.hcl":       []byte("workflow {}"),
		"steps/one.hcl":      []byte("step \"one\" {}"),
		"steps/deep/two.hcl": []byte("step \"two\" {}"),
	})
	sum := sha256.Sum256(body)
	wantDigest := "sha256:" + hex.EncodeToString(sum[:])
	source := "/flows/rel.tar.gz"

	var fetches int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		fetches++
		mu.Unlock()
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	f := newTestFetcher(t, srv.Client())
	fullSource := srv.URL + source

	dir, locked, err := f.Fetch(context.Background(), t.TempDir(), fullSource)
	require.NoError(t, err)

	lockedRefEqual(t, locked, fullSource, wantDigest, "archive")

	got, err := os.ReadFile(filepath.Join(dir, "workflow.hcl"))
	require.NoError(t, err)
	assert.Equal(t, "workflow {}", string(got))
	got, err = os.ReadFile(filepath.Join(dir, "steps", "deep", "two.hcl"))
	require.NoError(t, err)
	assert.Equal(t, "step \"two\" {}", string(got))

	wantDir := filepath.Join(f.cacheRoot, slugify(fullSource), wantDigest)
	assert.Equal(t, wantDir, dir, "cache layout must be cache/workflows/<slug>/<version>")

	dir2, locked2, err := f.Fetch(context.Background(), t.TempDir(), fullSource)
	require.NoError(t, err)
	assert.Equal(t, dir, dir2)
	lockedRefEqual(t, locked2, fullSource, wantDigest, "archive")
}

// TestFetchArchive_Zip covers the zip archive form end to end.
func TestFetchArchive_Zip(t *testing.T) {
	body := buildZip(t, map[string][]byte{
		"workflow.hcl": []byte("workflow {}"),
	})
	sum := sha256.Sum256(body)
	wantDigest := "sha256:" + hex.EncodeToString(sum[:])
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	f := newTestFetcher(t, srv.Client())
	fullSource := srv.URL + "/flows/rel.zip"

	dir, locked, err := f.Fetch(context.Background(), t.TempDir(), fullSource)
	require.NoError(t, err)

	lockedRefEqual(t, locked, fullSource, wantDigest, "archive")
	got, err := os.ReadFile(filepath.Join(dir, "workflow.hcl"))
	require.NoError(t, err)
	assert.Equal(t, "workflow {}", string(got))
	assert.Equal(t, filepath.Join(f.cacheRoot, slugify(fullSource), wantDigest), dir)
}

// TestFetchArchive_UnsupportedFormat verifies that archives with an unknown
// suffix are rejected after download, matching the previous contract.
func TestFetchArchive_UnsupportedFormat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("data"))
	}))
	defer srv.Close()

	f := newTestFetcher(t, srv.Client())
	fullSource := srv.URL + "/flows/rel.bin"

	_, _, err := f.Fetch(context.Background(), t.TempDir(), fullSource)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unsupported archive format for "`)
}

// TestFetchArchive_DownloadError verifies download failures surface an error
// naming the source.
func TestFetchArchive_DownloadError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "missing", http.StatusNotFound)
	}))
	defer srv.Close()

	f := newTestFetcher(t, srv.Client())
	fullSource := srv.URL + "/flows/rel.tar.gz"

	_, _, err := f.Fetch(context.Background(), t.TempDir(), fullSource)
	require.Error(t, err)
	assert.Contains(t, err.Error(), fmt.Sprintf("download archive %q", fullSource))
}

// TestFetch_UnsupportedScheme verifies the unsupported-scheme error contract.
func TestFetch_UnsupportedScheme(t *testing.T) {
	f := newTestFetcher(t, http.DefaultClient)
	source := "ftp://example.com/workflow.tar.gz"

	_, _, err := f.Fetch(context.Background(), t.TempDir(), source)
	require.Error(t, err)
	assert.Contains(t, err.Error(), fmt.Sprintf("unsupported workflow source scheme %q for %q", "ftp", source))
}

// TestSlugify pins the slug rules that determine cache directory names.
func TestSlugify(t *testing.T) {
	assert.Equal(t, "https___host_org_repo.git", slugify("https://host/org/repo.git"))
	assert.Equal(t, "http___host_a.tar.gz_ref_v1", slugify("http://host/a.tar.gz?ref=v1"))
	assert.Equal(t, "git_host_org_repo.git", slugify("git@host:org/repo.git"))
}

// TestFetch_ConcurrentGit verifies rename-in race safety: concurrent fetches
// of the same git source all observe the same complete tree, leave exactly one
// cache version, and clean up their temporary clone directories.
func TestFetch_ConcurrentGit(t *testing.T) {
	fx := createGitFixture(t)
	f := newTestFetcher(t, http.DefaultClient)
	source := "git::file://" + fx.path + "?ref=main"

	const n = 6
	dirs := make([]string, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			dir, _, err := f.Fetch(context.Background(), t.TempDir(), source)
			dirs[i], errs[i] = dir, err
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "fetch %d failed", i)
		require.NotEmpty(t, dirs[i])
	}
	for i := 1; i < n; i++ {
		assert.Equal(t, dirs[0], dirs[i], "all racing fetches must adopt the winning tree")
	}
	requireGitTree(t, dirs[0])

	repoDir := filepath.Join(f.cacheRoot, slugify("file://"+fx.path))
	versions, err := filepath.Glob(filepath.Join(repoDir, "*"))
	require.NoError(t, err)
	assert.Len(t, versions, 1, "exactly one cache version must exist")
	assert.Equal(t, fx.headSHA, filepath.Base(versions[0]))

	leftovers, err := filepath.Glob(filepath.Join(repoDir, "clone-*"))
	require.NoError(t, err)
	assert.Empty(t, leftovers, "temporary clone dirs must be cleaned up")
}

// TestFetch_ConcurrentArchive verifies rename-in race safety for archives:
// concurrent fetches converge on one digest directory with complete contents
// and leave no temporary download or extract dirs behind.
func TestFetch_ConcurrentArchive(t *testing.T) {
	body := buildTarGz(t, map[string][]byte{
		"workflow.hcl": []byte("workflow {}"),
	})
	sum := sha256.Sum256(body)
	wantDigest := "sha256:" + hex.EncodeToString(sum[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	f := newTestFetcher(t, srv.Client())
	fullSource := srv.URL + "/flows/rel.tar.gz"

	const n = 6
	dirs := make([]string, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			dir, _, err := f.Fetch(context.Background(), t.TempDir(), fullSource)
			dirs[i], errs[i] = dir, err
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "fetch %d failed", i)
		require.NotEmpty(t, dirs[i])
	}
	for i := 1; i < n; i++ {
		assert.Equal(t, dirs[0], dirs[i], "all racing fetches must adopt the winning tree")
	}
	got, err := os.ReadFile(filepath.Join(dirs[0], "workflow.hcl"))
	require.NoError(t, err)
	assert.Equal(t, "workflow {}", string(got))

	slugDir := filepath.Join(f.cacheRoot, slugify(fullSource))
	versions, err := filepath.Glob(filepath.Join(slugDir, "*"))
	require.NoError(t, err)
	assert.Len(t, versions, 1, "exactly one cache version must exist")
	assert.Equal(t, wantDigest, filepath.Base(versions[0]))

	leftovers, err := filepath.Glob(filepath.Join(slugDir, "fetch-*"))
	require.NoError(t, err)
	assert.Empty(t, leftovers, "temp fetch dirs must be cleaned up")
}
