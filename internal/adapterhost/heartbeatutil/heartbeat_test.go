package heartbeatutil

import (
	"context"
	"sync"
	"testing"
	"time"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"

	"github.com/brokenbots/criteria/internal/tunables"
)

// heartbeatRecorder collects heartbeat events sent through the sender seam.
type heartbeatRecorder struct {
	mu      sync.Mutex
	records int
	err     error
}

func (r *heartbeatRecorder) Send(*v2.LogEvent) error {
	if r.err != nil {
		return r.err
	}
	r.mu.Lock()
	r.records++
	r.mu.Unlock()
	return nil
}

func (r *heartbeatRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.records
}

// TestRunLogHeartbeatEnvCadence pins the KB-132 hatch replacement: the
// registered lenient override (CRITERIA_HEARTBEAT_INTERVAL) drives the
// cadence, so a test override yields a fast cadence without waiting the
// production interval.
func TestRunLogHeartbeatEnvCadence(t *testing.T) {
	t.Setenv(tunables.EnvHeartbeatInterval, "5ms")
	rec := &heartbeatRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunLogHeartbeat(ctx, rec) }()
	// Two beats need ~10ms; a second is a generous, race-safe bound while
	// still failing loudly if the cadence fell back to the 30s default.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && rec.count() < 2 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunLogHeartbeat on host-initiated cancel: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("RunLogHeartbeat did not return after cancellation")
	}
	if got := rec.count(); got < 2 {
		t.Fatalf("heartbeat cadence override %s=%s did not apply: %d beats, want >= 2", tunables.EnvHeartbeatInterval, "5ms", got)
	}
}

// TestRunLogHeartbeatDefaultCadenceSelection pins that an unset, malformed,
// or non-positive override keeps the built-in default instead of disabling
// or shortening the cadence.
func TestRunLogHeartbeatDefaultCadenceSelection(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		{"unset", ""},
		{"malformed", "garbage"},
		{"zero", "0s"},
		{"negative", "-1s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.value != "" {
				t.Setenv(tunables.EnvHeartbeatInterval, tc.value)
			}
			if got := tunables.FromEnv().HeartbeatInterval; got != tunables.DefaultHeartbeatInterval {
				t.Errorf("cadence with %s=%q = %s, want the built-in default %s (and it must never read 0)", tunables.EnvHeartbeatInterval, tc.value, got, tunables.DefaultHeartbeatInterval)
			}
		})
	}
}