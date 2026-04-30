package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/brokenbots/criteria/internal/adapters/shell"
	"github.com/brokenbots/criteria/internal/cli/localresume"
	"github.com/brokenbots/criteria/internal/engine"
	"github.com/brokenbots/criteria/internal/plugin"
	"github.com/brokenbots/criteria/internal/run"
	servertrans "github.com/brokenbots/criteria/internal/transport/server"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	"github.com/brokenbots/criteria/workflow"
)

type applyOptions struct {
	workflowPath string
	serverURL    string
	eventsPath   string
	name         string
	codec        string
	tlsMode      string
	tlsCA        string
	tlsCert      string
	tlsKey       string
	varOverrides []string // raw "key=value" pairs from --var flags
	output       string   // "auto" | "concise" | "json"
}

func NewApplyCmd() *cobra.Command {
	var opts applyOptions

	cmd := &cobra.Command{
		Use:   "apply <workflow.hcl>",
		Short: "Execute a workflow locally or against a server",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.workflowPath = args[0]

			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			return runApply(ctx, opts)
		},
	}

	cmd.Flags().StringVar(&opts.serverURL, "server", envOrDefault("CRITERIA_SERVER_URL", ""), "server base URL (optional for local mode)")
	cmd.Flags().StringVar(&opts.eventsPath, "events-file", "", "Write ND-JSON events to this path in local mode (always written when set, regardless of --output)")
	cmd.Flags().StringVar(&opts.name, "name", envOrDefault("CRITERIA_NAME", ""), "Agent name (server mode, defaults to hostname)")
	cmd.Flags().StringVar(&opts.codec, "server-codec", envOrDefault("CRITERIA_SERVER_CODEC", "proto"), "Connect codec: proto or json")
	cmd.Flags().StringVar(&opts.tlsMode, "server-tls", envOrDefault("CRITERIA_SERVER_TLS", ""), "TLS mode: disable|tls|mtls")
	cmd.Flags().StringVar(&opts.tlsCA, "tls-ca", envOrDefault("CRITERIA_TLS_CA", ""), "Path to CA bundle PEM")
	cmd.Flags().StringVar(&opts.tlsCert, "tls-cert", envOrDefault("CRITERIA_TLS_CERT", ""), "Path to client cert PEM")
	cmd.Flags().StringVar(&opts.tlsKey, "tls-key", envOrDefault("CRITERIA_TLS_KEY", ""), "Path to client key PEM")
	cmd.Flags().StringArrayVar(&opts.varOverrides, "var", nil, "Override a workflow variable: key=value (repeatable)")
	cmd.Flags().StringVar(&opts.output, "output", envOrDefault("CRITERIA_OUTPUT", "auto"), "Standalone output format: auto|concise|json (auto: concise on TTY, json when piped)")
	return cmd
}

func runApply(ctx context.Context, opts applyOptions) error {
	if strings.TrimSpace(opts.workflowPath) == "" {
		return errors.New("workflow path is required")
	}
	if strings.TrimSpace(opts.serverURL) != "" {
		return runApplyServer(ctx, opts)
	}
	return runApplyLocal(ctx, opts)
}

