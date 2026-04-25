package plugin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"

	"github.com/brokenbots/overlord/overseer/internal/adapter"
	pb "github.com/brokenbots/overlord/shared/pb/overlord/v1"
	"github.com/brokenbots/overlord/workflow"
	hplugin "github.com/hashicorp/go-plugin"
)

type Loader interface {
	// Resolve returns a Plugin handle for the named adapter, spawning
	// the binary if necessary. Multiple calls with the same name return
	// distinct Plugin handles (one per session).
	Resolve(ctx context.Context, name string) (Plugin, error)
	Shutdown(ctx context.Context) error
}

type Plugin interface {
	Info(ctx context.Context) (Info, error)
	OpenSession(ctx context.Context, id string, config map[string]string) error
	Execute(ctx context.Context, sessionID string, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error)
	Permit(ctx context.Context, sessionID, permID string, allow bool, reason string) error
	CloseSession(ctx context.Context, id string) error
	Kill()
}

type Info struct {
	Name         string
	Version      string
	Capabilities []string
}

type DiscoveryFunc func(name string) (string, error)
type BuiltinFactory func() Plugin

type DefaultLoader struct {
	mu       sync.Mutex
	discover DiscoveryFunc
	builtins map[string]BuiltinFactory
	active   map[*rpcPlugin]struct{}
}

func NewLoader() *DefaultLoader {
	return &DefaultLoader{
		discover: DiscoverBinary,
		builtins: map[string]BuiltinFactory{},
		active:   map[*rpcPlugin]struct{}{},
	}
}

func NewLoaderWithDiscovery(discover DiscoveryFunc) *DefaultLoader {
	ldr := NewLoader()
	if discover != nil {
		ldr.discover = discover
	}
	return ldr
}

func (l *DefaultLoader) RegisterBuiltin(name string, factory BuiltinFactory) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if stringsTrim(name) == "" || factory == nil {
		return
	}
	l.builtins[name] = factory
}

func (l *DefaultLoader) Resolve(ctx context.Context, name string) (Plugin, error) {
	if stringsTrim(name) == "" {
		return nil, errors.New("adapter name is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	l.mu.Lock()
	if factory, ok := l.builtins[name]; ok {
		l.mu.Unlock()
		return factory(), nil
	}
	discover := l.discover
	l.mu.Unlock()

	path, err := discover(name)
	if err != nil {
		return nil, err
	}

	client := hplugin.NewClient(&hplugin.ClientConfig{
		HandshakeConfig: HandshakeConfig,
		Plugins:         PluginMap(nil),
		// Use a process command decoupled from per-step timeout contexts.
		// Session and loader shutdown are the only teardown mechanisms.
		Cmd:              exec.Command(path),
		AllowedProtocols: []hplugin.Protocol{hplugin.ProtocolGRPC},
		StartTimeout:     5 * time.Second,
	})

	rpcClient, err := client.Client()
	if err != nil {
		client.Kill()
		return nil, fmt.Errorf("start plugin %q: %w", name, err)
	}
	raw, err := rpcClient.Dispense(PluginName)
	if err != nil {
		client.Kill()
		return nil, fmt.Errorf("dispense plugin %q: %w", name, err)
	}

	adapterClient, ok := raw.(Client)
	if !ok {
		client.Kill()
		return nil, fmt.Errorf("unexpected plugin client type %T for %q", raw, name)
	}

	rp := &rpcPlugin{name: name, client: client, rpc: adapterClient}
	l.mu.Lock()
	l.active[rp] = struct{}{}
	l.mu.Unlock()
	rp.onKill = func() {
		l.mu.Lock()
		delete(l.active, rp)
		l.mu.Unlock()
	}

	return rp, nil
}

func (l *DefaultLoader) Shutdown(context.Context) error {
	l.mu.Lock()
	active := make([]*rpcPlugin, 0, len(l.active))
	for p := range l.active {
		active = append(active, p)
	}
	l.active = map[*rpcPlugin]struct{}{}
	l.mu.Unlock()

	for _, p := range active {
		p.Kill()
	}
	return nil
}

type rpcPlugin struct {
	name   string
	client *hplugin.Client
	rpc    Client

	mu     sync.Once
	onKill func()
}

func (p *rpcPlugin) Info(ctx context.Context) (Info, error) {
	resp, err := p.rpc.Info(ctx, &pb.InfoRequest{})
	if err != nil {
		return Info{}, err
	}
	return Info{Name: resp.GetName(), Version: resp.GetVersion(), Capabilities: append([]string(nil), resp.GetCapabilities()...)}, nil
}

func (p *rpcPlugin) OpenSession(ctx context.Context, id string, config map[string]string) error {
	_, err := p.rpc.OpenSession(ctx, &pb.OpenSessionRequest{SessionId: id, Config: cloneConfig(config)})
	return err
}

func (p *rpcPlugin) Execute(ctx context.Context, sessionID string, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	recv, err := p.rpc.Execute(ctx, &pb.ExecuteRequest{SessionId: sessionID, StepName: step.Name, Config: cloneConfig(step.Config)})
	if err != nil {
		return adapter.Result{Outcome: "failure"}, err
	}
	for {
		evt, recvErr := recv.Recv()
		if recvErr != nil {
			if errors.Is(recvErr, io.EOF) {
				return adapter.Result{Outcome: "failure"}, errors.New("plugin execute stream ended without result")
			}
			return adapter.Result{Outcome: "failure"}, recvErr
		}
		if logEvt := evt.GetLog(); logEvt != nil {
			sink.Log(logEvt.GetStream(), logEvt.GetChunk())
			continue
		}
		if adapterEvt := evt.GetAdapter(); adapterEvt != nil {
			if adapterEvt.GetData() != nil {
				sink.Adapter(adapterEvt.GetKind(), adapterEvt.GetData().AsMap())
			} else {
				sink.Adapter(adapterEvt.GetKind(), nil)
			}
			continue
		}
		if req := evt.GetPermission(); req != nil {
			sink.Adapter("permission.request", map[string]any{
				"id":            req.GetId(),
				"tool":          req.GetPermission(),
				"permission_id": req.GetId(),
				"permission":    req.GetPermission(),
				"details":       req.GetDetails(),
				"decision":      "deny",
			})
			if permitErr := p.Permit(ctx, sessionID, req.GetId(), false, "permission gating not implemented"); permitErr != nil {
				return adapter.Result{Outcome: "failure"}, permitErr
			}
			continue
		}
		if resultEvt := evt.GetResult(); resultEvt != nil {
			return adapter.Result{Outcome: resultEvt.GetOutcome()}, nil
		}
	}
}

func (p *rpcPlugin) Permit(ctx context.Context, sessionID, permID string, allow bool, reason string) error {
	_, err := p.rpc.Permit(ctx, &pb.PermitRequest{
		SessionId:    sessionID,
		PermissionId: permID,
		Allow:        allow,
		Reason:       reason,
	})
	return err
}

func (p *rpcPlugin) CloseSession(ctx context.Context, id string) error {
	_, err := p.rpc.CloseSession(ctx, &pb.CloseSessionRequest{SessionId: id})
	return err
}

func (p *rpcPlugin) Kill() {
	p.mu.Do(func() {
		if p.client != nil {
			p.client.Kill()
		}
		if p.onKill != nil {
			p.onKill()
		}
	})
}

func cloneConfig(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func stringsTrim(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\n' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 {
		last := s[len(s)-1]
		if last != ' ' && last != '\t' && last != '\n' && last != '\r' {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}
