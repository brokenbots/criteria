//go:build windows

package shell

import (
	"os/exec"
)

// setSIGTERMCancel is a no-op on Windows. The default cmd.Cancel (Kill) is
// used; cmd.WaitDelay gives the process a grace period before WaitDelay fires
// and forcefully terminates it.
func setSIGTERMCancel(_ *exec.Cmd) {}

// isTerminalStdin reports whether os.Stdin is attached to a terminal on Windows.
// TERM is not meaningful on Windows, so this always returns false.
func isTerminalStdin() bool { return false }
