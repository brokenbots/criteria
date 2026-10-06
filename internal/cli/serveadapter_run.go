package cli

// Child-run machinery for serve-adapter: the workflow executes on the engine's
// REAL local-run path (same record/checkpoint/audit plumbing as `criteria
// apply`, without the resume-loopback listener) and is projected onto the v2
// event stream and log ring by a bridge sink (ADR-0008 D2).

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	criteriav2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	structpb "google.golang.org/protobuf/types/known/structpb"
	timestamppb "google.golang.org/protobuf/types/known/timestamppb"

	"github.com/google/uuid"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/adapterhost/heartbeatutil"
	"github.com/brokenbots/criteria/internal/engine"
	"github.com/brokenbots/criteria/internal/runstate"
	"github.com/brokenbots/criteria/internal/tunables"
	criteriav1 "github.com/brokenbots/criteria/sdk/pb/criteria/v1"

	"github.com/zclconf/go-cty/cty"
)

const (
	// serveAdapterEventQueueCapacity bounds the event bridge between the
	// engine's sink callbacks (plus child-adapter step sinks) and the Execute
	// stream pump. Overflow drops non-terminal events with a log line rather
	// than blocking engine goroutines on a stalled host stream.
	serveAdapterEventQueueCapacity = 512

	// serveAdapterLogRingCapacity bounds the per-session replay ring for Log
	// streams (drop-oldest). Generous: host-side Log consumers replay this
	// fully on attach.
	serveAdapterLogRingCapacity = 4096
)

// serveAdapterLogRing is a bounded drop-oldest ring of log lines per session
// (in insertion order), closed on session teardown. Tail replays everything
// buffered and then follows until the caller's context ends.
type serveAdapterLogRing struct {
	mu     sync.Mutex
	cond   *sync.Cond
	cap    int
	events []*criteriav2.LogEvent
	closed bool
	// tailCursor is the ring position Tail has already replayed (events are
	// dropped from the front, so positions shift; Tail tracks by identity).
	tailSeen int
}

func newServeAdapterLogRing(cap int) *serveAdapterLogRing {
	r := &serveAdapterLogRing{cap: cap}
	r.cond = sync.NewCond(&r.mu)
	return r
}

func (r *serveAdapterLogRing) append(ev *criteriav2.LogEvent) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	if len(r.events) > r.cap {
		drop := len(r.events) - r.cap
		r.events = r.events[drop:]
		if r.tailSeen > drop {
			r.tailSeen -= drop
		} else {
			r.tailSeen = 0
		}
	}
	r.cond.Broadcast()
	r.mu.Unlock()
}

// Tail replays buffered lines and follows new ones until ctx is done, keeping
// the stream warm with heartbeat events from the shared heartbeat utility
// (same interval as local log tailing). Lines and heartbeats share one send
// mutex; the stream closes when ctx is done (host detach) or the session is
// removed.
func (r *serveAdapterLogRing) Tail(ctx context.Context, sender *logSinkSender) error {
	sendMu := &sync.Mutex{}
	wrap := &ringSendGuard{inner: sender, mu: sendMu}

	heartCtx, cancelHeart := context.WithCancel(ctx)
	defer cancelHeart()
	var heartWG sync.WaitGroup
	heartWG.Add(1)
	go func() {
		defer heartWG.Done()
		_ = heartbeatutil.RunLogHeartbeat(heartCtx, wrap)
	}()
	defer func() {
		cancelHeart()
		heartWG.Wait()
	}()

	for {
		r.mu.Lock()
		for r.tailSeen < len(r.events) {
			ev := r.events[r.tailSeen]
			r.tailSeen++
			sendMu.Lock()
			r.mu.Unlock()
			err := wrap.inner.Send(ev)
			sendMu.Unlock()
			if err != nil {
				return err
			}
			r.mu.Lock()
		}
		r.cond.Wait()
		if r.closed {
			r.mu.Unlock()
			return nil
		}
		r.mu.Unlock()
		if ctx.Err() != nil {
			return nil
		}
	}
}

// Close wakes any Tail followers; they return without error (the stream ends
// cleanly at session teardown).
func (r *serveAdapterLogRing) Close() {
	r.mu.Lock()
	r.closed = true
	r.cond.Broadcast()
	r.mu.Unlock()
}

// ringSendGuard serializes line sends against heartbeat sends on one stream.
type ringSendGuard struct {
	inner *logSinkSender
	mu    *sync.Mutex
}

