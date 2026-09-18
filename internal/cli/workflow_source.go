package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

// WorkflowOrigin records the resolved provenance of a workflow source
// (ADR-0005 D4/D6): where the content came from, the immutable version that
// was resolved, and the local path it was materialized into. RunMetadata
// provenance recording (CRI-225) consumes this record; expected-pin
// enforcement on the resolved ref is CRI-226's job and deliberately not
// checked here.
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
func resolveWorkflowSource(ctx context.Context, source string) (string, *WorkflowOrigin, error) {
	if !isRemoteWorkflowSource(source) {
		return source, nil, nil
	}

	dir, pin, err := newWorkflowFetcherFunc().Fetch(ctx, ".", source)
	if err != nil {
		return "", nil, err
	}
	if pin == nil {
		return "", nil, fmt.Errorf("remote workflow source %q resolved without a pin", source)
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
// ("https://user:token@host/x.tar.gz" → "https://redacted@host/x.tar.gz") so
// secrets never reach structured logs — or the recorded RunMetadata
// provenance, which stores this redacted form (CRI-225). WorkflowOrigin in
// memory and the lockfile keep the raw source. Sources without a "://"
// separator (local paths, scp-style git forms) are returned unchanged.
func redactSourceForLog(source string) string {
	idx := strings.Index(source, "://")
	if idx == -1 {
		return source
	}
	rest := source[idx+3:]
	// The userinfo delimiter can only appear in the authority component,
	// which ends at the first "/", "?" or "#"; an "@" later in the path must
	// not be mistaken for one. Within the authority, the first "@" is the
	// delimiter (userinfo cannot contain a literal "@").
	authority := rest
	if end := strings.IndexAny(rest, "/?#"); end != -1 {
		authority = rest[:end]
	}
	at := strings.Index(authority, "@")
	if at == -1 {
		return source
	}
	return source[:idx+3] + "redacted@" + rest[at+1:]
}
