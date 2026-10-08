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
//
// The multi-adapter manifest variables (CRITERIA_ADAPTERS,
// CRITERIA_ADAPTERS_DIR, CRITERIA_ADAPTER_<NAME>_*, KB-213) are scrubbed the
// same way: they describe which children the PARENT hosts and must not ride
// into any child process. The four legacy CRITERIA_ADAPTER_* names keep
// today's passthrough behavior.
const remoteEnvPrefix = "CRITERIA_REMOTE_"

var scrubbedAdapterVars = map[string]bool{
	EnvAdapters:    true,
	EnvAdaptersDir: true,
}

// adapterOverrideSuffixes are the per-adapter manifest override suffixes;
// any CRITERIA_ADAPTER_<NAME>_<SUFFIX> other than the four legacy names is
// scrubbed from child environments.
var adapterOverrideSuffixes = [...]string{"_BINARY", "_VERSION", "_DIGEST", "_MANIFEST"}

func scrubbedPeerVar(name string) bool {
	if name == "CRITERIA_REMOTE" || strings.HasPrefix(name, remoteEnvPrefix) {
		return true
	}
	if scrubbedAdapterVars[name] {
		return true
	}
	if !strings.HasPrefix(name, "CRITERIA_ADAPTER_") {
		return false
	}
	switch name {
	case EnvAdapterName, EnvAdapterVersion, EnvAdapterBinary, EnvAdapterManifest:
		return false
	}
	for _, suffix := range adapterOverrideSuffixes {
		if strings.HasSuffix(name, suffix) && len(name) > len("CRITERIA_ADAPTER_")+len(suffix) {
			return true
		}
	}
	return false
}

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
		if scrubbedPeerVar(name) {
			continue
		}
		out = append(out, kv)
	}
	return out
}
