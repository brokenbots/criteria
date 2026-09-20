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

// ProcessExited reports whether the go-plugin client backing the handle has
// observed its subprocess exit (CRI-271). Non-gRPC handles (built-in
// adapters, fakes) report false: no exit signal exists for them, so callers
// must fall back to error-message heuristics.
func ProcessExited(p Handle) bool {
	rpc, isRPC := p.(*rpcHandle)
	if !isRPC || rpc == nil || rpc.client == nil {
		return false
	}
	return rpc.client.Exited()
}