func runApplyLocal(ctx context.Context, opts applyOptions) error { //nolint:funlen // W03: local apply orchestrates engine lifecycle, event routing, and output rendering in one function
	log := newApplyLogger()

	mode, err := resolveOutputMode(opts.output, os.Stdout)
	if err != nil {
		return err
	}
	jsonOut, cleanup, err := openNDJSONWriter(opts.eventsPath, mode)
	if err != nil {
		return err
	}
	defer cleanup()

	resumeLocalInFlightRuns(ctx, log, jsonOut, mode)

	src, graph, loader, err := compileForExecution(ctx, opts.workflowPath, log)
	if err != nil {
		return err
	}
	defer loader.Shutdown(context.Background())

	resumer, err := buildLocalResumer(log)
	if err != nil {
		return err
	}
	if err := ensureLocalModeSupported(graph, resumer != nil); err != nil {
		return err
	}

	runID := uuid.NewString()
	checkpointFn := func(step string, attempt int) {
		cp := &StepCheckpoint{
			RunID:        runID,
			Workflow:     graph.Name,
			WorkflowPath: opts.workflowPath,
			CurrentStep:  step,
			Attempt:      attempt,
			StartedAt:    time.Now().UTC(),
		}
		if cpErr := WriteStepCheckpoint(cp); cpErr != nil {
			log.Warn("failed to write step checkpoint; crash recovery may not work", "error", cpErr)
		}
	}
	baseSink := buildLocalSink(runID, jsonOut, mode, graph.StepOrder(), checkpointFn)
	tracker := &pauseTracker{Sink: baseSink}

	log.Info("starting local run",
		"run_id", runID,
		"workflow", graph.Name,
		"file", filepath.Base(opts.workflowPath))

	state := &localRunState{
		PID:       os.Getpid(),
		RunID:     runID,
		Workflow:  graph.Name,
		ServerURL: "",
		StartedAt: time.Now().UTC(),
	}
	_ = writeLocalRunState(state)
	defer removeLocalRunState()
	defer RemoveStepCheckpoint(runID)

	// src (raw HCL bytes) is consumed only by server mode for signed payload delivery;
	// local mode has no signing step, so src is intentionally unused here.
	_ = src
	eng := engine.New(graph, loader, tracker,
		engine.WithVarOverrides(parseVarOverrides(opts.varOverrides)),
		engine.WithWorkflowDir(filepath.Dir(opts.workflowPath)),
	)
	if err := eng.Run(ctx); err != nil {
		log.Error("local run failed", "run_id", runID, "error", err)
		return err
	}

	if resumer != nil {
		if err := drainLocalResumeCycles(ctx, log, graph, loader, tracker, resumer, runID, opts, eng); err != nil {
			return err
		}
	}

	log.Info("local run completed", "run_id", runID)
	return nil
}

func newApplyLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func applyClientOptions(opts applyOptions) servertrans.Options {
	return servertrans.Options{
		Codec:    servertrans.Codec(opts.codec),
		TLSMode:  servertrans.TLSMode(opts.tlsMode),
		CAFile:   opts.tlsCA,
		CertFile: opts.tlsCert,
		KeyFile:  opts.tlsKey,
	}
}

func writeRunCheckpoint(log *slog.Logger, runID, graphName, workflowPath, serverURL, step string, attempt int, criteriaID, token string) {
	cp := &StepCheckpoint{
		RunID:        runID,
		Workflow:     graphName,
		WorkflowPath: workflowPath,
		CurrentStep:  step,
		Attempt:      attempt,
		StartedAt:    time.Now().UTC(),
		ServerURL:    serverURL,
		CriteriaID:   criteriaID,
		Token:        token,
	}
	if cpErr := WriteStepCheckpoint(cp); cpErr != nil {
		log.Warn("failed to write step checkpoint; crash recovery may not work", "error", cpErr)
	}
}

func buildServerSink(client *servertrans.Client, runID string, graph *workflow.FSMGraph, workflowPath, serverURL string, log *slog.Logger) *run.Sink {
	return &run.Sink{
		RunID:  runID,
		Client: client,
		Log:    log.With("run_id", runID),
		CheckpointFn: func(step string, attempt int) {
			writeRunCheckpoint(log, runID, graph.Name, workflowPath, serverURL, step, attempt, client.CriteriaID(), client.Token())
		},
	}
}

func newLocalRunState(runID, graphName, serverURL string) *localRunState {
	return &localRunState{
		PID:       os.Getpid(),
		RunID:     runID,
		Workflow:  graphName,
		ServerURL: serverURL,
		StartedAt: time.Now().UTC(),
	}
}

