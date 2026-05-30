//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"

	"github.com/brokenbots/criteria/workflow"
	"github.com/zclconf/go-cty/cty"
)

// MaybeUseBubblewrap inspects the environment and host. If
// bwrap is on PATH and the environment opts in
// (environment.sandbox = "bwrap"), this returns a command wrapper
// that exec's `bwrap` with the appropriate args, replacing the in-process
// namespace setup. Returns nil if not applicable.
func MaybeUseBubblewrap(prep LinuxPrepared, env *workflow.EnvironmentNode) *exec.Cmd {
	if env == nil {
		return nil
	}
	optIn := false
	if env.TypeSpecific != nil {
		if v, ok := env.TypeSpecific["sandbox"]; ok {
			if v.Type() == cty.String && v.IsKnown() && !v.IsNull() {
				optIn = v.AsString() == "bwrap"
			}
		}
	}
	if !optIn {
		return nil
	}
	bwrapPath, err := exec.LookPath("bwrap")
	if err != nil {
		return nil
	}

	args := []string{"bwrap"}

	// Unshare all namespaces.
	args = append(args, "--unshare-all")

	// User namespace: map current user to root inside.
	args = append(args, "--uid", "0", "--gid", "0")

	// Filesystem read paths.
	for _, p := range pathListFromObject(getObject(env.TypeSpecific, "filesystem"), "read") {
		args = append(args, "--ro-bind", p, p)
	}
	// Filesystem write paths.
	for _, p := range pathListFromObject(getObject(env.TypeSpecific, "filesystem"), "write") {
		args = append(args, "--bind", p, p)
	}

	// If no paths are specified, provide a minimal tmpfs so the
	// process has a writable /tmp.
	args = append(args, "--tmpfs", "/tmp")

	// Network: if no network allow list, lock down completely.
	netAllow := pathListFromObject(getObject(env.TypeSpecific, "network"), "allow")
	if len(netAllow) == 0 {
		args = append(args, "--unshare-net")
	}

	// Resource limits: bubblewrap has --rlimit-* options.
	resObj := getObject(env.TypeSpecific, "resources")
	memStr := stringFromObject(resObj, "memory")
	if memStr == "" && prep.Rlimits != nil {
		// Fallback: derive from RlimitConfig if we already parsed it.
		for _, rl := range prep.Rlimits {
			if rl.Resource == 9 { // RLIMIT_AS
				memStr = strconv.FormatUint(rl.Rlimit.Cur, 10)
				break
			}
		}
	}
	if memBytes := parseMemoryLimit(memStr); memBytes > 0 {
		args = append(args, "--rlimit-as", strconv.FormatUint(memBytes, 10))
	}

	cpuStr := stringFromObject(resObj, "cpu")
	if cpuVal := parseCPULimit(cpuStr); cpuVal > 0 {
		// bwrap does not have a direct CPU limit flag; cgroups are
		// the preferred mechanism. We document this gap.
		_ = cpuVal
	}

	timeoutStr := stringFromObject(resObj, "timeout")
	if timeoutDur := parseTimeout(timeoutStr); timeoutDur > 0 {
		// bwrap supports --timeout but it is wall-clock, not CPU.
		args = append(args, "--timeout", fmt.Sprintf("%d", int(timeoutDur.Seconds())))
	}

	// New privileges: bubblewrap already drops them; mirror the flag.
	args = append(args, "--die-with-parent", "--new-session")

	// Append the target adapter command.
	args = append(args, prep.TargetPath)

	cmd := exec.Command(bwrapPath, args[1:]...)
	cmd.Env = scrubEnv(os.Environ())
	return cmd
}

func getObject(m map[string]cty.Value, key string) cty.Value {
	if m == nil {
		return cty.NilVal
	}
	if v, ok := m[key]; ok {
		return v
	}
	return cty.NilVal
}
