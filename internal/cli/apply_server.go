package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/engine"
	"github.com/brokenbots/criteria/internal/run"
	servertrans "github.com/brokenbots/criteria/internal/transport/server"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	"github.com/brokenbots/criteria/workflow"
)

func applyClientOptions(opts applyOptions) servertrans.Options {
	return servertrans.Options{
		Codec:    servertrans.Codec(opts.codec),
		TLSMode:  servertrans.TLSMode(opts.tlsMode),
		CAFile:   opts.tlsCA,
		CertFile: opts.tlsCert,
		KeyFile:  opts.tlsKey,
	}
}

// resolveServerBootstrapToken resolves the --server-bootstrap-token value for
// the X-Server-Bootstrap Register header. A "file:" prefix reads the token
// from a file so mounted secrets never appear in process arguments or
// environment listings. Empty input disables bootstrap auth.
func resolveServerBootstrapToken(spec string) (string, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return "", nil
	}
	if !strings.HasPrefix(spec, "file:") {
		return spec, nil
	}
	path := strings.TrimPrefix(spec, "file:")
	if strings.TrimSpace(path) == "" {
		return "", errors.New("invalid --server-bootstrap-token file: prefix without a path")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read server bootstrap token from %q: %w", path, err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("server bootstrap token file %q is empty", path)
	}
	return token, nil
}

// buildServerSink constructs a run.Sink wired to the given publisher.
// authClient provides the criteria id and token persisted in crash-recovery
// checkpoints; it may be nil in tests that do not exercise checkpoints.
// getVisits, if non-nil, is called on each checkpoint to capture the current
// per-step visit counts for crash-recovery persistence (W07).
func buildServerSink(ctx context.Context, publisher run.Publisher, authClient *servertrans.Client, runID string, graph *workflow.FSMGraph, workflowPath, serverURL string, log *slog.Logger, getVisits func() map[string]int) *run.Sink {
	criteriaID, token := "", ""
	if authClient != nil {
		criteriaID, token = authClient.CriteriaID(), authClient.Token()
	}
	return &run.Sink{
		RunID:  runID,
		Client: publisher,
		Log:    log.With("run_id", runID),
		Ctx:    ctx,
		CheckpointFn: func(step string, attempt int) {
			var visits map[string]int
			if getVisits != nil {
				visits = getVisits()
			}
			writeRunCheckpoint(log, runID, graph.Name, workflowPath, serverURL, step, attempt, criteriaID, token, visits)
		},
	}
}

// dualWriteSink wraps a server sink with a LocalSink mirroring events into
// the ND-JSON events file so server-mode runs keep dual-writing after a
// crash resume. Returns the server sink unchanged when eventsOut is nil.
// Pause/resume tracking must keep using the raw *run.Sink; only the engine
// sink is wrapped.
func dualWriteSink(sink *run.Sink, runID string, eventsOut io.Writer) engine.Sink {
	local := eventsFileSink(runID, eventsOut)
	if local == nil {
		return sink
	}
	return run.NewMultiSink(sink, local)
}

// eventsFileSink returns a LocalSink mirroring events for the given run into
// the events file, or nil when dual-write is off (no --events-file).
func eventsFileSink(runID string, eventsOut io.Writer) *run.LocalSink {
	if eventsOut == nil {
		return nil
	}
	return &run.LocalSink{RunID: runID, Out: eventsOut}
}

func executeServerRun(ctx context.Context, log *slog.Logger, loader adapterhost.Loader, client *servertrans.Client, state *localRunState, graph *workflow.FSMGraph, opts applyOptions, eventsOut io.Writer) error {
	_ = writeLocalRunState(state)
	defer removeLocalRunState(state.RunID)
	defer RemoveStepCheckpoint(state.RunID)

	log.Info("starting run",
		"run_id", state.RunID,
		"workflow", graph.Name,
		"file", filepath.Base(opts.workflowPath))

	// Declare eng first so the checkpoint closure can capture live visit counts.
	var eng *engine.Engine
	sink := buildServerSink(ctx, client, client, state.RunID, graph, opts.workflowPath, opts.serverURL, log,
		func() map[string]int {
			if eng != nil {
				return eng.VisitCounts()
			}
			return nil
		})
	runSink := &terminalSuccessSink{Sink: sink}
	if eventsOut != nil {
		// Dual-write: mirror every engine event into the ND-JSON events file
		// in addition to the server stream, so operators consuming the file
		// keep working while the direct server stream is validated.
		runSink = &terminalSuccessSink{Sink: run.NewMultiSink(sink, &run.LocalSink{RunID: state.RunID, Out: eventsOut})}
	}

	eng, err := buildServerRunEngine(graph, loader, runSink, state, opts)
	if err != nil {
		return err
	}
	if err := eng.Run(ctx); err != nil {
		log.Error("run failed", "error", err)
		return err
	}
	log.Info("run completed", "run_id", state.RunID)

	if err := drainResumeCycles(ctx, log, loader, sink, runSink, client.ResumeCh(), state, graph, workflowDirFromPath(opts.workflowPath), eng); err != nil {
		return err
	}

	// Flush queued events before inspecting the terminal result so the server
	// receives the RunCompleted envelope regardless of success.
	drainCtx, drainCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	client.Drain(drainCtx)
	drainCancel()

	if finalState, success, ok := runSink.TerminalSuccess(); ok && !success {
		return fmt.Errorf("run completed with terminal state %q (success=false)", finalState)
	}
	return nil
}

