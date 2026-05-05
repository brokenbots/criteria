package workflow

// compile_validators.go — validation helpers for adapter names and crash policies,
// plus the reserved-name check for the engine-internal "_continue" target.

import (
	"fmt"
	"strings"

	"github.com/hashicorp/hcl/v2"
)

func isValidOnCrash(v string) bool {
	switch v {
	case onCrashFail, onCrashRespawn, onCrashAbortRun:
		return true
	default:
		return false
	}
}

func isValidAdapterName(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	for _, r := range v {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

// checkReservedNames rejects any spec node that uses the engine-internal
// "_continue" target as its declared name, and rejects any node named "return"
// since that string is the reserved outcome routing sentinel for all node kinds.
func checkReservedNames(spec *Spec) hcl.Diagnostics {
	var diags hcl.Diagnostics
	for _, st := range spec.States {
		diags = append(diags, reservedNameDiags("state", st.Name)...)
	}
	for i := range spec.Steps {
		// "return" as a step name is caught by validateStepNameNotReturn.
		if spec.Steps[i].Name == "_continue" {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: `"_continue" is a reserved engine-internal target and may not be declared as a step`})
		}
	}
	for _, w := range spec.Waits {
		diags = append(diags, reservedNameDiags("wait", w.Name)...)
	}
	for _, a := range spec.Approvals {
		diags = append(diags, reservedNameDiags("approval", a.Name)...)
	}
	for _, b := range spec.Branches {
		if b.Name == ReturnSentinel {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: `"return" is a reserved outcome routing sentinel and may not be declared as a branch`})
		}
	}
	return diags
}

// reservedNameDiags returns diagnostics for a state/wait/approval named with
// either of the engine-internal reserved names ("_continue", "return").
func reservedNameDiags(kind, name string) hcl.Diagnostics {
	var diags hcl.Diagnostics
	if name == "_continue" {
		diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf(`"_continue" is a reserved engine-internal target and may not be declared as a %s`, kind)})
	}
	if name == ReturnSentinel {
		diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf(`"return" is a reserved outcome routing sentinel and may not be declared as a %s`, kind)})
	}
	return diags
}