func executeServerRun(ctx context.Context, log *slog.Logger, loader plugin.Loader, sink *run.Sink, client *servertrans.Client, state *localRunState, graph *workflow.FSMGraph, opts applyOptions) error {
	_ = writeLocalRunState(state)
	defer removeLocalRunState()
	defer RemoveStepCheckpoint(state.RunID)

	log.Info("starting run",
		"run_id", state.RunID,
		"workflow", graph.Name,
		"file", filepath.Base(opts.workflowPath))

	eng := engine.New(graph, loader, sink,
		engine.WithVarOverrides(parseVarOverrides(opts.varOverrides)),
		engine.WithWorkflowDir(filepath.Dir(opts.workflowPath)),
	)
	if err := eng.Run(ctx); err != nil {
		log.Error("run failed", "error", err)
		return err
	}
	log.Info("run completed", "run_id", state.RunID)

	if err := drainResumeCycles(ctx, log, loader, sink, client, state, graph, opts, eng); err != nil {
		return err
	}

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
	client.Drain(drainCtx)
	drainCancel()
	return nil
}

// drainResumeCycles handles the pause/resume loop: each time the sink is
// paused it waits for a matching ResumeRun message and restarts the engine
// from the paused node, updating eng to the most recently completed engine.
func drainResumeCycles(ctx context.Context, log *slog.Logger, loader plugin.Loader, sink *run.Sink, client *servertrans.Client, state *localRunState, graph *workflow.FSMGraph, opts applyOptions, eng *engine.Engine) error {
	for sink.IsPaused() {
		log.Info("run paused; waiting for resume signal", "run_id", state.RunID, "node", sink.PausedAt())
		var resumeMsg *pb.ResumeRun
		select {
		case <-ctx.Done():
			return ctx.Err()
		case resumeMsg = <-client.ResumeCh():
		}
		if resumeMsg.RunId != state.RunID {
			log.Warn("received resume for unexpected run", "expected", state.RunID, "got", resumeMsg.RunId)
			continue
		}
		log.Info("received resume signal", "run_id", state.RunID, "signal", resumeMsg.Signal)
		pausedNode := sink.PausedAt()
		sink.ClearPaused()
		resumedEng := engine.New(graph, loader, sink,
			engine.WithResumedVars(eng.VarScope()),
			engine.WithResumePayload(resumeMsg.Payload),
			engine.WithWorkflowDir(filepath.Dir(opts.workflowPath)),
		)
		if err := resumedEng.RunFrom(ctx, pausedNode, 1); err != nil {
			log.Error("run failed after resume", "error", err)
			return err
		}
		eng = resumedEng
		log.Info("run resumed and completed", "run_id", state.RunID)
	}
	return nil
}

func runApplyServer(ctx context.Context, opts applyOptions) error {
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	log := newApplyLogger()
	src, graph, loader, err := compileForExecution(runCtx, opts.workflowPath, log)
	if err != nil {
		return err
	}
	defer loader.Shutdown(context.Background())

	client, runID, err := setupServerRun(runCtx, log, graph, src, opts.serverURL, opts.name, applyClientOptions(opts), cancelRun)
	if err != nil {
		return err
	}
	defer client.Close()

	sink := buildServerSink(client, runID, graph, opts.workflowPath, opts.serverURL, log)
	state := newLocalRunState(runID, graph.Name, opts.serverURL)
	return executeServerRun(runCtx, log, loader, sink, client, state, graph, opts)
}

func setupServerRun(ctx context.Context, log *slog.Logger, graph *workflow.FSMGraph, src []byte, serverURL, name string, clientOpts servertrans.Options, cancelRun func()) (*servertrans.Client, string, error) {
	client, err := servertrans.NewClient(serverURL, log, clientOpts)
	if err != nil {
		return nil, "", err
	}
	hostname, _ := os.Hostname()
	if name == "" {
		name = hostname
	}
	if err := client.Register(ctx, name, hostname, "0.1.0"); err != nil {
		client.Close()
		return nil, "", fmt.Errorf("register: %w", err)
	}

	resumeInFlightRuns(ctx, log, clientOpts)

	runID, err := client.CreateRun(ctx, graph.Name, string(src))
	if err != nil {
		client.Close()
		return nil, "", fmt.Errorf("create run: %w", err)
	}
	if err := client.StartStreams(ctx, runID); err != nil {
		client.Close()
		return nil, "", fmt.Errorf("server streams: %w", err)
	}
	client.StartHeartbeat(ctx, 10*time.Second)

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case cancelRunID := <-client.RunCancelCh():
				if cancelRunID == runID {
					log.Info("received run.cancel control", "run_id", runID)
					if cancelRun != nil {
						cancelRun()
					}
				}
			}
		}
	}()

	return client, runID, nil
}