// buildServerRunEngine wires the engine for a fresh server-mode run, including
// the run data dir so per-scope remote sessions can rotate tokens.
func buildServerRunEngine(graph *workflow.FSMGraph, loader adapterhost.Loader, sink engine.Sink, state *localRunState, opts applyOptions) (*engine.Engine, error) {
	auditPath, _ := auditLogPath(state.RunID)
	auditWriter := adapterhost.NewFileAuditWriter(auditPath)
	dataDir, err := runDataDir(state.RunID)
	if err != nil {
		return nil, err
	}
	mergedVars, err := mergeVarSources(opts.varFiles, opts.varOverrides)
	if err != nil {
		return nil, err
	}
	return engine.New(graph, loader, sink,
		engine.WithVarOverrides(mergedVars),
		engine.WithWorkflowDir(workflowDirFromPath(opts.workflowPath)),
		engine.WithAuditWriter(auditWriter),
		engine.WithDataDir(dataDir),
	), nil
}

// drainResumeCycles handles the pause/resume loop: each time the sink is
// paused it waits for a matching ResumeRun message on resumeCh and restarts
// the engine from the paused node, updating eng to the most recently
// completed engine. runSink is the sink passed to every engine instance so
// that terminal-state capture is consistent across the original run and all
// resume cycles.
func drainResumeCycles(ctx context.Context, log *slog.Logger, loader adapterhost.Loader, sink *run.Sink, runSink engine.Sink, resumeCh <-chan *pb.ResumeRun, state *localRunState, graph *workflow.FSMGraph, workflowDir string, eng *engine.Engine) error {
	dataDir, err := runDataDir(state.RunID)
	if err != nil {
		return fmt.Errorf("resolve run data dir: %w", err)
	}
	for sink.IsPaused() {
		log.Info("run paused; waiting for resume signal", "run_id", state.RunID, "node", sink.PausedAt())
		var resumeMsg *pb.ResumeRun
		select {
		case <-ctx.Done():
			return ctx.Err()
		case resumeMsg = <-resumeCh:
		}
		if resumeMsg == nil {
			return errors.New("resume channel closed")
		}
		if resumeMsg.RunId != state.RunID {
			log.Warn("received resume for unexpected run", "expected", state.RunID, "got", resumeMsg.RunId)
			continue
		}
		log.Info("received resume signal", "run_id", state.RunID, "signal", resumeMsg.Signal)
		pausedNode := sink.PausedAt()
		sink.ClearPaused()
		resumedEng := engine.New(graph, loader, runSink,
			engine.WithResumedVars(eng.VarScope()),
			engine.WithResumedVisits(eng.VisitCounts()),
			engine.WithResumePayload(resumeMsg.Payload),
			engine.WithWorkflowDir(workflowDir),
			engine.WithDataDir(dataDir),
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

	// Resolve bootstrap auth before any server interaction so a bad token
	// spec fails fast with a CLI-adjacent error.
	bootstrapToken, err := resolveServerBootstrapToken(opts.serverBootstrapToken)
	if err != nil {
		return err
	}

	// Open the events file up front so a bad events path fails fast before
	// any server interaction, mirroring local mode (the file is created even
	// when compilation later fails). A nil writer disables dual-write and
	// leaves server-only behavior unchanged.
	eventsOut, closeEvents, err := openServerEventsWriter(opts.eventsPath)
	if err != nil {
		return err
	}
	defer closeEvents()

	log := newApplyLogger()
	src, graph, loader, err := compileForExecution(runCtx, opts.workflowPath, log, opts.warnsAsErrors, opts.allowUnsigned, opts.subworkflowRoots...)
	if err != nil {
		return err
	}
	defer func() { _ = loader.Shutdown(context.WithoutCancel(runCtx)) }()

	copts := applyClientOptions(opts)
	copts.BootstrapToken = bootstrapToken
	client, runID, err := setupServerRun(runCtx, log, graph, src, opts.serverURL, opts.name, &copts, cancelRun, eventsOut)
	if err != nil {
		return err
	}
	defer client.Close()

	state := newLocalRunState(runID, graph.Name, opts.serverURL)
	return executeServerRun(runCtx, log, loader, client, state, graph, opts, eventsOut)
}

func setupServerRun(ctx context.Context, log *slog.Logger, graph *workflow.FSMGraph, src []byte, serverURL, name string, clientOpts *servertrans.Options, cancelRun func(), eventsOut io.Writer) (*servertrans.Client, string, error) {
	client, err := servertrans.NewClient(serverURL, log, *clientOpts)
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

	resumeInFlightRuns(ctx, log, clientOpts, eventsOut)

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
