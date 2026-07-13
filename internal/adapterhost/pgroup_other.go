//go:build !unix

package adapterhost

import (
	"os/exec"
	"syscall"
)

// setProcessGroup is a no-op off Unix: Windows has no POSIX process group, and
// adapter teardown relies on go-plugin's own process reaping there.
func setProcessGroup(_ *exec.Cmd) {}

// signalProcessGroup is a no-op off Unix for the same reason.
func signalProcessGroup(_ *exec.Cmd, _ syscall.Signal) {}
