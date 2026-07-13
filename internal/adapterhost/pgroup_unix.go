//go:build unix

package adapterhost

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the adapter subprocess in its own process group so the
// whole tree — the adapter plus any children it spawns (e.g. the claude-agent
// adapter's Claude Code subprocess) — can be torn down together. Without this,
// host teardown kills only the adapter PID and orphans its grandchildren, which
// is what produces the "plugin failed to exit gracefully" / "signal: killed"
// teardown noise: a grandchild holding the host's inherited stdio pipes keeps
// go-plugin's drain goroutines from seeing EOF.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// signalProcessGroup sends sig to every process in the adapter's process group.
// With Setpgid the group id equals the leader's PID, so a negative PID targets
// the group. Best-effort: by the time the backstop SIGKILL fires the group may
// already be gone (ESRCH), which is expected and ignored.
func signalProcessGroup(cmd *exec.Cmd, sig syscall.Signal) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, sig) // best-effort teardown
}
