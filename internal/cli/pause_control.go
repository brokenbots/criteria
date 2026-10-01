package cli

import (
	"context"
	"log/slog"
	"time"

	"github.com/brokenbots/criteria/internal/engine"
	"github.com/brokenbots/criteria/internal/run"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// pauseAckResult reports the ack outcome of one consumed pause_run command.
// Every outcome is observable (CRI-62): the orchestrator sees either a
// RunPaused event (published by the sink on landing) or a timeout that keeps
// the latch armed for the next boundary.
type pauseAckResult string

const (
	pauseAckLanded pauseAckResult = "paused"      // landed at a node; RunPaused published by the sink
	pauseAckEnded  pauseAckResult = "run_ended"   // terminal or not running; not retryable
	pauseAckTimed  pauseAckResult = "ack_timeout" // step never reached a boundary within the budget (latch stays armed; retryable)
)

// getEngine returns the active engine for the router, nil when detached.
type getEngine func() *engine.Engine

// controlPauseRouter consumes orchestrator-issued pause_run commands for one
// owned run (CRI-254) and drives Engine.Pause semantics at step boundaries,
// mirroring the local-control (mode "external") PauseRun path: drain-first,
// durable checkpoint, no adapter kill. Castle stays the control plane; the
// agent owns pause execution.
//
// A pause that cannot land is never dropped silently: the outcome is logged
// with a drop_reason (run_ended_not_running, run_id_mismatch, ack_timeout)
// and, while landed or racing, the RunPaused event acks it upstream. Castle
// detects the missing ack via its control-plane timeout.
type controlPauseRouter struct {
	runID string
	sink  *run.Sink
	log   *slog.Logger
	eng   getEngine
}

func newControlPauseRouter(runID string, sink *run.Sink, log *slog.Logger) *controlPauseRouter {
	return &controlPauseRouter{runID: runID, sink: sink, log: log}
}

// setEngineProvider wires the latest engine instance (initial or resumed).
func (r *controlPauseRouter) setEngineProvider(provider getEngine) {
	r.eng = provider
}

// setEngine pins the current engine instance (the common one-engine case).
func (r *controlPauseRouter) setEngine(eng *engine.Engine) {
	r.eng = func() *engine.Engine { return eng }
}

// consume reads pause commands off the transport channel until it closes
// (stream teardown). Per-command failures are logged with their drop reason;
// the channel contract keeps the router alive for the next command.
func (r *controlPauseRouter) consume(ctx context.Context, pauseCh <-chan *pb.PauseRun) {
	for msg := range pauseCh {
		if msg == nil {
			continue
		}
		r.handle(ctx, msg)
	}
}

// handle lands one pause_run command and returns its ack outcome.
func (r *controlPauseRouter) handle(ctx context.Context, msg *pb.PauseRun) pauseAckResult {
	if msg.GetRunId() == "" {
		r.warnDrop("pause_run without run_id", "no_required_run_id")
		return pauseAckEnded
	}
	if msg.GetRunId() != r.runID {
		r.log.Warn("dropping pause_run for foreign run; the agent owns one run per router",
			slog.String("run_id", r.runID), slog.String("drop_reason", "run_id_mismatch"),
			slog.String("msg_run_id", msg.GetRunId()))
		return pauseAckEnded
	}
	if node := r.sink.PausedAt(); node != "" {
		r.log.Info("run already paused at node; pause_run is idempotent",
			slog.String("run_id", r.runID), slog.String("node", node),
			slog.String("requested_at", msg.GetReason()))
		return pauseAckLanded
	}
	eng := r.eng
	if eng == nil {
		r.warnDrop("pause_run for run with no active engine", "run_not_running")
		return pauseAckEnded
	}
	active := eng()
	if active == nil {
		r.warnDrop("pause_run for run with no active engine", "run_not_running")
		return pauseAckEnded
	}
	ack, ok := active.RequestPause()
	if !ok {
		if node := r.sink.PausedAt(); node != "" {
			// The run paused at a node while the latch was being taken.
			r.log.Info("run paused at node while the pause latch was being taken",
				slog.String("run_id", r.runID), slog.String("node", node))
			return pauseAckLanded
		}
		r.warnDrop("pause_run: run is no longer running", "run_not_running")
		return pauseAckEnded
	}
	return r.waitForPauseAck(ctx, ack, msg.GetReason())
}

// waitForPauseAck waits for the engine pause to land at a boundary or times
// out. The ack channel closes on both a real landing (after OnRunPaused
// recorded the node and the durable checkpoint was written) and a run-loop
// exit that dropped the latch; the pause tracker disambiguates the two.
func (r *controlPauseRouter) waitForPauseAck(ctx context.Context, ack <-chan struct{}, reason string) pauseAckResult {
	if reason != "" {
		r.log.Info("pause requested by orchestrator",
			slog.String("run_id", r.runID), slog.String("reason", reason))
	}
	timeout := time.NewTimer(pauseAckTimeout)
	defer timeout.Stop()
	select {
	case <-ack:
		if node := r.sink.PausedAt(); node != "" {
			r.log.Info("run paused at boundary",
				slog.String("run_id", r.runID), slog.String("node", node))
			return pauseAckLanded
		}
		r.warnDrop("run ended while the pause latch was pending; pause not honored", "run_ended_without_pausing")
		return pauseAckEnded
	case <-ctx.Done():
		r.warnDrop("pause wait cancelled by teardown (latch state unchanged)", "context_cancelled")
		return pauseAckTimed
	case <-timeout.C:
		r.warnDrop("pause request still pending after the ack budget; the latch stays armed and the pause lands at the next boundary",
			"ack_timeout")
		return pauseAckTimed
	}
}

// warnDrop logs a pause that will not land. The orchestrator detects the
// missing RunPaused ack via its control-plane timeout (CRI-62).
func (r *controlPauseRouter) warnDrop(msg, dropReason string) {
	r.log.Warn(msg, slog.String("run_id", r.runID), slog.String("drop_reason", dropReason))
}
