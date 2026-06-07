package adapterhost

import (
	"context"
	"sync"

	"github.com/brokenbots/criteria/internal/adapter"
)

type recordingLoader struct {
	inner   Loader
	mu      sync.Mutex
	handles []Handle
}

func (l *recordingLoader) Resolve(ctx context.Context, name string) (Handle, error) {
	p, err := l.inner.Resolve(ctx, name)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	l.handles = append(l.handles, p)
	l.mu.Unlock()
	return p, nil
}

func (l *recordingLoader) Shutdown(ctx context.Context) error { return l.inner.Shutdown(ctx) }

func (l *recordingLoader) lastHandle() Handle {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.handles) == 0 {
		return nil
	}
	return l.handles[len(l.handles)-1]
}

type adapterEventCollector struct {
	mu     sync.Mutex
	events []adapterEvent
}

type adapterEvent struct {
	kind string
	data map[string]any
}

func (c *adapterEventCollector) Log(string, []byte) {}

func (c *adapterEventCollector) Adapter(kind string, data any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var payload map[string]any
	if m, ok := data.(map[string]any); ok {
		payload = m
	}
	c.events = append(c.events, adapterEvent{kind: kind, data: payload})
}

func (c *adapterEventCollector) saw(kind string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, evt := range c.events {
		if evt.kind == kind {
			return true
		}
	}
	return false
}

func (c *adapterEventCollector) first(kind string) (map[string]any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, evt := range c.events {
		if evt.kind == kind {
			return evt.data, true
		}
	}
	return nil, false
}

var _ adapter.EventSink = (*adapterEventCollector)(nil)
