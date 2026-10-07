package cli

// The adapter v2 AdapterService contract fronting the served workflow
// (ADR-0008 D2): sessions are the host's view of an adapter session; Execute
// opens ONE child run process-wide and streams workflow.v1-prefixed adapter
// events; Pause/Resume delegate to the engine's real pause machinery through
// the same in-process control bus apply uses (localRunControl, CRI-255)
// without its loopback listener. Control verbs arrive over the peer
// phone-home Control stream (internal/peer, KB-94 U1).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	structpb "google.golang.org/protobuf/types/known/structpb"
	timestamppb "google.golang.org/protobuf/types/known/timestamppb"

	criteriav2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/peer"
	"github.com/brokenbots/criteria/internal/tunables"
	"github.com/brokenbots/criteria/workflow"

	"github.com/zclconf/go-cty/cty"
)

// serveAdapterOutcomeSuccess / serveAdapterOutcomeFailure are the terminal
// mappings Execute guarantees: the graph's terminal StateNode success/failure
// projection (ADR-0008 D2: "terminal state maps to ExecuteResult.outcome").
const (
	serveAdapterOutcomeSuccess = "success"
	serveAdapterOutcomeFailure = "failure"
)

// serveAdapterCapability is the well-known capability string identifying this
// mode on the Info handshake.
const serveAdapterCapability = "workflow.v1"

// closeSessionSettleTimeout bounds how long CloseSession waits for the
// cancelled child run goroutine to settle (its terminal run record is stamped
// in the run goroutine's teardown) before failing closed instead of exiting
// with an unstamped record. Engine teardown on cancel is prompt, so this is
// generous bookkeeping headroom only.
const closeSessionSettleTimeout = 30 * time.Second

// errSessionUnknownConnect is the sentinel every session-scoped verb returns
// for an unknown session id; connectErrorStatus maps it to CodeNotFound.
var errSessionUnknownConnect = errors.New("unknown session")

// errChildRunStillStarting is the sentinel control verbs return when the
// in-flight child run is published but its control bus is not installed yet
// (the openChildRun → driveChildRun build window). The verbs fail closed with
// CodeFailedPrecondition instead of dereferencing a nil ctrl: grpc-go does
// not recover handler panics and a host Pause/Resume landing in that window
// must not kill the served adapter process.
var errChildRunStillStarting = errors.New("child run is still starting")

// errSessionIDRequired is the missing session_id open failure;
// connectErrorStatus maps it to CodeInvalidArgument.
var errSessionIDRequired = errors.New("session_id is required")

// errSessionAlreadyExists marks a duplicate OpenSession session id;
// connectErrorStatus maps it to CodeAlreadyExists.
type errSessionAlreadyExists struct {
	sessionID string
}

func (e *errSessionAlreadyExists) Error() string {
	return fmt.Sprintf("session %q already exists", e.sessionID)
}

// ErrChildRunInFlight is the typed fail-closed error returned when Execute is
// called while another child run is still executing (ADR-0008 acceptance 2:
// re-Execute while in-flight = typed error).
type ErrChildRunInFlight struct {
	RunID    string
	Workflow string
}

func (e *ErrChildRunInFlight) Error() string {
	return fmt.Sprintf("child run %q is still in flight for workflow %q; one child run executes at a time (cancel it via CloseSession or the Control cancel arm before re-executing)", e.RunID, e.Workflow)
}

// serveAdapterClientOptions carries the compiled workflow and its serve-time
// bindings for newServeAdapterClient.
type serveAdapterClientOptions struct {
	graph        *workflow.FSMGraph
	loader       *adapterhost.DefaultLoader
	digest       string
	sourceHash   string
	workflowPath string
	vars         map[string]cty.Value
	journal      *peer.EventJournal
	log          *slog.Logger
	baseCtx      context.Context
}

// serveAdapterClient implements the full v2 surface (adapterhost.Client) and
// the peer Control cancel arm (peer.ChildRunCanceler) for one served workflow.
type serveAdapterClient struct {
	graph        *workflow.FSMGraph
	loader       *adapterhost.DefaultLoader
	digest       string
	sourceHash   string
	workflowPath string
	vars         map[string]cty.Value
	journal      *peer.EventJournal
	log          *slog.Logger
	baseCtx      context.Context

	mu       sync.Mutex
	exitFn   func()
	sessions map[string]*serveAdapterSession
	run      *serveAdapterRun
}

