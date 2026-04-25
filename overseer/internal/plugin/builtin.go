package plugin

import (
	"context"
	"fmt"
	"sync"

	"github.com/brokenbots/overlord/overseer/internal/adapter"
	"github.com/brokenbots/overlord/workflow"
)

func BuiltinFactoryForAdapter(ad adapter.Adapter) BuiltinFactory {
	return func() Plugin {
		return NewBuiltinAdapterPlugin(ad)
	}
}

func NewBuiltinAdapterPlugin(ad adapter.Adapter) Plugin {
	return &builtinAdapterPlugin{
		adapter:  ad,
		sessions: map[string]map[string]string{},
	}
}

type builtinAdapterPlugin struct {
	adapter adapter.Adapter

	mu       sync.Mutex
	sessions map[string]map[string]string
}

func (p *builtinAdapterPlugin) Info(context.Context) (Info, error) {
	if p.adapter == nil {
		return Info{}, fmt.Errorf("builtin adapter implementation is nil")
	}
	return Info{Name: p.adapter.Name(), Version: "builtin", Capabilities: nil}, nil
}

func (p *builtinAdapterPlugin) OpenSession(_ context.Context, id string, config map[string]string) error {
	if p.adapter == nil {
		return fmt.Errorf("builtin adapter implementation is nil")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.sessions[id]; exists {
		return fmt.Errorf("session %q already open", id)
	}
	p.sessions[id] = cloneConfig(config)
	return nil
}

func (p *builtinAdapterPlugin) Execute(ctx context.Context, sessionID string, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
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

func (p *builtinAdapterPlugin) Permit(context.Context, string, string, bool, string) error {
	return fmt.Errorf("permission gating is not implemented for builtin adapters")
}

func (p *builtinAdapterPlugin) CloseSession(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.sessions, id)
	return nil
}

func (p *builtinAdapterPlugin) Kill() {}
