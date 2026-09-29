package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/cli/localresume"
	"github.com/brokenbots/criteria/internal/engine"
	"github.com/brokenbots/criteria/workflow"
)

const (
	errSignalWait   = "signal waits pause the run: resolve via the run's control listener (ResolveResume; started by apply), the local-mode env CRITERIA_LOCAL_APPROVAL={stdin|file|env|auto-approve}, or --answers <file> (an interactive TTY prompts by default)"
	errApprovalNode = "approval nodes pause the run: resolve via the run's control listener (ResolveResume; started by apply), the local-mode env CRITERIA_LOCAL_APPROVAL={stdin|file|env|auto-approve}, or --answers <file> (an interactive TTY prompts by default)"
)

// pauseTracker wraps an engine.Sink and tracks pause state for the local approval
// resume loop. It intercepts OnRunPaused to record the paused node name, and
// captures approval/signal details so the resume loop knows what to resolve.
// PauseCheckpointFn, when set, is called each time the engine pauses so that a
// crash while waiting for an approval or signal can be recovered on restart.
type pauseTracker struct {
	engine.Sink
	mu                sync.Mutex
	pausedNode        string
	approvalDetail    *approvalDetail
	signalDetail      *signalDetail
	PauseCheckpointFn func(node string)
	// OnNewPause, when set, is invoked at the start of every new pause
	// cycle (the control bus clears stale parked resume tokens/decisions).
	OnNewPause func()
}

type approvalDetail struct {
	approvers []string
	reason    string
}

type signalDetail struct {
	signalName string
}

func (t *pauseTracker) OnRunPaused(node, mode, signal string) {
	if t.OnNewPause != nil {
		t.OnNewPause()
	}
	t.Sink.OnRunPaused(node, mode, signal)
	t.mu.Lock()
	t.pausedNode = node
	t.mu.Unlock()
	if t.PauseCheckpointFn != nil {
		t.PauseCheckpointFn(node)
	}
}

func (t *pauseTracker) OnApprovalRequested(node string, approvers []string, reason string) {
	t.Sink.OnApprovalRequested(node, approvers, reason)
	t.mu.Lock()
	t.approvalDetail = &approvalDetail{approvers: approvers, reason: reason}
	t.mu.Unlock()
}

func (t *pauseTracker) OnWaitEntered(node, mode, duration, signal string) {
	t.Sink.OnWaitEntered(node, mode, duration, signal)
	if mode == "signal" {
		t.mu.Lock()
		t.signalDetail = &signalDetail{signalName: signal}
		t.mu.Unlock()
	}
}

func (t *pauseTracker) IsPaused() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.pausedNode != ""
}

func (t *pauseTracker) PausedAt() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.pausedNode
}

func (t *pauseTracker) ClearPaused() {
	t.mu.Lock()
	t.pausedNode = ""
	t.approvalDetail = nil
	t.signalDetail = nil
	t.mu.Unlock()
}

