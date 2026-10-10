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

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/bootgate"
	"github.com/brokenbots/criteria/internal/engine"
	"github.com/brokenbots/criteria/internal/run"
	servertrans "github.com/brokenbots/criteria/internal/transport/server"
	"github.com/brokenbots/criteria/internal/tunables"
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
// fingerprint is the invocation identity (CRI-125) persisted in checkpoints so
// a restarted runner can match them and resume instead of forking a second
// run. getVisits, if non-nil, is called on each checkpoint to capture the
// current per-step visit counts for crash-recovery persistence (W07).
func buildServerSink(ctx context.Context, publisher run.Publisher, authClient *servertrans.Client, runID string, graph *workflow.FSMGraph, workflowPath, serverURL, fingerprint string, log *slog.Logger, getVisits func() map[string]int) *run.Sink {
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
			writeRunCheckpoint(log, runID, graph.Name, workflowPath, serverURL, fingerprint, step, attempt, criteriaID, token, visits)
		},
	}
}

// buildServerRunSinks assembles the sink chain for a server-side run: the
// primary server sink (with the live visit-count checkpoint closure), the
// terminal-success wrapper the terminal drain inspects, and the ND-JSON
// dual-write mirror when eventsOut is set. emitWorkflowGraphsServer runs
// here so the once-per-run WorkflowGraphs event (CRI-278) still lands before
// the engine starts, on both the server stream and the dual-write mirror.
func buildServerRunSinks(ctx context.Context, log *slog.Logger, client *servertrans.Client, state *localRunState, graph *workflow.FSMGraph, opts applyOptions, eventsOut io.Writer, fingerprint string, getVisits func() map[string]int) (*terminalSuccessSink, *run.Sink, *run.LocalSink) {
	sink := buildServerSink(ctx, client, client, state.RunID, graph, opts.workflowPath, opts.serverURL, fingerprint, log, getVisits)
	runSink := &terminalSuccessSink{Sink: sink}
	eventsMirror := eventsFileSink(state.RunID, eventsOut)
	if eventsMirror != nil {
		runSink = &terminalSuccessSink{Sink: run.NewMultiSink(sink, eventsMirror)}
	}
	emitWorkflowGraphsServer(ctx, log, sink, eventsMirror, graph)
	return runSink, sink, eventsMirror
}

// dualWriteSink wraps an engine sink with a LocalSink mirroring events into
// the ND-JSON events file so server-mode runs keep dual-writing after a
// crash resume. Returns the sink unchanged when eventsOut is nil. Only the
// engine sink is wrapped: pause/resume tracking must keep using the raw
// *run.Sink.
func dualWriteSink(sink engine.Sink, runID string, eventsOut io.Writer) engine.Sink {
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
	// CRI-125: checkpoints written during this run carry the invocation
	// fingerprint so a restarted runner can match and resume this run
	// instead of forking a second one.
	fingerprint := runIdentityFingerprint(opts.workflowPath, opts.serverURL, opts.varFiles, opts.varOverrides)
	runSink, sink, _ := buildServerRunSinks(ctx, log, client, state, graph, opts, eventsOut, fingerprint, func() map[string]int {
		if eng != nil {
			return eng.VisitCounts()
		}
		return nil
	})

	eng, err := buildServerRunEngine(graph, loader, runSink, state, opts, fingerprint, client.AgentPromptCh(), client.CriteriaID())
	if err != nil {
		return err
	}
	// CRI-254: consume orchestrator-issued pause_run commands and drive the
	// boundary-pause machinery (drain-first, durable checkpoint); the
	// consumer stops once the run is terminal and is joined at run return.
	pauseRouter := newControlPauseRouter(state.RunID, sink, log)
	pauseDone, pauseCancel := pauseRouter.startPauseConsume(ctx, eng, client.PauseRunCh())
	defer func() {
		pauseCancel()
		<-pauseDone
	}()
	if err := eng.Run(ctx); err != nil {
		log.Error("run failed", "error", err)
		return err
	}
	log.Info("run completed", "run_id", state.RunID)

	// KB-56: capture, don't return on, a resume-loop failure so the terminal
	// drain below still runs. The pending step.outcome / RunCompleted tail is
	// what transitions the server-side run row out of 'running'; an early
	// error return drops that tail (the observed stuck-'running' castle row).
	resumeErr := drainResumeCycles(ctx, log, loader, sink, runSink, client.ResumeCh(), client.AgentPromptCh(), client.CriteriaID(), state, graph, workflowDirFromPath(opts.workflowPath), eng, fingerprint, pauseRouter)

	// Flush queued events before inspecting the terminal result so the server
	// receives the RunCompleted envelope regardless of success.
	drainCtx, drainCancel := context.WithTimeout(context.WithoutCancel(ctx), terminalDrainTimeout)
	client.Drain(drainCtx)
	drainCancel()

	if resumeErr != nil {
		return resumeErr
	}

	if finalState, success, ok := runSink.TerminalSuccess(); ok && !success {
		return terminalStateFailureError(finalState)
	}
	return nil
}

