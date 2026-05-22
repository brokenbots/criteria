package adapterhost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	hplugin "github.com/hashicorp/go-plugin"

	"github.com/brokenbots/criteria/internal/adapter"
	v2 "github.com/brokenbots/criteria/proto/criteria/v2"
	"github.com/brokenbots/criteria/workflow"
)

// adapterClientLogger returns the hclog logger handed to go-plugin clients.
// go-plugin's default logger emits TRACE/DEBUG lines for every handshake and
// stdio frame, which dominates standalone output. Default to WARN; allow
// override via CRITERIA_LOG_LEVEL=trace|debug|info|warn|error.
func adapterClientLogger() hclog.Logger {
	level := hclog.Warn
	if v := strings.TrimSpace(os.Getenv("CRITERIA_LOG_LEVEL")); v != "" {
		if parsed := hclog.LevelFromString(v); parsed != hclog.NoLevel {
			level = parsed
		}
	}
	return hclog.New(&hclog.LoggerOptions{
		Name:   "adapter",
		Output: os.Stderr,
		Level:  level,
	})
}

type Loader interface {
	// Resolve returns a Handle for the named adapter, spawning
	// the binary if necessary. Multiple calls with the same name return
	// distinct Handle values (one per session).
	Resolve(ctx context.Context, name string) (Handle, error)
	Shutdown(ctx context.Context) error
}

