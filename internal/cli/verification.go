package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/brokenbots/criteria/internal/adapter/signing"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

// allowUnsignedEnv, when truthy, forces the unsigned-override on for any
// verification-consuming command (pull/lock/compile/apply). It is the
// lowest-precedence way to enable the override, below the --allow-unsigned flag.
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
// The flag and env only ever *enable* the override (force ModeOff); they never
// tighten verification. signing.PolicyFor consumes the returned context.
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
//     issuer/subject so only that identity verifies.
//
// base is returned unchanged when nothing is pinned.
func policyForPin(base signing.Policy, pin *lockfile.LockedSignature) signing.Policy {
	if pin == nil {
		return base
	}
	p := base
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
		p.TrustedKeys = nil
		if pin.Keyless.Issuer != "" {
			p.TrustedIssuers = []string{pin.Keyless.Issuer}
		}
		if pin.Keyless.Subject != "" {
			p.SubjectPatterns = []string{pin.Keyless.Subject}
		}
	}
	return p
}

// assertSignerMatchesPin confirms a freshly verified signer matches the signer
// pinned in the lockfile — the "lockfile is the trust anchor" enforcement
// (WS47/WS48). It is a no-op when verification was skipped (signer == nil, e.g.
// ModeOff or a ModeWarn failure) or when the lockfile entry has no pinned signer
// (legacy lockfiles, or an artifact that was unsigned at lock time).
func assertSignerMatchesPin(adapterKey string, signer *signing.SignerIdentity, pin *lockfile.LockedSignature) error {
	if signer == nil || pin == nil {
		return nil
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
		if signer.Keyless.Issuer != pin.Keyless.Issuer || signer.Keyless.Subject != pin.Keyless.Subject {
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
