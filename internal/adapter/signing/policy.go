package signing

import (
	"fmt"
	"strings"
)

// WildcardIssuer is the explicit "any issuer" pattern accepted in
// Policy.TrustedIssuers and in a lockfile keyless pin. Trusting any issuer is
// a loud, deliberate relaxation: pin resolution (cli.policyForPin) warns about
// it, and consumers who need tighter guarantees pin a concrete issuer instead.
const WildcardIssuer = "*"

// IssuerMatches reports whether an OIDC issuer satisfies a trusted-issuer
// entry. It uses the same glob surface as subject patterns (see
// SubjectMatches), so "*" is the explicit wildcard and entries stay anchored
// at both ends — a typo'd issuer entry can only fail verification, never
// silently widen trust beyond what was written.
func IssuerMatches(pattern, issuer string) bool {
	if pattern == "" {
		return false
	}
	return globMatch(pattern, issuer)
}

// SubjectMatches reports whether a subject satisfies a policy subject pattern.
// The surface is glob-shaped: "*" matches any sequence of characters —
// including "/", so "https://github.com/org/*" reaches deep workflow URLs —
// and "?" matches a single character; everything else is a literal. This is
// the familiar path.Match syntax family minus its separator rule: plain
// path.Match would stop "*" at every "/" in the URL and break exactly the
// "https://github.com/org/*" patterns this matcher exists for.
//
// An empty pattern matches nothing: emptiness is rejected when a policy is
// built (Policy.Validate), so it can never silently mean "allow anything".
func SubjectMatches(pattern, subject string) bool {
	if pattern == "" {
		return false
	}
	return globMatch(pattern, subject)
}

// globMatch is the shared matcher for issuer and subject entries, using the
// classic greedy '*' scan with single-position backtracking. An empty pattern
// matches nothing — Validate rejects empty entries, so callers that reach
// here with one fail closed instead of allow-all.
//
// The matcher operates on bytes, not runes: '?' consumes exactly one byte, so
// a multi-byte rune in the subject would need one '?' per byte. Subjects and
// issuers are ASCII OIDC URLs in practice, and the byte-wise behavior can only
// over-restrict matches (reject, never widen trust), so it fails closed.
func globMatch(pattern, s string) bool {
	if pattern == "" {
		return false
	}
	pIdx, sIdx := 0, 0
	starP, starS := -1, -1
	for sIdx < len(s) {
		switch {
		case pIdx < len(pattern) && pattern[pIdx] == '*':
			starP, starS = pIdx, sIdx
			pIdx++
		case pIdx < len(pattern) && (pattern[pIdx] == '?' || pattern[pIdx] == s[sIdx]):
			pIdx++
			sIdx++
		case starP >= 0:
			starS++
			sIdx = starS
			pIdx = starP + 1
		default:
			return false
		}
	}
	for pIdx < len(pattern) && pattern[pIdx] == '*' {
		pIdx++
	}
	return pIdx == len(pattern)
}

// Validate checks that the policy's keyless identity surface is well-formed.
// This is the fail-closed counterpart of identity matching — before it
// existed, an empty pattern entry (in a default policy, a trust config, or a
// lockfile pin) silently meant "allow anything" because the matchers treated
// "" as a wildcard, and a nil list meant the same in identityFromCert.
// PolicyFor calls it when a policy is built and Verify at every verification,
// so a malformed trust shape fails loudly instead of widening trust silently.
//
// A policy with explicit TrustedKeys may legitimately leave the keyless
// surface unset: identity checks still dispatch to the keyless path for
// keyless signature material, so a pure key-mode policy (no keyless
// issuers/patterns configured) is valid, while an issuer-only policy without
// trusted keys must carry at least one issuer and one subject pattern.
func (p *Policy) Validate() error {
	for i, iss := range p.TrustedIssuers {
		if strings.TrimSpace(iss) == "" {
			return fmt.Errorf("trusted issuer %d is empty; name an OIDC issuer URL (or the explicit %q wildcard)", i, WildcardIssuer)
		}
	}
	for i, pat := range p.SubjectPatterns {
		if strings.TrimSpace(pat) == "" {
			return fmt.Errorf("empty subject pattern %d: a keyless policy must use non-empty globs (use %q to deliberately allow any subject)", i, WildcardIssuer)
		}
	}
	if p.IsKeyless() && p.Mode != ModeOff {
		if len(p.TrustedIssuers) == 0 {
			return fmt.Errorf("keyless policy has no trusted issuers; configure at least one issuer (e.g. %s) or the explicit %q wildcard", DefaultTrustedIssuers[0], WildcardIssuer)
		}
		if len(p.SubjectPatterns) == 0 {
			return fmt.Errorf("keyless policy has no subject patterns; configure at least one glob (the %q pattern deliberately allows any subject)", WildcardIssuer)
		}
	}
	return nil
}

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

	// Build-time validation: the resolved policy's identity surface (issuer +
	// subject patterns) must be well-formed — no empty entries that the
	// matchers would silently read as "allow anything". The built-in defaults
	// validate cleanly; this guards future/default changes at the same
	// fail-closed standard the digest binding already enforces.
	if err := policy.Validate(); err != nil {
		return Policy{}, fmt.Errorf("invalid verification policy: %w", err)
	}

	return policy, nil
}