// serveAdapterSession is one host-opened adapter session.
type serveAdapterSession struct {
	id      string
	config  map[string]cty.Value
	secrets map[string]cty.Value
	created time.Time

	// actMu guards lastActivity, bumped from the bridge's event-emission path
	// and the run's settle (whichever goroutine observes the latest activity)
	// so Inspect reports the session's real latest activity, not creation.
	actMu        sync.Mutex
	lastActivity time.Time

	// ring buffers the session's log lines for Log replay/tail. Created in
	// OpenSession so a Log stream may attach before Execute ever runs.
	ring *serveAdapterLogRing

	// pendingRestore parks a validated snapshot envelope delivered by
	// Restore on THIS session; the next Execute consumes it exactly once
	// (openChildRun pops it) to restart the run from the snapshot's paused
	// node. Guarded by the client's mutex, like the sessions map.
	pendingRestore *serveAdapterSnapshotV1
}

// markActivity records the latest activity observed for the session; a
// monotonic max so late stamps never regress the recorded value.
func (s *serveAdapterSession) markActivity(at time.Time) {
	if s == nil {
		return
	}
	s.actMu.Lock()
	if at.After(s.lastActivity) {
		s.lastActivity = at
	}
	s.actMu.Unlock()
}

// lastActivityTime returns the latest recorded activity, falling back to
// the session's creation time when nothing has been recorded yet.
func (s *serveAdapterSession) lastActivityTime() time.Time {
	s.actMu.Lock()
	defer s.actMu.Unlock()
	if s.lastActivity.IsZero() {
		return s.created
	}
	return s.lastActivity
}

// serveAdapterRun tracks the single in-flight child run the v2 surface drives.
type serveAdapterRun struct {
	id      string
	session *serveAdapterSession

	// restore, when non-nil, parks the consumed snapshot envelope until
	// buildChildEngine seeds the fresh run from it and runEngineCycle
	// enters via RunFrom instead of Run. Written by openChildRun under the
	// client lock and consumed on the run goroutine; never read after nil-ing.
	restore *serveAdapterSnapshotV1

	mu sync.Mutex
	// ctrl is the in-process pause/resume bus wrapping the live engine
	// (localRunControl without its loopback listener).
	ctrl         *localRunControl
	tracker      *pauseTracker
	bridge       *serveAdapterBridge
	visitsFn     func() map[string]int
	currentStep  string
	pausedNode   string
	lastActivity time.Time
	outputs      map[string]string
	// terminal fields are written by the run goroutine before done closes.
	terminal        string
	terminalSuccess bool
	terminalComment string
	terminated      bool
	cancelRequested bool

	cancel context.CancelFunc
	ctx    context.Context
	done   chan struct{}
}

// Compile-time proof the client satisfies both contract surfaces.
var (
	_ adapterhost.Client    = (*serveAdapterClient)(nil)
	_ peer.ChildRunCanceler = (*serveAdapterClient)(nil)
)

func newServeAdapterClient(opts *serveAdapterClientOptions) *serveAdapterClient {
	return &serveAdapterClient{
		graph:        opts.graph,
		loader:       opts.loader,
		digest:       opts.digest,
		sourceHash:   opts.sourceHash,
		workflowPath: opts.workflowPath,
		vars:         opts.vars,
		journal:      opts.journal,
		log:          opts.log,
		baseCtx:      opts.baseCtx,
		sessions:     make(map[string]*serveAdapterSession),
	}
}

// setExit arms the process-exit signal used by CloseSession teardown
// (peer.Server.RequestExit).
func (c *serveAdapterClient) setExit(fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.exitFn = fn
}

