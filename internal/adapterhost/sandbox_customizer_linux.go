//go:build linux

package adapterhost

import (
	"os/exec"

	"golang.org/x/sys/unix"

	"github.com/brokenbots/criteria/internal/adapter/environment/sandbox"
)

// configureLinuxShimForTest sets up the prepared sandbox config for the
// test-only dedicated shim helper path. It is a no-op for the production path
// (shimBin == "").
func configureLinuxShimForTest(prep *sandbox.LinuxPrepared, shimBin string) {
	if shimBin == "" {
		return
	}
	// test-only: the dedicated shim helper is a short-lived wrapper. The Go
	// runtime starts worker threads before main, so installing a TSYNC seccomp
	// filter in-process is fragile on some kernels; skip landlock, seccomp,
	// rlimits and PR_SET_NO_NEW_PRIVS in the helper and just exec the real
	// adapter. The remaining rlimits are still inherited by the target.
	prep.SkipShimRestrictions = true
	prep.Rlimits = withoutRlimitNproc(prep.Rlimits)
}

// finalizeLinuxShimCmd performs platform-specific post-processing on cmd when
// a dedicated test shim binary is in use. It is a no-op for production.
func finalizeLinuxShimCmd(cmd *exec.Cmd, shimBin string) {
	if shimBin == "" {
		return
	}
	// test-only: keep the dedicated shim helper in host namespaces so
	// applyShimRestrictions can install seccomp without EPERM. seccomp,
	// landlock and PR_SET_NO_NEW_PRIVS survive syscall.Exec; namespaces do
	// not. The production path (shimBin == "") is unaffected.
	cmd.SysProcAttr = nil
}

// seedLinuxTargetPath copies cmdPath into prep.TargetPath when TargetPath is
// empty. The Linux ApplyToCmd path uses TargetPath as the executable to
// shim/restrict, so an empty value would cause a failed exec. Darwin's
// ApplyToCmd uses cmd.Path directly and has no TargetPath field.
func seedLinuxTargetPath(prep *sandbox.LinuxPrepared, cmdPath string) {
	if prep.TargetPath == "" {
		prep.TargetPath = cmdPath
	}
}

// withoutRlimitNproc returns a copy of rls with any RLIMIT_NPROC entries
// removed. It is used for the test-only dedicated shim helper.
func withoutRlimitNproc(rls []sandbox.RlimitConfig) []sandbox.RlimitConfig {
	out := make([]sandbox.RlimitConfig, 0, len(rls))
	for _, rl := range rls {
		if rl.Resource == unix.RLIMIT_NPROC {
			continue
		}
		out = append(out, rl)
	}
	return out
}
