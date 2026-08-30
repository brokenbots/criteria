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

// activeRun tracks the currently executing assignment, pending queued
// assignments, and the channels used to route control messages to the worker.
// All mutable access is protected by activeRun.mu.
type activeRun struct {
	mu       sync.Mutex
	runID    string
	cancel   context.CancelFunc
	resumeCh chan *pb.ResumeRun
	done     chan struct{}
	pending  []*pb.WorkflowAssignment
}

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

func (a *activeRun) enqueue(assignment *pb.WorkflowAssignment) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pending = append(a.pending, assignment)
}

func (a *activeRun) nextPending() *pb.WorkflowAssignment {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.pending) == 0 {
		return nil
	}
	assignment := a.pending[0]
	a.pending = a.pending[1:]
	return assignment
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

func (a *activeRun) startNext(ctx context.Context, log *slog.Logger, client *servertrans.Client, opts *agentOptions) {
	assignment := a.nextPending()
	if assignment == nil {
		return
	}

	runCtx, cancel := context.WithCancel(ctx)
	resumeCh := make(chan *pb.ResumeRun, 1)
	done := make(chan struct{})
	a.beginRun(assignment.GetRunId(), cancel, resumeCh, done)

	log.Info("accepted assignment", "run_id", assignment.GetRunId(), "workflow", assignment.GetWorkflowName())
	go func(aa *pb.WorkflowAssignment) {
		defer a.finishRun()
		if err := executeAgentAssignment(runCtx, log, client, aa, opts, resumeCh); err != nil {
			log.Error("assignment execution failed", "run_id", aa.GetRunId(), "error", err)
		} else {
			log.Info("assignment completed", "run_id", aa.GetRunId())
		}
	}(assignment)
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
	l.active.enqueue(assignment)
	if l.active.isIdle() {
		l.active.startNext(l.ctx, l.log, l.client, l.opts)
		return
	}
	l.log.Info("assignment queued behind active run",
		"active_run_id", l.active.activeRunID(),
		"queued_run_id", assignment.GetRunId())
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

// executeAgentAssignment materialises the assignment source into a temporary
// workflow directory, compiles and executes it, and reports progress via a
// per-run publisher.
func executeAgentAssignment(ctx context.Context, log *slog.Logger, client *servertrans.Client, assignment *pb.WorkflowAssignment, opts *agentOptions, resumeCh <-chan *pb.ResumeRun) error {
	dir, workflowPath, err := prepareAgentAssignmentDir(assignment)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	graph, loader, err := compileAgentWorkflow(ctx, workflowPath, log, opts)
	if err != nil {
		return err
	}
	defer func() { _ = loader.Shutdown(context.WithoutCancel(ctx)) }()

	// Start the per-run publisher with a context that ignores run-level
	// cancellation so terminal events (including RunFailed after a cancel)
	// can still be flushed before the publisher is closed.
	publisher, err := client.NewRunPublisher(context.WithoutCancel(ctx), assignment.GetRunId())
	if err != nil {
		return fmt.Errorf("create run publisher: %w", err)
	}
	defer publisher.Close()

	eng, sink, runSink, state, err := buildAgentRun(ctx, log, client, assignment, opts, publisher, graph, loader, dir, workflowPath)
	if err != nil {
		return err
	}

	if err := runAndDrain(ctx, log, eng, loader, sink, runSink, resumeCh, state, graph, dir, assignment.GetRunId(), publisher); err != nil {
		return err
	}

	if finalState, success, ok := runSink.TerminalSuccess(); ok && !success {
		return fmt.Errorf("run completed with terminal state %q (success=false)", finalState)
	}
	return nil
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
// assignment. The engine is returned ready to run but not started.
func buildAgentRun(ctx context.Context, log *slog.Logger, client *servertrans.Client, assignment *pb.WorkflowAssignment, opts *agentOptions, publisher *servertrans.RunPublisher, graph *workflow.FSMGraph, loader adapterhost.Loader, workflowDir, workflowPath string) (*engine.Engine, *run.Sink, *terminalSuccessSink, *localRunState, error) {
	state := newLocalRunState(assignment.GetRunId(), graph.Name, opts.serverURL)
	var eng *engine.Engine
	sink := buildServerSink(ctx, publisher, client, assignment.GetRunId(), graph, workflowPath, opts.serverURL, log,
		func() map[string]int {
			if eng != nil {
				return eng.VisitCounts()
			}
			return nil
		})
	runSink := &terminalSuccessSink{Sink: sink}

	auditPath, _ := auditLogPath(assignment.GetRunId())
	auditWriter := adapterhost.NewFileAuditWriter(auditPath)
	mergedVars, err := mergeVarSources(opts.varFiles, opts.varOverrides)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	eng = engine.New(graph, loader, runSink,
		engine.WithVarOverrides(mergedVars),
		engine.WithWorkflowDir(workflowDir),
		engine.WithAuditWriter(auditWriter),
	)
	return eng, sink, runSink, state, nil
}

// runAndDrain runs the engine, drains resume cycles, and flushes terminal
// events through the publisher. It returns the original run error (if any).
func runAndDrain(ctx context.Context, log *slog.Logger, eng *engine.Engine, loader adapterhost.Loader, sink *run.Sink, runSink engine.Sink, resumeCh <-chan *pb.ResumeRun, state *localRunState, graph *workflow.FSMGraph, workflowDir, runID string, publisher *servertrans.RunPublisher) error {
	runErr := eng.Run(ctx)
	if runErr != nil {
		log.Error("run failed", "run_id", runID, "error", runErr)
		sink.RunFailed(ctx, runErr.Error(), "")
	} else {
		log.Info("run completed", "run_id", runID)
	}

	if err := drainResumeCycles(ctx, log, loader, sink, runSink, resumeCh, state, graph, workflowDir, eng); err != nil {
		return err
	}

	drainCtx, drainCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	publisher.Drain(drainCtx)
	drainCancel()

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
