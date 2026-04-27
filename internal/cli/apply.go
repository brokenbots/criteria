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
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/brokenbots/overseer/internal/adapters/shell"
	"github.com/brokenbots/overseer/internal/engine"
	"github.com/brokenbots/overseer/internal/plugin"
	"github.com/brokenbots/overseer/internal/run"
	castletrans "github.com/brokenbots/overseer/internal/transport/castle"
	pb "github.com/brokenbots/overseer/sdk/pb/v1"
	"github.com/brokenbots/overseer/workflow"
)

type applyOptions struct {
	workflowPath string
	castleURL    string
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
		Short: "Execute a workflow locally or against Castle",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.workflowPath = args[0]

			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			return runApply(ctx, opts)
		},
	}

	cmd.Flags().StringVar(&opts.castleURL, "castle", envOrDefault("OVERSEER_CASTLE_URL", ""), "Castle base URL (optional for local mode)")
	cmd.Flags().StringVar(&opts.eventsPath, "events-file", "", "Write ND-JSON events to this path in local mode (always written when set, regardless of --output)")
	cmd.Flags().StringVar(&opts.name, "name", envOrDefault("OVERSEER_NAME", ""), "Overseer name (Castle mode, defaults to hostname)")
	cmd.Flags().StringVar(&opts.codec, "castle-codec", envOrDefault("OVERSEER_CASTLE_CODEC", "proto"), "Connect codec: proto or json")
	cmd.Flags().StringVar(&opts.tlsMode, "castle-tls", envOrDefault("OVERSEER_CASTLE_TLS", ""), "TLS mode: disable|tls|mtls")
	cmd.Flags().StringVar(&opts.tlsCA, "tls-ca", envOrDefault("OVERSEER_TLS_CA", ""), "Path to CA bundle PEM")
	cmd.Flags().StringVar(&opts.tlsCert, "tls-cert", envOrDefault("OVERSEER_TLS_CERT", ""), "Path to client cert PEM")
	cmd.Flags().StringVar(&opts.tlsKey, "tls-key", envOrDefault("OVERSEER_TLS_KEY", ""), "Path to client key PEM")
	cmd.Flags().StringArrayVar(&opts.varOverrides, "var", nil, "Override a workflow variable: key=value (repeatable)")
	cmd.Flags().StringVar(&opts.output, "output", envOrDefault("OVERSEER_OUTPUT", "auto"), "Standalone output format: auto|concise|json (auto: concise on TTY, json when piped)")
	return cmd
}

func runApply(ctx context.Context, opts applyOptions) error {
	if strings.TrimSpace(opts.workflowPath) == "" {
		return errors.New("workflow path is required")
	}
	if strings.TrimSpace(opts.castleURL) != "" {
		return runApplyCastle(ctx, opts)
	}
	return runApplyLocal(ctx, opts)
}

