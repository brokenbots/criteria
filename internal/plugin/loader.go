package plugin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	hplugin "github.com/hashicorp/go-plugin"

	"github.com/brokenbots/criteria/internal/adapter"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	"github.com/brokenbots/criteria/workflow"
)

// pluginClientLogger returns the hclog logger handed to go-plugin clients.
// go-plugin's default logger emits TRACE/DEBUG lines for every handshake and
// stdio frame, which dominates standalone output. Default to WARN; allow
// override via CRITERIA_LOG_LEVEL=trace|debug|info|warn|error.
func pluginClientLogger() hclog.Logger {
	level := hclog.Warn
	if v := strings.TrimSpace(os.Getenv("CRITERIA_LOG_LEVEL")); v != "" {
		if parsed := hclog.LevelFromString(v); parsed != hclog.NoLevel {
			level = parsed
		}
	}
	return hclog.New(&hclog.LoggerOptions{
		Name:   "plugin",
		Output: os.Stderr,
		Level:  level,
	})
}

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
	AdapterInfo  workflow.AdapterInfo
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

func (l *DefaultLoader) Resolve(ctx context.Context, name string) (Plugin, error) { //nolint:funlen // W03: resolver must handle builtin registry, discovery, launch, handshake, and caching paths
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
		Plugins:         PluginMap(),
		// Use a process command decoupled from per-step timeout contexts.
		// Session and loader shutdown are the only teardown mechanisms.
		Cmd:              exec.Command(path),
		AllowedProtocols: []hplugin.Protocol{hplugin.ProtocolGRPC},
		StartTimeout:     5 * time.Second,
		Logger:           pluginClientLogger(),
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
	return Info{
		Name:         resp.GetName(),
		Version:      resp.GetVersion(),
		Capabilities: append([]string(nil), resp.GetCapabilities()...),
		AdapterInfo:  AdapterInfoFromProto(resp),
	}, nil
}

func (p *rpcPlugin) OpenSession(ctx context.Context, id string, config map[string]string) error {
	_, err := p.rpc.OpenSession(ctx, &pb.OpenSessionRequest{SessionId: id, Config: cloneConfig(config)})
	return err
}

func (p *rpcPlugin) Execute(ctx context.Context, sessionID string, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) { //nolint:funlen,gocognit,gocyclo // W03: execute path handles permission gating, event routing, and partial failure recovery
	recv, err := p.rpc.Execute(ctx, &pb.ExecuteRequest{
		SessionId:       sessionID,
		StepName:        step.Name,
		Config:          cloneConfig(step.Input),
		AllowedOutcomes: collectAllowedOutcomes(step),
	})
	if err != nil {
		return adapter.Result{Outcome: "failure"}, err
	}
	policy := NewPolicyWithAliases(step.AllowTools, adapterPermissionAliases[p.name])
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
			pr := PermissionRequest{
				ID:      req.GetId(),
				Tool:    req.GetPermission(),
				Details: req.GetDetails(),
			}
			allow, reason := policy.Decide(pr)
			if allow {
				sink.Adapter("permission.granted", map[string]any{
					"tool":       pr.Tool,
					"pattern":    strings.TrimPrefix(reason, "matched: "),
					"request_id": pr.ID,
				})
			} else {
				allowTools := step.AllowTools
				if allowTools == nil {
					allowTools = []string{}
				}
				denial := map[string]any{
					"tool":        pr.Tool,
					"reason":      reason,
					"request_id":  pr.ID,
					"allow_tools": allowTools,
				}
				if suggestion := PermissionDenialSuggestion(p.name, pr.Tool); suggestion != "" {
					denial["suggestion"] = suggestion
				}
				sink.Adapter("permission.denied", denial)
			}
			if permitErr := p.Permit(ctx, sessionID, req.GetId(), allow, reason); permitErr != nil {
				return adapter.Result{Outcome: "failure"}, permitErr
			}
			continue
		}
		if resultEvt := evt.GetResult(); resultEvt != nil {
			result := adapter.Result{Outcome: resultEvt.GetOutcome()}
			if outs := resultEvt.GetOutputs(); len(outs) > 0 {
				result.Outputs = make(map[string]string, len(outs))
				for k, v := range outs {
					result.Outputs[k] = v
				}
			}
			return result, nil
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

// collectAllowedOutcomes returns the declared outcome names for a step,
// sorted ascending for determinism. Returns an empty (non-nil) slice
// when the step has no outcomes declared (terminal-routing steps,
// iteration steps that route via cursor outcomes, etc.).
func collectAllowedOutcomes(step *workflow.StepNode) []string {
	if len(step.Outcomes) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(step.Outcomes))
	for name := range step.Outcomes {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
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

// AdapterInfoFromProto translates a proto InfoResponse into a workflow.AdapterInfo.
// Legacy plugins that do not populate config_schema or input_schema will yield
// an empty AdapterInfo (permissive: any keys accepted by the compiler).
func AdapterInfoFromProto(resp *pb.InfoResponse) workflow.AdapterInfo {
	return workflow.AdapterInfo{
		ConfigSchema: protoToConfigSchema(resp.GetConfigSchema()),
		InputSchema:  protoToConfigSchema(resp.GetInputSchema()),
	}
}

func protoToConfigSchema(s *pb.AdapterSchemaProto) map[string]workflow.ConfigField {
	if s == nil || len(s.GetFields()) == 0 {
		return nil
	}
	out := make(map[string]workflow.ConfigField, len(s.GetFields()))
	for k, f := range s.GetFields() {
		out[k] = workflow.ConfigField{
			Required: f.GetRequired(),
			Type:     protoToConfigFieldType(f.GetType()),
			Doc:      f.GetDoc(),
		}
	}
	return out
}

func protoToConfigFieldType(t string) workflow.ConfigFieldType {
	switch t {
	case "number":
		return workflow.ConfigFieldNumber
	case "bool":
		return workflow.ConfigFieldBool
	case "list_string":
		return workflow.ConfigFieldListString
	default:
		return workflow.ConfigFieldString
	}
}
