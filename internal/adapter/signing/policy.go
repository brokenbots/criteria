package signing

import (
	"fmt"
	"strings"
)

// PullContext carries the inputs needed to resolve the effective verification
// policy for a pull operation. It is populated by the CLI layer (WS08) and
// workflow parser (WS09).
type PullContext struct {
	// WorkflowVerification is the workflow-level "verification" setting
	// (off|warn|strict). Empty means not set.
	WorkflowVerification string

	// AllowUnsigned is true when the --allow-unsigned CLI flag is set,
	// forcing ModeOff for this invocation only.
	AllowUnsigned bool

	// GlobalConfigPath is the path to the global HCL config file. When empty
	// the default ~/.criteria/config.hcl is used.
	GlobalConfigPath string
}

// PolicyFor resolves the effective Policy for a pull operation, combining:
//   - global config at ~/.criteria/config.hcl (trusted_issuers, etc.)
//   - workflow-level "verification" setting (off|warn|strict)
//   - --allow-unsigned CLI flag (forces ModeOff for this invocation only)
func PolicyFor(ctx PullContext) (Policy, error) {
	if ctx.AllowUnsigned {
		return Policy{Mode: ModeOff}, nil
	}

	// Start from defaults.
	policy := Policy{
		Mode:            ModeStrict,
		TrustedIssuers:  append([]string(nil), DefaultTrustedIssuers...),
		SubjectPatterns: []string{"*"},
	}

	// Apply workflow-level setting if present.
	if ctx.WorkflowVerification != "" {
		mode := VerificationMode(strings.ToLower(ctx.WorkflowVerification))
		switch mode {
		case ModeOff, ModeWarn, ModeStrict:
			policy.Mode = mode
		default:
			return Policy{}, fmt.Errorf("invalid workflow verification mode %q", ctx.WorkflowVerification)
		}
	}

	// TODO: parse global config file (HCL) and merge trusted_issuers,
	// subject_patterns, and trusted_keys. Deferred until WS08/WS09 provide
	// config parsing helpers or the global config schema is stable.

	return policy, nil
}
