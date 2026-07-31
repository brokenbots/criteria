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

	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

// defaultWorkflowFetcher resolves git refs and HTTP(S) archives into the local
// cache and returns the materialized directory plus a pin for the parent lockfile.
type defaultWorkflowFetcher struct {
	cacheRoot string
	http      *http.Client
}

// newWorkflowFetcher creates a fetcher backed by the shared criteria cache.
func newWorkflowFetcher() workflowFetcher {
	base := os.Getenv("CRITERIA_STATE_DIR")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			base = os.TempDir()
		} else {
			base = filepath.Join(home, ".criteria")
		}
	}
	return &defaultWorkflowFetcher{
		cacheRoot: filepath.Join(base, "cache", "workflows"),
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
		return treeDir, &lockfile.LockedWorkflowRef{Name: "", Source: source, ResolvedRef: resolvedRef, Kind: "git"}, nil
	}

	return materializeGitTree(ctx, source, repoURL, resolvedRef, repoDir)
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

// materializeGitTree clones and checks out the resolved ref into the cache.
func materializeGitTree(ctx context.Context, source, repoURL, resolvedRef, repoDir string) (string, *lockfile.LockedWorkflowRef, error) {
	treeDir := filepath.Join(repoDir, resolvedRef)
	tmpDir, err := os.MkdirTemp(repoDir, "clone-")
	if err != nil {
		return "", nil, fmt.Errorf("create temp clone dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	if _, err := exec.CommandContext(ctx, "git", "clone", "--no-checkout", "--filter=blob:none", repoURL, tmpDir).Output(); err != nil {
		return "", nil, fmt.Errorf("clone %q: %w", repoURL, err)
	}
	if _, err := exec.CommandContext(ctx, "git", "-C", tmpDir, "checkout", resolvedRef).Output(); err != nil {
		return "", nil, fmt.Errorf("checkout %q in %q: %w", resolvedRef, repoURL, err)
	}

	if err := os.Rename(tmpDir, treeDir); err != nil {
		// Another goroutine may have created treeDir in a race.
		if info, err := os.Stat(treeDir); err == nil && info.IsDir() {
			os.RemoveAll(tmpDir)
			return treeDir, &lockfile.LockedWorkflowRef{Name: "", Source: source, ResolvedRef: resolvedRef, Kind: "git"}, nil
		}
		return "", nil, fmt.Errorf("move cloned workflow into cache: %w", err)
	}

	return treeDir, &lockfile.LockedWorkflowRef{Name: "", Source: source, ResolvedRef: resolvedRef, Kind: "git"}, nil
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

	if strings.Contains(source, "//") {
		parts := strings.SplitN(source, "//", 2)
		repoURL = parts[0]
		remainder := parts[1]
		if idx := strings.Index(remainder, "?"); idx != -1 {
			q, _ := url.ParseQuery(remainder[idx+1:])
			if q.Has("ref") {
				ref = q.Get("ref")
			}
			remainder = remainder[:idx]
		}
		if ref == "" {
			ref = "HEAD"
		}
		_ = remainder
		return repoURL, ref, nil
	}

	return source, "HEAD", nil
}

func isCommitSHA(s string) bool {
	return len(s) == 40 && regexp.MustCompile(`^[0-9a-f]+$`).MatchString(s)
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
	body, digest, err := f.downloadArchive(ctx, source)
	if err != nil {
		return "", nil, err
	}

	slug := slugify(source)
	archiveDir := filepath.Join(f.cacheRoot, slug, digest)
	if info, err := os.Stat(archiveDir); err == nil && info.IsDir() {
		return archiveDir, &lockfile.LockedWorkflowRef{Name: "", Source: source, ResolvedRef: digest, Kind: "archive"}, nil
	}

	tmpDir, err := os.MkdirTemp(filepath.Join(f.cacheRoot, slug), "extract-")
	if err != nil {
		return "", nil, fmt.Errorf("create temp extract dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	if err := extractArchive(source, tmpDir, body); err != nil {
		return "", nil, err
	}

	if err := os.MkdirAll(filepath.Dir(archiveDir), 0o755); err != nil {
		return "", nil, err
	}
	if err := os.Rename(tmpDir, archiveDir); err != nil {
		if info, err := os.Stat(archiveDir); err == nil && info.IsDir() {
			os.RemoveAll(tmpDir)
			return archiveDir, &lockfile.LockedWorkflowRef{Name: "", Source: source, ResolvedRef: digest, Kind: "archive"}, nil
		}
		return "", nil, fmt.Errorf("move extracted workflow into cache: %w", err)
	}

	return archiveDir, &lockfile.LockedWorkflowRef{Name: "", Source: source, ResolvedRef: digest, Kind: "archive"}, nil
}

// downloadArchive fetches the archive body and returns its sha256 content digest.
func (f *defaultWorkflowFetcher) downloadArchive(ctx context.Context, source string) (archiveBody []byte, contentDigest string, downloadErr error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, http.NoBody)
	if err != nil {
		return nil, "", fmt.Errorf("build archive request: %w", err)
	}
	resp, err := f.http.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("download archive %q: %w", source, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("download archive %q returned status %d", source, resp.StatusCode)
	}

	h := sha256.New()
	body, err := io.ReadAll(io.TeeReader(resp.Body, h))
	if err != nil {
		return nil, "", fmt.Errorf("read archive body: %w", err)
	}
	return body, "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// extractArchive dispatches to the correct extractor based on the source suffix.
func extractArchive(source, dst string, body []byte) error {
	switch {
	case strings.HasSuffix(source, ".tar.gz") || strings.HasSuffix(source, ".tgz"):
		if err := extractTarGz(dst, body); err != nil {
			return fmt.Errorf("extract tar.gz %q: %w", source, err)
		}
	case strings.HasSuffix(source, ".zip"):
		if err := extractZip(dst, body); err != nil {
			return fmt.Errorf("extract zip %q: %w", source, err)
		}
	default:
		return fmt.Errorf("unsupported archive format for %q", source)
	}
	return nil
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

func extractTarGz(dst string, data []byte) error {
	gz, err := gzip.NewReader(strings.NewReader(string(data)))
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if !validTarHeader(h) {
			continue
		}
		target := filepath.Join(dst, filepath.Clean(h.Name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(h.Mode&0o777))
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			return err
		}
		f.Close()
	}
	return nil
}

func extractZip(dst string, data []byte) error {
	r, err := zip.NewReader(strings.NewReader(string(data)), int64(len(data)))
	if err != nil {
		return err
	}
	for _, zf := range r.File {
		if zf.FileInfo().IsDir() {
			continue
		}
		target := filepath.Join(dst, filepath.Clean(zf.Name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, zf.Mode())
		if err != nil {
			return err
		}
		rc, err := zf.Open()
		if err != nil {
			f.Close()
			return err
		}
		if _, err := io.Copy(f, rc); err != nil {
			rc.Close()
			f.Close()
			return err
		}
		rc.Close()
		f.Close()
	}
	return nil
}

func validTarHeader(h *tar.Header) bool {
	if h == nil {
		return false
	}
	if h.Typeflag != tar.TypeReg && h.Typeflag != 0 {
		return false
	}
	return true
}
