// Package cli holds the cobra subcommands for the criteria binary.
package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/brokenbots/criteria/internal/adapters/shell"
	"github.com/brokenbots/criteria/internal/plugin"
	"github.com/brokenbots/criteria/workflow"
)

func NewValidateCmd() *cobra.Command {
	var subworkflowRoots []string

	cmd := &cobra.Command{
		Use:   "validate <workflow.hcl> [more.hcl ...]",
		Short: "Parse and validate a workflow HCL file without executing it",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			anyErr := false
			ctx := context.Background()
			for _, path := range args {
				spec, diags := workflow.ParseFile(path)
				if diags.HasErrors() {
					anyErr = true
					fmt.Fprintf(os.Stderr, "%s: parse failed:\n%s\n", path, diags.Error())
					continue
				}
				loader := plugin.NewLoader()
				loader.RegisterBuiltin(shell.Name, plugin.BuiltinFactoryForAdapter(shell.New()))
				schemas := collectSchemas(ctx, loader, spec, nil)
				_ = loader.Shutdown(ctx)

				_, diags = workflow.CompileWithOpts(spec, schemas, workflow.CompileOpts{
					WorkflowDir:         filepath.Dir(path),
					SubWorkflowResolver: &workflow.LocalSubWorkflowResolver{AllowedRoots: subworkflowRoots},
					Schemas:             schemas,
				})
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

	cmd.Flags().StringArrayVar(&subworkflowRoots, "subworkflow-root", nil, "Restrict subworkflow source resolution to this root path (repeatable; empty = no restriction)")
	return cmd
}