func (g *ringSendGuard) Send(ev *criteriav2.LogEvent) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.inner.Send(ev)
}

// serveAdapterEvent is one queued event waiting for the Execute stream pump.
type serveAdapterEvent struct {
	ev      *criteriav2.ExecuteEvent
	dropOK  bool // non-terminal events may be dropped on overflow
	kindTag string
}

// serveAdapterBridge wraps the local sink chain (terminal capture over the
// local NDJSON sink, over the pause tracker) and forwards lifecycle events to
// the queue for Execute plus the session log ring for Log. All non-delegate
// work is non-blocking and must never panic the engine goroutine.
type serveAdapterBridge struct {
	engine.Sink
	run   *serveAdapterRun
	ring  *serveAdapterLogRing
	queue chan serveAdapterEvent
	log   *slog.Logger

	pushMu sync.Mutex
}

// push enqueues an event, dropping non-terminal ones on overflow.
func (b *serveAdapterBridge) push(ev *criteriav2.ExecuteEvent, dropOK bool, kindTag string) {
	b.pushMu.Lock()
	defer b.pushMu.Unlock()
	select {
	case b.queue <- serveAdapterEvent{ev: ev, dropOK: dropOK, kindTag: kindTag}:
	default:
		if !dropOK {
			b.log.Error("event queue overflow; terminal marker event delayed", "kind", kindTag)
			b.queue <- serveAdapterEvent{ev: ev, dropOK: dropOK, kindTag: kindTag}
		} else {
			b.log.Warn("event queue overflow; dropping non-terminal event", "kind", kindTag)
		}
	}
}

func (b *serveAdapterBridge) stampActivity() {
	b.run.mu.Lock()
	b.run.lastActivity = time.Now().UTC()
	b.run.mu.Unlock()
}

// pushLifecycle builds and queues a workflow.v1-prefixed adapter event.
func (b *serveAdapterBridge) pushLifecycle(kind string, fields map[string]any) {
	payload, err := structpbMap(fields)
	if err != nil {
		b.log.Warn("could not encode adapter event payload", "kind", kind, "error", err)
		return
	}
	b.stampActivity()
	adapterEv := &criteriav2.AdapterEvent{
		EventKind: kind,
		Payload:   payload,
		EmittedAt: timestamppb.Now(),
	}
	b.push(&criteriav2.ExecuteEvent{Event: &criteriav2.ExecuteEvent_Adapter{Adapter: adapterEv}}, true, kind)
}

// structpbMap marshals a field map into a Struct payload.
func structpbMap(fields map[string]any) (*structpb.Struct, error) {
	return structpb.NewStruct(fields)
}

func (b *serveAdapterBridge) OnRunStarted(workflowName, initialStep string) {
	b.Sink.OnRunStarted(workflowName, initialStep)
	b.pushLifecycle(serveAdapterEventRunStarted, map[string]any{
		"workflow":     workflowName,
		"initial_step": initialStep,
		"run_id":       b.run.id,
	})
}

func (b *serveAdapterBridge) OnStepEntered(step, adapterName string, attempt int) {
	b.Sink.OnStepEntered(step, adapterName, attempt)
	b.run.mu.Lock()
	b.run.currentStep = step
	b.run.mu.Unlock()
	b.pushLifecycle(serveAdapterEventStepStarted, map[string]any{
		"step":    step,
		"adapter": adapterName,
		"attempt": float64(attempt),
	})
}

func (b *serveAdapterBridge) OnStepOutcome(step, outcome string, duration time.Duration, err error, comment string) {
	b.Sink.OnStepOutcome(step, outcome, duration, err, comment)
	fields := map[string]any{
		"step":        step,
		"outcome":     outcome,
		"duration_ms": float64(duration.Milliseconds()),
	}
	if err != nil {
		fields["error"] = err.Error()
	}
	if comment != "" {
		fields["comment"] = comment
	}
	b.pushLifecycle(serveAdapterEventStepOutcome, fields)
}

func (b *serveAdapterBridge) OnStepOutcomeInvalid(step, outcome string, issues []string, attempt int) {
	b.Sink.OnStepOutcomeInvalid(step, outcome, issues, attempt)
	fields := map[string]any{
		"step":    step,
		"outcome": outcome,
		"invalid": true,
		"attempt": float64(attempt),
	}
	if len(issues) > 0 {
		fields["issues"] = issues
	}
	b.pushLifecycle(serveAdapterEventStepOutcome, fields)
}

