package engine

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/zclconf/go-cty/cty"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"

	"github.com/brokenbots/criteria/internal/adapter/environment/sandbox"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/lockfile"
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

// WithVarOverrides applies CLI-supplied variable values on top of the
// variable defaults at run start. Values are typed cty.Values produced by the
// CLI parsing layer and are coerced to each declared variable type by the
// eval layer.
func WithVarOverrides(overrides map[string]cty.Value) Option {
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

// WithLockfile sets the parsed adapter lockfile used for container-mode
// adapter resolution. It is the fallback in the engine's effective pin set
// rule (Engine.effectivePinSet): a compiled graph pin set — built at compile
// time from the workflow tree's .criteria.lock.hcl files — always wins, so
// URL-sourced runs never need this option.
func WithLockfile(lf *lockfile.Lockfile) Option {
	return func(e *Engine) {
		e.lockfile = lf
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

// WithAuditWriter sets the audit writer used by the session manager to
// record permission decisions. When nil, no audit entries are written.
func WithAuditWriter(w adapterhost.AuditWriter) Option {
	return func(e *Engine) {
		e.auditWriter = w
	}
}

// WithSnapshotBase sets the base directory for persisting session snapshots
// during Pause and reading them during Resume (WS18). When empty, snapshots
// are not persisted.
func WithSnapshotBase(dir string) Option {
	return func(e *Engine) {
		e.snapshotBase = dir
	}
}

// WithRunID sets the run identifier used to namespace snapshot files.
func WithRunID(id string) Option {
	return func(e *Engine) {
		e.runID = id
	}
}

// WithAgentPrompts wires the run's injected-prompt channel (fed by the CLI
// from the orchestrator's Control stream), the run owner identity used by
// the delivery-side caller re-check (ADR-0006 D4), and the run id prompts
// are addressed to (defaults to the snapshot run id). A nil channel leaves
// the prompt path disabled.
func WithAgentPrompts(ch <-chan *pb.AgentPrompt, ownerID, runID string) Option {
	return func(e *Engine) {
		e.agentPromptCh = ch
		e.promptOwnerID = ownerID
		e.promptRunID = runID
	}
}

// WithDataDir sets the run data directory used for rotated remote adapter
// accept-token files and other per-run transient state. When empty, token
// rotation is disabled and the legacy run-wide accept token is used.
func WithDataDir(dir string) Option {
	return func(e *Engine) {
		e.dataDir = dir
	}
}

// WithPauseToolCallDrainTimeout sets the bounded wait the drain-first pause
// posture (CRI-169, ADR-0004 §11) gives in-flight nested adapter tool calls
// to settle before canceling them. Zero means the SessionManager default
// (60s). Primarily a test hook so conformance tests can exercise the pause
// straggler path in bounded time.
func WithPauseToolCallDrainTimeout(d time.Duration) Option {
	return func(e *Engine) {
		e.pauseToolCallDrainTimeout = d
	}
}

// WithWorkingDirAllowedRoots restricts the directories an environment may bind
// to. A resolved working_directory that lies outside every configured root is
// rejected at run start, before any step executes. Empty (the default) disables
// the additional root check; paths containing ".." are always rejected.
func WithWorkingDirAllowedRoots(roots []string) Option {
	return func(e *Engine) {
		e.workingDirAllowedRoots = append([]string(nil), roots...)
	}
}

// WithSandboxProbeOverride is a test-only option that replaces the host sandbox
// capability probe used by the session manager. It allows tests to simulate a
// host with missing sandbox primitives (for example, a strict-mode sandbox
// adapter running on a kernel without landlock).
func WithSandboxProbeOverride(fn func() sandbox.Capabilities) Option {
	return func(e *Engine) {
		e.sandboxProbeOverride = fn
	}
}

// WithLocalShimIsolation enables CRI-293 shim address isolation for local
// runs. When two or more remote environments declare the same fixed
// listen_address, each environment's shim binds its own auto-chosen free
// loopback port (127.0.0.1:0) and that per-environment address is published
// to the environment's adapters exactly like a fixed address. Local runs bind
// every shim in one process, so a shared fixed port collides with
// EADDRINUSE; server runs put each environment's shim in its own adapter pod
// and must keep the declared address, so this option is wired only from
// local-mode entrypoints. Port-0 addresses and unix-socket listen values
// cannot collide this way and are left untouched, and a fixed port occupied
// by a foreign process still fails with the usual bind error naming the
// address.
func WithLocalShimIsolation() Option {
	return func(e *Engine) {
		e.localShimIsolation = true
	}
}

// WithAdoptableRunDirs lists prior invocations' run data directories whose
// surviving per-scope adapter instances may be adopted instead of rotating
// fresh scope tokens (CRI-304). A fresh replay after a checkpoint-consuming
// resume would otherwise reject the prior run's still-running pods' handshakes
// until the shim's verify budget expires, wedging the run for the full
// timeout; adoption re-handshakes those pods with the new run's shim instead.
// Directories are consulted in order and must have been produced by an
// invocation of the same run identity (the CLI derives them from persisted
// invocation-identity markers, excluding in-flight checkpoints). Adoption
// copies the chosen token into the run's own data directory and tombstones it
// in the prior directory, so each instance can be adopted by at most one
// later run.
func WithAdoptableRunDirs(dirs []string) Option {
	return func(e *Engine) {
		e.adoptableRunDirs = dirs
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

type ParallelTaskSpec struct{}

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
	Run(ctx context.Context, branches []ParallelTaskSpec, join JoinPolicy) (BranchResult, error)
}
