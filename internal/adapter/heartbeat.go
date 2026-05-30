package adapter

import (
	"sync/atomic"
	"time"
)

// HeartbeatMonitor tracks the most recent heartbeat timestamp and reports
// whether the adapter has gone silent for longer than a threshold.  It is
// safe for concurrent use.
type HeartbeatMonitor struct {
	lastNs atomic.Int64 // Unix nanoseconds; 0 means no heartbeat yet
}

// Record stores the current time as the latest heartbeat.  Called by the
// host-side log consumer whenever a Heartbeat message arrives from the
// adapter (WS15).
func (m *HeartbeatMonitor) Record() {
	m.lastNs.Store(time.Now().UnixNano())
}

// RecordAt stores an explicit timestamp.  Useful for tests.
func (m *HeartbeatMonitor) RecordAt(t time.Time) {
	m.lastNs.Store(t.UnixNano())
}

// Last returns the most recently recorded heartbeat time, or the zero time
// if none has been recorded.
func (m *HeartbeatMonitor) Last() time.Time {
	ns := m.lastNs.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// Stalled reports true when no heartbeat has been recorded in the last
// `maxAge` duration.  If no heartbeat has ever been recorded, Stalled
// returns true (the first heartbeat is considered due immediately).
func (m *HeartbeatMonitor) Stalled(maxAge time.Duration) bool {
	ns := m.lastNs.Load()
	if ns == 0 {
		return true
	}
	return time.Since(time.Unix(0, ns)) > maxAge
}
