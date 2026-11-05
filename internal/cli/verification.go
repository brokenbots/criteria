package cli

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/brokenbots/criteria/internal/adapter/signing"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

// allowUnsignedEnv, when truthy, forces the unsigned-override on for any
// verification-consuming command (pull/lock/compile/apply). It is the
// lowest-precedence way to enable the override, below the --allow-unsigned
// flag. The override lowers a warn/unset posture to ModeOff; it is refused
// when the posture resolves to strict (signing.PolicyFor fails closed).
const allowUnsignedEnv = "CRITERIA_ALLOW_UNSIGNED"

// transitionDefaultMode is the effective verification mode used when neither the
// unsigned-override nor an explicit workflow `verification` attribute is set.
//
// It is deliberately `warn` (log, do not fail) during the signing-completion
// transition — decision D-WS46-1 — so existing unsigned/legacy artifacts do not
// break `lock`/`apply` while keyless signing is being made verifiable end-to-end
// (WS48). Flip this single constant to signing.ModeStrict once keyless is
// verifiable (the WS48 Step 5 follow-up).
const transitionDefaultMode = signing.ModeWarn

// resolveVerification builds the effective signing.PullContext from, in
// precedence order (highest first):
//
//  1. --allow-unsigned flag        (allowUnsigned == true)
//  2. CRITERIA_ALLOW_UNSIGNED env  (1/true/yes/on)
//  3. workflow `verification` attr (off|warn|strict)
//  4. transition default           (transitionDefaultMode)
//
// The flag and env only ever *enable* the override; they never tighten
// verification. The override is resolved against the workflow/default posture
// inside signing.PolicyFor: it lowers a warn/unset posture to ModeOff, and is
// refused there when that posture is strict.
func resolveVerification(allowUnsigned bool, workflowVerification string, trustedKeys []signing.KeyIdentity) signing.PullContext {
	wv := strings.TrimSpace(workflowVerification)
	if wv == "" {
		wv = string(transitionDefaultMode)
	}
	return signing.PullContext{
		AllowUnsigned:        allowUnsigned || envAllowUnsigned(),
		WorkflowVerification: wv,
		TrustedKeys:          trustedKeys,
	}
}

// resolveSigningPolicy is the single entry point every verification-consuming
// command uses to obtain its effective signing.Policy, keeping the override
// surface (flag + env + workflow attr) and the enterprise trusted-key surface
// uniform across pull/lock/compile/apply.
func resolveSigningPolicy(allowUnsigned bool, workflowVerification string, trustedKeys []signing.KeyIdentity) (signing.Policy, error) {
	return signing.PolicyFor(resolveVerification(allowUnsigned, workflowVerification, trustedKeys))
}

// policyForPin returns a copy of base tightened to the signer pinned in the
// lockfile, so verification itself enforces the trust anchor (not just a
// post-hoc identity comparison):
//
//   - key pin: TrustedKeys is restricted to the pinned fingerprint (a subset of
//     the configured keys). If no configured key matches, no key remains and a
//     key-signed artifact fails closed under strict.
//   - keyless pin: TrustedIssuers/SubjectPatterns are narrowed to the pinned
//     issuer/subject so only that identity verifies. A keyless pin must record
//     an issuer: the lockfile deferring the trust decision to whatever the
//     first lock accepted is exactly the "anyone's CI can sign what I lock
//     first" trap, so an empty issuer (or subject) is an error — re-lock to
//     record the real identity. The explicit "*" wildcard issuer is allowed
//     but warned loudly.
//
// base is returned unchanged when nothing is pinned.
func policyForPin(base *signing.Policy, pin *lockfile.LockedSignature) (signing.Policy, error) {
	if pin == nil {
		return *base, nil
	}
	p := *base
	switch {
	case pin.Key != nil:
		var kept []signing.KeyIdentity
		for _, k := range base.TrustedKeys {
			if k.Fingerprint == pin.Key.Fingerprint {
				kept = append(kept, k)
			}
		}
		p.TrustedKeys = kept
	case pin.Keyless != nil:
		pinIssuer := strings.TrimSpace(pin.Keyless.Issuer)
		pinSubject := strings.TrimSpace(pin.Keyless.Subject)
		if pinIssuer == "" || pinSubject == "" {
			return signing.Policy{}, fmt.Errorf(
				"keyless pin is missing its recorded %s (issuer %q, subject %q); a keyless pin must carry both, or verification would defer the trust decision to the policy defaults — re-run `criteria adapter lock` to record the signer identity",
				missingPinField(pinIssuer, pinSubject), pin.Keyless.Issuer, pin.Keyless.Subject)
		}
		p.TrustedKeys = nil
		if pinIssuer == signing.WildcardIssuer {
			slog.Warn("lockfile keyless pin trusts ANY OIDC issuer",
				"pinned_issuer", pinIssuer,
				"pinned_subject", pinSubject,
				"hint", "prefer a concrete issuer URL (e.g. "+signing.DefaultTrustedIssuers[0]+"); a wildcard issuer accepts signatures from every Sigstore-flavored CI")
			p.TrustedIssuers = []string{pinIssuer}
		} else {
			p.TrustedIssuers = []string{pinIssuer}
		}
		if pinSubject == signing.WildcardIssuer {
			slog.Warn("lockfile keyless pin trusts any workflow subject",
				"pinned_issuer", pinIssuer,
				"pinned_subject", pinSubject,
				"hint", "prefer a subject glob that names the adapter's repo (e.g. \"https://github.com/org/repo/*\")")
			p.SubjectPatterns = []string{pinSubject}
		} else {
			p.SubjectPatterns = []string{pinSubject}
		}
		if err := p.Validate(); err != nil {
			return signing.Policy{}, fmt.Errorf("pinned verification policy: %w", err)
		}
	}
	return p, nil
}

