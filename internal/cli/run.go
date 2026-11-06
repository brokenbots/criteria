package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/brokenbots/criteria/internal/envreg"
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
		varFiles     []string
	)
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Deprecated alias for apply --server",
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			fmt.Fprintln(os.Stderr, "warning: `criteria run` is deprecated; use `criteria apply <workflow.chcl|workflow.hcl> --server <url>`")

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
				varFiles:     varFiles,
			})
		},
	}
	addRunFlags(cmd, &workflowPath, &serverURL, &name, &codec, &tlsMode, &tlsCA, &tlsCert, &tlsKey, &varFiles)
	return cmd
}

func addRunFlags(cmd *cobra.Command, workflowPath, serverURL, name, codec, tlsMode, tlsCA, tlsCert, tlsKey *string, varFiles *[]string) {
	cmd.Flags().StringVar(workflowPath, "workflow", envreg.CRITERIAWorkflow.StringOr(""), "Path to workflow .chcl or .hcl file (or CRITERIA_WORKFLOW)")
	cmd.Flags().StringArrayVar(varFiles, "var-file", nil, "Load variable overrides from a .chcl, .hcl, or .json file (repeatable)")
	cmd.Flags().StringVar(serverURL, "server", envreg.CRITERIAServerURL.StringOr("http://localhost:8080"), "Server base URL (or CRITERIA_SERVER_URL)")
	cmd.Flags().StringVar(name, "name", envreg.CRITERIAName.StringOr(""), "Agent name (defaults to hostname, or CRITERIA_NAME)")
	cmd.Flags().StringVar(codec, "server-codec", envreg.CRITERIAServerCodec.StringOr("proto"), "Connect codec: 'proto' (default) or 'json' (or CRITERIA_SERVER_CODEC)")
	cmd.Flags().StringVar(tlsMode, "server-tls", envreg.CRITERIAServerTLS.StringOr(""), "TLS mode: 'disable' | 'tls' | 'mtls'. Defaults to 'disable' for http:// and 'tls' for https:// (or CRITERIA_SERVER_TLS)")
	cmd.Flags().StringVar(tlsCA, "tls-ca", envreg.CRITERIATLSCA.StringOr(""), "Path to CA bundle PEM (or CRITERIA_TLS_CA)")
	cmd.Flags().StringVar(tlsCert, "tls-cert", envreg.CRITERIATLSCert.StringOr(""), "Path to client cert PEM for mTLS (or CRITERIA_TLS_CERT)")
	cmd.Flags().StringVar(tlsKey, "tls-key", envreg.CRITERIATLSKey.StringOr(""), "Path to client key PEM for mTLS (or CRITERIA_TLS_KEY)")
}
