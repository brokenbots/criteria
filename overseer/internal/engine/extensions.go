package engine

import (
	"context"

	"github.com/brokenbots/overlord/workflow"
)

// Option applies optional engine configuration.
type Option func(*Engine)

// WithSubWorkflowResolver configures sub-workflow resolution support.
func WithSubWorkflowResolver(r SubWorkflowResolver) Option {
	return func(e *Engine) {
		e.subWorkflowResolver = r
	}
}

// WithBranchScheduler configures branch scheduling support.
func WithBranchScheduler(s BranchScheduler) Option {
	return func(e *Engine) {
		e.branchScheduler = s
	}
}

type BranchSpec struct{}

type JoinPolicy struct{}

type BranchResult struct{}

// SubWorkflowResolver compiles and caches sub-workflow graphs by relative path.
// Implemented in Phase 1.6. The interface lives here so engine.Engine doesn't
// have to change shape when sub-workflow nodes land.
type SubWorkflowResolver interface {
	Resolve(ctx context.Context, callerPath, targetPath string) (*workflow.FSMGraph, error)
}

// BranchScheduler runs parallel branches concurrently and joins them according
// to the parallel node's join policy. Implemented in Phase 1.6.
type BranchScheduler interface {
	Run(ctx context.Context, branches []BranchSpec, join JoinPolicy) (BranchResult, error)
}