// buildServerRunEngine wires the engine for a fresh server-mode run, including
// the run data dir so per-scope remote sessions can rotate tokens. The
// invocation fingerprint (CRI-125) is recorded in the run data directory and
// drives CRI-304 cross-run per-scope adoption. promptCh/ownerID wire the
// run's agent-prompt path (ADR-0006 D1/D4); a nil promptCh disables it.
func buildServerRunEngine(graph *workflow.FSMGraph, loader adapterhost.Loader, sink engine.Sink, state *localRunState, opts applyOptions, fingerprint string, promptCh <-chan *pb.AgentPrompt, promptOwnerID string) (*engine.Engine, error) {
	auditPath, _ := auditLogPath(state.RunID)
	auditWriter := adapterhost.NewFileAuditWriter(auditPath)
	// CRI-202: shared base includes the checkpoint surface (snapshot base +
	// run id) so adapter state persists for later restore.
	dataDir, baseOpts, err := serverRunEngineOptions(state.RunID, opts.workflowPath)
	if err != nil {
		return nil, err
	}
	mergedVars, err := mergeVarSources(opts.varFiles, opts.varOverrides)
	if err != nil {
		return nil, err
	}
	engOpts := append([]engine.Option{
		engine.WithVarOverrides(mergedVars),
		engine.WithAuditWriter(auditWriter),
		engine.WithAgentPrompts(promptCh, promptOwnerID, state.RunID),
	}, baseOpts...)
	engOpts = append(engOpts, engineAdoptionOptions(dataDir, fingerprint, state.RunID)...)
	return engine.New(graph, loader, sink, engOpts...), nil
}

