package cli

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

// lockfileCoverageError is returned when an adapter declared in a workflow has
// no matching, fully-populated lockfile entry. It names the workflow directory,
// the adapter, and the fix command.
type lockfileCoverageError struct {
	WorkflowDir string
	AdapterKey  string // "type.name"
	Reason      string
}

func (e lockfileCoverageError) Error() string {
	return fmt.Sprintf("%s: adapter %s is %s; run `criteria adapter lock %s` to pin it", e.WorkflowDir, e.AdapterKey, e.Reason, e.WorkflowDir)
}

// collectUnpinnedAdapters walks every workflow directory reachable from rootDir
// and returns the first adapter that is declared but not fully pinned. A
// lockfile entry is only acceptable when it has reference, resolved_digest, and
// source_url populated.
func collectUnpinnedAdapters(ctx context.Context, rootDir string) ([]lockfileCoverageError, error) {
	var errs []lockfileCoverageError
	seen := make(map[string]bool)
	if err := walkWorkflowDirs(ctx, rootDir, seen, func(dir string, spec *workflow.Spec, lf *lockfile.Lockfile) error {
		adapters := collectWorkflowAdapters(spec)
		for key, wa := range adapters {
			if wa.Source == "" {
				continue // non-OCI adapter, not subject to lockfile pinning
			}
			entry := findLocked(lf, wa.Type, wa.Name)
			if entry == nil {
				errs = append(errs, lockfileCoverageError{WorkflowDir: dir, AdapterKey: key, Reason: "unpinned (no lockfile entry)"})
				continue
			}
			if entry.Reference == "" || entry.ResolvedDigest == "" || entry.SourceURL == "" {
				errs = append(errs, lockfileCoverageError{WorkflowDir: dir, AdapterKey: key, Reason: "incomplete lockfile entry (requires reference, resolved_digest, and source_url)"})
				continue
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return errs, nil
}

// walkWorkflowDirs visits every workflow directory reachable from rootDir,
// including local-path subworkflows and materialised remote subworkflows. The
// callback receives the parsed spec and the directory's lockfile (nil if
// absent).
func walkWorkflowDirs(ctx context.Context, rootDir string, seen map[string]bool, fn func(dir string, spec *workflow.Spec, lf *lockfile.Lockfile) error) error {
	abs, err := filepath.Abs(rootDir)
	if err != nil {
		return fmt.Errorf("resolve workflow dir: %w", err)
	}
	if seen[abs] {
		return nil
	}
	seen[abs] = true

	spec, diags := workflow.ParseFileOrDir(abs)
	if diags.HasErrors() {
		return fmt.Errorf("parse workflow %q: %w", abs, newDiagsError(diags))
	}

	lf, err := lockfile.ReadFromDir(abs)
	if err != nil {
		return fmt.Errorf("read lockfile %q: %w", abs, err)
	}

	if err := fn(abs, spec, lf); err != nil {
		return err
	}

	fetcher := newWorkflowFetcherFunc()
	for _, sw := range spec.Subworkflows {
		resolved, _, err := resolveSubworkflowForLock(ctx, abs, sw.Source, fetcher)
		if err != nil {
			return fmt.Errorf("subworkflow %q in %q: %w", sw.Name, abs, err)
		}
		if err := walkWorkflowDirs(ctx, resolved, seen, fn); err != nil {
			return err
		}
	}
	return nil
}
