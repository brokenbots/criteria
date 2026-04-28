//go:build unix

package shell

import (
	"os"
	"os/exec"
	"syscall"
)

// setSIGTERMCancel configures cmd so that when its context is cancelled the
// process receives SIGTERM instead of SIGKILL. Combined with cmd.WaitDelay
// (set to killGrace in Execute), this gives the child a grace period to clean
// up before SIGKILL is sent.
func setSIGTERMCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Signal(syscall.SIGTERM)
	}
}

// isTerminalStdin reports whether os.Stdin is attached to a terminal (TTY).
// Uses the character-device flag which is reliable on Linux and macOS.
func isTerminalStdin() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & (os.ModeCharDevice | os.ModeDevice)) == (os.ModeCharDevice | os.ModeDevice)
}
