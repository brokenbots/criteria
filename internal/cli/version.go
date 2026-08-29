package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/brokenbots/criteria/workflow/version"
)

// NewVersionCmd returns the `criteria version` command.
func NewVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version of the criteria binary",
		Long:  `Print the version of the currently running criteria binary.`,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return printVersion(cmd.OutOrStdout())
		},
	}
}

func printVersion(w io.Writer) error {
	info := version.Current()
	_, err := fmt.Fprintln(w, info.Display)
	return err
}
