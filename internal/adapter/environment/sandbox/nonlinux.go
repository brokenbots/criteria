//go:build !linux

package sandbox

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
)

// Capabilities is always empty on non-Linux platforms.
type Capabilities struct{}

// Missing always returns an empty slice on non-Linux.
func (c Capabilities) Missing() []string { return nil }

// Probe returns empty capabilities on non-Linux.
func Probe() Capabilities { return Capabilities{} }

// Handler is a no-op stub on non-Linux.
type Handler struct{}

// PrepareContext is a placeholder on non-Linux.
type PrepareContext struct{}

// Prepare always returns an error on non-Linux.
func (h Handler) Prepare(_ PrepareContext) (LinuxPrepared, error) {
	return LinuxPrepared{}, fmt.Errorf("sandbox environments are only supported on linux (current OS is %s)", runtime.GOOS)
}

// LinuxPrepared is a no-op placeholder on non-Linux.
type LinuxPrepared struct{}

// ApplyToCmd is a no-op on non-Linux.
func (prep *LinuxPrepared) ApplyToCmd(cmd *exec.Cmd, criteriaBin string) error {
	return errors.New("sandbox environments are only supported on Linux")
}

// ShimConfig is unused on non-Linux.
type ShimConfig struct{}

// ApplyEnv is a no-op on non-Linux.
func ApplyEnv() error { return nil }

// RunIfEnv always returns false on non-Linux.
func RunIfEnv() (bool, error) { return false, nil }

// RlimitConfig is unused on non-Linux.
type RlimitConfig struct{}
