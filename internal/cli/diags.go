package cli

import (
	"fmt"
	"strings"

	"github.com/hashicorp/hcl/v2"
)

// diagsError wraps hcl.Diagnostics as an error. Its Error() string formats each
// diagnostic on its own line with severity, file:line:col, summary, and detail.
// This replaces the single-line diags.Error() output that discards location info.
type diagsError struct {
	diags hcl.Diagnostics
}

func (e *diagsError) Error() string {
	return formatDiagnostics(e.diags)
}

// newDiagsError returns a *diagsError wrapping the provided diagnostics.
// Returns nil if diags contains no errors (warnings are dropped; call sites that
// want to surface warnings should do so before calling this).
func newDiagsError(diags hcl.Diagnostics) error {
	var errs hcl.Diagnostics
	for _, d := range diags {
		if d.Severity == hcl.DiagError {
			errs = append(errs, d)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return &diagsError{diags: errs}
}

// formatDiagnostics formats all diagnostics in diags, one per block, with
// file path and line/column information when available.
func formatDiagnostics(diags hcl.Diagnostics) string {
	var b strings.Builder
	for _, d := range diags {
		sev := "Error"
		if d.Severity == hcl.DiagWarning {
			sev = "Warning"
		}
		if d.Subject != nil && d.Subject.Filename != "" {
			fmt.Fprintf(&b, "%s: %s:%d,%d: %s\n",
				sev,
				d.Subject.Filename,
				d.Subject.Start.Line,
				d.Subject.Start.Column,
				d.Summary,
			)
		} else {
			fmt.Fprintf(&b, "%s: %s\n", sev, d.Summary)
		}
		if d.Detail != "" {
			// Indent detail lines for visual separation.
			for _, line := range strings.Split(strings.TrimRight(d.Detail, "\n"), "\n") {
				fmt.Fprintf(&b, "  %s\n", line)
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