// drainLocalResumeCycles drives the pause/resume loop for local-mode runs.
// Each time the engine pauses, the loop resolves the pause and drives a fresh
// engine from the paused node until the run is no longer paused.
//
// Resolution depends on the pause mode (CRI-255, CRI-256):
//
//   - approval and signal-wait nodes: resolution follows the CRI-256
//     selection — --answers file (path 2), the explicit CRITERIA_LOCAL_APPROVAL
//     resumer (stdin|file|env|auto-approve), the interactive TTY prompt
//     (path 1, default), or control-RPC-only; the configured surface races
//     the run's control listener (ResolveResume) with first-resolution-wins
//     semantics.
//   - checkpoint-boundary pauses (Engine.RequestPause from the control
//     listener): resolution is a boundary ResumeRun token; the fresh engine
//     re-enters the graph without a resume payload.
//
// runSink is the sink passed to every engine instance so that terminal-state
// capture is consistent across the original run and all resume cycles. eng
// must be the engine that produced the first pause; later cycles update
// ctrl's engine pointer so control RPCs address the active engine.
func drainLocalResumeCycles(ctx context.Context, log *slog.Logger, loader adapterhost.Loader, runSink engine.Sink, resolution approvalResolution, runID string, opts applyOptions, ctrl *localRunControl, eng *engine.Engine) error {
	dataDir, err := runDataDir(runID)
	if err != nil {
		return fmt.Errorf("resolve run data dir: %w", err)
	}
	tracker := ctrl.tracker
	for tracker.IsPaused() {
		pausedNode := tracker.PausedAt()

		var payload map[string]string
		var err error
		if isApprovalOrSignalNode(ctrl.graph, pausedNode) {
			log.Info("local run paused; awaiting an approval or signal decision",
				"run_id", runID, "node", pausedNode)
			payload, err = resolveApprovalPause(ctx, log, ctrl, resolution, runID, pausedNode)
		} else {
			log.Info("run paused at checkpoint boundary; resume via the run's control listener",
				"run_id", runID, "node", pausedNode)
			err = awaitBoundaryRelease(ctx, ctrl)
		}
		if err != nil {
			return fmt.Errorf("local pause at node %q: %w", pausedNode, err)
		}

		tracker.ClearPaused()
		resumeOpts, err := localRunEngineOptions(opts.workflowPath, dataDir, runID)
		if err != nil {
			return err
		}
		resumeOpts = append(resumeOpts,
			engine.WithResumedVars(eng.VarScope()),
			engine.WithResumedVisits(eng.VisitCounts()))
		if payload != nil {
			resumeOpts = append(resumeOpts, engine.WithResumePayload(payload))
		}
		resumedEng := engine.New(ctrl.graph, loader, runSink, resumeOpts...)
		ctrl.setEngine(resumedEng)
		eng = resumedEng
		if runErr := resumedEng.RunFrom(ctx, pausedNode, 1); runErr != nil {
			log.Error("local run failed after resume", "run_id", runID, "error", runErr)
			return runErr
		}
		log.Info("local run resumed", "run_id", runID)
	}
	return nil
}

// awaitBoundaryRelease blocks until the run's control listener delivers a
// boundary ResumeRun token (no payload) or ctx is canceled (stop verb).
func awaitBoundaryRelease(ctx context.Context, ctrl *localRunControl) error {
	if ctrl.awaitBoundaryResume(ctx) {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("run canceled while boundary-paused: %w", ctx.Err())
	}
	return fmt.Errorf("boundary pause released without resume")
}

// resolveApprovalPause resolves an approval or signal-wait pause (CRI-256's
// two designed paths plus the out-of-band control surface), keeping the CRI-255
// race contract: prompt and control-RPC must not race — the first resolution
// wins and the loser is cancelled (or noted) cleanly.
func resolveApprovalPause(ctx context.Context, log *slog.Logger, ctrl *localRunControl, resolution approvalResolution, runID, pausedNode string) (map[string]string, error) {
	if resolution.resumer == nil {
		// No configured resumer: control-RPC-only resolution (CRI-255
		// headless flow, preserved as the default for non-TTY sessions). A
		// resolvable pause awaits the listener; an unresolvable one (listener
		// never attached) fails loudly rather than hanging forever.
		if !ctrl.isListenerUp() {
			return nil, errors.New("approval pause is unresolvable: stdin is not interactive, --answers was not given, and the run's control listener is not attached; rerun with --answers <file>, CRITERIA_LOCAL_APPROVAL={stdin|file|env|auto-approve}, or a reachable --control-addr")
		}
		log.Warn("approval pause has no resolution mode configured and stdin is not a TTY; the run stays paused until a decision arrives via the run's control listener",
			"node", pausedNode,
			"choices", "ResolveResume on the control listener, --answers <file>, or CRITERIA_LOCAL_APPROVAL={stdin|file|env|auto-approve}")
		payload, ok := ctrl.awaitResolveResume(ctx)
		if !ok {
			return nil, ctx.Err()
		}
		return payload, nil
	}

	type resolved struct {
		payload map[string]string
		err     error
	}
	stderr := resolution.cfg.promptStderr()
	promptFallback := resolution.promptFallbackResumer()
	for attempt := 0; attempt < 2; attempt++ {
		// A decision delivered while the pause landed wins the race before any
		// configured resumer starts (first resolution wins).
		if payload, ok := ctrl.pollResolveResume(); ok {
			log.Info("approval pause resolved via the run's control listener",
				"run_id", runID, "node", pausedNode)
			return payload, nil
		}
		activeResumer := resolution.resumer
		prompting := localresume.Interactive(activeResumer)
		rctx, cancel := context.WithCancel(ctx)
		ch := make(chan resolved, 1)
		go func() {
			payload, err := resolveLocalPause(rctx, activeResumer, runID, pausedNode, ctrl.graph, ctrl.tracker)
			ch <- resolved{payload: payload, err: err}
		}()
		select {
		case r := <-ch:
			cancel()
			if localresume.IsUnanswered(r.err) && attempt == 0 && resolution.answersActive {
				// The node is missing from the answers file: fall back to the
				// prompt path when interactive, else fail loudly naming the
				// node (never silence, never implicit approval).
				if !resolution.ttyOK {
					return nil, fmt.Errorf("%w; rerun interactively to answer %q at its pause, add the node to %s, or deliver the decision via the run's control listener",
						r.err, pausedNode, resolution.answersPath)
				}
				log.Info("approval pause has no entry in the answers file; falling back to the interactive prompt",
					"node", pausedNode, "file", resolution.answersPath)
				resolution.resumer = promptFallback
				resolution.interactive = true
				continue
			}
			if r.err == nil {
				// The configured resumer won the race; surface a decision
				// parked mid-race instead of letting it linger (first
				// resolution wins, discard observable). The parked payload is
				// deliberately not logged: it carries the operator's free-text
				// reason.
				if _, ok := ctrl.pollResolveResume(); ok {
					log.Warn("a control-RPC decision arrived while the pause resolved via the configured resumer; the parked decision was discarded (first resolution wins)",
						"run_id", runID, "node", pausedNode)
				}
			}
			return r.payload, r.err
		case payload := <-ctrl.resolveResumeChan():
			// An RPC decision delivered mid-prompt: it wins the race and the
			// prompt is cancelled cleanly (first resolution wins).
			cancel()
			if prompting {
				fmt.Fprintf(stderr, "\n[criteria] approval %q was already resolved via the run's control listener; the interactive prompt was dismissed.\n", pausedNode)
			}
			return payload, nil
		case <-ctx.Done():
			cancel()
			return nil, ctx.Err()
		}
	}
	return nil, errors.New("approval pause could not be resolved")
}

