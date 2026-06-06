// Package cli holds the cobra subcommands for the criteria binary.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hashicorp/hcl/v2"
	"github.com/spf13/cobra"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/diagutil"
	"github.com/brokenbots/criteria/workflow"
)

func NewValidateCmd() *cobra.Command {
	var (
		subworkflowRoots []string
		diagJSONFlag     bool
		warnsAsErrors    bool
	)

	cmd := &cobra.Command{
		Use:   "validate <workflow.chcl|workflow.hcl|dir> [more ...]",
		Short: "Parse and validate a workflow HCL file or directory without executing it",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			if runValidate(args, subworkflowRoots, diagJSONFlag, warnsAsErrors) {
				os.Exit(1)
			}
			return nil
		},
	}

	cmd.Flags().StringArrayVar(&subworkflowRoots, "subworkflow-root", nil, "Restrict subworkflow source resolution to this root path (repeatable; empty = no restriction)")
	cmd.Flags().BoolVar(&diagJSONFlag, "diag-json", false, "Emit diagnostics as structured JSON to stdout instead of human-readable text to stderr")
	cmd.Flags().BoolVar(&warnsAsErrors, "warnings-as-errors", false, "Treat warnings (e.g. an adapter whose schema could not be verified) as errors")
	return cmd
}

func validatePath(ctx context.Context, path string, subworkflowRoots []string, diagJSON, warnsAsErrors bool) (ok bool) {
	spec, diags := workflow.ParseFileOrDir(path)
	if diags.HasErrors() {
		if diagJSON {
			printDiagnosticsJSON(diags)
		} else {
			fmt.Fprintf(os.Stderr, "%s: parse failed:\n%s\n", path, formatDiagnostics(diags))
		}
		return false
	}
	info, _ := os.Stat(path)
	workflowDir := path
	if info != nil && !info.IsDir() {
		workflowDir = filepath.Dir(path)
	}
	loader := adapterhost.NewLoader()
	schemas, schemaDiags := diagutil.CollectSchemas(ctx, loader, spec, nil)
	_ = loader.Shutdown(ctx)

	_, diags = workflow.CompileWithContext(ctx, spec, schemas, workflow.CompileOpts{
		WorkflowDir:         workflowDir,
		SubWorkflowResolver: &workflow.LocalSubWorkflowResolver{AllowedRoots: subworkflowRoots},
		Schemas:             schemas,
	})
	// Fold unverified-adapter warnings into the diagnostics; --warnings-as-errors
	// promotes them so they fail validation rather than only printing.
	diags = append(diags, promoteWarnings(schemaDiags, warnsAsErrors)...)
	if diags.HasErrors() {
		if diagJSON {
			printDiagnosticsJSON(diags)
		} else {
			fmt.Fprintf(os.Stderr, "%s: compile failed:\n%s\n", path, formatDiagnostics(diags))
		}
		return false
	}
	if !diagJSON {
		fmt.Printf("%s: ok\n", path)
	} else {
		printDiagnosticsJSON(nil)
	}
	if len(diags) > 0 {
		if diagJSON {
			printDiagnosticsJSON(diags)
		} else {
			fmt.Fprintf(os.Stderr, "%s: warnings:\n%s\n", path, formatDiagnostics(diags))
		}
	}
	return true
}

func runValidate(paths, subworkflowRoots []string, diagJSON, warnsAsErrors bool) bool {
	ctx := context.Background()
	anyErr := false
	for _, path := range paths {
		if !validatePath(ctx, path, subworkflowRoots, diagJSON, warnsAsErrors) {
			anyErr = true
		}
	}
	return anyErr
}

// validateDiagnostic is the JSON shape emitted by --diag-json.
type validateDiagnostic struct {
	Severity string `json:"severity"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Col      int    `json:"col"`
	EndLine  int    `json:"end_line"`
	EndCol   int    `json:"end_col"`
	Summary  string `json:"summary"`
	Detail   string `json:"detail,omitempty"`
}

func printDiagnosticsJSON(diags hcl.Diagnostics) {
	out := make([]validateDiagnostic, 0, len(diags))
	for _, d := range diags {
		sev := "warning"
		if d.Severity == hcl.DiagError {
			sev = "error"
		}
		vd := validateDiagnostic{
			Severity: sev,
			Summary:  d.Summary,
			Detail:   d.Detail,
		}
		if d.Subject != nil {
			vd.File = d.Subject.Filename
			vd.Line = d.Subject.Start.Line
			vd.Col = d.Subject.Start.Column
			vd.EndLine = d.Subject.End.Line
			vd.EndCol = d.Subject.End.Column
		}
		out = append(out, vd)
	}
	b, err := json.Marshal(out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to marshal diagnostics: %v\n", err)
		return
	}
	fmt.Println(string(b))
}
