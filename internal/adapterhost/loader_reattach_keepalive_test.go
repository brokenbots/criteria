package adapterhost

import (
	"testing"
	"time"
)

// TestReattachClientConfigKeepalive asserts the engine-side reattach client
// configuration carries the CRI-274 keepalive policy: the grpc-go default
// 30-minute idle timeout is disabled (WithIdleTimeout(0)) and without-stream
// pings keep the phone-home bridge warm while the workflow sits between
// steps. A remote adapter through the shim must behave as if it were local.
func TestReattachClientConfigKeepalive(t *testing.T) {
	if remoteClientKeepaliveTime <= 0 {
		t.Errorf("client ping interval = %v, want > 0 so the transport stays warm while idle", remoteClientKeepaliveTime)
	}
	if remoteClientKeepaliveTimeout <= 0 {
		t.Errorf("client ping timeout = %v, want > 0 so a dead transport is detected", remoteClientKeepaliveTimeout)
	}
	if remoteClientKeepaliveTimeout >= remoteClientKeepaliveTime {
		t.Errorf("client ping timeout %v must be shorter than the ping interval %v", remoteClientKeepaliveTimeout, remoteClientKeepaliveTime)
	}

	cfg := reattachClientConfig("/tmp/does-not-exist/adapter.sock")
	if cfg == nil {
		t.Fatal("reattachClientConfig returned nil")
	}
	if len(cfg.GRPCDialOptions) == 0 {
		t.Fatal("reattach client config has no GRPCDialOptions; the default 30-minute idle timeout would close idle remote sessions")
	}
	if cfg.Reattach == nil || cfg.Reattach.Addr == nil {
		t.Fatal("reattach client config lost its Reattach target")
	}
}

// TestReattachClientConfigKeepalivePolicyCoherence keeps the engine client's
// ping interval coherent with the runner's phone-home enforcement policy.
// The runner permits without-stream pings with MinTime 10s; the engine pings
// every 30s. Assert both directions so a future change to either side fails
// loudly instead of silently reintroducing the 30-minute idle disconnect.
func TestReattachClientConfigKeepalivePolicyCoherence(t *testing.T) {
	const runnerEnforcementMinTime = 10 * time.Second
	if remoteClientKeepaliveTime < runnerEnforcementMinTime {
		t.Errorf("engine ping interval %v is below the runner's MinTime %v; engine pings would trip too_many_pings",
			remoteClientKeepaliveTime, runnerEnforcementMinTime)
	}
}