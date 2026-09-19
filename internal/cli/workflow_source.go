package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/brokenbots/criteria/workflow"
)

// WorkflowOrigin records the resolved provenance of a workflow source
// (ADR-0005 D4/D6): where the content came from, the immutable version that
// was resolved, and the local path it was materialized into. RunMetadata
// provenance recording (CRI-225) consumes this record. Expected-pin
// enforcement on ResolvedRef (CRI-226) happens in resolveWorkflowSource,
// before the origin is produced.
type WorkflowOrigin struct {
	// Kind is the source form: "git" or "archive" (the fetcher's pin kinds).
	// Local sources produce no origin record.
	Kind string
	// Source is the workflow source exactly as declared by the caller.
	Source string
	// ResolvedRef is the immutable resolved version: a git commit SHA for
	// git sources or the sha256:<digest> content digest for archive
	// sources. Empty for local sources.
	ResolvedRef string
	// Path is the local directory holding the workflow module:
	// cache/workflows/<slug>/<version> for remote sources, the declared
	// path for local ones.
	Path string
	// FetchedAt is the UTC fetch timestamp (ADR-0005 D5/D6): when the
	// resolved tree was materialized into the cache. A warm-cache resolution
	// reports the original materialization time, not the resolution time.
	FetchedAt time.Time
}

// resolveWorkflowSource resolves a workflow source to a local directory.
// Remote sources — git ref forms and http(s) archive forms (ADR-0005 D1) —
// are materialized through the workflowFetcher into
// cache/workflows/<slug>/<version> and described by the returned origin.
// Local paths are returned unchanged with a nil origin: local behavior is
// unchanged and local sources never enter the fetcher (ADR-0005 D3).
//
// expectedRef is the caller-declared expected pin (ADR-0005 D7, CRI-226):
// the git commit SHA or "sha256:<digest>" the resolved source must match.
// When set, resolution fails closed on mismatch — or on a local source,
// where the expectation cannot be verified — before any execution. The
// declared value is compared exactly, so a whitespace-only pin is a declared
// (and unfulfillable) pin, matching the compiler's subworkflow ref semantics.
// When empty, behavior is unchanged.
func resolveWorkflowSource(ctx context.Context, source, expectedRef string) (string, *WorkflowOrigin, error) {
	if !isRemoteWorkflowSource(source) {
		if expectedRef != "" {
			return "", nil, fmt.Errorf("workflow source %q is local; ref %q declared but expected pins apply only to remote git or archive sources", redactSourceForLog(source), expectedRef)
		}
		return source, nil, nil
	}

	// The git:: subtree form (CRI-227 convention) appends the in-repo
	// workflow directory to the repository URL: git::<repo>//<subdir>. The
	// fetcher fetches the REPOSITORY (the part before //); the resolved
	// workflow directory is the fetched tree joined with the subdir, which
	// must exist and contain .chcl/.hcl files (fail closed otherwise).
	repoSource, subdir := splitGitSubtreeSuffix(source)

	dir, pin, err := newWorkflowFetcherFunc().Fetch(ctx, ".", repoSource)
	if err != nil {
		return "", nil, err
	}
	if pin == nil {
		return "", nil, fmt.Errorf("remote workflow source %q resolved without a pin", redactSourceForLog(repoSource))
	}
	if subdir != "" {
		dir, err = joinWorkflowSubtree(dir, subdir, source)
		if err != nil {
			return "", nil, err
		}
	}

	if expectedRef != "" && pin.ResolvedRef != expectedRef {
		return "", nil, fmt.Errorf("workflow source %s: expected-pin mismatch: expected %q, resolved %q; refusing to run",
			redactSourceForLog(source), expectedRef, pin.ResolvedRef)
	}

	return dir, &WorkflowOrigin{
		Kind:        pin.Kind,
		Source:      source,
		ResolvedRef: pin.ResolvedRef,
		Path:        dir,
		FetchedAt:   fetchedAt(dir),
	}, nil
}

// splitGitSubtreeSuffix splits the git:: //subdir convention (CRI-227):
// "git::<repo>//<subdir>" into the fetchable repository source and the
// in-repo workflow directory. Only remote forms (http/https/ssh repository
// URLs) split; file: URLs keep verbatim behavior (their "file:///path"
// authority is not a subtree split). Returns repoSource == source when no
// suffix is present.
func splitGitSubtreeSuffix(source string) (repoSource, subdir string) {
	if !strings.HasPrefix(source, "git::") {
		return source, ""
	}
	trimmed := strings.TrimPrefix(source, "git::")
	isRemoteForm := strings.HasPrefix(trimmed, "http://") ||
		strings.HasPrefix(trimmed, "https://") ||
		strings.HasPrefix(trimmed, "ssh://")
	if !isRemoteForm {
		return source, ""
	}
	idx := strings.LastIndex(trimmed, "//")
	if idx == -1 {
		return source, ""
	}
	return "git::" + trimmed[:idx], trimmed[idx+2:]
}

// joinWorkflowSubtree joins the fetched tree with the //subdir workflow
// directory, failing closed when the subdir is missing or contains no
// .chcl/.hcl workflow files. source is the caller-declared full form,
// used only for error messages.
func joinWorkflowSubtree(treeDir, subdir, source string) (string, error) {
	sub := filepath.Join(treeDir, subdir)
	info, serr := os.Stat(sub)
	if serr != nil || !info.IsDir() {
		return "", fmt.Errorf("workflow source %q: //subdir %q does not exist in the fetched tree", redactSourceForLog(source), subdir)
	}
	entries, err := os.ReadDir(sub)
	if err != nil {
		return "", fmt.Errorf("workflow source %q: read //subdir %q: %w", redactSourceForLog(source), subdir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".chcl") || strings.HasSuffix(name, ".hcl") {
			return sub, nil
		}
	}
	return "", fmt.Errorf("workflow source %q: //subdir %q contains no .chcl or .hcl files", redactSourceForLog(source), subdir)
}

// fetchedAt reports the fetch timestamp of a resolved cache tree, taken from
// the tree directory's mtime: a fresh fetch renames the just-materialized
// tree into place, so the mtime is the materialization time, and a
// warm-cache hit keeps the original materialization time unchanged. If the
// directory cannot be stat'ed, the resolution time is the best available
// answer.
func fetchedAt(dir string) time.Time {
	if info, err := os.Stat(dir); err == nil {
		return info.ModTime().UTC()
	}
	return time.Now().UTC()
}

// redactSourceForLog masks userinfo credentials in a URL-shaped source
// ("******host/x.tar.gz" → "https://redacted@host/x.tar.gz") so
// secrets never reach structured logs — or the recorded RunMetadata
// provenance, which stores this redacted form (CRI-225). WorkflowOrigin in
// memory and the lockfile keep the raw source. Sources without a "://"
// separator (local paths, scp-style git forms) are returned unchanged.
//
// The implementation is shared with the workflow module's compile-time
// diagnostics (workflow.RedactSource, CRI-226 review R1) so every rendered
// surface redacts identically.
func redactSourceForLog(source string) string {
	return workflow.RedactSource(source)
}
