package engine

import (
	"context"
	"log/slog"
	"strings"

	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/workflow"
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

// WithResumedVisits sets the per-step visit counts to restore at run start.
// Used during crash recovery to ensure max_visits limits count from the
// correct baseline after a resume (W07).
func WithResumedVisits(visits map[string]int) Option {
	return func(e *Engine) {
		e.resumedVisits = visits
	}
}

// WithResumedIter sets the IterCursor stack to restore at run start. Used during
// crash recovery when a step iteration was active at the time of the crash (W10).
// Formerly WithResumedIter(*workflow.IterCursor) (W07); updated to accept a
// slice for stack-based nested body support.
// Each cursor's Items field may be nil; the step re-evaluates the expression on
// first entry.
func WithResumedIter(stack []workflow.IterCursor) Option {
	return func(e *Engine) {
		e.resumedIterStack = stack
	}
}

// WithPendingSignal seeds RunState.PendingSignal at the start of RunFrom.
// Use this when re-attaching an adapter to a run that was paused mid-signal-
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

// WithVarOverrides applies CLI-supplied key=value pairs on top of the
// variable defaults at run start. Values are always treated as strings and
// coerced to the declared variable type by the eval layer.
func WithVarOverrides(overrides map[string]string) Option {
	return func(e *Engine) {
		e.varOverrides = overrides
	}
}

// WithWorkflowDir sets the directory containing the HCL workflow file.
// When set, file() and fileexists() expression functions resolve relative
// paths against this directory during workflow execution.
func WithWorkflowDir(dir string) Option {
	return func(e *Engine) {
		e.workflowDir = dir
	}
}

// WithLogger sets the structured logger used for internal engine warnings.
// When not set, slog.Default() is used.
// Pass the same logger used by the surrounding CLI command for consistent
// log routing.
func WithLogger(log *slog.Logger) Option {
	return func(e *Engine) {
		e.log = log
	}
}

// WithAutoBootstrapAdapters enables automatic session opening for adapters without
// explicit lifecycle "open" steps. This is a temporary measure pre-W12 and is enabled
// by default. Production workflows should use explicit lifecycle management (W12).
func WithAutoBootstrapAdapters() Option {
	return func(e *Engine) {
		e.autoBootstrapAdapters = true
	}
}

// WithStrictLifecycleSemantics disables automatic adapter bootstrap, requiring explicit
// lifecycle management. Useful for testing strict lifecycle semantics (pre-W12 validation).
func WithStrictLifecycleSemantics() Option {
	return func(e *Engine) {
		e.autoBootstrapAdapters = false
	}
}

// isSuccessOutcome returns true when the outcome name indicates a successful
// iteration. By convention, outcome names that equal "success" (case-
// insensitive) are treated as successes; all other names set AnyFailed=true
// when transitioning back via _continue. This matches the canonical naming
// in workstream examples ("success" vs "failure"). Workflows that use
// non-standard names should route non-success outcomes to a non-_continue
// target for explicit abort-on-failure behaviour.
func isSuccessOutcome(outcome string) bool {
	return strings.EqualFold(outcome, "success")
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
