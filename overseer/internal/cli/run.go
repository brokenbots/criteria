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

	"github.com/google/uuid"
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

			// Set up Castle transport.
			client, err := castletrans.NewClient(castleURL, log)
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
			if err := client.ConnectWS(ctx); err != nil {
				return fmt.Errorf("ws connect: %w", err)
			}
			defer client.Close()
			client.StartHeartbeat(ctx, 10*time.Second)

			// Wire dispatcher.
			disp := dispatcher.New()
			disp.Register(shell.New())
			disp.Register(copilot.New())

			sink := &run.Sink{
				RunID:         runID,
				CorrelationID: uuid.NewString(),
				Client:        client,
				Log:           log.With("run_id", runID),
			}

			log.Info("starting run",
				"run_id", runID,
				"workflow", graph.Name,
				"file", filepath.Base(workflowPath))

			eng := engine.New(graph, disp, sink)
			if err := eng.Run(ctx); err != nil {
				log.Error("run failed", "error", err)
				return err
			}
			log.Info("run completed", "run_id", runID)
			// Give the WS a moment to flush trailing events.
			time.Sleep(200 * time.Millisecond)
			return nil
		},
	}
	cmd.Flags().StringVar(&workflowPath, "workflow", "", "Path to workflow .hcl file")
	cmd.Flags().StringVar(&castleURL, "castle", "http://localhost:8080", "Castle base URL")
	cmd.Flags().StringVar(&name, "name", "", "Overseer name (defaults to hostname)")
	return cmd
}
