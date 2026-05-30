//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"

	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/workflow"
)

// MaybeUseBubblewrap inspects the environment and host. If
// bwrap is on PATH and the environment opts in
// (environment.sandbox = "bwrap"), this returns a command wrapper
// that exec's `bwrap` with the appropriate args, replacing the in-process
// namespace setup. Returns nil if not applicable.
func MaybeUseBubblewrap(prep *LinuxPrepared, env *workflow.EnvironmentNode) *exec.Cmd {
	if env == nil || !isBubblewrapOptIn(env) {
		return nil
	}
	bwrapPath, err := exec.LookPath("bwrap")
	if err != nil {
		return nil
	}

	args := []string{"bwrap"}

	// Unshare all namespaces and map current user to root inside.
	args = append(args, "--unshare-all", "--uid", "0", "--gid", "0")
	args = append(args, bwrapFilesystemArgs(env)...)
	args = append(args, bwrapNetworkArgs(env)...)
	args = append(args, "--tmpfs", "/tmp")
	args = append(args, bwrapResourceArgs(prep, env)...)
	timeoutStr := stringFromObject(getObject(env.TypeSpecific, "resources"), "timeout")
	if timeoutDur := parseTimeout(timeoutStr); timeoutDur > 0 {
		args = append(args, "--timeout", fmt.Sprintf("%d", int(timeoutDur.Seconds())),
			"--die-with-parent", "--new-session", prep.TargetPath)
	} else {
		args = append(args, "--die-with-parent", "--new-session", prep.TargetPath)
	}

	cmd := exec.Command(bwrapPath, args[1:]...)
	cmd.Env = scrubEnv(os.Environ())
	return cmd
}

func isBubblewrapOptIn(env *workflow.EnvironmentNode) bool {
	if env.TypeSpecific == nil {
		return false
	}
	v, ok := env.TypeSpecific["sandbox"]
	if !ok {
		return false
	}
	return v.Type() == cty.String && v.IsKnown() && !v.IsNull() && v.AsString() == "bwrap"
}

func bwrapFilesystemArgs(env *workflow.EnvironmentNode) []string {
	readPaths := pathListFromObject(getObject(env.TypeSpecific, "filesystem"), "read")
	writePaths := pathListFromObject(getObject(env.TypeSpecific, "filesystem"), "write")
	args := make([]string, 0, len(readPaths)*3+len(writePaths)*3)
	for _, p := range readPaths {
		args = append(args, "--ro-bind", p, p)
	}
	for _, p := range writePaths {
		args = append(args, "--bind", p, p)
	}
	return args
}

func bwrapNetworkArgs(env *workflow.EnvironmentNode) []string {
	netAllow := pathListFromObject(getObject(env.TypeSpecific, "network"), "allow")
	if len(netAllow) == 0 {
		return []string{"--unshare-net"}
	}
	return nil
}

func bwrapResourceArgs(prep *LinuxPrepared, env *workflow.EnvironmentNode) []string {
	var args []string
	resObj := getObject(env.TypeSpecific, "resources")
	memStr := stringFromObject(resObj, "memory")
	if memStr == "" && prep.Rlimits != nil {
		for _, rl := range prep.Rlimits {
			if rl.Resource == syscall.RLIMIT_AS {
				memStr = strconv.FormatUint(rl.Rlimit.Cur, 10)
				break
			}
		}
	}
	if memBytes := parseMemoryLimit(memStr); memBytes > 0 {
		args = append(args, "--rlimit-as", strconv.FormatUint(memBytes, 10))
	}
	return args
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
