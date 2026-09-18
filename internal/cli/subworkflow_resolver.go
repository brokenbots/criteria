package cli

import (
	"context"
	"fmt"

	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

// fetchingSubWorkflowResolver resolves subworkflow sources for run-time
// compilation (CRI-224/CRI-226): local sources resolve in place through
// LocalSubWorkflowResolver (AllowedRoots preserved); remote sources are
// fetched through the workflowFetcher into the shared cache, and the
// fetcher's pin (git commit SHA or archive sha256 digest) is handed to the
// compiler so operator-declared expected pins can be enforced (CRI-226).
type fetchingSubWorkflowResolver struct {
	local   *workflow.LocalSubWorkflowResolver
	fetcher workflowFetcher
}

// newFetchingSubWorkflowResolver builds the run-time subworkflow resolver.
// subworkflowRoots keep their LocalSubWorkflowResolver AllowedRoots meaning:
// they constrain local source resolution only, never the fetched cache tree.
func newFetchingSubWorkflowResolver(subworkflowRoots []string) workflow.SubWorkflowResolver {
	return &fetchingSubWorkflowResolver{
		local:   &workflow.LocalSubWorkflowResolver{AllowedRoots: subworkflowRoots},
		fetcher: newWorkflowFetcherFunc(),
	}
}

func (r *fetchingSubWorkflowResolver) ResolveSource(ctx context.Context, callerDir, source string) (string, *lockfile.LockedWorkflowRef, error) {
	if !isRemoteWorkflowSource(source) {
		return r.local.ResolveSource(ctx, callerDir, source)
	}
	if r.fetcher == nil {
		return "", nil, fmt.Errorf("remote workflow source %q cannot be resolved: no fetcher configured", redactSourceForLog(source))
	}
	dir, pin, err := r.fetcher.Fetch(ctx, callerDir, source)
	if err != nil {
		return "", nil, err
	}
	if pin == nil {
		return "", nil, fmt.Errorf("remote workflow source %q resolved without a pin", redactSourceForLog(source))
	}
	return dir, pin, nil
}