// drainResumeCycles handles the pause/resume loop: each time the sink is
// paused it waits for a matching ResumeRun message on resumeCh and restarts
// the engine from the paused node, updating eng to the most recently
// completed engine. runSink is the sink passed to every engine instance so
// that terminal-state capture is consistent across the original run and all
// resume cycles. The invocation fingerprint (CRI-125) is recorded in the run
// data directory and lets each resumed engine adopt surviving per-scope
// instances from prior invocations of the same run (CRI-304); callers without
// a fingerprint (agent runs) pass "" and get neither marker nor adoption.
// pauseRouter (CRI-254, nil in tests) is re-pinned to each resumed engine
// before RunFrom starts it, so pause latches address the engine that is
// actually running mid-flight.
func drainResumeCycles(ctx context.Context, log *slog.Logger, loader adapterhost.Loader, sink *run.Sink, runSink engine.Sink, resumeCh <-chan *pb.ResumeRun, promptCh <-chan *pb.AgentPrompt, promptOwnerID string, state *localRunState, graph *workflow.FSMGraph, workflowDir string, eng *engine.Engine, fingerprint string, pauseRouter *controlPauseRouter) error {
	// CRI-202: shared base includes the checkpoint surface (snapshot base +
	// run id) so the resumed engine saves restored-session state and restores
	// adapter checkpoints from the original engine, exactly like a local
	// resume. Without it, wireCheckpointStore and bootstrapSessionsForResume
	// would silently skip checkpointing and the resume would be a fresh start.
	dataDir, baseOpts, err := serverRunEngineOptions(state.RunID, workflowDir)
	if err != nil {
		return fmt.Errorf("resolve engine options for resume: %w", err)
	}
	var adoptionOpts []engine.Option
	if fingerprint != "" {
		adoptionOpts = engineAdoptionOptions(dataDir, fingerprint, state.RunID)
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
		resumedOpts := []engine.Option{
			engine.WithResumedVars(eng.VarScope()),
			engine.WithResumedVisits(eng.VisitCounts()),
			engine.WithResumePayload(resumeMsg.Payload),
		}
		resumedOpts = append(resumedOpts, baseOpts...)
		// ADR-0006: resumed engines stay wired to the run's prompt channel so
		// injected prompts route to the replayed steps too.
		resumedOpts = append(resumedOpts, engine.WithAgentPrompts(promptCh, promptOwnerID, state.RunID))
		// CRI-304: resumed engines adopt surviving per-scope instances from
		// prior invocations of the same run instead of rotating fresh tokens
		// that would wedge the replay behind the shim's handshake timeout.
		resumedOpts = append(resumedOpts, adoptionOpts...)
		resumedEng := engine.New(graph, loader, runSink, resumedOpts...)
		// CRI-254 R3: pin the router to the resumed engine BEFORE it starts
		// running (mirrors localRunControl.setEngine in apply_resume.go): a
		// pause delivered while the resumed run executes must address the
		// engine that is actually in flight, not the one whose loop exited at
		// the pause — the old pin made RequestPause return not-running and
		// silently dropped the command.
		if pauseRouter != nil {
			pauseRouter.setEngine(resumedEng)
		}
		eng = resumedEng
		if err := resumedEng.RunFrom(ctx, pausedNode, 1); err != nil {
			log.Error("run failed after resume", "error", err)
			return err
		}
		log.Info("run resumed and completed", "run_id", state.RunID)
	}
	return nil
}

