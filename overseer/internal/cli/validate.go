// Package cli holds the cobra subcommands for the overseer binary.
package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/brokenbots/overlord/workflow"
)

func NewValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate <workflow.hcl> [more.hcl ...]",
		Short: "Parse and validate a workflow HCL file without executing it",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			anyErr := false
			for _, path := range args {
				spec, diags := workflow.ParseFile(path)
				if diags.HasErrors() {
					anyErr = true
					fmt.Fprintf(os.Stderr, "%s: parse failed:\n%s\n", path, diags.Error())
					continue
				}
				_, diags = workflow.Compile(spec)
				if diags.HasErrors() {
					anyErr = true
					fmt.Fprintf(os.Stderr, "%s: compile failed:\n%s\n", path, diags.Error())
					continue
				}
				fmt.Printf("%s: ok\n", path)
				if len(diags) > 0 {
					fmt.Fprintf(os.Stderr, "%s: warnings:\n%s\n", path, diags.Error())
				}
			}
			if anyErr {
				os.Exit(1)
			}
			return nil
		},
	}
}
