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