func runApplyServer(ctx context.Context, opts applyOptions) error {
	if strings.TrimSpace(opts.answersPath) != "" {
		return errors.New("--answers applies to local runs only; server runs resolve approvals through the orchestrator")
	}
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	log := opts.log
	if log == nil {
		log = newApplyLogger()
	}

	// KB-234 boot gate: before any castle interaction, resolve this
	// runner's operator identity and consult the operator's view of its
	// CriteriaRun. Rule 3 of KB-233 covers adoption (checkpointed runs that
	// castle reports terminal exit 0); a FRESH start has no run identity to
	// ask castle about, so the mechanical check derives from the CR object
	// itself. A terminal CR — or a CR deleted while this pod is still alive
	// — means the operator will never provision for this boot again, and
	// registering or creating a run would mint a pending-forever castle run
	// (the KB-225 boot-1 orphan). Exit 0 instead.
	gate, gateEnabled, err := bootgate.Configure(os.LookupEnv, tunables.FromEnv().BootGateProbeTimeout)
	if err != nil {
		return err
	}
	if gateEnabled {
		outcome, detail := gate.Check(runCtx)
		switch outcome {
		case bootgate.OutcomeTerminal, bootgate.OutcomeDeleted:
			log.Info("runner boot gate: operator CriteriaRun view blocks this boot; exiting without registering with castle",
				"outcome", outcome.String(), "phase", detail, "ticket", gate.Ticket, "job", gate.Job)
			return nil
		case bootgate.OutcomeUnknown:
			// Fail open: an unavailable view is not an operator statement
			// that the CR is dead. The residual exposure stays bounded by
			// KB-233 rule 1 (terminal CRs delete runner Job pods) and castle
			// rule 2 (created-never-started runs are reaped).
			log.Warn("runner boot gate: operator CriteriaRun view unavailable; proceeding",
				"detail", detail, "ticket", gate.Ticket, "job", gate.Job)
		default:
			log.Info("runner boot gate: operator CriteriaRun view is live; proceeding",
				"phase", detail, "ticket", gate.Ticket, "job", gate.Job)
		}
	}

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

	src, graph, loader, err := compileForExecution(runCtx, opts.workflowPath, log, opts.warnsAsErrors, opts.allowUnsigned, opts.subworkflowRoots...)
	if err != nil {
		return err
	}
	defer func() { _ = loader.Shutdown(context.WithoutCancel(runCtx)) }()

	copts := applyClientOptions(opts)
	copts.BootstrapToken = bootstrapToken
	// CRI-125: identify this invocation so a restarted runner resumes (or
	// keeps failed) the original run instead of forking a second run with a
	// fresh run_id against stale adapter state.
	fingerprint := runIdentityFingerprint(opts.workflowPath, opts.serverURL, opts.varFiles, opts.varOverrides)
	client, runID, resumedMatching, resumeErr, err := setupServerRun(runCtx, log, graph, src, opts.serverURL, opts.name, &copts, cancelRun, eventsOut, fingerprint)
	if err != nil {
		return err
	}
	defer client.Close()
	if resumedMatching {
		log.Info("in-flight run for this identity resumed; not starting a second run",
			"file", filepath.Base(opts.workflowPath),
			"server", opts.serverURL)
		// CRI-125: surface the resumed run's outcome — suppressing the second
		// run must not also mask the original run's failure with exit 0.
		return resumeErr
	}

	state := newLocalRunState(runID, graph.Name, opts.serverURL)
	// CRI-225: publish the resolved workflow origin for this fresh server
	// run; resumed-matching invocations return above and leave the original
	// run's record untouched. Local sources (nil origin) record nothing.
	publishRunMetadata(runID, opts.origin, log)
	return executeServerRun(runCtx, log, loader, client, state, graph, opts, eventsOut)
}

// setupServerRun registers with the server and creates a fresh run, resuming
// any in-flight checkpointed runs first (CRI-125 crash recovery). resumed
// reports whether a checkpoint matching fingerprint was consumed by the
// resume pass: the caller must then NOT create a fresh run (runID is empty in
// that case) and must propagate resumeErr, which carries the resumed run's
// outcome.
func setupServerRun(ctx context.Context, log *slog.Logger, graph *workflow.FSMGraph, src []byte, serverURL, name string, clientOpts *servertrans.Options, cancelRun func(), eventsOut io.Writer, fingerprint string) (client *servertrans.Client, runID string, resumed bool, resumeErr, err error) {
	client, err = servertrans.NewClient(serverURL, log, *clientOpts)
	if err != nil {
		return nil, "", false, nil, err
	}
	hostname, _ := os.Hostname()
	if name == "" {
		name = hostname
	}
	if err := client.Register(ctx, name, hostname, "0.1.0"); err != nil {
		client.Close()
		return nil, "", false, nil, fmt.Errorf("register: %w", err)
	}

	matched, outcome := resumeInFlightRuns(ctx, log, clientOpts, eventsOut, fingerprint)
	if matched {
		return client, "", true, outcome, nil
	}

	runID, err = client.CreateRun(ctx, graph.Name, string(src))
	if err != nil {
		client.Close()
		return nil, "", false, nil, fmt.Errorf("create run: %w", err)
	}
	if err := client.StartStreams(ctx, runID); err != nil {
		client.Close()
		return nil, "", false, nil, fmt.Errorf("server streams: %w", err)
	}
	client.StartHeartbeat(ctx, tunables.FromEnv().AgentHeartbeatInterval)

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

	return client, runID, false, nil, nil
}
