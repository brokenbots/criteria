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

	// AllowUnsigned is true when the --allow-unsigned CLI flag is set or
	// CRITERIA_ALLOW_UNSIGNED is truthy, lowering verification to ModeOff for
	// this invocation. It only ever lowers a warn/unset posture: when the
	// workflow setting — or, once the unset posture resolves strict, the
	// default — is strict, PolicyFor refuses the override instead.
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

// PolicyFor resolves the effective Policy for a verification-consuming
// operation, combining:
//   - workflow-level "verification" setting (off|warn|strict), falling back
//     to the ModeStrict SDK default when unset
//   - the --allow-unsigned CLI flag / CRITERIA_ALLOW_UNSIGNED env override
//     (forces ModeOff for this invocation only)
//
// The override is resolved last and can only LOWER a warn/unset posture to
// ModeOff; when the posture resolves to strict — via an explicit workflow
// setting now, or via the unset default once the transition default flips —
// the override is refused with an error naming the conflict. An env override
// must never silently defeat an explicit strict trust decision.
func PolicyFor(ctx PullContext) (Policy, error) {
	// Start from defaults (decision D-WS48-1): trust the well-known CI OIDC
	// issuers (DefaultTrustedIssuers, incl. GitHub Actions) and accept any
	// subject ("*") at first lock. The *specific* identity is then pinned into
	// the lockfile (LockedSignature.Keyless) and enforced on every subsequent
	// pull/apply (cli.policyForPin narrows issuer+subject to the pin), so "an
	// adapter signed by its own repo's CI" verifies with no per-consumer config
	// while the lockfile remains the trust anchor. Enterprises tighten via the
	// trust config.
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

	// Resolution order is deliberate: the trust posture is resolved from the
	// workflow setting (or the built-in default) BEFORE the unsigned override
	// is considered, so the override can never silently win over an explicit
	// strict trust decision. Warn/unset postures are lowered to ModeOff for
	// this invocation.
	if ctx.AllowUnsigned {
		if policy.Mode == ModeStrict {
			return Policy{}, fmt.Errorf(
				"strict verification conflicts with the unsigned override (--allow-unsigned flag or CRITERIA_ALLOW_UNSIGNED env); unset it or lower the workflow verification to warn")
		}
		return Policy{Mode: ModeOff}, nil
	}

	// Attach the CLI-resolved trusted keys (enterprise key mode, WS47). The CLI
	// loads these from the global + workflow trust config and --trusted-key
	// flags; verifyKeyBased matches a signature against them by fingerprint.
	if len(ctx.TrustedKeys) > 0 {
		policy.TrustedKeys = append([]KeyIdentity(nil), ctx.TrustedKeys...)
	}

	return policy, nil
}
