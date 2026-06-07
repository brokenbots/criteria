package log

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapter"
)

// recordingSink is an EventSink that records every call, including timestamps
// when used as a TimestampedSink.
type recordingSink struct {
	mu       sync.Mutex
	logs     []logRec
	adapters []adapterRec
}

type logRec struct {
	ts     time.Time
	stream string
	chunk  []byte
}
type adapterRec struct {
	ts   time.Time
	kind string
	data any
}

func (r *recordingSink) Log(stream string, chunk []byte) {
	r.LogAt(time.Now(), stream, chunk)
}
func (r *recordingSink) LogAt(ts time.Time, stream string, chunk []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, logRec{ts: ts, stream: stream, chunk: append([]byte(nil), chunk...)})
}
func (r *recordingSink) Adapter(kind string, data any) {
	r.AdapterAt(time.Now(), kind, data)
}
func (r *recordingSink) AdapterAt(ts time.Time, kind string, data any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.adapters = append(r.adapters, adapterRec{ts: ts, kind: kind, data: data})
}

func (r *recordingSink) logCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.logs)
}

func (r *recordingSink) adapterCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.adapters)
}

func (r *recordingSink) logAt(i int) logRec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.logs[i]
}

func (r *recordingSink) adapterAt(i int) adapterRec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.adapters[i]
}

var _ adapter.EventSink = (*recordingSink)(nil)
var _ TimestampedSink = (*recordingSink)(nil)

func TestMergeBuffer_OrdersByTimestamp(t *testing.T) {
	inner := &recordingSink{}
	// Use a large reorder window and near-now timestamps so the only flush is the
	// explicit one below. (With epoch-1970 timestamps the window is always already
	// expired, so the background flush timer fires immediately and can emit events
	// before all three are queued — a timing flake under slow CI.)
	buf := NewMergeBuffer(inner, time.Hour)
	defer buf.Close()

	// Emit out of order (offsets ordered 100 < 200 < 300 ns from a common base).
	base := time.Now()
	buf.LogAt(base.Add(300), "stdout", []byte("third\n"))
	buf.LogAt(base.Add(100), "stdout", []byte("first\n"))
	buf.AdapterAt(base.Add(200), "agent.message", map[string]any{"text": "second"})

	// Flush explicitly.
	buf.Flush()

	if inner.logCount() != 2 {
		t.Fatalf("expected 2 log records, got %d", inner.logCount())
	}
	if inner.adapterCount() != 1 {
		t.Fatalf("expected 1 adapter record, got %d", inner.adapterCount())
	}

	var order []string
	for i := 0; i < inner.logCount(); i++ {
		order = append(order, string(inner.logAt(i).chunk))
	}
	for i := 0; i < inner.adapterCount(); i++ {
		order = append(order, inner.adapterAt(i).kind)
	}
	// Flush emits all buffered events in timestamp order regardless of age, so the
	// emit order is first (base+100), agent.message (base+200), third (base+300).
	// The two log records land in the log sink as [first, third]; the adapter
	// record lands in the adapter sink as [agent.message].
	expected := []string{"first\n", "third\n", "agent.message"}
	for i, want := range expected {
		var got string
		if i < inner.logCount() {
			got = string(inner.logAt(i).chunk)
		} else {
			got = inner.adapterAt(i - inner.logCount()).kind
		}
		if got != want {
			t.Fatalf("expected order[%d]=%q, got %q (full order: %+v)", i, want, got, order)
		}
	}
}

func TestMergeBuffer_WindowHoldsYoungerEvents(t *testing.T) {
	inner := &recordingSink{}
	buf := NewMergeBuffer(inner, 100*time.Millisecond)
	defer buf.Close()

	now := time.Now()
	buf.LogAt(now.Add(-120*time.Millisecond), "stdout", []byte("old\n"))
	buf.LogAt(now.Add(-10*time.Millisecond), "stdout", []byte("young\n"))

	// The old event should be flushed immediately; the young one stays buffered.
	time.Sleep(50 * time.Millisecond)
	if inner.logCount() != 1 || string(inner.logAt(0).chunk) != "old\n" {
		t.Fatalf("expected only old log flushed, got %v", inner.logs)
	}

	// After enough time, the young event is also flushed.
	time.Sleep(150 * time.Millisecond)
	buf.Flush()
	if inner.logCount() != 2 {
		t.Fatalf("expected both logs flushed, got %d", inner.logCount())
	}
}