func (b *serveAdapterBridge) OnStepOutcomeUnknown(step, outcome string) {
	b.Sink.OnStepOutcomeUnknown(step, outcome)
	b.pushLifecycle(serveAdapterEventStepOutcome, map[string]any{
		"step": step, "outcome": outcome, "unknown": true,
	})
}

func (b *serveAdapterBridge) OnStepOutcomeDefaulted(step, original, mapped string) {
	b.Sink.OnStepOutcomeDefaulted(step, original, mapped)
	b.pushLifecycle(serveAdapterEventStepOutcome, map[string]any{
		"step": step, "outcome": mapped, "defaulted_from": original,
	})
}

func (b *serveAdapterBridge) OnStepTransition(from, to, viaOutcome string) {
	b.Sink.OnStepTransition(from, to, viaOutcome)
	b.pushLifecycle(serveAdapterEventStepTransition, map[string]any{
		"from": from, "to": to, "via_outcome": viaOutcome,
	})
}

func (b *serveAdapterBridge) OnRunPaused(node, mode, signal string) {
	b.Sink.OnRunPaused(node, mode, signal)
	b.run.mu.Lock()
	b.run.pausedNode = node
	b.run.mu.Unlock()
	b.pushLifecycle(serveAdapterEventRunPaused, map[string]any{
		"node": node, "mode": mode, "signal": signal,
	})
}

func (b *serveAdapterBridge) OnRunResumed(node string) {
	b.Sink.OnRunResumed(node)
	b.run.mu.Lock()
	b.run.pausedNode = ""
	b.run.mu.Unlock()
	b.pushLifecycle(serveAdapterEventRunResumed, map[string]any{"node": node})
}

func (b *serveAdapterBridge) OnVariableSet(name, value, source string) {
	b.Sink.OnVariableSet(name, value, source)
	b.pushLifecycle(serveAdapterEventVariableSet, map[string]any{
		"name": name, "value": value, "source": source,
	})
}

func (b *serveAdapterBridge) OnRunOutputs(outputs []map[string]string) {
	b.Sink.OnRunOutputs(outputs)
	projected := map[string]string{}
	for _, o := range outputs {
		if name := o["name"]; name != "" {
			projected[name] = o["value"]
		}
	}
	b.run.mu.Lock()
	b.run.outputs = projected
	b.run.mu.Unlock()
}

func (b *serveAdapterBridge) OnRunFailed(reason, step string) {
	b.Sink.OnRunFailed(reason, step)
	b.run.mu.Lock()
	b.run.terminalComment = reason
	b.run.mu.Unlock()
	b.pushLifecycle(serveAdapterEventRunFailed, map[string]any{
		"reason": reason, "step": step,
	})
}

func (b *serveAdapterBridge) OnStepResumed(step string, attempt int, reason string) {
	b.Sink.OnStepResumed(step, attempt, reason)
	b.pushLifecycle(serveAdapterEventStepResumed, map[string]any{
		"step": step, "attempt": float64(attempt), "reason": reason,
	})
}

// StepEventSink wraps the delegate's per-step sink so child adapter output
// lines are ALSO mirrored into the session log ring and event queue (the
// workflow.v1.log event kind). Adapter-only payload events stay off the
// stream: permission requests and finalized-outcome markers are host-side
// control plane events with their own forwarding paths, and blindly
// re-emitting them from the bridge would double-deliver.
func (b *serveAdapterBridge) StepEventSink(step string) adapter.EventSink {
	return &bridgeStepSink{delegate: b.Sink.StepEventSink(step), bridge: b, step: step}
}

type bridgeStepSink struct {
	delegate adapter.EventSink
	bridge   *serveAdapterBridge
	step     string
}

func (s *bridgeStepSink) Log(stream string, chunk []byte) {
	if s.delegate != nil {
		s.delegate.Log(stream, chunk)
	}
	s.bridge.mirrorStepLog(s.step, stream, chunk)
}

func (s *bridgeStepSink) Adapter(kind string, data any) {
	if s.delegate != nil {
		s.delegate.Adapter(kind, data)
	}
}

