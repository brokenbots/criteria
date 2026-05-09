package cli

import (
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
)

func hclRange(filename string, startLine, startCol int) *hcl.Range {
	return &hcl.Range{
		Filename: filename,
		Start:    hcl.Pos{Line: startLine, Column: startCol},
		End:      hcl.Pos{Line: startLine, Column: startCol + 1},
	}
}

func TestFormatDiagnostics_WithSubject(t *testing.T) {
	diags := hcl.Diagnostics{{
		Severity: hcl.DiagError,
		Summary:  "something went wrong",
		Subject:  hclRange("filename.hcl", 3, 5),
	}}
	out := formatDiagnostics(diags)
	if !strings.Contains(out, "filename.hcl:3,5:") {
		t.Errorf("expected file:line,col in output; got: %q", out)
	}
	if !strings.Contains(out, "something went wrong") {
		t.Errorf("expected summary in output; got: %q", out)
	}
}

func TestFormatDiagnostics_WithDetail(t *testing.T) {
	diags := hcl.Diagnostics{{
		Severity: hcl.DiagError,
		Summary:  "short summary",
		Detail:   "longer explanation here",
	}}
	out := formatDiagnostics(diags)
	if !strings.Contains(out, "short summary") {
		t.Errorf("expected summary in output; got: %q", out)
	}
	if !strings.Contains(out, "  longer explanation here") {
		t.Errorf("expected detail indented by two spaces; got: %q", out)
	}
}

func TestFormatDiagnostics_NoSubject(t *testing.T) {
	diags := hcl.Diagnostics{{
		Severity: hcl.DiagError,
		Summary:  "no location available",
		Subject:  nil,
	}}
	out := formatDiagnostics(diags)
	if !strings.Contains(out, "no location available") {
		t.Errorf("expected summary in output; got: %q", out)
	}
	// Should not contain file:line,col pattern (no colon-separated path).
	if strings.Contains(out, ":1,") || strings.Contains(out, ":0,") {
		t.Errorf("unexpected file:line,col in no-subject output; got: %q", out)
	}
}

func TestFormatDiagnostics_MultipleErrors(t *testing.T) {
	diags := hcl.Diagnostics{
		{
			Severity: hcl.DiagError,
			Summary:  "first error",
		},
		{
			Severity: hcl.DiagError,
			Summary:  "second error",
		},
	}
	out := formatDiagnostics(diags)
	if !strings.Contains(out, "first error") {
		t.Errorf("expected first error in output; got: %q", out)
	}
	if !strings.Contains(out, "second error") {
		t.Errorf("expected second error in output; got: %q", out)
	}
	// Both should appear on separate lines — no semicolon separator.
	if strings.Contains(out, "; ") {
		t.Errorf("output must not use semicolon separator; got: %q", out)
	}
	lines := strings.Split(out, "\n")
	if len(lines) < 2 {
		t.Errorf("expected at least 2 lines for two errors; got: %q", out)
	}
}

func TestFormatDiagnostics_WarningLabel(t *testing.T) {
	diags := hcl.Diagnostics{{
		Severity: hcl.DiagWarning,
		Summary:  "this is a warning",
	}}
	out := formatDiagnostics(diags)
	if !strings.HasPrefix(out, "Warning:") {
		t.Errorf("expected output to start with 'Warning:'; got: %q", out)
	}
}

func TestNewDiagsError_NilOnWarningsOnly(t *testing.T) {
	diags := hcl.Diagnostics{{
		Severity: hcl.DiagWarning,
		Summary:  "just a warning",
	}}
	if err := newDiagsError(diags); err != nil {
		t.Errorf("expected nil for warnings-only diagnostics; got: %v", err)
	}
}

func TestNewDiagsError_NonNilOnErrors(t *testing.T) {
	diags := hcl.Diagnostics{
		{Severity: hcl.DiagWarning, Summary: "a warning"},
		{Severity: hcl.DiagError, Summary: "an actual error"},
	}
	err := newDiagsError(diags)
	if err == nil {
		t.Fatal("expected non-nil error for diagnostics with errors")
	}
	if !strings.Contains(err.Error(), "an actual error") {
		t.Errorf("error string should contain error summary; got: %q", err.Error())
	}
	// Warnings should not appear in the error output.
	if strings.Contains(err.Error(), "a warning") {
		t.Errorf("warnings should be dropped; got: %q", err.Error())
	}
}
