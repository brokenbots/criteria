package cli

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
)

type applyOptions struct {
	workflowPath     string
	serverURL        string
	eventsPath       string
	name             string
	codec            string
	tlsMode          string
	tlsCA            string
	tlsCert          string
	tlsKey           string
	varOverrides     []string     // raw "key=value" pairs from --var flags
	output           string       // "auto" | "concise" | "json"
	subworkflowRoots []string     // --subworkflow-root flag (repeatable); populates AllowedRoots on LocalSubWorkflowResolver
	stdin            io.Reader    // stdin for local-mode approval prompts; nil → os.Stdin
	log              *slog.Logger // nil → newApplyLogger(); injectable for tests
}

func NewApplyCmd() *cobra.Command {
	var opts applyOptions

	cmd := &cobra.Command{
		Use:   "apply <workflow.hcl|dir>",
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
	cmd.Flags().StringArrayVar(&opts.subworkflowRoots, "subworkflow-root", nil, "Restrict subworkflow source resolution to this root path (repeatable; empty = no restriction)")
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