// mirrorStepLog fans a step's adapter output line into the ring and queue.
func (b *serveAdapterBridge) mirrorStepLog(step, stream string, chunk []byte) {
	b.stampActivity()
	line := append([]byte(nil), chunk...)
	if logEv := b.stepLogEvent(step, stream, line); logEv != nil {
		b.ring.append(logEv)
	}
	payload, err := structpbMap(map[string]any{
		"step":   step,
		"stream": stream,
		"line":   string(line),
	})
	if err != nil {
		b.log.Warn("could not encode log event payload", "error", err)
		return
	}
	b.push(&criteriav2.ExecuteEvent{Event: &criteriav2.ExecuteEvent_Adapter{Adapter: &criteriav2.AdapterEvent{
		EventKind: serveAdapterEventLog,
		Payload:   payload,
		EmittedAt: timestamppb.Now(),
	}}}, true, serveAdapterEventLog)
}

func (b *serveAdapterBridge) stepLogEvent(step, stream string, line []byte) *criteriav2.LogEvent {
	return &criteriav2.LogEvent{
		SessionId:  b.run.session.id,
		StepName:   step,
		StreamName: stream,
		Line:       line,
		Timestamp:  timestamppb.Now(),
	}
}

// ---- wire event kinds (workflow.v1-prefixed) ----

const (
	serveAdapterEventRunStarted     = "workflow.v1.run_started"
	serveAdapterEventStepStarted    = "workflow.v1.step_started"
	serveAdapterEventStepOutcome    = "workflow.v1.step_outcome"
	serveAdapterEventStepTransition = "workflow.v1.step_transition"
	serveAdapterEventStepResumed    = "workflow.v1.step_resumed"
	serveAdapterEventRunPaused      = "workflow.v1.run_paused"
	serveAdapterEventRunResumed     = "workflow.v1.run_resumed"
	serveAdapterEventVariableSet    = "workflow.v1.variable_set"
	serveAdapterEventLog            = "workflow.v1.log"
	serveAdapterEventRunFailed      = "workflow.v1.run_failed"
)

