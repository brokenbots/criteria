package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/cli/localresume"
	"github.com/brokenbots/criteria/internal/engine"
	"github.com/brokenbots/criteria/workflow"
)

const (
	errSignalWait    = "signal waits are resolved via the run's local control listener (started by apply), --server <url>, or the local-mode env CRITERIA_LOCAL_APPROVAL={stdin|file|env|auto-approve}"
	errApprovalNode = "approval nodes are resolved via the run's local control listener (started by apply), --server <url>, or the local-mode env CRITERIA_LOCAL_APPROVAL={stdin|file|env|auto-approve}"
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
}

type approvalDetail struct {
	approvers []string
	reason    string
}

type signalDetail struct {
	signalName string
}

func (t *pauseTracker) OnRunPaused(node, mode, signal string) {
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

// buildLocalResumer constructs a LocalResumer from CRITERIA_LOCAL_APPROVAL and
// CRITERIA_LOCAL_APPROVAL_FILE_TIMEOUT. Returns nil, nil when
// CRITERIA_LOCAL_APPROVAL is unset (local approval not enabled).
// stdin is used for stdin-mode prompts; nil falls back to os.Stdin.
func buildLocalResumer(log *slog.Logger, stdin io.Reader) (localresume.LocalResumer, error) {
	raw := os.Getenv("CRITERIA_LOCAL_APPROVAL")
	if raw == "" {
		return nil, nil
	}
	m, err := localresume.ParseMode(raw)
	if err != nil {
		return nil, err
	}
	opts := localresume.Options{
		Log:            log,
		Stdin:          stdin, // nil → Options.applyDefaults uses os.Stdin
		DecisionPathFn: ApprovalDecisionPath,
		RequestPathFn:  ApprovalRequestPath,
	}
	if rawTimeout := os.Getenv("CRITERIA_LOCAL_APPROVAL_FILE_TIMEOUT"); rawTimeout != "" {
		d, err := time.ParseDuration(rawTimeout)
		if err != nil {
			return nil, fmt.Errorf("invalid CRITERIA_LOCAL_APPROVAL_FILE_TIMEOUT=%q: %w", rawTimeout, err)
		}
		opts.FileTimeout = d
	}
	return localresume.New(m, opts), nil
}

// drainLocalResumeCycles drives the pause/resume loop for local-mode runs.
// Each time the engine pauses, the loop resolves the pause and drives a fresh
// engine from the paused node until the run is no longer paused.
//
// Resolution depends on the pause mode (CRI-255):
//
//   - approval and signal-wait nodes: the decision rides the run's local
//     control listener (ResolveResume RPC — the primary surface), racing a
//     configured CRITERIA_LOCAL_APPROVAL resumer (file/stdin/env — the
//     out-of-band surface kept for scripted use).
//   - checkpoint-boundary pauses (Engine.RequestPause from the control
//     listener): resolution is a boundary ResumeRun token; the fresh engine
//     re-enters the graph without a resume payload.
//
// runSink is the sink passed to every engine instance so that terminal-state
// capture is consistent across the original run and all resume cycles. eng
// must be the engine that produced the first pause; later cycles update
// ctrl's engine pointer so control RPCs address the active engine.
func drainLocalResumeCycles(ctx context.Context, log *slog.Logger, loader adapterhost.Loader, runSink engine.Sink, resumer localresume.LocalResumer, runID string, opts applyOptions, ctrl *localRunControl, eng *engine.Engine) error {
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
			payload, err = resolveApprovalPause(ctx, ctrl, resumer, runID, pausedNode)
		} else {
			log.Info("run paused at checkpoint boundary; resume via the run's control listener",
				"run_id", runID, "node", pausedNode)
			payload, err = awaitBoundaryResumePayload(ctx, ctrl)
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

// awaitBoundaryResumePayload blocks until the run's control listener delivers
// a boundary ResumeRun token (nil payload) or ctx is canceled (stop verb).
func awaitBoundaryResumePayload(ctx context.Context, ctrl *localRunControl) (map[string]string, error) {
	if !ctrl.awaitBoundaryResume(ctx) {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("run canceled while boundary-paused: %w", ctx.Err())
		}
		return nil, fmt.Errorf("boundary pause released without resume")
	}
	return nil, nil
}

// resolveApprovalPause resolves an approval or signal-wait pause: the
// primary surface is the run's control listener (ResolveResume); when a
// CRITERIA_LOCAL_APPROVAL resumer is configured it races the listener.
func resolveApprovalPause(ctx context.Context, ctrl *localRunControl, resumer localresume.LocalResumer, runID, pausedNode string) (map[string]string, error) {
	if resumer == nil {
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
	file := make(chan resolved, 1)
	go func() {
		payload, err := resolveLocalPause(ctx, resumer, runID, pausedNode, ctrl.graph, ctrl.tracker)
		file <- resolved{payload: payload, err: err}
	}()
	select {
	case r := <-file:
		return r.payload, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
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
