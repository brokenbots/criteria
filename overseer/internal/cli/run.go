package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/brokenbots/overlord/overseer/internal/adapters/copilot"
	"github.com/brokenbots/overlord/overseer/internal/adapters/shell"
	"github.com/brokenbots/overlord/overseer/internal/dispatcher"
	"github.com/brokenbots/overlord/overseer/internal/engine"
	"github.com/brokenbots/overlord/overseer/internal/run"
	castletrans "github.com/brokenbots/overlord/overseer/internal/transport/castle"
	"github.com/brokenbots/overlord/workflow"
)

func NewRunCmd() *cobra.Command {
	var (
		workflowPath string
		castleURL    string
		name         string
		codec        string
		tlsMode      string
		tlsCA        string
		tlsCert      string
		tlsKey       string
	)
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Execute a workflow against a Castle",
		RunE: func(cmd *cobra.Command, args []string) error {
			if workflowPath == "" {
				return fmt.Errorf("--workflow is required")
			}
			if castleURL == "" {
				return fmt.Errorf("--castle is required")
			}

			log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			// Parse + compile workflow.
			src, err := os.ReadFile(workflowPath)
			if err != nil {
				return err
			}
			spec, diags := workflow.Parse(workflowPath, src)
			if diags.HasErrors() {
				return fmt.Errorf("parse: %s", diags.Error())
			}
			graph, diags := workflow.Compile(spec)
			if diags.HasErrors() {
				return fmt.Errorf("compile: %s", diags.Error())
			}

			// Set up Castle Connect transport.
			clientOpts := castletrans.Options{
				Codec:    castletrans.Codec(codec),
				TLSMode:  castletrans.TLSMode(tlsMode),
				CAFile:   tlsCA,
				CertFile: tlsCert,
				KeyFile:  tlsKey,
			}
			client, err := castletrans.NewClient(castleURL, log, clientOpts)
			if err != nil {
				return err
			}
			hostname, _ := os.Hostname()
			if name == "" {
				name = hostname
			}
			if err := client.Register(ctx, name, hostname, "0.1.0"); err != nil {
				return fmt.Errorf("register: %w", err)
			}
			runID, err := client.CreateRun(ctx, graph.Name, string(src))
			if err != nil {
				return fmt.Errorf("create run: %w", err)
			}
			if err := client.StartStreams(ctx, runID); err != nil {
				return fmt.Errorf("castle streams: %w", err)
			}
			defer client.Close()
			client.StartHeartbeat(ctx, 10*time.Second)

			go func() {
				for {
					select {
					case <-ctx.Done():
						return
					case cancelRunID := <-client.RunCancelCh():
						if cancelRunID == runID {
							log.Info("received run.cancel control", "run_id", runID)
							cancel()
						}
					}
				}
			}()

			// Wire dispatcher.
			disp := dispatcher.New()
			disp.Register(shell.New())
			disp.Register(copilot.New())

			sink := &run.Sink{
				RunID:  runID,
				Client: client,
				Log:    log.With("run_id", runID),
			}

			log.Info("starting run",
				"run_id", runID,
				"workflow", graph.Name,
				"file", filepath.Base(workflowPath))

			state := &localRunState{
				PID:       os.Getpid(),
				RunID:     runID,
				Workflow:  graph.Name,
				CastleURL: castleURL,
				StartedAt: time.Now().UTC(),
			}
			_ = writeLocalRunState(state)
			defer removeLocalRunState()

			eng := engine.New(graph, disp, sink)
			if err := eng.Run(ctx); err != nil {
				log.Error("run failed", "error", err)
				return err
			}
			log.Info("run completed", "run_id", runID)
			// Deterministically drain any trailing envelopes before Close()
			// tears the SubmitEvents stream down. Bounded by a short timeout
			// so a stalled Castle cannot hang shutdown.
			drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
			client.Drain(drainCtx)
			drainCancel()
			return nil
		},
	}
	cmd.Flags().StringVar(&workflowPath, "workflow", envOrDefault("OVERSEER_WORKFLOW", ""), "Path to workflow .hcl file (or OVERSEER_WORKFLOW)")
	cmd.Flags().StringVar(&castleURL, "castle", envOrDefault("OVERSEER_CASTLE_URL", "http://localhost:8080"), "Castle base URL (or OVERSEER_CASTLE_URL)")
	cmd.Flags().StringVar(&name, "name", envOrDefault("OVERSEER_NAME", ""), "Overseer name (defaults to hostname, or OVERSEER_NAME)")
	cmd.Flags().StringVar(&codec, "castle-codec", envOrDefault("OVERSEER_CASTLE_CODEC", "proto"), "Connect codec: 'proto' (default) or 'json' (or OVERSEER_CASTLE_CODEC)")
	cmd.Flags().StringVar(&tlsMode, "castle-tls", envOrDefault("OVERSEER_CASTLE_TLS", ""), "TLS mode: 'disable' | 'tls' | 'mtls'. Defaults to 'disable' for http:// and 'tls' for https:// (or OVERSEER_CASTLE_TLS)")
	cmd.Flags().StringVar(&tlsCA, "tls-ca", envOrDefault("OVERSEER_TLS_CA", ""), "Path to CA bundle PEM (or OVERSEER_TLS_CA)")
	cmd.Flags().StringVar(&tlsCert, "tls-cert", envOrDefault("OVERSEER_TLS_CERT", ""), "Path to client cert PEM for mTLS (or OVERSEER_TLS_CERT)")
	cmd.Flags().StringVar(&tlsKey, "tls-key", envOrDefault("OVERSEER_TLS_KEY", ""), "Path to client key PEM for mTLS (or OVERSEER_TLS_KEY)")
	return cmd
}
