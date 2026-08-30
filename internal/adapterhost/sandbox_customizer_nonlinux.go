//go:build !linux

package adapterhost

import (
	"os/exec"

	"github.com/brokenbots/criteria/internal/adapter/environment/sandbox"
)

// configureLinuxShimForTest is a no-op on non-Linux platforms; the dedicated
// shim helper and its rlimit adjustments are Linux-only.
func configureLinuxShimForTest(_ *sandbox.LinuxPrepared, _ string) {}

// finalizeLinuxShimCmd is a no-op on non-Linux platforms; SysProcAttr and the
// dedicated shim helper are Linux-only concepts.
func finalizeLinuxShimCmd(_ *exec.Cmd, _ string) {}

// seedLinuxTargetPath is a no-op on non-Linux platforms; the field does not
// exist in the non-Linux LinuxPrepared stubs.
func seedLinuxTargetPath(_ *sandbox.LinuxPrepared, _ string) {}