// Info answers the Info handshake from the compiled workflow: config schema
// from variable declarations, output schema from the terminal output
// projection, capabilities include workflow.v1 and the outcomes vocabulary is
// the graph's declared outcomes (ADR-0008 D1; the vocabulary rides the
// description because the v2 Info message has no dedicated outcomes field).
func (c *serveAdapterClient) Info(_ context.Context, _ *criteriav2.InfoRequest) (*criteriav2.InfoResponse, error) {
	outcomes := serveAdapterOutcomes(c.graph)
	return &criteriav2.InfoResponse{
		Name:         c.graph.Name,
		Version:      serveAdapterVersionLabel(),
		Description:  fmt.Sprintf("criteria workflow adapter (ADR-0008 child role): serves workflow %q compiled from %s; outcomes vocabulary: %s", c.graph.Name, c.digest, strings.Join(outcomes, ", ")),
		Capabilities: []string{serveAdapterCapability},
		// supported_features: only the controls this mode really implements.
		// Snapshot/Restore ride the engine's real pause/checkpoint machinery
		// (serveadapter_snapshot.go): Snapshot captures the durable
		// checkpoint boundary state, Restore replays it through RunFrom.
		SupportedFeatures: []string{"pause", "resume", "inspect", "snapshot", "restore"},
		ConfigSchema:      serveAdapterConfigSchema(c.graph),
		OutputSchema:      serveAdapterOutputSchema(c.graph),
	}, nil
}

