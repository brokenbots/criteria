// Package log provides host-side log stream utilities for the Criteria runtime.
package log

import (
	"container/heap"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/brokenbots/criteria/internal/adapter"
)

// EventKind distinguishes log lines from adapter events.
type EventKind int

const (
	KindLog EventKind = iota
	KindAdapter
)

// TimestampedSink extends adapter.EventSink with timestamp-aware methods.
// LogForwardSink uses this interface when available so that adapter-supplied
// timestamps are preserved instead of being replaced with arrival time.
type TimestampedSink interface {
	adapter.EventSink
	LogAt(ts time.Time, stream string, chunk []byte)
	AdapterAt(ts time.Time, kind string, data any)
}

// MergeBuffer buffers log and adapter events and flushes them in timestamp
// order. Out-of-order arrival within maxDelay (default 500 ms) is tolerated:
// events younger than (oldest + maxDelay) are held; older events are flushed.
//
// MergeBuffer implements adapter.EventSink so it can be used as a downstream
// sink wrapper. Log events carry their timestamp from the adapter; adapter
// events use the time of arrival unless an explicit timestamp is provided via
// AdapterAt.
type MergeBuffer struct {
	inner    adapter.EventSink
	maxDelay time.Duration

	mu       sync.Mutex
	queue    eventHeap
	timer    *time.Timer
	flushDue time.Time // zero when no timer is active
}

// NewMergeBuffer creates a merge buffer that flushes to sink. maxDelay
// controls the reordering window; zero defaults to 500 ms.
func NewMergeBuffer(sink adapter.EventSink, maxDelay time.Duration) *MergeBuffer {
	if maxDelay <= 0 {
		maxDelay = 500 * time.Millisecond
	}
	return &MergeBuffer{
		inner:    sink,
		maxDelay: maxDelay,
	}
}

// Log buffers a log line with the given timestamp.
func (m *MergeBuffer) LogAt(ts time.Time, stream string, chunk []byte) {
	m.mu.Lock()
	heap.Push(&m.queue, &eventItem{ts: ts, kind: KindLog, stream: stream, line: append([]byte(nil), chunk...)})
	m.scheduleFlushLocked()
	m.mu.Unlock()
}

// Log implements adapter.EventSink, using the current time as the timestamp.
func (m *MergeBuffer) Log(stream string, chunk []byte) {
	m.LogAt(time.Now(), stream, chunk)
}

// AdapterAt buffers an adapter event with an explicit timestamp.
func (m *MergeBuffer) AdapterAt(ts time.Time, kind string, data any) {
	m.mu.Lock()
	heap.Push(&m.queue, &eventItem{ts: ts, kind: KindAdapter, kindName: kind, data: data})
	m.scheduleFlushLocked()
	m.mu.Unlock()
}

// Adapter implements adapter.EventSink, using the current time as timestamp.
func (m *MergeBuffer) Adapter(kind string, data any) {
	m.AdapterAt(time.Now(), kind, data)
}

// Flush forces all buffered events to be emitted in timestamp order.
func (m *MergeBuffer) Flush() {
	m.mu.Lock()
	m.flushAllLocked()
	m.mu.Unlock()
}

// scheduleFlushLocked starts or extends a timer so that events older than
// maxDelay from the newest event are flushed.
func (m *MergeBuffer) scheduleFlushLocked() {
	if m.queue.Len() == 0 {
		return
	}
	// The earliest event determines when we must flush (earliest + maxDelay).
	earliest := m.queue[0].ts
	due := earliest.Add(m.maxDelay)
	if m.timer != nil {
		// Extend the timer if this event pushes the due time further out.
		if due.After(m.flushDue) {
			m.flushDue = due
			if !m.timer.Stop() {
				<-m.timer.C
			}
			m.timer.Reset(time.Until(due))
		}
		return
	}
	m.flushDue = due
	m.timer = time.AfterFunc(time.Until(due), func() {
		m.mu.Lock()
		m.flushLocked()
		m.mu.Unlock()
	})
}

// flushLocked emits every event whose timestamp is <= now - maxDelay.
// Must be called with mu held.
func (m *MergeBuffer) flushLocked() {
	now := time.Now()
	cutoff := now.Add(-m.maxDelay)
	for m.queue.Len() > 0 {
		item := m.queue[0]
		if item.ts.After(cutoff) {
			break
		}
		heap.Pop(&m.queue)
		switch item.kind {
		case KindLog:
			m.inner.Log(item.stream, item.line)
		case KindAdapter:
			m.inner.Adapter(item.kindName, item.data)
		}
	}
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
	m.flushDue = time.Time{}
	// If there are still items, reschedule for the next earliest event.
	if m.queue.Len() > 0 {
		m.scheduleFlushLocked()
	}
}

// flushAllLocked emits every buffered event regardless of age.
// Must be called with mu held.
func (m *MergeBuffer) flushAllLocked() {
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
	m.flushDue = time.Time{}
	tsSink, _ := m.inner.(TimestampedSink)
	for m.queue.Len() > 0 {
		item := heap.Pop(&m.queue).(*eventItem)
		switch item.kind {
		case KindLog:
			if tsSink != nil {
				tsSink.LogAt(item.ts, item.stream, item.line)
			} else {
				m.inner.Log(item.stream, item.line)
			}
		case KindAdapter:
			if tsSink != nil {
				tsSink.AdapterAt(item.ts, item.kindName, item.data)
			} else {
				m.inner.Adapter(item.kindName, item.data)
			}
		}
	}
}

// Close flushes all remaining events regardless of age.
func (m *MergeBuffer) Close() {
	m.mu.Lock()
	m.flushAllLocked()
	m.mu.Unlock()
}

// WaitFlush is a test helper that blocks until the internal queue is empty.
func (m *MergeBuffer) WaitFlush(ctx context.Context) error {
	for {
		m.mu.Lock()
		empty := m.queue.Len() == 0
		m.mu.Unlock()
		if empty {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// ---- heap implementation ----

type eventItem struct {
	ts       time.Time
	kind     EventKind
	stream   string
	line     []byte
	kindName string
	data     any
}

type eventHeap []*eventItem

func (h eventHeap) Len() int           { return len(h) }
func (h eventHeap) Less(i, j int) bool { return h[i].ts.Before(h[j].ts) }
func (h eventHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *eventHeap) Push(x any)        { *h = append(*h, x.(*eventItem)) }
func (h *eventHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}

var _ adapter.EventSink = (*MergeBuffer)(nil)

// ---- helpers ----

// FormatLogLine returns a printable string for a log chunk, trimming the
// trailing newline if present.
func FormatLogLine(stream string, chunk []byte) string {
	line := string(chunk)
	if line != "" && line[len(line)-1] == '\n' {
		line = line[:len(line)-1]
	}
	return fmt.Sprintf("[%s] %s", stream, line)
}
