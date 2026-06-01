package cli

import (
	"github.com/spf13/cobra"

	"github.com/brokenbots/criteria/internal/langserver"
)

func NewLangserverCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "langserver",
		Short:  "Start the Criteria LSP language server (reads JSON-RPC from stdin)",
		Hidden: false,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return langserver.Serve()
		},
	}
	return cmd
}
