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
