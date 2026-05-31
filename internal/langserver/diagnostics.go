package langserver

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/adapters/shell"
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
	loader.RegisterBuiltin(shell.Name, adapterhost.BuiltinFactoryForAdapter(shell.New()))
	schemas := collectLangserverSchemas(ctx, loader, spec)
	_ = loader.Shutdown(ctx)

	_, compileDiags := workflow.CompileWithContext(ctx, spec, schemas, workflow.CompileOpts{
		WorkflowDir:         dir,
		SubWorkflowResolver: &workflow.LocalSubWorkflowResolver{},
		Schemas:             schemas,
	})
	return hclDiagsToCompileDiags(compileDiags)
}

// collectLangserverSchemas resolves Info() for every adapter referenced in spec
// and returns a schemas map suitable for workflow.Compile. Adapters that cannot
// be resolved are silently skipped so compile still runs in permissive mode.
func collectLangserverSchemas(ctx context.Context, loader adapterhost.Loader, spec *workflow.Spec) map[string]workflow.AdapterInfo {
	if loader == nil || spec == nil {
		return nil
	}

	seen := map[string]bool{}
	for _, ad := range spec.Adapters {
		if ad.Type != "" {
			seen[ad.Type] = true
		}
	}
	for i := range spec.Steps {
		st := &spec.Steps[i]
		if adapterRef, present, _ := workflow.ResolveStepAdapterRef(st.Remain); present && adapterRef != "" {
			parts := strings.Split(adapterRef, ".")
			if len(parts) == 2 && parts[0] != "" {
				seen[parts[0]] = true
			}
		}
	}

	if len(seen) == 0 {
		return nil
	}

	schemas := make(map[string]workflow.AdapterInfo, len(seen))
	for typeName := range seen {
		p, err := loader.Resolve(ctx, typeName)
		if err != nil {
			slog.Debug("schema collection: could not resolve adapter", "adapter_type", typeName, "err", err)
			continue
		}
		info, err := p.Info(ctx)
		p.Kill()
		if err != nil {
			slog.Debug("schema collection: Info() failed", "adapter_type", typeName, "err", err)
			continue
		}
		schemas[typeName] = info.AdapterInfo
	}
	return schemas
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
