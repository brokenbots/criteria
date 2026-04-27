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
		Short: "Deprecated alias for apply --castle",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(os.Stderr, "warning: `overseer run` is deprecated; use `overseer apply <workflow.hcl> --castle <url>`")

			if workflowPath == "" {
				return fmt.Errorf("--workflow is required")
			}
			if castleURL == "" {
				return fmt.Errorf("--castle is required")
			}

			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			return runApply(ctx, applyOptions{
				workflowPath: workflowPath,
				castleURL:    castleURL,
				name:         name,
				codec:        codec,
				tlsMode:      tlsMode,
				tlsCA:        tlsCA,
				tlsCert:      tlsCert,
				tlsKey:       tlsKey,
			})
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