// OpenSession records a host-opened session. Config entries are coerced
// against the workflow's variable declarations exactly like --var values;
// unknown variable names, or values that do not coerce, fail the open (fail
// closed rather than silently dropping a mistyped binding).
func (c *serveAdapterClient) OpenSession(_ context.Context, req *criteriav2.OpenSessionRequest) (*criteriav2.OpenSessionResponse, error) {
	id := strings.TrimSpace(req.GetSessionId())
	if id == "" {
		return nil, connectErrorStatus(errSessionIDRequired)
	}
	config, err := coerceSessionConfig(c.graph, req.GetConfig())
	if err != nil {
		return nil, connectErrorStatus(fmt.Errorf("session config: %w", err))
	}
	secrets, err := coerceSessionConfig(c.graph, req.GetSecrets())
	if err != nil {
		return nil, connectErrorStatus(fmt.Errorf("session secrets: %w", err))
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.sessions[id]; exists {
		return nil, connectErrorStatus(&errSessionAlreadyExists{sessionID: id})
	}
	c.sessions[id] = &serveAdapterSession{
		id:      id,
		config:  config,
		secrets: secrets,
		created: time.Now().UTC(),
		ring:    newServeAdapterLogRing(serveAdapterLogRingCapacity),
	}
	return &criteriav2.OpenSessionResponse{}, nil
}

// coerceSessionConfig turns the wire's string map into cty values coerced
// against the workflow's variable declarations (mirrors --var semantics).
func coerceSessionConfig(graph *workflow.FSMGraph, m map[string]string) (map[string]cty.Value, error) {
	if len(m) == 0 {
		return nil, nil
	}
	out := make(map[string]cty.Value, len(m))
	for _, name := range sortedStringKeys(m) {
		v, ok := graph.Variables[name]
		if !ok {
			return nil, fmt.Errorf("unknown workflow variable %q (declared variables: %s)", name, strings.Join(sortedStringKeys(graph.Variables), ", "))
		}
		cv, err := workflow.CoerceStringToCty(m[name], v.Type)
		if err != nil {
			return nil, fmt.Errorf("variable %q: %w", name, err)
		}
		out[name] = cv
	}
	return out, nil
}

// Execute opens ONE child run process-wide (ADR-0008 D2) and streams the
// run's node lifecycle + logs as workflow.v1-prefixed adapter events until
// the terminal state maps to ExecuteResult.outcome + outputs_json (from the
// child's projected output map). Re-Execute while in-flight returns the
// typed ErrChildRunInFlight error (fail closed). The child run anchors to the
// client's base context — the process signals context, per the keepalive
// doctrine: a host disconnect mid-Execute must not orphan-kill the run;
// CloseSession or the Control cancel arm is the explicit teardown path.
func (c *serveAdapterClient) Execute(_ context.Context, req *criteriav2.ExecuteRequest, sink adapterhost.ExecuteEventSink) error {
	c.mu.Lock()
	if c.run != nil {
		inFlight := &ErrChildRunInFlight{RunID: c.run.id, Workflow: c.graph.Name}
		c.mu.Unlock()
		return connectErrorStatus(inFlight)
	}
	sess, ok := c.sessions[req.GetSessionId()]
	if !ok {
		c.mu.Unlock()
		return connectErrorStatus(errSessionUnknownConnect)
	}
	c.mu.Unlock()

	run, err := c.openChildRun(sess)
	if err != nil {
		return connectErrorStatus(err)
	}
	c.log.Info("execute: opening child run",
		"run_id", run.id, "workflow", c.graph.Name, "session_id", sess.id,
		"step", req.GetStepName(),
		"concurrency_budget", tunables.FromEnv().ServeAdapterConcurrency)

	// driveChildRun runs the engine on a separate goroutine and streams this
	// call's ExecuteEvent sink; it returns only after a terminal state (or
	// cancel) has been projected.
	if err := c.driveChildRun(run, sink); err != nil {
		return err
	}
	c.clearRun(run)
	return nil
}

// CancelChildRun implements peer.ChildRunCanceler: the Control CancelChildRun
// arm tears the child run down through the engine's real stop machinery (the
// engine turns context cancellation into a real terminal run-failure event).
// An empty run id targets "the current one": the child hosts at most one
// live run per session, and the parent's teardown sends the empty id to
// cover a child run it has not observed on its tracker yet (KB-95 wire
// contract, KB-96 teardown evidence).
func (c *serveAdapterClient) CancelChildRun(runID string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.run == nil || (runID != "" && c.run.id != runID) {
		return "", false
	}
	return c.requestRunCancelLocked(c.run.id), true
}

// requestRunCancelLocked cancels the named live run if it is still active.
// Caller holds c.mu. The matched run id is returned for the journal trail.
func (c *serveAdapterClient) requestRunCancelLocked(runID string) string {
	run := c.run
	if run == nil || run.id != runID {
		return ""
	}
	run.mu.Lock()
	already := run.cancelRequested
	run.cancelRequested = true
	run.mu.Unlock()
	if already {
		return run.id
	}
	c.log.Info("child run cancel requested", "run_id", run.id, "workflow", c.graph.Name)
	run.cancel()
	return run.id
}

// Log streams the child run's output lines for a session: it replays the
// session's bounded ring and then tails until ctx is done (the host's
// stream detach) — the convention the noop fixture establishes (Log stays
// open, RunLogHeartbeat keeps it warm).
func (c *serveAdapterClient) Log(ctx context.Context, req *criteriav2.LogRequest, sink adapterhost.LogEventSink) error {
	c.mu.Lock()
	sess, ok := c.sessions[req.GetSessionId()]
	if !ok {
		c.mu.Unlock()
		return connectErrorStatus(errSessionUnknownConnect)
	}
	ring := sess.ring
	c.mu.Unlock()
	if ring == nil {
		return nil
	}
	sender := &logSinkSender{session: sess.id, sink: sink}
	return ring.Tail(ctx, sender)
}

// logSinkSender adapts adapterhost.LogEventSink to heartbeatutil's sender.
type logSinkSender struct {
	session string
	sink    adapterhost.LogEventSink
}

func (s *logSinkSender) Send(ev *criteriav2.LogEvent) error {
	return s.sink.Emit(ev)
}

// Prompt fails closed: an ADR-0006 agent prompt cannot be injected into the
// served workflow's steps from the host side in this mode — the child run's
// steps run their own agent sessions under the child's own policy, and the
// parent-adjacent prompt route is out of scope for v0 (D6 adjacency).
func (c *serveAdapterClient) Prompt(_ context.Context, req *adapterhost.PromptRequest) (*adapterhost.PromptResponse, error) {
	c.mu.Lock()
	_, ok := c.sessions[req.SessionID]
	c.mu.Unlock()
	if !ok {
		return nil, connectErrorStatus(errSessionUnknownConnect)
	}
	return &adapterhost.PromptResponse{
		Accepted: false,
		Detail:   "prompt injection is not part of the serve-adapter workflow surface; prompts ride the child run's own step sessions",
	}, nil
}

// Permissions fails closed: the served workflow's child adapter sessions run
// under the child's own permission policy (host-of-record doctrine), and no
// host-driven permission surface exists for this mode in v0 (ADR-0008 D6
// non-goal adjacency). The request channel is drained so the host never
// deadlocks, then the verb reports unsupported.
func (c *serveAdapterClient) Permissions(_ context.Context, requests <-chan *criteriav2.PermissionEvent) error {
	for range requests {
	}
	return connectErrorStatus(errors.New("permission gating is not part of the serve-adapter workflow surface; child adapter permissions follow the child run's own local policy"))
}

// Pause delegates to the child run's real pause machinery (engine.RequestPause
// → boundary drain → durable checkpoint + adapter session snapshots) through
// the control bus. A session without an in-flight child run fails closed, as
// does a run still in its build window (between openChildRun publishing the
// run and driveChildRun installing its control bus): grpc-go does not recover
// handler panics and a host Pause landing there must not kill the process.
func (c *serveAdapterClient) Pause(ctx context.Context, req *criteriav2.PauseRequest) (*criteriav2.PauseResponse, error) {
	run, err := c.sessionRun(req.GetSessionId(), "pause")
	if err != nil {
		return nil, connectErrorStatus(err)
	}
	if run == nil {
		return nil, connectErrorStatus(errors.New("no in-flight child run to pause"))
	}
	run.mu.Lock()
	ctrl := run.ctrl
	run.mu.Unlock()
	if ctrl == nil {
		return nil, connectErrorStatus(errChildRunStillStarting)
	}
	if err := ctrl.pause(ctx); err != nil {
		return nil, connectErrorStatus(err)
	}
	c.refreshPausedNode(run)
	return &criteriav2.PauseResponse{}, nil
}

// Resume releases a checkpoint-boundary pause the same machinery landed; the
// run loop's boundary-resume cycle then drives the fresh engine (real resume:
// WithResumedVars/WithResumedVisits + RunFrom) to the next pause point or the
// terminal state. The response returns when the resume was ACCEPTED, not when
// the run completes (matching ResumeRun semantics in local mode).
func (c *serveAdapterClient) Resume(_ context.Context, req *criteriav2.ResumeRequest) (*criteriav2.ResumeResponse, error) {
	run, err := c.sessionRun(req.GetSessionId(), "resume")
	if err != nil {
		return nil, connectErrorStatus(err)
	}
	if run == nil {
		return nil, connectErrorStatus(errors.New("no in-flight child run to resume"))
	}
	run.mu.Lock()
	ctrl := run.ctrl
	run.mu.Unlock()
	if ctrl == nil {
		return nil, connectErrorStatus(errChildRunStillStarting)
	}
	if node := ctrl.tracker.PausedAt(); node == "" {
		return nil, connectErrorStatus(errors.New("child run is not paused at a node"))
	}
	if err := ctrl.resume(); err != nil {
		// Wait/approval nodes are gate-rejected at compile in this mode, so
		// the "deliver via ResolveResume" branch of the bus is unreachable;
		// any residual message names the paused node, which is the truth.
		return nil, connectErrorStatus(err)
	}
	c.refreshPausedNode(run)
	return &criteriav2.ResumeResponse{}, nil
}

// Snapshot and Restore delegate to the engine's real pause/checkpoint
// machinery; they live in serveadapter_snapshot.go together with the
// envelope codec and the parked-restore consumption path.

// Inspect surfaces the child run's live position through the real machinery:
// the current step from the run's live engine state, the paused node when a
// pause landed, and the child run id (the child's local run-store record).
func (c *serveAdapterClient) Inspect(_ context.Context, req *criteriav2.InspectRequest) (*criteriav2.InspectResponse, error) {
	run, err := c.sessionRun(req.GetSessionId(), "inspect")
	if err != nil {
		return nil, connectErrorStatus(err)
	}
	c.mu.Lock()
	sess, ok := c.sessions[req.GetSessionId()]
	c.mu.Unlock()
	if !ok {
		return nil, connectErrorStatus(errSessionUnknownConnect)
	}

	resp := &criteriav2.InspectResponse{
		LastActivityAt: sess.lastActivityTimestamp(),
	}
	if run != nil {
		run.mu.Lock()
		resp.CurrentStep = run.currentStep
		if run.pausedNode != "" {
			resp.CurrentStep = run.pausedNode
		}
		if !run.lastActivity.IsZero() {
			resp.LastActivityAt = timestamppb.New(run.lastActivity)
		}
		runID := run.id
		terminal := run.terminal
		run.mu.Unlock()
		resp.Fields = append(resp.Fields,
			&criteriav2.InspectField{Key: "run_id", Label: "Child run id", Value: structpbStringValue(runID)},
			&criteriav2.InspectField{Key: "workflow", Label: "Workflow", Value: structpbStringValue(c.graph.Name)},
			&criteriav2.InspectField{Key: "digest", Label: "Workflow digest", Value: structpbStringValue(c.digest)},
		)
		if terminal != "" {
			resp.Fields = append(resp.Fields, &criteriav2.InspectField{Key: "terminal", Label: "Terminal state", Value: structpbStringValue(terminal)})
		}
	}
	return resp, nil
}

// CloseSession is teardown: cancel the in-flight child run via the engine's
// real stop machinery, wait for the run goroutine to settle (the cancelled
// run record is stamped before done closes), then arm the process-exit signal
// — the peer Serve loop exits after the host's teardown round-trip completes
// for this RPC.
func (c *serveAdapterClient) CloseSession(ctx context.Context, req *criteriav2.CloseSessionRequest) (*criteriav2.CloseSessionResponse, error) {
	c.mu.Lock()
	sess, ok := c.sessions[req.GetSessionId()]
	if !ok {
		c.mu.Unlock()
		return nil, connectErrorStatus(errSessionUnknownConnect)
	}
	run := c.run
	var done chan struct{}
	if run != nil && run.session != nil && run.session.id != sess.id {
		c.mu.Unlock()
		return nil, connectErrorStatus(&errChildRunNotOwnedBySession{runID: run.id, owner: run.session.id})
	}
	if run != nil {
		c.requestRunCancelLocked(run.id)
		done = run.done
	}
	delete(c.sessions, req.GetSessionId())
	c.mu.Unlock()

	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return nil, connectErrorStatus(ctx.Err())
		case <-time.After(closeSessionSettleTimeout):
			return nil, connectErrorStatus(fmt.Errorf("child run did not settle within %s after cancel", closeSessionSettleTimeout))
		}
	}

	c.mu.Lock()
	exit := c.exitFn
	c.mu.Unlock()
	if exit != nil {
		c.log.Info("close-session teardown complete; exiting", "session_id", req.GetSessionId())
		// exit() arms the phone-home watcher while this RPC is still in
		// flight: the watcher drains with a bounded GracefulStop, which
		// completes pending RPCs — this response included — before closing
		// transports, so the host receives the successful response before
		// the teardown takes effect (the peerServerStopGrace force-close
		// fallback keeps a lingering stream from hanging the exit).
		exit()
	}
	return &criteriav2.CloseSessionResponse{}, nil
}

