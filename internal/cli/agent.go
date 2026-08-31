package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/engine"
	"github.com/brokenbots/criteria/internal/run"
	servertrans "github.com/brokenbots/criteria/internal/transport/server"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	"github.com/brokenbots/criteria/workflow"
	workflowversion "github.com/brokenbots/criteria/workflow/version"
)

// agentOptions configures the long-lived agent mode.
type agentOptions struct {
	serverURL        string
	name             string
	version          string
	codec            string
	tlsMode          string
	tlsCA            string
	tlsCert          string
	tlsKey           string
	varOverrides     []string // raw "key=value" pairs from --var flags
	varFiles         []string // paths from --var-file flags
	warnsAsErrors    bool     // refuse to run when an adapter schema can't be verified
	allowUnsigned    bool     // skip adapter signature verification
	subworkflowRoots []string // restrict subworkflow source resolution to these roots
	log              *slog.Logger
}

// NewAgentCmd returns the `criteria agent` command.
func NewAgentCmd() *cobra.Command {
	var opts agentOptions

	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Run as a long-lived agent waiting for server workflow assignments",
		Long: `The agent command connects to a central Criteria server, registers once,
and waits in a queue for workflow assignments. Assignments are executed one at a
time; progress, cancellation, and resume commands are relayed over the server
stream. The agent reconnects automatically after transport loss and continues
processing later assignments without restarting.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			return runAgent(ctx, &opts)
		},
	}

	cmd.Flags().StringVar(&opts.serverURL, "server", envOrDefault("CRITERIA_SERVER_URL", ""), "server base URL (required for agent mode)")
	cmd.Flags().StringVar(&opts.name, "name", envOrDefault("CRITERIA_NAME", ""), "Agent name (defaults to hostname)")
	cmd.Flags().StringVar(&opts.version, "version", envOrDefault("CRITERIA_AGENT_VERSION", ""), "Agent version reported during registration (defaults to binary version)")
	cmd.Flags().StringVar(&opts.codec, "server-codec", envOrDefault("CRITERIA_SERVER_CODEC", "proto"), "Connect codec: proto or json")
	cmd.Flags().StringVar(&opts.tlsMode, "server-tls", envOrDefault("CRITERIA_SERVER_TLS", ""), "TLS mode: disable|tls|mtls")
	cmd.Flags().StringVar(&opts.tlsCA, "tls-ca", envOrDefault("CRITERIA_TLS_CA", ""), "Path to CA bundle PEM")
	cmd.Flags().StringVar(&opts.tlsCert, "tls-cert", envOrDefault("CRITERIA_TLS_CERT", ""), "Path to client cert PEM")
	cmd.Flags().StringVar(&opts.tlsKey, "tls-key", envOrDefault("CRITERIA_TLS_KEY", ""), "Path to client key PEM")
	cmd.Flags().StringArrayVar(&opts.varOverrides, "var", nil, "Override a workflow variable: key=value (repeatable)")
	cmd.Flags().StringArrayVar(&opts.varFiles, "var-file", nil, "Load variable overrides from a .chcl, .hcl, or .json file (repeatable; --var takes precedence)")
	cmd.Flags().StringArrayVar(&opts.subworkflowRoots, "subworkflow-root", nil, "Restrict subworkflow source resolution to this root path (repeatable; empty = no restriction)")
	cmd.Flags().BoolVar(&opts.warnsAsErrors, "warnings-as-errors", false, "Refuse to run when a warning is raised")
	cmd.Flags().BoolVar(&opts.allowUnsigned, "allow-unsigned", false, "Skip adapter signature verification")
	return cmd
}

func agentClientOptions(opts *agentOptions) servertrans.Options {
	return servertrans.Options{
		Codec:    servertrans.Codec(opts.codec),
		TLSMode:  servertrans.TLSMode(opts.tlsMode),
		CAFile:   opts.tlsCA,
		CertFile: opts.tlsCert,
		KeyFile:  opts.tlsKey,
	}
}

// queuedAssignment pairs a workflow assignment with the server client that
// owns the run. Fresh assignments use the long-lived agent client; recovered
// runs use a temporary client authenticated with the persisted credentials of
// the crashed incarnation.
type queuedAssignment struct {
	assignment *pb.WorkflowAssignment
	client     *servertrans.Client
}

// activeRun tracks the currently executing assignment, pending queued
// assignments, and the channels used to route control messages to the worker.
// All mutable access is protected by activeRun.mu.
type activeRun struct {
	mu        sync.Mutex
	runID     string
	cancel    context.CancelFunc
	resumeCh  chan *pb.ResumeRun
	done      chan struct{}
	pending   []*queuedAssignment
	completed map[string]struct{} // terminal run ids observed by this process
}

// agentRunStatus values persisted in localRunState.Status.
const (
	agentRunStatusRunning  = "running"
	agentRunStatusTerminal = "terminal"
	agentRunStatusPending  = "pending"
	agentRunStatusPaused   = "paused"
)

func (a *activeRun) isIdle() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.runID == ""
}

func (a *activeRun) activeRunID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.runID
}

func (a *activeRun) doneCh() <-chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.done
}

func (a *activeRun) isKnown(runID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if runID == "" {
		return false
	}
	if runID == a.runID {
		return true
	}
	for _, p := range a.pending {
		if p.assignment.GetRunId() == runID {
			return true
		}
	}
	_, ok := a.completed[runID]
	return ok
}

func (a *activeRun) markCompleted(runID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.completed == nil {
		a.completed = make(map[string]struct{})
	}
	a.completed[runID] = struct{}{}
}

// shutdownSuppressingSink wraps a *run.Sink and drops OnRunFailed plus
// error-bearing OnStepOutcome callbacks when the agent is shutting down.
// Without this, a container exit mid-run would publish a terminal RunFailed
// event and prevent crash recovery from resuming the run.
type shutdownSuppressingSink struct {
	*run.Sink
	agentCtx context.Context
}

func (s *shutdownSuppressingSink) OnRunFailed(reason, step string) {
	if s.agentCtx.Err() != nil {
		return
	}
	s.Sink.OnRunFailed(reason, step)
}

func (s *shutdownSuppressingSink) OnStepOutcome(step, outcome string, duration time.Duration, err error) {
	if err != nil && s.agentCtx.Err() != nil {
		return
	}
	s.Sink.OnStepOutcome(step, outcome, duration, err)
}

func (a *activeRun) enqueue(assignment *pb.WorkflowAssignment, client *servertrans.Client) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pending = append(a.pending, &queuedAssignment{assignment: assignment, client: client})
}

func (a *activeRun) nextPending() *queuedAssignment {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.pending) == 0 {
		return nil
	}
	qa := a.pending[0]
	a.pending = a.pending[1:]
	return qa
}

func (a *activeRun) beginRun(runID string, cancel context.CancelFunc, resumeCh chan *pb.ResumeRun, done chan struct{}) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.runID = runID
	a.cancel = cancel
	a.resumeCh = resumeCh
	a.done = done
}

func (a *activeRun) finishRun() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.done != nil {
		close(a.done)
	}
	a.runID = ""
	a.cancel = nil
	a.resumeCh = nil
	a.done = nil
}

func (a *activeRun) cancelActive(runID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if runID == a.runID && a.cancel != nil {
		a.cancel()
		return true
	}
	return false
}

func (a *activeRun) resumeActive(msg *pb.ResumeRun) (matched bool, activeID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	activeID = a.runID
	if msg == nil || msg.RunId != a.runID || a.resumeCh == nil {
		return false, activeID
	}
	select {
	case a.resumeCh <- msg:
	default:
	}
	return true, activeID
}

func (a *activeRun) shutdown() <-chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		a.cancel()
	}
	return a.done
}

func (a *activeRun) startNext(ctx context.Context, log *slog.Logger, defaultClient *servertrans.Client, opts *agentOptions) {
	qa := a.nextPending()
	if qa == nil {
		return
	}
	client := qa.client
	if client == nil {
		client = defaultClient
	}

	runCtx, cancel := context.WithCancel(ctx)
	resumeCh := make(chan *pb.ResumeRun, 1)
	done := make(chan struct{})
	a.beginRun(qa.assignment.GetRunId(), cancel, resumeCh, done)

	log.Info("accepted assignment", "run_id", qa.assignment.GetRunId(), "workflow", qa.assignment.GetWorkflowName())
	go func(q *queuedAssignment) {
		defer a.finishRun()
		if client != defaultClient {
			defer client.Close()
		}
		if err := executeAgentAssignment(ctx, runCtx, log, client, q.assignment, opts, resumeCh, a.markCompleted); err != nil {
			log.Error("assignment execution failed", "run_id", q.assignment.GetRunId(), "error", err)
		} else {
			log.Info("assignment completed", "run_id", q.assignment.GetRunId())
		}
		// Remember terminal runs for the remainder of this process so duplicate
		// deliveries are declined without re-executing the workflow. Use the
		// agent context so that run-level cancellation still marks the run as
		// completed; only agent shutdown should leave recovery state intact.
		if ctx.Err() == nil {
			a.markCompleted(q.assignment.GetRunId())
		}
	}(qa)
}

// agentLoop binds the agent event loop to its mutable state and dependencies.
type agentLoop struct {
	ctx    context.Context
	log    *slog.Logger
	client *servertrans.Client
	opts   *agentOptions
	active *activeRun
}

func (l *agentLoop) handleCancel(runID string) {
	if l.active.cancelActive(runID) {
		l.log.Info("received run.cancel control", "run_id", runID)
		return
	}
	l.log.Warn("received run.cancel for inactive run", "run_id", runID, "active_run_id", l.active.activeRunID())
}

func (l *agentLoop) handleResume(msg *pb.ResumeRun) {
	ok, activeID := l.active.resumeActive(msg)
	if ok {
		l.log.Info("routing resume to active run", "run_id", msg.RunId, "signal", msg.Signal, "active_run_id", activeID)
		return
	}
	l.log.Warn("received resume for inactive run", "run_id", msg.GetRunId(), "active_run_id", activeID)
}

func (l *agentLoop) handleAssignment(assignment *pb.WorkflowAssignment) {
	runID := assignment.GetRunId()
	if l.active.isKnown(runID) {
		l.log.Info("declining duplicate assignment",
			"run_id", runID,
			"active_run_id", l.active.activeRunID())
		return
	}
	l.active.enqueue(assignment, l.client)
	if l.active.isIdle() {
		l.active.startNext(l.ctx, l.log, l.client, l.opts)
		return
	}
	l.log.Info("assignment queued behind active run",
		"active_run_id", l.active.activeRunID(),
		"queued_run_id", runID)
}

func (l *agentLoop) handleDone() {
	l.active.startNext(l.ctx, l.log, l.client, l.opts)
}

func (l *agentLoop) shutdown() {
	l.log.Info("agent shutting down")
	if done := l.active.shutdown(); done != nil {
		<-done
	}
}

// setupAgentClient validates options, builds the transport client, registers
// once with the server, and starts the control and heartbeat streams.
func setupAgentClient(ctx context.Context, opts *agentOptions, log *slog.Logger) (*servertrans.Client, error) {
	copts := agentClientOptions(opts)
	client, err := servertrans.NewClient(opts.serverURL, log, copts)
	if err != nil {
		return nil, err
	}

	hostname, _ := os.Hostname()
	name := opts.name
	if name == "" {
		name = hostname
	}
	version := opts.version
	if version == "" {
		version = workflowversion.Current().Display
	}

	if err := client.Register(ctx, name, hostname, version); err != nil {
		client.Close()
		return nil, fmt.Errorf("register: %w", err)
	}
	if err := client.StartControl(ctx); err != nil {
		client.Close()
		return nil, fmt.Errorf("control stream: %w", err)
	}
	client.StartHeartbeat(ctx, 10*time.Second)
	log.Info("agent registered and waiting for assignments",
		"name", name,
		"version", version,
		"criteria_id", client.CriteriaID(),
		"server", opts.serverURL)
	return client, nil
}

// runAgent starts the agent loop: register once, start control/heartbeat
// streams, then process workflow assignments sequentially until ctx is done.
func runAgent(ctx context.Context, opts *agentOptions) error {
	log := opts.log
	if log == nil {
		log = newApplyLogger()
	}
	if strings.TrimSpace(opts.serverURL) == "" {
		return errors.New("--server is required for agent mode")
	}

	client, err := setupAgentClient(ctx, opts, log)
	if err != nil {
		return err
	}
	defer client.Close()

	loop := &agentLoop{
		ctx:    ctx,
		log:    log,
		client: client,
		opts:   opts,
		active: &activeRun{},
	}

	// Before accepting new assignments, reconstruct any in-flight runs this
	// agent was executing before a crash. Recovered runs enter the same idle
	// assignment loop as fresh assignments.
	recoverAgentRuns(ctx, log, client, loop.active, opts)

	for {
		select {
		case <-ctx.Done():
			loop.shutdown()
			return nil

		case cancelID := <-client.RunCancelCh():
			loop.handleCancel(cancelID)

		case resumeMsg := <-client.ResumeCh():
			loop.handleResume(resumeMsg)

		case assignment := <-client.AssignmentCh():
			loop.handleAssignment(assignment)

		case <-loop.active.doneCh():
			loop.handleDone()
		}
	}
}

// recoverAgentRuns scans persisted per-run state left by a previous agent
// process and reconstructs assignments that are still owned and resumable.
// Terminal or foreign runs are removed from disk and remembered as completed
// so duplicate deliveries are declined. The function enqueues recoverable runs
// and starts the first one before the main loop begins accepting new
// assignments.
func recoverAgentRuns(ctx context.Context, log *slog.Logger, client *servertrans.Client, active *activeRun, opts *agentOptions) {
	states, err := ListLocalRunStates()
	if err != nil {
		log.Warn("failed to list local run states", "error", err)
		return
	}
	if len(states) == 0 {
		return
	}

	copts := agentClientOptions(opts)
	for _, st := range states {
		recoverSingleRun(ctx, log, &copts, active, st)
	}

	if active.isIdle() {
		active.startNext(ctx, log, client, opts)
	}
}

func clearRecoveredRun(active *activeRun, runID string, rc *servertrans.Client) {
	if active != nil {
		active.markCompleted(runID)
	}
	removeLocalRunState(runID)
	RemoveStepCheckpoint(runID)
	if rc != nil {
		rc.Close()
	}
}

func recoverSingleRun(ctx context.Context, log *slog.Logger, copts *servertrans.Options, active *activeRun, st *localRunState) {
	if st.RunID == "" {
		return
	}
	if st.CriteriaID == "" || st.Token == "" {
		log.Warn("run state missing credentials; cannot recover", "run_id", st.RunID)
		clearRecoveredRun(active, st.RunID, nil)
		return
	}
	if st.Status == agentRunStatusTerminal {
		clearRecoveredRun(active, st.RunID, nil)
		return
	}

	rc, err := servertrans.NewClient(st.ServerURL, log, *copts)
	if err != nil {
		log.Warn("cannot build recovery client", "run_id", st.RunID, "error", err)
		return
	}
	rc.SetCredentials(st.CriteriaID, st.Token)

	log.Info("attempting to recover in-flight run", "run_id", st.RunID, "criteria_id", st.CriteriaID)
	resp, err := rc.ReattachRun(ctx, st.RunID, st.CriteriaID)
	if err != nil {
		log.Warn("reattach failed during recovery", "run_id", st.RunID, "error", err)
		rc.Close()
		return
	}
	if !resp.CanResume || isTerminalRunStatus(resp.Status) {
		log.Info("recovered run is terminal on server; clearing local state", "run_id", st.RunID, "status", resp.Status)
		clearRecoveredRun(active, st.RunID, rc)
		return
	}

	if st.WorkflowSource == "" {
		log.Warn("recovered run state missing workflow source; cannot resume", "run_id", st.RunID)
		clearRecoveredRun(active, st.RunID, rc)
		return
	}
	assignment := &pb.WorkflowAssignment{
		RunId:          st.RunID,
		WorkflowName:   st.Workflow,
		WorkflowSource: st.WorkflowSource,
		LockfileSource: st.LockfileSource,
	}
	active.enqueue(assignment, rc)
	log.Info("recovered run queued for execution", "run_id", st.RunID)
}

// errRunAlreadyTerminal signals that the server already considers a run
// terminal, so the agent must decline the assignment without re-executing it.
var errRunAlreadyTerminal = errors.New("run already terminal on server")

// cleanupAgentRunState removes the durable local metadata and step checkpoint
// for a run that has reached a terminal state.
func cleanupAgentRunState(runID string) {
	removeLocalRunState(runID)
	RemoveStepCheckpoint(runID)
}

// executeAgentAssignment materialises the assignment source into a temporary
// workflow directory, compiles and executes it, and reports progress via a
// per-run publisher.
func executeAgentAssignment(agentCtx, runCtx context.Context, log *slog.Logger, client *servertrans.Client, assignment *pb.WorkflowAssignment, opts *agentOptions, resumeCh <-chan *pb.ResumeRun, markCompleted func(string)) error {
	runID := assignment.GetRunId()
	dir, workflowPath, err := prepareAgentAssignmentDir(assignment)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	// Start the per-run publisher early so that failures which occur before
	// execution (workflow compile or run initialization) still report a
	// terminal RunFailed event centrally. The publisher uses a context that
	// ignores run-level cancellation so terminal events can be flushed even
	// after the run context is cancelled.
	publisher, closePublisher, err := newRunPublisher(runCtx, client, runID)
	if err != nil {
		return err
	}
	defer closePublisher()

	// If a previous execution of this assignment left a step checkpoint, the
	// agent crashed mid-run. Query the server for the authoritative status and
	// either resume from the checkpoint or clean up a run that has already
	// reached a terminal state.
	cp, reattachResp, err := loadResumeState(runCtx, log, client, runID)
	if err != nil {
		if errors.Is(err, errRunAlreadyTerminal) {
			markCompleted(runID)
			return nil
		}
		return err
	}

	graph, loader, err := compileAgentWorkflow(runCtx, workflowPath, log, opts)
	if err != nil {
		reportAgentAssignmentFailed(runCtx, log, publisher, runID, err)
		return err
	}
	defer func() { _ = loader.Shutdown(context.WithoutCancel(runCtx)) }()

	eng, sink, runSink, state, err := buildAgentRun(agentCtx, runCtx, log, client, assignment, opts, publisher, graph, loader, dir, workflowPath, resumeEngineOptions(cp, reattachResp, graph, log, runID)...)
	if err != nil {
		reportAgentAssignmentFailed(runCtx, log, publisher, runID, err)
		return err
	}

	// Persist the in-flight run metadata before execution so a restarted agent
	// can locate this assignment and avoid duplicate execution.
	if err := writeLocalRunState(state); err != nil {
		log.Error("failed to persist run state", "run_id", runID, "error", err)
	}

	if err := runAndDrain(agentCtx, runCtx, log, eng, loader, sink, runSink, resumeCh, state, graph, dir, runID, publisher, cp, reattachResp); err != nil {
		return err
	}

	// Terminal runs are no longer recoverable; clear local durable state while
	// preserving it when the agent is shutting down so the next startup can
	// resume. The completed run is remembered in-memory for the remainder of
	// this process so duplicate deliveries are declined.
	if agentCtx.Err() == nil {
		cleanupAgentRunState(runID)
	}

	return terminalRunError(runSink)
}

// terminalRunError returns an error when the run finished in a terminal state
// that is not successful.
func terminalRunError(runSink *terminalSuccessSink) error {
	if finalState, success, ok := runSink.TerminalSuccess(); ok && !success {
		return fmt.Errorf("run completed with terminal state %q (success=false)", finalState)
	}
	return nil
}

// newRunPublisher starts the per-run event publisher for a run, returning the
// publisher and a cleanup function that must be deferred by the caller.
func newRunPublisher(ctx context.Context, client *servertrans.Client, runID string) (*servertrans.RunPublisher, func(), error) {
	publisher, err := client.NewRunPublisher(context.WithoutCancel(ctx), runID)
	if err != nil {
		return nil, nil, fmt.Errorf("create run publisher: %w", err)
	}
	return publisher, func() { publisher.Close() }, nil
}

// reportAgentAssignmentFailed emits a terminal RunFailed event for a run that
// failed before reaching execution, then drains the publisher so the event is
// persisted centrally. Use this for compile-time and initialization-time
// failures where the ordinary run sink has not yet been established.
func reportAgentAssignmentFailed(ctx context.Context, log *slog.Logger, publisher *servertrans.RunPublisher, runID string, err error) {
	log.Error("assignment failed before execution", "run_id", runID, "error", err)
	sink := &run.Sink{
		RunID:  runID,
		Client: publisher,
		Log:    log.With("run_id", runID),
		Ctx:    ctx,
	}
	sink.RunFailed(ctx, err.Error(), "")

	drainCtx, drainCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer drainCancel()
	publisher.Drain(drainCtx)
}

// loadResumeState checks for a local step checkpoint from a previous agent
// process and queries the server to decide whether the run should resume.
// If the server reports the run is already terminal, local recovery state is
// cleaned up and the assignment can be ignored. A nil checkpoint means the
// run has never been started by this agent.
func loadResumeState(ctx context.Context, log *slog.Logger, client *servertrans.Client, runID string) (*StepCheckpoint, *pb.ReattachRunResponse, error) {
	cp, err := readStepCheckpoint(runID)
	if err != nil {
		log.Warn("failed to read step checkpoint", "run_id", runID, "error", err)
		return nil, nil, nil
	}
	if cp == nil {
		return nil, nil, nil
	}

	log.Info("found step checkpoint, querying server for run status", "run_id", runID)
	resp, err := client.ReattachRun(ctx, runID, client.CriteriaID())
	if err != nil {
		// Without the server's blessing we cannot safely resume. Leave the
		// checkpoint in place so a future restart can try again.
		return cp, nil, fmt.Errorf("reattach run %s: %w", runID, err)
	}
	if !resp.CanResume || isTerminalRunStatus(resp.Status) {
		log.Info("run is terminal on server; clearing local recovery state", "run_id", runID, "status", resp.Status)
		removeLocalRunState(runID)
		RemoveStepCheckpoint(runID)
		return nil, nil, errRunAlreadyTerminal
	}
	return cp, resp, nil
}

// isTerminalRunStatus reports whether the server status string represents a
// finished run that must not be resumed.
func isTerminalRunStatus(status string) bool {
	switch status {
	case "succeeded", "failed", "cancelled":
		return true
	default:
		return false
	}
}

// resumeEngineOptions builds engine options that restore a run's execution
// state when a checkpoint and a resumable server response are available.
func resumeEngineOptions(cp *StepCheckpoint, reattachResp *pb.ReattachRunResponse, graph *workflow.FSMGraph, log *slog.Logger, runID string) []engine.Option {
	if cp == nil || reattachResp == nil || !reattachResp.CanResume || reattachResp.CurrentStep == "" {
		return nil
	}
	var opts []engine.Option
	restoredVars, restoredIter, err := workflow.RestoreVarScope(reattachResp.VariableScope, graph)
	if err != nil {
		log.Error("failed to restore variable scope", "run_id", runID, "error", err)
	} else {
		opts = append(opts,
			engine.WithResumedVars(restoredVars),
			engine.WithResumedIter(restoredIter),
			engine.WithResumedVisits(cp.Visits),
		)
	}
	if reattachResp.Status == agentRunStatusPaused {
		opts = append(opts, engine.WithPendingSignal(reattachResp.PendingSignal))
	}
	return opts
}

// prepareAgentAssignmentDir writes the workflow (and optional lockfile) source
// from an assignment into a temporary directory. The returned directory must be
// removed by the caller.
func prepareAgentAssignmentDir(assignment *pb.WorkflowAssignment) (dir, workflowPath string, err error) {
	dir, err = os.MkdirTemp("", "criteria-agent-*")
	if err != nil {
		return "", "", fmt.Errorf("create workflow dir: %w", err)
	}
	workflowPath = filepath.Join(dir, "workflow.hcl")
	if err := os.WriteFile(workflowPath, []byte(assignment.GetWorkflowSource()), 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", "", fmt.Errorf("write workflow source: %w", err)
	}
	if assignment.GetLockfileSource() != "" {
		lockfilePath := filepath.Join(dir, ".criteria.lock.hcl")
		if err := os.WriteFile(lockfilePath, []byte(assignment.GetLockfileSource()), 0o600); err != nil {
			_ = os.RemoveAll(dir)
			return "", "", fmt.Errorf("write lockfile source: %w", err)
		}
	}
	return dir, workflowPath, nil
}

// buildAgentRun constructs the engine, sink, and local run state for an
// assignment. The engine is returned ready to run but not started. Additional
// engine options can be supplied for crash-recovery resume (restored variable
// scope, iteration cursor, visit counts, and pending signal).
func buildAgentRun(agentCtx, runCtx context.Context, log *slog.Logger, client *servertrans.Client, assignment *pb.WorkflowAssignment, opts *agentOptions, publisher *servertrans.RunPublisher, graph *workflow.FSMGraph, loader adapterhost.Loader, workflowDir, workflowPath string, engineOpts ...engine.Option) (*engine.Engine, *run.Sink, *terminalSuccessSink, *localRunState, error) {
	state := newLocalRunState(assignment.GetRunId(), graph.Name, opts.serverURL)
	state.CriteriaID = client.CriteriaID()
	state.Token = client.Token()
	state.WorkflowSource = assignment.GetWorkflowSource()
	state.LockfileSource = assignment.GetLockfileSource()
	state.Status = agentRunStatusRunning

	var eng *engine.Engine
	sink := buildServerSink(runCtx, publisher, client, assignment.GetRunId(), graph, workflowPath, opts.serverURL, log,
		func() map[string]int {
			if eng != nil {
				return eng.VisitCounts()
			}
			return nil
		})
	engineSink := &shutdownSuppressingSink{Sink: sink, agentCtx: agentCtx}
	runSink := &terminalSuccessSink{Sink: engineSink}

	auditPath, _ := auditLogPath(assignment.GetRunId())
	auditWriter := adapterhost.NewFileAuditWriter(auditPath)
	mergedVars, err := mergeVarSources(opts.varFiles, opts.varOverrides)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	baseOpts := []engine.Option{
		engine.WithVarOverrides(mergedVars),
		engine.WithWorkflowDir(workflowDir),
		engine.WithAuditWriter(auditWriter),
	}
	eng = engine.New(graph, loader, runSink, append(baseOpts, engineOpts...)...)
	return eng, sink, runSink, state, nil
}

// runAndDrain runs the engine, drains resume cycles, and flushes terminal
// events through the publisher. It returns the original run error (if any).
// When cp and reattachResp are non-nil, the engine resumes from the server's
// reported current step and attempt instead of starting from the beginning.
func runAndDrain(agentCtx, runCtx context.Context, log *slog.Logger, eng *engine.Engine, loader adapterhost.Loader, sink *run.Sink, runSink engine.Sink, resumeCh <-chan *pb.ResumeRun, state *localRunState, graph *workflow.FSMGraph, workflowDir, runID string, publisher *servertrans.RunPublisher, cp *StepCheckpoint, reattachResp *pb.ReattachRunResponse) error {
	var runErr error
	resuming := cp != nil && reattachResp != nil && reattachResp.CanResume && reattachResp.CurrentStep != ""
	if resuming {
		log.Info("resuming run from checkpoint", "run_id", runID, "step", reattachResp.CurrentStep, "attempt", reattachResp.Attempt)
		runErr = eng.RunFrom(runCtx, reattachResp.CurrentStep, int(reattachResp.Attempt))
	} else {
		runErr = eng.Run(runCtx)
	}

	shutdown := agentCtx.Err() != nil
	if runErr != nil {
		if shutdown {
			// Agent shutdown is not a terminal failure. Do not emit RunFailed so
			// the server leaves the run in-flight for crash recovery.
			log.Info("run interrupted by agent shutdown", "run_id", runID, "error", runErr)
		} else {
			log.Error("run failed", "run_id", runID, "error", runErr)
			sink.RunFailed(runCtx, runErr.Error(), "")
		}
	} else if !shutdown {
		log.Info("run completed", "run_id", runID)
	}

	// Only wait for resume cycles when the agent is not shutting down. Pending
	// non-terminal events are still drained below so the server can ack them
	// and the next process replays by correlation ID.
	if !shutdown {
		if err := drainResumeCycles(runCtx, log, loader, sink, runSink, resumeCh, state, graph, workflowDir, eng); err != nil {
			return err
		}
	}

	drainCtx, drainCancel := context.WithTimeout(context.WithoutCancel(runCtx), 5*time.Second)
	publisher.Drain(drainCtx)
	drainCancel()

	if shutdown {
		return runCtx.Err()
	}
	return runErr
}

// compileAgentWorkflow parses and compiles the workflow at workflowPath using
// the same pipeline as local/server apply. It returns the compiled graph and
// the adapter loader (which the caller must shut down).
func compileAgentWorkflow(ctx context.Context, workflowPath string, log *slog.Logger, opts *agentOptions) (*workflow.FSMGraph, *adapterhost.DefaultLoader, error) {
	_, graph, loader, err := compileForExecution(ctx, workflowPath, log, opts.warnsAsErrors, opts.allowUnsigned, opts.subworkflowRoots...)
	if err != nil {
		return nil, nil, err
	}
	return graph, loader, nil
}
