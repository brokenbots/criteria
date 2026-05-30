package adapter_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/brokenbots/criteria/internal/adapter"
)

func TestHeartbeatMonitor_RecordAndLast(t *testing.T) {
	m := &adapter.HeartbeatMonitor{}
	assert.True(t, m.Last().IsZero(), "zero value before any record")

	now := time.Now().Truncate(0) // strip monotonic component
	m.RecordAt(now)
	assert.Equal(t, now, m.Last())
}

func TestHeartbeatMonitor_Stalled(t *testing.T) {
	m := &adapter.HeartbeatMonitor{}
	assert.True(t, m.Stalled(time.Second), "no heartbeat → stalled")

	m.RecordAt(time.Now().Add(-2 * time.Second))
	assert.True(t, m.Stalled(time.Second), "2s ago > 1s threshold → stalled")
	assert.False(t, m.Stalled(5*time.Second), "2s ago < 5s threshold → not stalled")
}

func TestHeartbeatMonitor_Concurrent(t *testing.T) {
	m := &adapter.HeartbeatMonitor{}
	m.Record()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			m.Record()
		}
		close(done)
	}()

	for i := 0; i < 1000; i++ {
		_ = m.Stalled(time.Hour)
		_ = m.Last()
	}
	<-done

	assert.False(t, m.Stalled(time.Hour), "should not be stalled after concurrent records")
}