// resolveLocalPause determines whether the paused node is an approval or
// signal-wait and calls the appropriate resumer method.
func resolveLocalPause(ctx context.Context, resumer localresume.LocalResumer, runID, pausedNode string, graph *workflow.FSMGraph, tracker *pauseTracker) (map[string]string, error) {
	if _, isApproval := graph.Approvals[pausedNode]; isApproval {
		tracker.mu.Lock()
		ad := tracker.approvalDetail
		tracker.mu.Unlock()
		var approvers []string
		var reason string
		if ad != nil {
			approvers = ad.approvers
			reason = ad.reason
		}
		return resumer.ResumeApproval(ctx, runID, pausedNode, approvers, reason)
	}
	if wait, isWait := graph.Waits[pausedNode]; isWait && wait.Signal != "" {
		tracker.mu.Lock()
		sd := tracker.signalDetail
		tracker.mu.Unlock()
		signalName := wait.Signal
		if sd != nil {
			signalName = sd.signalName
		}
		validOutcomes := make([]string, 0, len(wait.Outcomes))
		for o := range wait.Outcomes {
			validOutcomes = append(validOutcomes, o)
		}
		sort.Strings(validOutcomes)
		return resumer.ResumeSignal(ctx, runID, pausedNode, signalName, validOutcomes)
	}
	return nil, fmt.Errorf("paused at node %q which is neither an approval nor a signal wait", pausedNode)
}

func ensureLocalModeSupported(graph *workflow.FSMGraph) error {
	// Since CRI-255 the run's control listener is always attached to local
	// apply, so approval and signal-wait pauses are supported without
	// CRITERIA_LOCAL_APPROVAL: resolution arrives via the ResolveResume RPC
	// (or a configured file/env resumer). No first-class gating remains.
	// Legacy state.Requires shapes are unsupported in local mode
	// regardless of CRITERIA_LOCAL_APPROVAL.
	for _, state := range graph.States {
		switch strings.ToLower(strings.TrimSpace(state.Requires)) {
		case "signal", "wait_signal", "wait.signal":
			return errors.New(errSignalWait)
		case "approval", "wait_approval", "wait.approval":
			return errors.New(errApprovalNode)
		}
	}
	return nil
}