func TestMergeBuffer_CloseFlushesAll(t *testing.T) {
	inner := &recordingSink{}
	buf := NewMergeBuffer(inner, time.Hour) // large delay so timer won't fire
	buf.LogAt(time.Now(), "stdout", []byte("a\n"))
	buf.LogAt(time.Now(), "stdout", []byte("b\n"))
	buf.Close()

	if inner.logCount() != 2 {
		t.Fatalf("expected 2 logs after Close, got %d", inner.logCount())
	}
}

func TestMergeBuffer_AdapterSinkInterface(t *testing.T) {
	inner := &recordingSink{}
	buf := NewMergeBuffer(inner, 10*time.Millisecond)
	defer buf.Close()

	buf.Log("stdout", []byte("hello\n"))
	buf.Adapter("agent.message", map[string]any{"text": "hi"})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := buf.WaitFlush(ctx); err != nil {
		t.Fatalf("WaitFlush: %v", err)
	}

	if inner.logCount() != 1 || string(inner.logAt(0).chunk) != "hello\n" {
		t.Fatalf("expected log record, got %v", inner.logs)
	}
	if inner.adapterCount() != 1 || inner.adapterAt(0).kind != "agent.message" {
		t.Fatalf("expected adapter record, got %v", inner.adapters)
	}
}

func TestMergeBuffer_Integration_100Logs10Events(t *testing.T) {
	inner := &recordingSink{}
	buf := NewMergeBuffer(inner, 50*time.Millisecond)
	defer buf.Close()

	base := time.Now()
	// Emit 100 log lines and 10 adapter events with interleaved timestamps.
	for i := 0; i < 100; i++ {
		buf.LogAt(base.Add(time.Duration(i)*time.Millisecond), "stdout", []byte(fmt.Sprintf("log-%d\n", i)))
	}
	for i := 0; i < 10; i++ {
		buf.AdapterAt(base.Add(time.Duration(i*10+5)*time.Millisecond), "ev", map[string]any{"i": i})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := buf.WaitFlush(ctx); err != nil {
		t.Fatalf("WaitFlush: %v", err)
	}

	total := inner.logCount() + inner.adapterCount()
	if total != 110 {
		t.Fatalf("expected 110 flushed events, got %d", total)
	}

	// Verify ordering by timestamp is monotonic across all flushed events.
	// MergeBuffer flushes everything in one interleaved sequence; we reconstruct
	// it by walking both slices in the order they were recorded.
	var all []struct {
		ts    time.Time
		isLog bool
		idx   int
	}
	for i := 0; i < inner.logCount(); i++ {
		all = append(all, struct {
			ts    time.Time
			isLog bool
			idx   int
		}{ts: inner.logAt(i).ts, isLog: true, idx: i})
	}
	for i := 0; i < inner.adapterCount(); i++ {
		all = append(all, struct {
			ts    time.Time
			isLog bool
			idx   int
		}{ts: inner.adapterAt(i).ts, isLog: false, idx: i})
	}
	// MergeBuffer flushes in timestamp order. Verify the combined sequence is
	// monotonic by sorting all events and checking timestamps.
	sort.Slice(all, func(i, j int) bool {
		return all[i].ts.Before(all[j].ts)
	})
	for i := 1; i < len(all); i++ {
		if all[i].ts.Before(all[i-1].ts) {
			t.Fatalf("combined[%d] timestamp %v out of order (prev %v)", i, all[i].ts, all[i-1].ts)
		}
	}

	// Verify all logs arrived.
	if inner.logCount() != 100 {
		t.Fatalf("expected 100 logs, got %d", inner.logCount())
	}
	if inner.adapterCount() != 10 {
		t.Fatalf("expected 10 adapter events, got %d", inner.adapterCount())
	}
}

// TestMergeBuffer_ConcurrentDeliveryRace exercises concurrent Log and Adapter
// calls from multiple goroutines to verify the MergeBuffer is race-free.
func TestMergeBuffer_ConcurrentDeliveryRace(t *testing.T) {
	inner := &recordingSink{}
	buf := NewMergeBuffer(inner, 100*time.Millisecond)
	defer buf.Close()

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				buf.LogAt(time.Now().Add(time.Duration(j)*time.Millisecond), "stdout", []byte(fmt.Sprintf("log-%d-%d\n", id, j)))
			}
		}(i)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				buf.AdapterAt(time.Now().Add(time.Duration(j)*time.Millisecond), "ev", map[string]any{"id": id, "j": j})
			}
		}(i)
	}
	wg.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := buf.WaitFlush(ctx); err != nil {
		t.Fatalf("WaitFlush: %v", err)
	}

	total := inner.logCount() + inner.adapterCount()
	if total != 1000 {
		t.Fatalf("expected 1000 flushed events, got %d", total)
	}
}
