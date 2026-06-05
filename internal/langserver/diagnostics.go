package langserver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hashicorp/hcl/v2"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/diagutil"
	"github.com/brokenbots/criteria/workflow"
)

// compileDiagnostic is a single structured diagnostic extracted from hcl.Diagnostics.
type compileDiagnostic struct {
	severity hcl.DiagnosticSeverity
	file     string
	line     int
	col      int
	endLine  int
	endCol   int
	message  string
}

func makeDirDiag(dir string, severity hcl.DiagnosticSeverity, message string) compileDiagnostic {
	return compileDiagnostic{
		severity: severity,
		file:     dir,
		line:     1,
		col:      1,
		endLine:  1,
		endCol:   1,
		message:  message,
	}
}

func findFirstHCLFile(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := filepath.Ext(entry.Name())
		if ext == ".chcl" || ext == ".hcl" {
			return filepath.Join(dir, entry.Name()), nil
		}
	}
	return "", fmt.Errorf("no .chcl or .hcl files in directory")
}

func (s *server) compileDiagnostics(dir string) []compileDiagnostic {
	entryPath, err := findFirstHCLFile(dir)
	if err != nil {
		return []compileDiagnostic{makeDirDiag(dir, hcl.DiagError, err.Error())}
	}

	spec, diags := workflow.ParseFileOrDir(entryPath)
	if diags.HasErrors() {
		return hclDiagsToCompileDiags(diags)
	}

	ctx := context.Background()
	loader := adapterhost.NewLoader()
	schemas := diagutil.CollectSchemas(ctx, loader, spec, nil)
	_ = loader.Shutdown(ctx)

	_, compileDiags := workflow.CompileWithContext(ctx, spec, schemas, workflow.CompileOpts{
		WorkflowDir:         dir,
		SubWorkflowResolver: &workflow.LocalSubWorkflowResolver{},
		Schemas:             schemas,
	})
	return hclDiagsToCompileDiags(compileDiags)
}

func hclDiagsToCompileDiags(diags hcl.Diagnostics) []compileDiagnostic {
	result := make([]compileDiagnostic, 0, len(diags))
	for _, d := range diags {
		cd := compileDiagnostic{
			severity: d.Severity,
			message:  d.Summary,
		}
		if d.Detail != "" {
			cd.message = d.Summary + "\n" + d.Detail
		}
		if d.Subject != nil {
			cd.file = d.Subject.Filename
			cd.line = d.Subject.Start.Line
			cd.col = d.Subject.Start.Column
			cd.endLine = d.Subject.End.Line
			cd.endCol = d.Subject.End.Column
		} else {
			cd.file = ""
			cd.line = 1
			cd.col = 1
			cd.endLine = 1
			cd.endCol = 1
		}
		result = append(result, cd)
	}
	return result
}
