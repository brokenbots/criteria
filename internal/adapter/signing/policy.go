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

	// TrustedKeys are explicit public keys (enterprise key mode, WS47) resolved
	// by the CLI from the global trust config, the workflow-dir trust config,
	// and ad-hoc --trusted-key flags. When non-empty they are copied onto the
	// resolved Policy so key-signed artifacts verify against them.
	TrustedKeys []KeyIdentity
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

	// Attach the CLI-resolved trusted keys (enterprise key mode, WS47). The CLI
	// loads these from the global + workflow trust config and --trusted-key
	// flags; verifyKeyBased matches a signature against them by fingerprint.
	if len(ctx.TrustedKeys) > 0 {
		policy.TrustedKeys = append([]KeyIdentity(nil), ctx.TrustedKeys...)
	}

	return policy, nil
}
