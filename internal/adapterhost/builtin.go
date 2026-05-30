package adapterhost

import (
	"context"
	"fmt"
	"sync"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/workflow"
)

func BuiltinFactoryForAdapter(ad adapter.Adapter) BuiltinFactory {
	return func() Handle {
		return NewBuiltinAdapter(ad)
	}
}

func NewBuiltinAdapter(ad adapter.Adapter) Handle {
	return &builtinAdapter{
		adapter:  ad,
		sessions: map[string]map[string]string{},
	}
}

type builtinAdapter struct {
	adapter adapter.Adapter

	mu       sync.Mutex
	sessions map[string]map[string]string
}

func (p *builtinAdapter) Info(context.Context) (Info, error) {
	if p.adapter == nil {
		return Info{}, fmt.Errorf("builtin adapter implementation is nil")
	}
	adInfo := p.adapter.Info()
	return Info{
		Name:         p.adapter.Name(),
		Version:      "builtin",
		Capabilities: append([]string(nil), adInfo.Capabilities...),
		AdapterInfo:  adInfo,
	}, nil
}

func (p *builtinAdapter) OpenSession(_ context.Context, id string, config, secrets map[string]string) error {
	if p.adapter == nil {
		return fmt.Errorf("builtin adapter implementation is nil")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.sessions[id]; exists {
		return fmt.Errorf("session %q already open", id)
	}
	merged := cloneConfig(config)
	for k, v := range secrets {
		merged[k] = v
	}
	p.sessions[id] = merged
	return nil
}

func (p *builtinAdapter) Execute(ctx context.Context, sessionID string, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	if p.adapter == nil {
		return adapter.Result{Outcome: "failure"}, fmt.Errorf("builtin adapter implementation is nil")
	}
	p.mu.Lock()
	_, exists := p.sessions[sessionID]
	p.mu.Unlock()
	if !exists {
		return adapter.Result{Outcome: "failure"}, fmt.Errorf("unknown session %q", sessionID)
	}
	return p.adapter.Execute(ctx, step, sink)
}

func (p *builtinAdapter) CloseSession(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.sessions, id)
	return nil
}

func (p *builtinAdapter) Kill() {}
