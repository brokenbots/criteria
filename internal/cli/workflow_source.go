package cli

import (
	"context"
	"fmt"
	"os"
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

	dir, pin, err := newWorkflowFetcherFunc().Fetch(ctx, ".", source)
	if err != nil {
		return "", nil, err
	}
	if pin == nil {
		return "", nil, fmt.Errorf("remote workflow source %q resolved without a pin", redactSourceForLog(source))
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
