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

	"github.com/brokenbots/criteria/internal/cli/localresume"
	"github.com/brokenbots/criteria/internal/engine"
	"github.com/brokenbots/criteria/internal/plugin"
	"github.com/brokenbots/criteria/workflow"
)

const (
	errSignalWait   = "signal waits require an orchestrator (e.g. --server <url>) or the local-mode env CRITERIA_LOCAL_APPROVAL={stdin|file|env|auto-approve}"
	errApprovalNode = "approval nodes require an orchestrator (e.g. --server <url>) or the local-mode env CRITERIA_LOCAL_APPROVAL={stdin|file|env|auto-approve}"
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

// drainLocalResumeCycles drives the pause/resume loop for local-mode runs with
// CRITERIA_LOCAL_APPROVAL set. Each time the engine pauses, it calls the
// resumer, populates a new engine with the resulting payload, and re-invokes
// RunFrom until the run is no longer paused.
func drainLocalResumeCycles(ctx context.Context, log *slog.Logger, graph *workflow.FSMGraph, loader plugin.Loader, tracker *pauseTracker, resumer localresume.LocalResumer, runID string, opts applyOptions, eng *engine.Engine) error { //nolint:gocritic // hugeParam: opts passes applyOptions by value; pointer conversion is a separate workstream
	for tracker.IsPaused() {
		pausedNode := tracker.PausedAt()
		log.Info("local run paused; resolving via local resumer", "run_id", runID, "node", pausedNode)

		payload, err := resolveLocalPause(ctx, resumer, runID, pausedNode, graph, tracker)
		if err != nil {
			return fmt.Errorf("local approval at node %q: %w", pausedNode, err)
		}

		tracker.ClearPaused()
		resumedEng := engine.New(graph, loader, tracker,
			engine.WithResumedVars(eng.VarScope()),
			engine.WithResumedVisits(eng.VisitCounts()),
			engine.WithResumePayload(payload),
			engine.WithWorkflowDir(workflowDirFromPath(opts.workflowPath)),
		)
		if runErr := resumedEng.RunFrom(ctx, pausedNode, 1); runErr != nil {
			log.Error("local run failed after resume", "run_id", runID, "error", runErr)
			return runErr
		}
		eng = resumedEng
		log.Info("local run resumed", "run_id", runID)
	}
	return nil
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

func ensureLocalModeSupported(graph *workflow.FSMGraph, localApprovalEnabled bool) error {
	if !localApprovalEnabled {
		// First-class approval and signal-wait nodes require local approval mode or a server.
		for _, wn := range graph.Waits {
			if wn.Signal != "" {
				return errors.New(errSignalWait)
			}
		}
		if len(graph.Approvals) > 0 {
			return errors.New(errApprovalNode)
		}
	}
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
