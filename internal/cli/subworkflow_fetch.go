package cli

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hashicorp/go-getter"

	"github.com/brokenbots/criteria/internal/dirs"
	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

// defaultWorkflowFetcher resolves git refs and HTTP(S) archives into the local
// cache and returns the materialized directory plus a pin for the parent lockfile.
//
// Remote sources are fetched with hashicorp/go-getter behind the workflowFetcher
// interface (ADR-0005 D3). The getter surface is a fixed allowlist of the audited
// built-ins git, http(s), and file: the default detectors, decompressors, and
// getters shipped by go-getter are all replaced with restricted sets, so no
// additional scheme (S3, GCS, ...) is reachable. Local path sources never enter
// go-getter: they are resolved by LocalSubWorkflowResolver before Fetch.
type defaultWorkflowFetcher struct {
	cacheRoot string
	http      *http.Client
}

// newWorkflowFetcherFunc is the factory used by run-time paths that need to
// resolve remote workflow sources. It is overridable in tests.
var newWorkflowFetcherFunc = newWorkflowFetcher

// newWorkflowFetcher creates a fetcher backed by the shared criteria cache.
func newWorkflowFetcher() workflowFetcher {
	cacheRoot, err := dirs.CacheWorkflows()
	if err != nil {
		cacheRoot = filepath.Join(os.TempDir(), "cache", "workflows")
	}
	return &defaultWorkflowFetcher{
		cacheRoot: cacheRoot,
		http:      http.DefaultClient,
	}
}

func (f *defaultWorkflowFetcher) Fetch(ctx context.Context, callerDir, source string) (string, *lockfile.LockedWorkflowRef, error) {
	u, err := url.Parse(source)
	if err != nil {
		return "", nil, fmt.Errorf("parse workflow source %q: %w", source, err)
	}

	// Local path sources are resolved before calling the fetcher; this is a guard.
	if u.Scheme == "" || u.Scheme == "file" {
		local := &workflow.LocalSubWorkflowResolver{}
		dir, err := local.ResolveSource(ctx, callerDir, source)
		if err != nil {
			return "", nil, err
		}
		return dir, nil, nil
	}

	if u.Scheme == "http" || u.Scheme == "https" {
		return f.fetchArchive(ctx, source)
	}

	if strings.HasPrefix(source, "git::") || looksLikeGitURL(source) || u.Scheme == "git" || u.Scheme == "ssh" {
		return f.fetchGit(ctx, source)
	}

	return "", nil, fmt.Errorf("unsupported workflow source scheme %q for %q", u.Scheme, source)
}

var gitURLPattern = regexp.MustCompile(`^(git@|git://|ssh://|https?://.*\.git|https?://github\.com|https?://gitlab\.com)`)

func looksLikeGitURL(source string) bool {
	return gitURLPattern.MatchString(source)
}

func (f *defaultWorkflowFetcher) fetchGit(ctx context.Context, source string) (string, *lockfile.LockedWorkflowRef, error) {
	repoURL, ref, err := splitGitSource(source)
	if err != nil {
		return "", nil, err
	}

	slug := slugify(repoURL)
	repoDir := filepath.Join(f.cacheRoot, slug)
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("create workflow cache %q: %w", repoDir, err)
	}

	resolvedRef, err := resolveGitRef(ctx, repoURL, ref)
	if err != nil {
		return "", nil, err
	}

	treeDir := filepath.Join(repoDir, resolvedRef)
	if info, err := os.Stat(treeDir); err == nil && info.IsDir() {
		return treeDir, gitLockedWorkflowRef(source, resolvedRef), nil
	}

	return f.materializeGitTree(ctx, source, repoURL, resolvedRef, repoDir)
}

// resolveGitRef maps a branch/tag/HEAD reference to a commit SHA.
func resolveGitRef(ctx context.Context, repoURL, ref string) (string, error) {
	if isCommitSHA(ref) {
		return ref, nil
	}
	out, err := exec.CommandContext(ctx, "git", "ls-remote", repoURL, ref).Output()
	if err != nil {
		return "", fmt.Errorf("resolve git ref %q in %q: %w", ref, repoURL, err)
	}
	resolvedRef := parseFirstLSRemote(string(out))
	if resolvedRef == "" {
		return "", fmt.Errorf("git ref %q not found in %q", ref, repoURL)
	}
	return resolvedRef, nil
}