// sessionRun resolves the control-plane run handle for a control verb. The
// session itself must exist (unknown sessions fail closed as CodeNotFound),
// and — same invariant CloseSession enforces — a session may only operate on
// the run IT opened: the single in-flight run is bound to its opening session,
// so a request naming a different session gets the typed
// errChildRunNotOwnedBySession (FailedPrecondition over the wire) and nil
// (fail closed), never a handle silently transplanted across sessions.
// (nil, nil) means the session is legitimate but has no in-flight run.
func (c *serveAdapterClient) sessionRun(sessionID, verb string) (*serveAdapterRun, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.sessions[sessionID]; !ok {
		c.log.Warn("control verb rejected: unknown session", "verb", verb, "session_id", sessionID)
		return nil, errSessionUnknownConnect
	}
	if c.run != nil && c.run.session != nil && c.run.session.id != sessionID {
		c.log.Warn("control verb rejected: session does not own the in-flight child run",
			"verb", verb, "session_id", sessionID, "run_session_id", c.run.session.id)
		return nil, &errChildRunNotOwnedBySession{runID: c.run.id, owner: c.run.session.id}
	}
	return c.run, nil
}

// refreshPausedNode syncs the run's cached paused-node from the tracker.
func (c *serveAdapterClient) refreshPausedNode(run *serveAdapterRun) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.run != run {
		return
	}
	run.mu.Lock()
	if run.ctrl != nil {
		run.pausedNode = run.ctrl.tracker.PausedAt()
	}
	run.mu.Unlock()
}

// structpbStringValue wraps a string for InspectField payloads.
func structpbStringValue(s string) *structpb.Value {
	v, err := structpb.NewValue(s)
	if err != nil {
		return nil
	}
	return v
}

// sess.lastActivityTimestamp keeps the timestamppb conversion next to the
// session type; the session's tracked activity (bumped from the bridge event
// path and the run settle) is the truth, falling back to creation.
func (s *serveAdapterSession) lastActivityTimestamp() *timestamppb.Timestamp {
	return timestamppb.New(s.lastActivityTime())
}
