// Criteria CLI entrypoint.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/brokenbots/criteria/internal/adapter/environment/sandbox"
	"github.com/brokenbots/criteria/internal/cli"
)

func main() {
	// Sandbox shim entry point: if the criteria binary is re-exec'd
	// as a pre-exec wrapper for a sandboxed adapter, apply the
	// restrictions and jump to the real adapter binary before any
	// CLI logic runs.
	if ran, err := sandbox.RunIfEnv(); ran {
		if err != nil {
			fmt.Fprintln(os.Stderr, "sandbox shim failed:", err)
			os.Exit(125)
		}
		// Should not reach here — RunIfEnv calls syscall.Exec.
		os.Exit(0)
	} else if err != nil {
		fmt.Fprintln(os.Stderr, "sandbox shim check failed:", err)
		os.Exit(125)
	}

	root := &cobra.Command{
		Use:           "criteria",
		Short:         "Criteria agent — local workflow executor",
		SilenceErrors: true,
	}
	root.AddCommand(cli.NewCompileCmd())
	root.AddCommand(cli.NewPlanCmd())
	root.AddCommand(cli.NewApplyCmd())
	root.AddCommand(cli.NewRunCmd())
	root.AddCommand(cli.NewValidateCmd())
	root.AddCommand(cli.NewSpecCmd())
	root.AddCommand(cli.NewStatusCmd())
	root.AddCommand(cli.NewStopCmd())
	root.AddCommand(cli.NewPauseCmd())
	root.AddCommand(cli.NewResumeCmd())
	root.AddCommand(cli.NewInspectCmd())
	root.AddCommand(cli.NewAdapterCmd())
	root.AddCommand(cli.NewLangserverCmd())
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