func missingPinField(issuer, subject string) string {
	switch {
	case issuer == "" && subject == "":
		return "issuer and subject"
	case issuer == "":
		return "issuer"
	default:
		return "subject"
	}
}

// assertSignerMatchesPin confirms a freshly verified signer matches the signer
// pinned in the lockfile — the "lockfile is the trust anchor" enforcement
// (WS47/WS48). mode is the verification mode the signer was checked under:
//
//   - ModeOff deliberately skips verification, so a nil signer is expected and
//     the pin (if any) is left unasserted; the override reaching ModeOff goes
//     through the same skip.
//   - In every other mode a nil signer means verification failed without
//     aborting — the warn posture downgrades failures to a proceed. A pinned
//     signer must not be silently bypassed that way, so the call fails closed;
//     ModeStrict failures error inside signing.Verify and never reach this call
//     with a nil signer.
//
// It is also a no-op when the lockfile entry has no pinned signer (legacy
// lockfiles, or an artifact that was unsigned at lock time): there is then no
// pin to enforce.
func assertSignerMatchesPin(adapterKey string, signer *signing.SignerIdentity, pin *lockfile.LockedSignature, mode signing.VerificationMode) error {
	if pin == nil {
		return nil
	}
	if signer == nil {
		if mode == signing.ModeOff {
			return nil
		}
		return fmt.Errorf("adapter %q: the lockfile pins a signer but verification produced none under %q mode; fail closed — re-run `criteria adapter lock` if the signer change is intentional", adapterKey, mode)
	}
	switch {
	case signer.Key != nil:
		if pin.Key == nil {
			return fmt.Errorf("adapter %q: artifact is key-signed but lockfile pins a keyless signer; re-run `criteria adapter lock`", adapterKey)
		}
		if signer.Key.Fingerprint != pin.Key.Fingerprint {
			return fmt.Errorf("adapter %q: signer key fingerprint %s does not match pinned %s; re-run `criteria adapter lock` if the signer changed intentionally", adapterKey, signer.Key.Fingerprint, pin.Key.Fingerprint)
		}
	case signer.Keyless != nil:
		if pin.Keyless == nil {
			return fmt.Errorf("adapter %q: artifact is keyless-signed but lockfile pins a key signer; re-run `criteria adapter lock`", adapterKey)
		}
		// Pattern-aware like the verification itself: a wildcard-issuer or
		// wildcard-subject pin matches any issuer/subject, exact pin values
		// compare literally. Both sides must accept; a mismatch is the
		// signer-change signal the re-lock hint covers.
		issuerOK := signing.IssuerMatches(pin.Keyless.Issuer, signer.Keyless.Issuer)
		subjectOK := signing.SubjectMatches(pin.Keyless.Subject, signer.Keyless.Subject)
		if !issuerOK || !subjectOK {
			return fmt.Errorf("adapter %q: signer identity %s/%s does not match pinned %s/%s; re-run `criteria adapter lock` if the signer changed intentionally",
				adapterKey, signer.Keyless.Issuer, signer.Keyless.Subject, pin.Keyless.Issuer, pin.Keyless.Subject)
		}
	}
	return nil
}

// envAllowUnsigned reports whether CRITERIA_ALLOW_UNSIGNED is set to a truthy
// value.
func envAllowUnsigned() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(allowUnsignedEnv))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
