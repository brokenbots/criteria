package adapterhost

// ProcessPID returns the underlying adapter subprocess PID when available.
// Built-in adapters and unsupported handle implementations return ok=false.
func ProcessPID(p Handle) (pid int, ok bool) {
	rpc, isRPC := p.(*rpcHandle)
	if !isRPC || rpc == nil || rpc.client == nil {
		return 0, false
	}
	rc := rpc.client.ReattachConfig()
	if rc == nil || rc.Pid <= 0 {
		return 0, false
	}
	return rc.Pid, true
}

// ProcessWaitReporter is an optional Handle capability: reporting the
// observed OS exit status of the adapter subprocess after it has exited
// (CRI-287). *rpcHandle implements it from the exec.Cmd wait status; test
// fakes may implement it to feed synthetic facts into classification paths.
type ProcessWaitReporter interface {
	ProcessWaitStatus() (exitCode, signal int, ok bool)
}

// ProcessWaitStatus reports the child's observed OS exit status once the
// child has exited: the terminating signal number (with exit code -1) when
// the process was killed by a signal, the exit code (with signal 0) on a
// normal exit. Handles that cannot observe a wait status — handles without a
// subprocess, subprocesses not yet reaped, or fake handles without a
// synthetic status — report ok=false, and callers must fall back to
// exit code -1 / signal 0 (unknown).
func ProcessWaitStatus(p Handle) (exitCode, signal int, ok bool) {
	reporter, isReporter := p.(ProcessWaitReporter)
	if !isReporter || reporter == nil {
		return -1, 0, false
	}
	return reporter.ProcessWaitStatus()
}

// ProcessExitReporter is an optional Handle capability: reporting whether the
// adapter subprocess backing the handle has exited (CRI-271). *rpcHandle
// implements it through the go-plugin client; test fakes implement it to
// exercise the process-exit classification without a real subprocess.
type ProcessExitReporter interface {
	ProcessExited() bool
}

// ProcessExited reports whether the go-plugin client backing the handle has
// observed its subprocess exit (CRI-271). Handles that cannot report exit
// (built-in adapters, plain fakes) report false: no exit signal exists for
// them, so callers must fall back to error-message heuristics.
func ProcessExited(p Handle) bool {
	reporter, ok := p.(ProcessExitReporter)
	if !ok || reporter == nil {
		return false
	}
	return reporter.ProcessExited()
}

// isPeerSupervised reports whether the handle's process-exit facts come
// from peer supervision (ADR-0007): a SupervisedHandle's ProcessExited is a
// journal fact delivered by the peer that observed the child, so the CRI-287
// teardown-window carve-out (a child dying with the engine's canceled turn)
// applies to it. Legacy-runner handles keep the conservative local rule.
func isPeerSupervised(h Handle) bool {
	_, ok := h.(SupervisedHandle)
	return ok
}
