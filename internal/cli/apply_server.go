package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/brokenbots/criteria/internal/engine"
	"github.com/brokenbots/criteria/internal/plugin"
	"github.com/brokenbots/criteria/internal/run"
	servertrans "github.com/brokenbots/criteria/internal/transport/server"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	"github.com/brokenbots/criteria/workflow"
)

func applyClientOptions(opts applyOptions) servertrans.Options { //nolint:gocritic // hugeParam: opts passes applyOptions by value; pointer conversion is a separate workstream
	return servertrans.Options{
		Codec:    servertrans.Codec(opts.codec),
		TLSMode:  servertrans.TLSMode(opts.tlsMode),
		CAFile:   opts.tlsCA,
		CertFile: opts.tlsCert,
		KeyFile:  opts.tlsKey,
	}
}

// buildServerSink constructs a run.Sink wired to the given server client.
// getVisits, if non-nil, is called on each checkpoint to capture the current
// per-step visit counts for crash-recovery persistence (W07).
func buildServerSink(ctx context.Context, client *servertrans.Client, runID string, graph *workflow.FSMGraph, workflowPath, serverURL string, log *slog.Logger, getVisits func() map[string]int) *run.Sink {
	return &run.Sink{
		RunID:  runID,
		Client: client,
		Log:    log.With("run_id", runID),
		Ctx:    ctx,
		CheckpointFn: func(step string, attempt int) {
			var visits map[string]int
			if getVisits != nil {
				visits = getVisits()
			}
			writeRunCheckpoint(log, runID, graph.Name, workflowPath, serverURL, step, attempt, client.CriteriaID(), client.Token(), visits)
		},
	}
}

func executeServerRun(ctx context.Context, log *slog.Logger, loader plugin.Loader, client *servertrans.Client, state *localRunState, graph *workflow.FSMGraph, opts applyOptions) error { //nolint:gocritic // hugeParam: opts passes applyOptions by value; pointer conversion is a separate workstream
	_ = writeLocalRunState(state)
	defer removeLocalRunState()
	defer RemoveStepCheckpoint(state.RunID)

	log.Info("starting run",
		"run_id", state.RunID,
		"workflow", graph.Name,
		"file", filepath.Base(opts.workflowPath))

	// Declare eng first so the checkpoint closure can capture live visit counts.
	var eng *engine.Engine
	sink := buildServerSink(ctx, client, state.RunID, graph, opts.workflowPath, opts.serverURL, log,
		func() map[string]int {
			if eng != nil {
				return eng.VisitCounts()
			}
			return nil
		})

	eng = engine.New(graph, loader, sink,
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

	drainCtx, drainCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	client.Drain(drainCtx)
	drainCancel()
	return nil
}

// drainResumeCycles handles the pause/resume loop: each time the sink is
// paused it waits for a matching ResumeRun message and restarts the engine
// from the paused node, updating eng to the most recently completed engine.
func drainResumeCycles(ctx context.Context, log *slog.Logger, loader plugin.Loader, sink *run.Sink, client *servertrans.Client, state *localRunState, graph *workflow.FSMGraph, opts applyOptions, eng *engine.Engine) error { //nolint:gocritic // hugeParam: opts passes applyOptions by value; pointer conversion is a separate workstream
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
			engine.WithResumedVisits(eng.VisitCounts()),
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

func runApplyServer(ctx context.Context, opts applyOptions) error { //nolint:gocritic // hugeParam: opts passes applyOptions by value; pointer conversion is a separate workstream
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	log := newApplyLogger()
	src, graph, loader, err := compileForExecution(runCtx, opts.workflowPath, log)
	if err != nil {
		return err
	}
	defer func() { _ = loader.Shutdown(context.WithoutCancel(runCtx)) }()

	copts := applyClientOptions(opts)
	client, runID, err := setupServerRun(runCtx, log, graph, src, opts.serverURL, opts.name, &copts, cancelRun)
	if err != nil {
		return err
	}
	defer client.Close()

	state := newLocalRunState(runID, graph.Name, opts.serverURL)
	return executeServerRun(runCtx, log, loader, client, state, graph, opts)
}

func setupServerRun(ctx context.Context, log *slog.Logger, graph *workflow.FSMGraph, src []byte, serverURL, name string, clientOpts *servertrans.Options, cancelRun func()) (*servertrans.Client, string, error) {
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