// materializeGitTree clones and checks out the resolved ref into the cache via
// go-getter's git getter. The destination must not exist: go-getter only takes
// the clone path for a fresh destination, and its update path cannot resolve a
// raw commit SHA.
func (f *defaultWorkflowFetcher) materializeGitTree(ctx context.Context, source, repoURL, resolvedRef, repoDir string) (string, *lockfile.LockedWorkflowRef, error) {
	treeDir := filepath.Join(repoDir, resolvedRef)
	tmpDir, err := os.MkdirTemp(repoDir, "clone-")
	if err != nil {
		return "", nil, fmt.Errorf("create temp clone dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	client := &getter.Client{
		Ctx:  ctx,
		Src:  "git::" + repoURL + "?ref=" + resolvedRef,
		Dst:  filepath.Join(tmpDir, "tree"),
		Mode: getter.ClientModeDir,
		// ADR-0005 D3: only the git getter is reachable for git sources, and
		// the scp-style detector converts "git@host:path" forms to ssh://.
		// The default detector, decompressor, and getter sets are replaced.
		Getters:       map[string]getter.Getter{"git": &getter.GitGetter{}},
		Detectors:     []getter.Detector{&getter.GitDetector{}},
		Decompressors: map[string]getter.Decompressor{},
	}
	if err := client.Get(); err != nil {
		return "", nil, fmt.Errorf("clone %q: %w", repoURL, err)
	}

	if err := os.Rename(filepath.Join(tmpDir, "tree"), treeDir); err != nil {
		// Another goroutine may have created treeDir in a race.
		if info, err := os.Stat(treeDir); err == nil && info.IsDir() {
			return treeDir, gitLockedWorkflowRef(source, resolvedRef), nil
		}
		return "", nil, fmt.Errorf("move cloned workflow into cache: %w", err)
	}

	return treeDir, gitLockedWorkflowRef(source, resolvedRef), nil
}

func splitGitSource(source string) (repoURL, ref string, err error) {
	source = strings.TrimPrefix(source, "git::")

	if idx := strings.Index(source, "?"); idx != -1 {
		q, err := url.ParseQuery(source[idx+1:])
		if err != nil {
			return "", "", fmt.Errorf("parse git source query %q: %w", source, err)
		}
		repoURL = source[:idx]
		for _, key := range []string{"ref", "branch", "tag"} {
			if q.Has(key) {
				ref = q.Get(key)
				break
			}
		}
		if ref == "" {
			ref = "HEAD"
		}
		return repoURL, ref, nil
	}

	return source, "HEAD", nil
}

var commitSHAPattern = regexp.MustCompile(`^[0-9a-f]+$`)

func isCommitSHA(s string) bool {
	return len(s) == 40 && commitSHAPattern.MatchString(s)
}

func parseFirstLSRemote(out string) string {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 1 {
			return fields[0]
		}
	}
	return ""
}

