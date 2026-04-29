package workflow

// compile_lifecycle.go — lifecycle and crash-policy validation helpers, plus
// the reserved-name check for the engine-internal "_continue" target.

import (
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

func isValidLifecycle(v string) bool {
	switch v {
	case lifecycleOpen, lifecycleClose:
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
// "_continue" target as its declared name.
func checkReservedNames(spec *Spec) hcl.Diagnostics {
	var diags hcl.Diagnostics
	for _, st := range spec.States {
		if st.Name == "_continue" {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: `"_continue" is a reserved engine-internal target and may not be declared as a state`})
		}
	}
	for _, st := range spec.Steps {
		if st.Name == "_continue" {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: `"_continue" is a reserved engine-internal target and may not be declared as a step`})
		}
	}
	for _, w := range spec.Waits {
		if w.Name == "_continue" {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: `"_continue" is a reserved engine-internal target and may not be declared as a wait`})
		}
	}
	for _, a := range spec.Approvals {
		if a.Name == "_continue" {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: `"_continue" is a reserved engine-internal target and may not be declared as an approval`})
		}
	}
	return diags
}
