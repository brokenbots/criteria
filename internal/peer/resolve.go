package peer

import (
	"strings"
)

// remoteEnvPrefix is the prefix of phone-home connection variables that must
// never leak into a child adapter's environment. Prefix-based scrubbing
// (rather than a hardcoded name list) closes review §4.5: new CRITERIA_REMOTE_*
// settings added later are scrubbed automatically, and adapters cannot sniff
// into phone-home mode via any remote variable, including e.g. a
// CRITERIA_REMOTE_SCOPE the parent kept for its own use.
//
// This matters because adapters detect CRITERIA_REMOTE_HOST in their
// environment and switch into ServeRemote/phone-home mode, abandoning the
// local go-plugin handshake (criteria-adapter-shell >= v0.5.3,
// criteria-adapter-copilot >= v0.5.5).
const remoteEnvPrefix = "CRITERIA_REMOTE_"

// ChildEnv returns env with every CRITERIA_REMOTE_* variable (and the bare
// CRITERIA_REMOTE name itself) removed, plus malformed entries dropped. The
// result is safe to hand to a child adapter process: no phone-home
// connection settings and no accept/scope tokens ride along. Explicit
// unsetting is unnecessary — with the prefix removed from the child's
// environment the variables are absent, not merely empty.
func ChildEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, found := strings.Cut(kv, "=")
		if !found {
			continue
		}
		if name == "CRITERIA_REMOTE" || strings.HasPrefix(name, remoteEnvPrefix) {
			continue
		}
		out = append(out, kv)
	}
	return out
}
