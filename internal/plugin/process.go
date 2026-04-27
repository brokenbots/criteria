package plugin

// ProcessPID returns the underlying plugin subprocess PID when available.
// Built-in adapters and unsupported plugin implementations return ok=false.
func ProcessPID(p Plugin) (pid int, ok bool) {
	rpc, isRPC := p.(*rpcPlugin)
	if !isRPC || rpc == nil || rpc.client == nil {
		return 0, false
	}
	rc := rpc.client.ReattachConfig()
	if rc == nil || rc.Pid <= 0 {
		return 0, false
	}
	return rc.Pid, true
}