func runApplyLocal(ctx context.Context, opts applyOptions) error {
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

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

	if err := ensureLocalModeSupported(graph); err != nil {
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
	sink := buildLocalSink(runID, jsonOut, mode, graph.StepOrder(), checkpointFn)

	log.Info("starting local run",
		"run_id", runID,
		"workflow", graph.Name,
		"file", filepath.Base(opts.workflowPath))

	state := &localRunState{
		PID:       os.Getpid(),
		RunID:     runID,
		Workflow:  graph.Name,
		CastleURL: "",
		StartedAt: time.Now().UTC(),
	}
	_ = writeLocalRunState(state)
	defer removeLocalRunState()
	defer RemoveStepCheckpoint(runID)

	// src (raw HCL bytes) is consumed only by Castle mode for signed payload delivery;
	// local mode has no signing step, so src is intentionally unused here.
	_ = src
	eng := engine.New(graph, loader, sink, engine.WithVarOverrides(parseVarOverrides(opts.varOverrides)))
	if err := eng.Run(ctx); err != nil {
		log.Error("local run failed", "run_id", runID, "error", err)
		return err
	}
	log.Info("local run completed", "run_id", runID)
	return nil
}

func runApplyCastle(ctx context.Context, opts applyOptions) error {
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	src, graph, loader, err := compileForExecution(runCtx, opts.workflowPath, log)
	if err != nil {
		return err
	}
	defer loader.Shutdown(context.Background())

	clientOpts := castletrans.Options{
		Codec:    castletrans.Codec(opts.codec),
		TLSMode:  castletrans.TLSMode(opts.tlsMode),
		CAFile:   opts.tlsCA,
		CertFile: opts.tlsCert,
		KeyFile:  opts.tlsKey,
	}
	client, runID, err := setupCastleRun(runCtx, log, graph, src, opts.castleURL, opts.name, clientOpts, cancelRun)
	if err != nil {
		return err
	}
	defer client.Close()

	sink := &run.Sink{
		RunID:  runID,
		Client: client,
		Log:    log.With("run_id", runID),
		CheckpointFn: func(step string, attempt int) {
			cp := &StepCheckpoint{
				RunID:        runID,
				Workflow:     graph.Name,
				WorkflowPath: opts.workflowPath,
				CurrentStep:  step,
				Attempt:      attempt,
				StartedAt:    time.Now().UTC(),
				CastleURL:    opts.castleURL,
				OverseerID:   client.OverseerID(),
				Token:        client.Token(),
			}
			if cpErr := WriteStepCheckpoint(cp); cpErr != nil {
				log.Warn("failed to write step checkpoint; crash recovery may not work", "error", cpErr)
			}
		},
	}

	log.Info("starting run",
		"run_id", runID,
		"workflow", graph.Name,
		"file", filepath.Base(opts.workflowPath))

	state := &localRunState{
		PID:       os.Getpid(),
		RunID:     runID,
		Workflow:  graph.Name,
		CastleURL: opts.castleURL,
		StartedAt: time.Now().UTC(),
	}
	_ = writeLocalRunState(state)
	defer removeLocalRunState()
	defer RemoveStepCheckpoint(runID)

	eng := engine.New(graph, loader, sink, engine.WithVarOverrides(parseVarOverrides(opts.varOverrides)))
	if err := eng.Run(runCtx); err != nil {
		log.Error("run failed", "error", err)
		return err
	}
	log.Info("run completed", "run_id", runID)

	// If the run paused (ErrPaused would have been handled by the engine
	// loop; the Sink records the paused node for us), wait for a ResumeRun
	// control message then restart the engine from the paused node.
	for sink.IsPaused() {
		log.Info("run paused; waiting for resume signal", "run_id", runID, "node", sink.PausedAt())
		var resumeMsg *pb.ResumeRun
		select {
		case <-runCtx.Done():
			return runCtx.Err()
		case resumeMsg = <-client.ResumeCh():
		}
		if resumeMsg.RunId != runID {
			// Message for a different run; re-queue and continue waiting.
			log.Warn("received resume for unexpected run", "expected", runID, "got", resumeMsg.RunId)
			continue
		}
		log.Info("received resume signal", "run_id", runID, "signal", resumeMsg.Signal)
		pausedNode := sink.PausedAt()
		sink.ClearPaused()
		resumedEng := engine.New(graph, loader, sink,
			engine.WithResumedVars(eng.VarScope()),
			engine.WithResumePayload(resumeMsg.Payload),
		)
		if err := resumedEng.RunFrom(runCtx, pausedNode, 1); err != nil {
			log.Error("run failed after resume", "error", err)
			return err
		}
		eng = resumedEng
		log.Info("run resumed and completed", "run_id", runID)
	}

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
	client.Drain(drainCtx)
	drainCancel()
	return nil
}

func setupCastleRun(ctx context.Context, log *slog.Logger, graph *workflow.FSMGraph, src []byte, castleURL, name string, clientOpts castletrans.Options, cancelRun func()) (*castletrans.Client, string, error) {
	client, err := castletrans.NewClient(castleURL, log, clientOpts)
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
		return nil, "", fmt.Errorf("castle streams: %w", err)
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
	graph, diags := workflow.Compile(spec, schemas)
	if diags.HasErrors() {
		loader.Shutdown(context.Background())
		return nil, nil, nil, fmt.Errorf("compile: %s", diags.Error())
	}

	return src, graph, loader, nil
}

func ensureLocalModeSupported(graph *workflow.FSMGraph) error {
	// Signal-based wait nodes require an orchestrator to deliver the signal.
	for _, wn := range graph.Waits {
		if wn.Signal != "" {
			return errors.New("signal waits require an orchestrator (e.g. --castle <url>)")
		}
	}
	// Approval nodes always require an orchestrator.
	if len(graph.Approvals) > 0 {
		return errors.New("approval nodes require an orchestrator (e.g. --castle <url>)")
	}
	// Legacy step lifecycle checks kept for forward-compat.
	for _, step := range graph.Steps {
		if step.Lifecycle == "wait" {
			return errors.New("signal waits require an orchestrator (e.g. --castle <url>)")
		}
		if step.Lifecycle == "approval" {
			return errors.New("approval nodes require an orchestrator (e.g. --castle <url>)")
		}
	}
	for _, state := range graph.States {
		requires := strings.ToLower(strings.TrimSpace(state.Requires))
		switch requires {
		case "signal", "wait_signal", "wait.signal":
			return errors.New("signal waits require an orchestrator (e.g. --castle <url>)")
		case "approval", "wait_approval", "wait.approval":
			return errors.New("approval nodes require an orchestrator (e.g. --castle <url>)")
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
		if strings.TrimSpace(cp.CastleURL) != "" {
			continue
		}
		resumeOneLocalRun(ctx, log, cp, out, mode)
	}
}

func resumeOneLocalRun(ctx context.Context, log *slog.Logger, cp *StepCheckpoint, out io.Writer, mode outputMode) {
	graph, err := parseWorkflowFromPath(cp.WorkflowPath)
	if err != nil {
		log.Warn("cannot parse workflow for crashed local run; abandoning", "run_id", cp.RunID, "error", err)
		RemoveStepCheckpoint(cp.RunID)
		return
	}
	if err := ensureLocalModeSupported(graph); err != nil {
		log.Warn("local checkpoint requires castle; clearing", "run_id", cp.RunID, "error", err)
		RemoveStepCheckpoint(cp.RunID)
		return
	}

	nextAttempt := cp.Attempt + 1
	maxAttempts := 1 + graph.Policy.MaxStepRetries
	if nextAttempt > maxAttempts {
		sink := buildLocalSink(cp.RunID, out, mode, graph.StepOrder(), nil)
		reason := fmt.Sprintf("exceeded max_step_retries on resume at step %q (attempt %d)", cp.CurrentStep, nextAttempt)
		sink.OnRunFailed(reason, cp.CurrentStep)
		RemoveStepCheckpoint(cp.RunID)
		return
	}

	loader := plugin.NewLoader()
	loader.RegisterBuiltin(shell.Name, plugin.BuiltinFactoryForAdapter(shell.New()))
	defer loader.Shutdown(context.Background())

	checkpointFn := func(step string, attempt int) {
		next := *cp
		next.CurrentStep = step
		next.Attempt = attempt
		next.StartedAt = time.Now().UTC()
		if cpErr := WriteStepCheckpoint(&next); cpErr != nil {
			log.Warn("failed to update local checkpoint", "run_id", cp.RunID, "error", cpErr)
		}
	}
	sink := buildLocalSink(cp.RunID, out, mode, graph.StepOrder(), checkpointFn)
	sink.OnStepResumed(cp.CurrentStep, nextAttempt, "overseer_restart")

	eng := engine.New(graph, loader, sink)
	if runErr := eng.RunFrom(ctx, cp.CurrentStep, nextAttempt); runErr != nil {
		log.Error("resumed local run failed", "run_id", cp.RunID, "error", runErr)
	} else {
		log.Info("resumed local run completed", "run_id", cp.RunID)
	}
	RemoveStepCheckpoint(cp.RunID)
}
