// Overseer CLI entrypoint.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/brokenbots/overseer/internal/cli"
)

func main() {
	root := &cobra.Command{
		Use:   "overseer",
		Short: "Overlord Overseer — local workflow executor",
	}
	root.AddCommand(cli.NewCompileCmd())
	root.AddCommand(cli.NewPlanCmd())
	root.AddCommand(cli.NewApplyCmd())
	root.AddCommand(cli.NewRunCmd())
	root.AddCommand(cli.NewValidateCmd())
	root.AddCommand(cli.NewStatusCmd())
	root.AddCommand(cli.NewStopCmd())
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
