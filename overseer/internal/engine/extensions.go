package engine

import (
	"context"

	"github.com/brokenbots/overlord/workflow"
	"github.com/zclconf/go-cty/cty"
)

// Option applies optional engine configuration.
type Option func(*Engine)

// WithResumedVars sets the vars map to use at run start instead of
// SeedVarsFromGraph. Used during crash recovery to restore captured step
// outputs and variable state (W04).
func WithResumedVars(vars map[string]cty.Value) Option {
	return func(e *Engine) {
		e.resumedVars = vars
	}
}

// WithPendingSignal seeds RunState.PendingSignal at the start of RunFrom.
// Use this when re-attaching an Overseer to a run that was paused mid-signal-
// wait: the wait node sees PendingSignal set and immediately re-issues
// ErrPaused so the run stays blocked until the real Resume RPC arrives (W05).
func WithPendingSignal(signal string) Option {
	return func(e *Engine) {
		e.pendingSignal = signal
	}
}

// WithResumePayload seeds RunState.ResumePayload at the start of RunFrom.
// Use this when re-entering a paused run after the orchestrator delivers a
// resume signal. The wait/approval node reads the payload to resolve its
// outcome and then clears the field (W05).
func WithResumePayload(payload map[string]string) Option {
	return func(e *Engine) {
		e.resumePayload = payload
	}
}

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