func compileForExecution(ctx context.Context, workflowPath string, log *slog.Logger) ([]byte, *workflow.FSMGraph, *plugin.DefaultLoader, error) {
	src, err := os.ReadFile(workflowPath)
	if err != nil {
		return nil, nil, nil, err
	}
	spec, diags := workflow.Parse(workflowPath, src)
	if diags.HasErrors() {
		return nil, nil, nil, fmt.Errorf("parse: %s", diags.Error())
	}

	loader := plugin.NewLoader()
	loader.RegisterBuiltin(shell.Name, plugin.BuiltinFactoryForAdapter(shell.New()))
	schemas := collectSchemas(ctx, loader, spec, log)
	graph, diags := workflow.CompileWithOpts(spec, schemas, workflow.CompileOpts{WorkflowDir: filepath.Dir(workflowPath)})
	if diags.HasErrors() {
		loader.Shutdown(context.Background())
		return nil, nil, nil, fmt.Errorf("compile: %s", diags.Error())
	}

	return src, graph, loader, nil
}

// pauseTracker wraps an engine.Sink and tracks pause state for the local approval
// resume loop. It intercepts OnRunPaused to record the paused node name, and
// captures approval/signal details so the resume loop knows what to resolve.
type pauseTracker struct {
	engine.Sink
	mu             sync.Mutex
	pausedNode     string
	approvalDetail *approvalDetail
	signalDetail   *signalDetail
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
func buildLocalResumer(log *slog.Logger) (localresume.LocalResumer, error) {
	raw := os.Getenv("CRITERIA_LOCAL_APPROVAL")
	if raw == "" {
		return nil, nil
	}
	m, err := localresume.ParseMode(raw)
	if err != nil {
		return nil, err
	}
	opts := localresume.Options{Log: log}
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
func drainLocalResumeCycles(ctx context.Context, log *slog.Logger, graph *workflow.FSMGraph, loader plugin.Loader, tracker *pauseTracker, resumer localresume.LocalResumer, runID string, opts applyOptions, eng *engine.Engine) error {
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
			engine.WithResumePayload(payload),
			engine.WithWorkflowDir(filepath.Dir(opts.workflowPath)),
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
		return resumer.ResumeSignal(ctx, runID, pausedNode, signalName)
	}
	return nil, fmt.Errorf("paused at node %q which is neither an approval nor a signal wait", pausedNode)
}

const (
	errSignalWait   = "signal waits require an orchestrator (e.g. --server <url>) or the local-mode env CRITERIA_LOCAL_APPROVAL={stdin|file|env|auto-approve}"
	errApprovalNode = "approval nodes require an orchestrator (e.g. --server <url>) or the local-mode env CRITERIA_LOCAL_APPROVAL={stdin|file|env|auto-approve}"
)

func ensureLocalModeSupported(graph *workflow.FSMGraph, localApprovalEnabled bool) error {
	if localApprovalEnabled {
		return nil
	}
	for _, wn := range graph.Waits {
		if wn.Signal != "" {
			return errors.New(errSignalWait)
		}
	}
	if len(graph.Approvals) > 0 {
		return errors.New(errApprovalNode)
	}
	for _, step := range graph.Steps {
		switch step.Lifecycle {
		case "wait":
			return errors.New(errSignalWait)
		case "approval":
			return errors.New(errApprovalNode)
		}
	}
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

func resumeLocalInFlightRuns(ctx context.Context, log *slog.Logger, out io.Writer, mode outputMode) {
	checkpoints, err := ListStepCheckpoints()
	if err != nil {
		log.Warn("could not list step checkpoints; skipping local crash recovery", "error", err)
		return
	}
	if len(checkpoints) == 0 {
		return
	}
	for _, cp := range checkpoints {
		if strings.TrimSpace(cp.ServerURL) != "" {
			continue
		}
		resumeOneLocalRun(ctx, log, cp, out, mode)
	}
}

// prepareReattach validates the checkpoint, builds a plugin loader, and
// constructs a local resumer. On failure it logs, clears the checkpoint,
// and returns zero values with false so the caller can skip the run.
func prepareReattach(ctx context.Context, log *slog.Logger, cp *StepCheckpoint) (*workflow.FSMGraph, plugin.Loader, localresume.LocalResumer, bool) {
	graph, err := parseWorkflowFromPath(cp.WorkflowPath)
	if err != nil {
		log.Warn("cannot parse workflow for crashed local run; abandoning", "run_id", cp.RunID, "error", err)
		RemoveStepCheckpoint(cp.RunID)
		return nil, nil, nil, false
	}
	resumer, resumerErr := buildLocalResumer(log)
	if resumerErr != nil {
		log.Warn("local checkpoint: invalid CRITERIA_LOCAL_APPROVAL; clearing", "run_id", cp.RunID, "error", resumerErr)
		RemoveStepCheckpoint(cp.RunID)
		return nil, nil, nil, false
	}
	if err := ensureLocalModeSupported(graph, resumer != nil); err != nil {
		log.Warn("local checkpoint requires server; clearing", "run_id", cp.RunID, "error", err)
		RemoveStepCheckpoint(cp.RunID)
		return nil, nil, nil, false
	}
	loader := plugin.NewLoader()
	loader.RegisterBuiltin(shell.Name, plugin.BuiltinFactoryForAdapter(shell.New()))
	_ = ctx // threaded for context propagation; parseWorkflowFromPath manages its own context internally
	return graph, loader, resumer, true
}

func resumeOneLocalRun(ctx context.Context, log *slog.Logger, cp *StepCheckpoint, out io.Writer, mode outputMode) {
	graph, loader, resumer, ok := prepareReattach(ctx, log, cp)
	if !ok {
		return
	}
	defer loader.Shutdown(context.Background())

	nextAttempt := cp.Attempt + 1
	maxAttempts := 1 + graph.Policy.MaxStepRetries
	if nextAttempt > maxAttempts {
		sink := buildLocalSink(cp.RunID, out, mode, graph.StepOrder(), nil)
		reason := fmt.Sprintf("exceeded max_step_retries on resume at step %q (attempt %d)", cp.CurrentStep, nextAttempt)
		sink.OnRunFailed(reason, cp.CurrentStep)
		RemoveStepCheckpoint(cp.RunID)
		return
	}

	checkpointFn := func(step string, attempt int) {
		next := *cp
		next.CurrentStep = step
		next.Attempt = attempt
		next.StartedAt = time.Now().UTC()
		if cpErr := WriteStepCheckpoint(&next); cpErr != nil {
			log.Warn("failed to update local checkpoint", "run_id", cp.RunID, "error", cpErr)
		}
	}
	baseSink := buildLocalSink(cp.RunID, out, mode, graph.StepOrder(), checkpointFn)
	tracker := &pauseTracker{Sink: baseSink}
	tracker.OnStepResumed(cp.CurrentStep, nextAttempt, "criteria_restart")

	opts := applyOptions{workflowPath: cp.WorkflowPath}
	eng := engine.New(graph, loader, tracker, engine.WithWorkflowDir(filepath.Dir(cp.WorkflowPath)))
	if runErr := eng.RunFrom(ctx, cp.CurrentStep, nextAttempt); runErr != nil {
		log.Error("resumed local run failed", "run_id", cp.RunID, "error", runErr)
		RemoveStepCheckpoint(cp.RunID)
		return
	}
	if resumer != nil {
		if cycleErr := drainLocalResumeCycles(ctx, log, graph, loader, tracker, resumer, cp.RunID, opts, eng); cycleErr != nil {
			log.Error("resumed local run failed during approval", "run_id", cp.RunID, "error", cycleErr)
		}
	} else {
		log.Info("resumed local run completed", "run_id", cp.RunID)
	}
	RemoveStepCheckpoint(cp.RunID)
}