func (f *defaultWorkflowFetcher) fetchArchive(ctx context.Context, source string) (string, *lockfile.LockedWorkflowRef, error) {
	slug := slugify(source)
	slugDir := filepath.Join(f.cacheRoot, slug)
	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("create workflow cache %q: %w", slugDir, err)
	}

	tmpDir, err := os.MkdirTemp(slugDir, "fetch-")
	if err != nil {
		return "", nil, fmt.Errorf("create temp fetch dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	digest, archivePath, err := f.downloadArchive(ctx, source, tmpDir)
	if err != nil {
		return "", nil, err
	}

	archiveDir := filepath.Join(slugDir, digest)
	if info, err := os.Stat(archiveDir); err == nil && info.IsDir() {
		return archiveDir, archiveLockedWorkflowRef(source, digest), nil
	}

	extractDir := filepath.Join(tmpDir, "tree")
	if err := extractArchive(source, archivePath, extractDir); err != nil {
		return "", nil, err
	}

	if err := os.Rename(extractDir, archiveDir); err != nil {
		// Another goroutine may have created archiveDir in a race.
		if info, err := os.Stat(archiveDir); err == nil && info.IsDir() {
			return archiveDir, archiveLockedWorkflowRef(source, digest), nil
		}
		return "", nil, fmt.Errorf("move extracted workflow into cache: %w", err)
	}

	return archiveDir, archiveLockedWorkflowRef(source, digest), nil
}

// httpGetters returns the http(s) getters used for archive downloads, wired to
// the fetcher's shared HTTP client (ADR-0005 D3 allowlist).
func (f *defaultWorkflowFetcher) httpGetters() map[string]getter.Getter {
	httpGetter := &getter.HttpGetter{Client: f.http, DoNotCheckHeadFirst: true}
	return map[string]getter.Getter{
		"http":  httpGetter,
		"https": httpGetter,
	}
}

// downloadArchive fetches the raw archive via go-getter into tmpDir and
// returns its sha256 content digest plus the path of the downloaded file.
// Decompression and detection are disabled so the digest is always computed
// over the untouched archive bytes, exactly as the previous extractor did.
// The caller owns tmpDir cleanup.
func (f *defaultWorkflowFetcher) downloadArchive(ctx context.Context, source, tmpDir string) (digest, archivePath string, err error) {
	archivePath = filepath.Join(tmpDir, "archive")

	client := &getter.Client{
		Ctx:  ctx,
		Src:  source,
		Dst:  archivePath,
		Mode: getter.ClientModeFile,
		// ADR-0005 D3: only the http(s) getters are reachable here; the
		// empty detector and decompressor sets keep the download byte-exact.
		Getters:       f.httpGetters(),
		Detectors:     []getter.Detector{},
		Decompressors: map[string]getter.Decompressor{},
	}
	if err := client.Get(); err != nil {
		return "", "", fmt.Errorf("download archive %q: %w", source, err)
	}

	h := sha256.New()
	file, err := os.Open(archivePath)
	if err != nil {
		return "", "", err
	}
	defer file.Close()
	if _, err := io.Copy(h, file); err != nil {
		return "", "", fmt.Errorf("read archive body: %w", err)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), archivePath, nil
}

// extractArchive dispatches to the correct go-getter decompressor based on the
// source suffix. Archive entries are pre-scanned so absolute paths and
// traversal attempts are rejected with the same errors as the previous
// extractor; go-getter would otherwise silently confine such entries inside
// the destination directory.
func extractArchive(source, archivePath, dst string) error {
	var decompressor getter.Decompressor
	var scan func(archivePath string) ([]string, error)
	switch {
	case strings.HasSuffix(source, ".tar.gz") || strings.HasSuffix(source, ".tgz"):
		decompressor, scan = &getter.TarGzipDecompressor{}, scanTarGzEntries
	case strings.HasSuffix(source, ".zip"):
		decompressor, scan = &getter.ZipDecompressor{}, scanZipEntries
	default:
		return fmt.Errorf("unsupported archive format for %q", source)
	}

	names, err := scan(archivePath)
	if err != nil {
		return err
	}
	for _, name := range names {
		if _, err := safeExtractPath(entryScanRoot, name); err != nil {
			return err
		}
	}

	return decompressor.Decompress(dst, archivePath, true, 0)
}

// entryScanRoot is the virtual destination used to validate archive entry
// names before extraction; see safeExtractPath.
const entryScanRoot = string(filepath.Separator) + "criteria-archive-scan"

func scanTarGzEntries(archivePath string) ([]string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names, nil
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		names = append(names, h.Name)
	}
}

func scanZipEntries(archivePath string) ([]string, error) {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	names := make([]string, 0, len(r.File))
	for _, zf := range r.File {
		names = append(names, zf.Name)
	}
	return names, nil
}

func gitLockedWorkflowRef(source, resolvedRef string) *lockfile.LockedWorkflowRef {
	return &lockfile.LockedWorkflowRef{Name: "", Source: source, ResolvedRef: resolvedRef, Kind: "git"}
}

func archiveLockedWorkflowRef(source, digest string) *lockfile.LockedWorkflowRef {
	return &lockfile.LockedWorkflowRef{Name: "", Source: source, ResolvedRef: digest, Kind: "archive"}
}

func slugify(s string) string {
	s = strings.ReplaceAll(s, ":", "_")
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "?", "_")
	s = strings.ReplaceAll(s, "&", "_")
	s = strings.ReplaceAll(s, "=", "_")
	s = strings.ReplaceAll(s, "@", "_")
	return s
}

// safeExtractPath joins dst with the archive entry name and confirms the result
// stays within dst. It rejects absolute paths and paths that escape the
// destination directory via ".." components.
func safeExtractPath(dst, name string) (string, error) {
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("archive entry %q is an absolute path", name)
	}
	target := filepath.Clean(filepath.Join(dst, name))
	rel, err := filepath.Rel(dst, target)
	if err != nil {
		return "", fmt.Errorf("archive entry %q cannot be constrained to destination: %w", name, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("archive entry %q escapes destination directory", name)
	}
	return target, nil
}
