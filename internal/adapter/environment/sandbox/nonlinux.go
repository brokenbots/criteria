//go:build !linux && !darwin

package sandbox

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"

	"github.com/brokenbots/criteria/workflow"
)

// Capabilities is always empty on non-Linux platforms.
type Capabilities struct{}

// Missing always returns an empty slice on non-Linux.
func (c Capabilities) Missing() []string { return nil }

// Probe returns empty capabilities on non-Linux.
func Probe() Capabilities { return Capabilities{} }

// Handler is a no-op stub on non-Linux.
type Handler struct{}

// PrepareContext is a placeholder on non-Linux. It mirrors the Linux
// struct shape so that cross-platform call sites compile without build
// tags.
type PrepareContext struct {
	Policy        *workflow.ResolvedPolicy
	Env           *workflow.EnvironmentNode
	Caps          Capabilities
	AdapterBinary string // populated at prepare time for darwin sandbox allow-listing; unused on non-linux
}

// Prepare always returns an error on non-Linux.
func (h Handler) Prepare(_ PrepareContext) (LinuxPrepared, error) {
	return LinuxPrepared{}, fmt.Errorf("sandbox environments are not supported on this OS (current OS is %s)", runtime.GOOS)
}

// LinuxPrepared is a no-op placeholder on non-Linux.
type LinuxPrepared struct {
	CgroupV2 *CgroupV2Config
}

// CgroupV2Config is unused on non-Linux.
type CgroupV2Config struct{}

// Cleanup is a no-op on non-Linux.
func (prep *LinuxPrepared) Cleanup() error { return nil }

// ApplyToCmd is a no-op on non-Linux.
func (prep *LinuxPrepared) ApplyToCmd(cmd *exec.Cmd, criteriaBin string) error {
	return errors.New("sandbox environments are not supported on this OS")
}

// MaybeUseBubblewrap always returns nil on non-Linux.
func MaybeUseBubblewrap(_ *LinuxPrepared, _ *workflow.EnvironmentNode, _ string) *exec.Cmd {
	return nil
}

// ShimConfig is unused on non-Linux.
type ShimConfig struct{}

// ApplyEnv is a no-op on non-Linux.
func ApplyEnv() error { return nil }

// RunIfEnv always returns false on non-Linux.
func RunIfEnv() (bool, error) { return false, nil }

// RlimitConfig is unused on non-Linux.
type RlimitConfig struct{}
