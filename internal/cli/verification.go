package cli

import (
	"os"
	"strings"

	"github.com/brokenbots/criteria/internal/adapter/signing"
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
func resolveVerification(allowUnsigned bool, workflowVerification string) signing.PullContext {
	wv := strings.TrimSpace(workflowVerification)
	if wv == "" {
		wv = string(transitionDefaultMode)
	}
	return signing.PullContext{
		AllowUnsigned:        allowUnsigned || envAllowUnsigned(),
		WorkflowVerification: wv,
	}
}

// resolveSigningPolicy is the single entry point every verification-consuming
// command uses to obtain its effective signing.Policy, keeping the override
// surface (flag + env + workflow attr) uniform across pull/lock/compile/apply.
func resolveSigningPolicy(allowUnsigned bool, workflowVerification string) (signing.Policy, error) {
	return signing.PolicyFor(resolveVerification(allowUnsigned, workflowVerification))
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