// openChildRun creates the single in-flight child run bookkeeping for an
// Execute call. The run anchors to the client's base context (signal-driven),
// per the keepalive doctrine: the Execute RPC stream dying must not kill the
// run; CloseSession or the Control cancel arm is the explicit teardown path.
func (c *serveAdapterClient) openChildRun(sess *serveAdapterSession, req *criteriav2.ExecuteRequest) (*serveAdapterRun, error) {
	ctx, cancel := context.WithCancel(c.baseCtx)
	run := &serveAdapterRun{
		id:      uuid.NewString(),
		session: sess,
		ctx:     ctx,
		cancel:  cancel,
		done:    make(chan struct{}),
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.run != nil {
		cancel()
		return nil, &ErrChildRunInFlight{RunID: c.run.id, Workflow: c.graph.Name}
	}
	c.sessions[sess.id] = sess
	c.run = run
	return run, nil
}

// driveChildRun runs the engine loop on its own goroutine and pumps the
// Execute stream until a terminal state or cancel lands (it returns only
// after the terminal Result event was delivered). The engine anchors to the
// child run's own context, not the RPC context.
func (c *serveAdapterClient) driveChildRun(run *serveAdapterRun, req *criteriav2.ExecuteRequest, sink adapterhost.ExecuteEventSink) error {
	defer run.cancel()

	ring := run.session.ring
	queue := make(chan serveAdapterEvent, serveAdapterEventQueueCapacity)

	// Local-run machinery mirror: NDJSON events tee, local checkpointing
	// function, and the output sink chain the pause tracker rides on.
	eventOut, closeEvents, err := openRunEventsFile(run.id)
	if err != nil {
		c.clearRun(run)
		return connectErrorStatus(fmt.Errorf("open run events file: %w", err))
	}
	getVisits := func() map[string]int { return run.engineVisits() }
	checkpointFn := buildLocalCheckpointFn(c.log, run.id, c.graph.Name, c.workflowPath, c.sourceHash, getVisits)
	baseSink, local := buildLocalSink(run.id, eventOut, outputModeJSON, c.graph.StepOrder(), checkpointFn, c.graph)
	tsSink := &terminalSuccessSink{Sink: baseSink}
	tracker := &pauseTracker{Sink: tsSink, PauseCheckpointFn: func(node string) { checkpointFn(node, 0) }}
	bridge := &serveAdapterBridge{
		Sink:  tracker,
		run:   run,
		ring:  ring,
		queue: queue,
		log:   c.log,
	}
	run.bridge = bridge
	run.tracker = tracker

	dataDir, err := runDataDir(run.id)
	if err != nil {
		c.clearRun(run)
		closeEvents()
		return connectErrorStatus(fmt.Errorf("resolve run data dir: %w", err))
	}
	engOpts, err := localRunEngineOptions(c.workflowPath, dataDir, run.id)
	if err != nil {
		c.clearRun(run)
		closeEvents()
		return connectErrorStatus(fmt.Errorf("resolve engine options: %w", err))
	}
	engOpts = append(engOpts,
		engine.WithVarOverrides(c.mergedSessionVars(run.session)),
		engine.WithAuditWriter(auditWriterFor(run.id)),
		engine.WithParallelCeiling(tunables.FromEnv().ServeAdapterConcurrency),
	)
	eng := engine.New(c.graph, c.loader, bridge, engOpts...)

	// Engine construction happens before the control bus is installed so
	// Pause/Resume/Inspect see a fully-built run.
	run.mu.Lock()
	run.ctrl = newLocalRunControl(run.id, c.graph, tracker, eng)
	run.visitsFn = eng.VisitCounts
	run.mu.Unlock()

	if err := c.writeChildRunState(run, req); err != nil {
		c.log.Warn("could not write local run state for child run", "run_id", run.id, "error", err)
	}
	if j := c.journal; j != nil {
		if _, err := j.Append(&criteriav1.SupervisionEvent_ChildRunStarted{
			ChildRunStarted: &criteriav1.ChildRunStarted{
				RunId:          run.id,
				WorkflowDigest: c.digest,
				Version:        serveAdapterVersionLabel(),
			},
		}, serveAdapterJournalAdapterType, c.graph.Name, run.session.id); err != nil {
			c.log.Warn("journal: child run start arm failed", "run_id", run.id, "error", err)
		}
	}
	emitWorkflowGraphsLocal(c.log, local, c.graph)

	// Run goroutine: real engine run, then the boundary-resume cycle loop
	// (the machinery drainLocalResumeCycles rides, minus the approval/signal
	// paths — those node kinds are compile-rejected in this mode).
	go func() {
		defer close(run.done)
		defer closeEvents()
		runErr := c.runEngineCycle(run, eng, tracker, run.ctrl)
		c.settleChildRun(run, runErr, tsSink)
	}()

	// Execute stream pump: forward queued events, keep the stream warm with
	// heartbeats, then deliver the terminal Result event.
	return c.pumpExecuteEvents(run, queue, sink)
}

// clearRun drops the run slot when Execute fails before the loop starts.
func (c *serveAdapterClient) clearRun(run *serveAdapterRun) {
	c.mu.Lock()
	if c.run == run {
		c.run = nil
	}
	c.mu.Unlock()
}

// mergedSessionVars overlays session config then secrets on the workflow's
// serve-time variable bindings (secrets win).
func (c *serveAdapterClient) mergedSessionVars(sess *serveAdapterSession) map[string]cty.Value {
	return mergeVarMaps(c.vars, sess.config, sess.secrets)
}

// mergeVarMaps shallow-merges cty value maps; later maps win.
func mergeVarMaps(maps ...map[string]cty.Value) map[string]cty.Value {
	out := map[string]cty.Value{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// auditWriterFor builds the audit writer for a child run (same audit surface
// apply uses).
func auditWriterFor(runID string) adapterhost.AuditWriter {
	path, err := auditLogPath(runID)
	if err != nil {
		return nil
	}
	return adapterhost.NewFileAuditWriter(path)
}

// writeChildRunState writes the child run's local record so it is visible in
// the child's own run store (host-of-record doctrine).
func (c *serveAdapterClient) writeChildRunState(run *serveAdapterRun, req *criteriav2.ExecuteRequest) error {
	st := newLocalRunState(run.id, c.graph.Name, "")
	st.Status = runstate.StatusRunning
	st.WorkflowHash = c.sourceHash
	return writeLocalRunState(st)
}

// engineVisits is the visits getter the checkpoint fn closes over.
func (r *serveAdapterRun) engineVisits() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.visitsFn == nil {
		return map[string]int{}
	}
	return r.visitsFn()
}

// runEngineCycle runs the engine and then the boundary-resume cycles (the
// real pause/resume path: checkpoints persist, the fresh engine resumes with
// WithResumedVars/WithResumedVisits + RunFrom). It returns when the run is
// terminal or canceled.
func (c *serveAdapterClient) runEngineCycle(run *serveAdapterRun, eng *engine.Engine, tracker *pauseTracker, ctrl *localRunControl) error {
	ctx := run.ctx
	if err := eng.Run(ctx); err != nil {
		c.log.Info("child run engine stopped", "run_id", run.id, "error", err)
		return err
	}
	for tracker.IsPaused() {
		pausedNode := tracker.PausedAt()
		// wait/approval nodes are compile-rejected in this mode, so a pause
		// here can only be a checkpoint boundary pause.
		if isApprovalOrSignalNode(ctrl.graph, pausedNode) {
			return fmt.Errorf("child run paused at wait/approval node %q; that node kind is rejected in serve-adapter mode", pausedNode)
		}
		c.log.Info("child run paused at checkpoint boundary; awaiting host resume",
			"run_id", run.id, "node", pausedNode)
		if err := awaitBoundaryRelease(ctx, ctrl); err != nil {
			return err
		}
		tracker.ClearPaused()
		dataDir, err := runDataDir(run.id)
		if err != nil {
			return err
		}
		resumeOpts, err := localRunEngineOptions(c.workflowPath, dataDir, run.id)
		if err != nil {
			return err
		}
		resumeOpts = append(resumeOpts,
			engine.WithResumedVars(eng.VarScope()),
			engine.WithResumedVisits(eng.VisitCounts()),
			engine.WithParallelCeiling(tunables.FromEnv().ServeAdapterConcurrency),
		)
		resumedEng := engine.New(ctrl.graph, c.loader, run.bridge, resumeOpts...)
		ctrl.setEngine(resumedEng)
		eng = resumedEng
		if runErr := resumedEng.RunFrom(ctx, pausedNode, 1); runErr != nil {
			return runErr
		}
		c.log.Info("child run resumed", "run_id", run.id)
	}
	return nil
}

// settleChildRun stamps the terminal result fields (before done closes), then
// tears the run record down: a cancelled run KEEPS its record and events and
// stamps status "cancelled" (acceptance 2: CloseSession cancels a live child
// run observably); a completed run removes the record and step checkpoints
// (the apply convention).
func (c *serveAdapterClient) settleChildRun(run *serveAdapterRun, runErr error, tsSink *terminalSuccessSink) {
	run.mu.Lock()
	cancelRequested := run.cancelRequested
	run.mu.Unlock()
	wasCanceled := cancelRequested || run.ctx.Err() != nil

	finalState, success, ok := tsSink.TerminalSuccess()
	terminal := serveAdapterOutcomeSuccess
	comment := ""
	if !ok || !success {
		terminal = serveAdapterOutcomeFailure
	}
	if !ok && runErr != nil {
		comment = runErr.Error()
	}
	_ = finalState

	run.mu.Lock()
	run.terminal = terminal
	run.terminalSuccess = ok && success
	if run.terminalComment != "" {
		comment = run.terminalComment
	}
	run.terminalComment = comment
	run.terminated = true
	run.mu.Unlock()

	if runErr != nil && !wasCanceled {
		c.log.Warn("child run ended with engine error", "run_id", run.id, "error", runErr)
	}

	if j := c.journal; j != nil {
		outcome := terminal
		if wasCanceled && (!ok || !success) {
			outcome = serveAdapterOutcomeCancelled
		}
		if _, err := j.Append(&criteriav1.SupervisionEvent_ChildRunTerminal{
			ChildRunTerminal: &criteriav1.ChildRunTerminal{
				RunId:   run.id,
				Outcome: outcome,
			},
		}, serveAdapterJournalAdapterType, c.graph.Name, ""); err != nil {
			c.log.Warn("journal: child run terminal arm failed", "run_id", run.id, "error", err)
		}
	}

	if wasCanceled && (!ok || !success) {
		if err := stampLocalRunStateCancelled(run.id); err != nil {
			c.log.Warn("could not stamp child run cancelled", "run_id", run.id, "error", err)
		}
		return
	}
	// Completed run: remove the local record and step checkpoints (apply
	// convention for finished runs).
	removeLocalRunState(run.id)
	RemoveStepCheckpoint(run.id)
}

// pumpExecuteEvents drains the queue onto the sink until the run goroutine
// closes done, then delivers the terminal Result event. Heartbeats keep the
// stream warm while the engine is quiet (tunable interval). A broken host
// stream never kills the child run (keepalive doctrine): the pump stops
// emitting and the run goroutine still settles.
func (c *serveAdapterClient) pumpExecuteEvents(run *serveAdapterRun, queue chan serveAdapterEvent, sink adapterhost.ExecuteEventSink) error {
	heartbeatEvery := tunables.FromEnv().HeartbeatInterval
	if heartbeatEvery <= 0 {
		heartbeatEvery = serveAdapterHeartbeatFallback
	}
	ticker := time.NewTicker(heartbeatEvery)
	defer ticker.Stop()

	streamDead := false
	emit := func(ev *criteriav2.ExecuteEvent) {
		if streamDead {
			return
		}
		if err := sink.Emit(ev); err != nil {
			streamDead = true
			c.log.Info("execute stream lost; child run continues", "run_id", run.id, "error", err)
		}
	}

	for !streamDead {
		select {
		case <-run.done:
			// Flush anything still queued so ordering holds, then terminal.
			for {
				select {
				case item := <-queue:
					emit(item.ev)
				default:
					return c.emitTerminalResult(run, sink)
				}
			}
		case <-ticker.C:
			emit(&criteriav2.ExecuteEvent{Event: &criteriav2.ExecuteEvent_Heartbeat{
				Heartbeat: &criteriav2.Heartbeat{StreamName: serveAdapterHeartbeatStream, SentAt: timestamppb.Now()},
			}})
		case item := <-queue:
			emit(item.ev)
		}
	}
	// Stream dead: let the child run settle, keep the RPC alive until done.
	<-run.done
	return nil
}

// emitTerminalResult projects the terminal state onto the stream. On cancel
// the outcome is failure with a cancellation comment; the record was stamped
// cancelled by the run goroutine's settle.
func (c *serveAdapterClient) emitTerminalResult(run *serveAdapterRun, sink adapterhost.ExecuteEventSink) error {
	run.mu.Lock()
	terminal := run.terminal
	comment := run.terminalComment
	outputs := run.outputs
	wasCanceled := run.cancelRequested || run.ctx.Err() != nil
	run.mu.Unlock()

	if terminal == "" {
		terminal = serveAdapterOutcomeFailure
		if comment == "" {
			comment = "child run canceled"
		}
	}
	if wasCanceled {
		if comment != "" {
			comment = comment + "; child run canceled"
		} else {
			comment = "child run canceled"
		}
	}

	// Engine-rendered output values are already JSON strings; embed them as
	// raw JSON so outputs_json is a clean object (no double encoding).
	raw := make(map[string]json.RawMessage, len(outputs))
	for name, value := range outputs {
		var v interface{}
		if len(value) > 0 && json.Unmarshal([]byte(value), &v) == nil {
			raw[name] = json.RawMessage(value)
		} else {
			enc, _ := json.Marshal(value)
			raw[name] = json.RawMessage(enc)
		}
	}
	outputsJSON, err := json.Marshal(raw)
	if err != nil || outputs == nil {
		outputsJSON = []byte("{}")
	}
	result := &criteriav2.ExecuteResult{
		Outcome:     terminal,
		OutputsJson: outputsJSON,
		Comment:     comment,
	}
	if err := sink.Emit(&criteriav2.ExecuteEvent{Event: &criteriav2.ExecuteEvent_Result{Result: result}}); err != nil {
		c.log.Warn("could not deliver terminal result to host", "run_id", run.id, "error", err)
	}
	c.log.Info("child run terminal", "run_id", run.id, "outcome", terminal, "canceled", wasCanceled)
	return nil
}

// stampLocalRunStateCancelled stamps the child run's local record with the
// cancelled status (U4 runstate vocabulary) and keeps the record on disk.
func stampLocalRunStateCancelled(runID string) error {
	st, err := readLocalRunState(runID)
	if err != nil {
		return err
	}
	st.Status = runstate.StatusCancelled
	return writeLocalRunState(st)
}

// constants for the Execute stream projection.
const (
	serveAdapterHeartbeatStream   = "execute"
	serveAdapterHeartbeatFallback = 15 * time.Second

	// serveAdapterJournalAdapterType marks peer journal arms emitted by this
	// mode (ADR-0008 child role, workflow.v1 capability prefix).
	serveAdapterJournalAdapterType = "workflow.v1"

	// serveAdapterOutcomeCancelled is the journal vocabulary for a child run
	// torn down mid-flight (CloseSession teardown or Control cancel; the
	// Execute stream itself still closes with the failure outcome).
	serveAdapterOutcomeCancelled = "cancelled"
)