type Handle interface {
	Info(ctx context.Context) (Info, error)
	OpenSession(ctx context.Context, id string, config map[string]string) error
	Execute(ctx context.Context, sessionID string, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error)
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
type BuiltinFactory func() Handle

type DefaultLoader struct {
	mu       sync.Mutex
	discover DiscoveryFunc
	builtins map[string]BuiltinFactory
	active   map[*rpcHandle]struct{}
}

func NewLoader() *DefaultLoader {
	return &DefaultLoader{
		discover: DiscoverBinary,
		builtins: map[string]BuiltinFactory{},
		active:   map[*rpcHandle]struct{}{},
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

func (l *DefaultLoader) Resolve(ctx context.Context, name string) (Handle, error) { //nolint:funlen // resolver must handle builtin registry, discovery, launch, handshake, and caching paths
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
		Plugins:         AdapterMap(),
		// Use a process command decoupled from per-step timeout contexts.
		// Session and loader shutdown are the only teardown mechanisms.
		Cmd:              exec.Command(path),
		AllowedProtocols: []hplugin.Protocol{hplugin.ProtocolGRPC},
		// 30 s gives adapter binaries enough time to start on loaded CI machines
		// and under the Go race detector, where process scheduling can be delayed
		// significantly. A typical local start takes well under 1 s.
		StartTimeout: 30 * time.Second,
		Logger:       adapterClientLogger(),
	})

	rpcClient, err := client.Client()
	if err != nil {
		client.Kill()
		return nil, fmt.Errorf("start adapter %q: %w", name, err)
	}
	raw, err := rpcClient.Dispense(AdapterName)
	if err != nil {
		client.Kill()
		return nil, fmt.Errorf("dispense adapter %q: %w", name, err)
	}

	adapterClient, ok := raw.(Client)
	if !ok {
		client.Kill()
		return nil, fmt.Errorf("unexpected adapter client type %T for %q", raw, name)
	}

	rp := &rpcHandle{name: name, client: client, rpc: adapterClient}
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
	active := make([]*rpcHandle, 0, len(l.active))
	for p := range l.active {
		active = append(active, p)
	}
	l.active = map[*rpcHandle]struct{}{}
	l.mu.Unlock()

	for _, p := range active {
		p.Kill()
	}
	return nil
}

type rpcHandle struct {
	name   string
	client *hplugin.Client
	rpc    Client

	mu     sync.Once
	onKill func()
}

func (p *rpcHandle) Info(ctx context.Context) (Info, error) {
	resp, err := p.rpc.Info(ctx, &v2.InfoRequest{})
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

func (p *rpcHandle) OpenSession(ctx context.Context, id string, config map[string]string) error {
	_, err := p.rpc.OpenSession(ctx, &v2.OpenSessionRequest{SessionId: id, Config: cloneConfig(config)})
	return err
}

// Execute streams step execution via the RPC adapter, handling concurrent log streaming,
// event routing, and partial failure recovery.
func (p *rpcHandle) Execute(ctx context.Context, sessionID string, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	req := &v2.ExecuteRequest{
		SessionId:       sessionID,
		StepName:        step.Name,
		Input:           cloneConfig(step.Input),
		AllowedOutcomes: collectAllowedOutcomes(step),
	}

	policy := NewPolicyWithAliases(step.AllowTools, adapterPermissionAliases[p.name])
	allowTools := step.AllowTools
	if allowTools == nil {
		allowTools = []string{}
	}
	captureSink := &executeCaptureSink{
		sink:        sink,
		policy:      policy,
		adapterName: p.name,
		allowTools:  allowTools,
	}

	// Open the Log stream concurrently with Execute. Adapters may emit log lines
	// at any time during Execute; we must be ready to receive them.
	logCtx, cancelLog := context.WithCancel(ctx)
	logDone := make(chan error, 1)
	go func() {
		logReq := &v2.LogRequest{SessionId: sessionID, StepName: step.Name}
		logDone <- p.rpc.Log(logCtx, logReq, &logForwardSink{sink: sink})
	}()

	execErr := p.rpc.Execute(ctx, req, captureSink)

	// Cancel the log stream and wait for the goroutine regardless of execErr.
	cancelLog()
	logErr := <-logDone

	if execErr != nil {
		return adapter.Result{Outcome: "failure"}, execErr
	}
	if logErr != nil && !errors.Is(logErr, context.Canceled) {
		return adapter.Result{Outcome: "failure"}, fmt.Errorf("adapter log stream: %w", logErr)
	}
	if !captureSink.done {
		return adapter.Result{Outcome: "failure"}, errors.New("adapter execute stream ended without result")
	}
	result := captureSink.result
	if captureSink.anyDenied && result.Outcome != "failure" {
		result.Outcome = "needs_review"
	}
	return result, nil
}

// executeCaptureSink implements ExecuteEventSink for use in rpcHandle.Execute.
// It routes AdapterEvent to the upstream EventSink, intercepts permission
// requests for host-side policy evaluation, and captures the final ExecuteResult.
type executeCaptureSink struct {
	sink        adapter.EventSink
	policy      PermissionPolicy
	adapterName string
	allowTools  []string
	result      adapter.Result
	done        bool
	anyDenied   bool
}

func (s *executeCaptureSink) Emit(ev *v2.ExecuteEvent) error {
	if adapterEvt := ev.GetAdapter(); adapterEvt != nil {
		if adapterEvt.GetEventKind() == "permission.request" {
			s.handlePermissionRequest(adapterEvt)
			return nil
		}
		if adapterEvt.GetPayload() != nil {
			s.sink.Adapter(adapterEvt.GetEventKind(), adapterEvt.GetPayload().AsMap())
		} else {
			s.sink.Adapter(adapterEvt.GetEventKind(), nil)
		}
		return nil
	}
	if resultEvt := ev.GetResult(); resultEvt != nil {
		s.result = adapter.Result{Outcome: resultEvt.GetOutcome()}
		if outs := resultEvt.GetOutputs(); len(outs) > 0 {
			s.result.Outputs = make(map[string]string, len(outs))
			for k, v := range outs {
				s.result.Outputs[k] = v
			}
		}
		s.done = true
		return nil
	}
	// ToolInvocation and Heartbeat are intentionally ignored at this layer.
	return nil
}

// handlePermissionRequest evaluates a permission.request AdapterEvent against
// the host policy and emits a permission.granted or permission.denied event.
// The raw permission.request is never forwarded to the upstream sink.
func (s *executeCaptureSink) handlePermissionRequest(adapterEvt *v2.AdapterEvent) {
	payload := map[string]any{}
	if adapterEvt.GetPayload() != nil {
		payload = adapterEvt.GetPayload().AsMap()
	}
	requestID, _ := payload["request_id"].(string)
	tool, _ := payload["tool"].(string)
	fullCommandText, _ := payload["full_command_text"].(string)

	var details map[string]string
	if fullCommandText != "" {
		details = map[string]string{"full_command_text": fullCommandText}
	}

	req := PermissionRequest{ID: requestID, Tool: tool, Details: details}
	allow, reason := s.policy.Decide(req)

	if allow {
		pattern := strings.TrimPrefix(reason, "matched: ")
		s.sink.Adapter("permission.granted", map[string]any{
			"request_id": requestID,
			"tool":       tool,
			"pattern":    pattern,
		})
	} else {
		suggestion := PermissionDenialSuggestion(s.adapterName, tool)
		s.sink.Adapter("permission.denied", map[string]any{
			"request_id":  requestID,
			"tool":        tool,
			"reason":      reason,
			"allow_tools": s.allowTools,
			"suggestion":  suggestion,
		})
		s.anyDenied = true
	}
}

// logForwardSink implements LogEventSink, forwarding log lines to the
// upstream adapter.EventSink.Log.
type logForwardSink struct {
	sink adapter.EventSink
}

func (s *logForwardSink) Emit(ev *v2.LogEvent) error {
	if ev.GetHeartbeat() != nil {
		return nil
	}
	s.sink.Log(ev.GetStreamName(), ev.GetLine())
	return nil
}

func (p *rpcHandle) CloseSession(ctx context.Context, id string) error {
	_, err := p.rpc.CloseSession(ctx, &v2.CloseSessionRequest{SessionId: id})
	return err
}

func (p *rpcHandle) Kill() {
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
// sorted ascending for determinism. If the step has no outcomes declared
// (terminal-routing steps, iteration steps that route via cursor outcomes,
// etc.), the result is empty.
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
	for s != "" && (s[0] == ' ' || s[0] == '\t' || s[0] == '\n' || s[0] == '\r') {
		s = s[1:]
	}
	for s != "" {
		last := s[len(s)-1]
		if last != ' ' && last != '\t' && last != '\n' && last != '\r' {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}

// AdapterInfoFromProto translates a v2 proto InfoResponse into a workflow.AdapterInfo.
// Legacy plugins that do not populate config_schema or input_schema will yield
// an empty AdapterInfo (permissive: any keys accepted by the compiler).
func AdapterInfoFromProto(resp *v2.InfoResponse) workflow.AdapterInfo {
	return workflow.AdapterInfo{
		ConfigSchema: protoToConfigSchema(resp.GetConfigSchema()),
		InputSchema:  protoToConfigSchema(resp.GetInputSchema()),
		Capabilities: append([]string(nil), resp.GetCapabilities()...),
	}
}

func protoToConfigSchema(s *v2.AdapterSchemaProto) map[string]workflow.ConfigField {
	if s == nil || len(s.GetFields()) == 0 {
		return nil
	}
	out := make(map[string]workflow.ConfigField, len(s.GetFields()))
	for k, f := range s.GetFields() {
		out[k] = workflow.ConfigField{
			Required: f.GetRequired(),
			Type:     protoToConfigFieldType(f.GetType()),
			Doc:      f.GetDescription(),
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
