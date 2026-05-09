package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

func NewRunCmd() *cobra.Command {
	var (
		workflowPath string
		serverURL    string
		name         string
		codec        string
		tlsMode      string
		tlsCA        string
		tlsCert      string
		tlsKey       string
	)
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Deprecated alias for apply --server",
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			fmt.Fprintln(os.Stderr, "warning: `criteria run` is deprecated; use `criteria apply <workflow.hcl> --server <url>`")

			if workflowPath == "" {
				return fmt.Errorf("--workflow is required")
			}
			if serverURL == "" {
				return fmt.Errorf("--server is required")
			}

			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			return runApply(ctx, applyOptions{
				workflowPath: workflowPath,
				serverURL:    serverURL,
				name:         name,
				codec:        codec,
				tlsMode:      tlsMode,
				tlsCA:        tlsCA,
				tlsCert:      tlsCert,
				tlsKey:       tlsKey,
			})
		},
	}
	cmd.Flags().StringVar(&workflowPath, "workflow", envOrDefault("CRITERIA_WORKFLOW", ""), "Path to workflow .hcl file (or CRITERIA_WORKFLOW)")
	cmd.Flags().StringVar(&serverURL, "server", envOrDefault("CRITERIA_SERVER_URL", "http://localhost:8080"), "Server base URL (or CRITERIA_SERVER_URL)")
	cmd.Flags().StringVar(&name, "name", envOrDefault("CRITERIA_NAME", ""), "Agent name (defaults to hostname, or CRITERIA_NAME)")
	cmd.Flags().StringVar(&codec, "server-codec", envOrDefault("CRITERIA_SERVER_CODEC", "proto"), "Connect codec: 'proto' (default) or 'json' (or CRITERIA_SERVER_CODEC)")
	cmd.Flags().StringVar(&tlsMode, "server-tls", envOrDefault("CRITERIA_SERVER_TLS", ""), "TLS mode: 'disable' | 'tls' | 'mtls'. Defaults to 'disable' for http:// and 'tls' for https:// (or CRITERIA_SERVER_TLS)")
	cmd.Flags().StringVar(&tlsCA, "tls-ca", envOrDefault("CRITERIA_TLS_CA", ""), "Path to CA bundle PEM (or CRITERIA_TLS_CA)")
	cmd.Flags().StringVar(&tlsCert, "tls-cert", envOrDefault("CRITERIA_TLS_CERT", ""), "Path to client cert PEM for mTLS (or CRITERIA_TLS_CERT)")
	cmd.Flags().StringVar(&tlsKey, "tls-key", envOrDefault("CRITERIA_TLS_KEY", ""), "Path to client key PEM for mTLS (or CRITERIA_TLS_KEY)")
	return cmd
}
