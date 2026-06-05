package adapterhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	hplugin "github.com/hashicorp/go-plugin"
	"github.com/hashicorp/go-plugin/runner"
	"github.com/zclconf/go-cty/cty"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/log"
	v2 "github.com/brokenbots/criteria/sdk/pb/criteria/v2"
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

// LogStreamStarter is implemented by handles that support a dedicated
// per-session Log server-stream RPC (v2 adapters). The host starts this
// stream once at session open and cancels it at session close.
type LogStreamStarter interface {
	StartLogStream(ctx context.Context, sessionID string, sink LogEventSink) (cancel func(), err error)
}

type Handle interface {
	Info(ctx context.Context) (Info, error)
	OpenSession(ctx context.Context, id string, config, secrets map[string]string) error
	Execute(ctx context.Context, sessionID string, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error)
	CloseSession(ctx context.Context, id string) error
	Kill()
	// Pause asks the adapter to halt work without losing state.
	Pause(ctx context.Context, sessionID string) error
	// Resume asks the adapter to continue from where it paused.
	Resume(ctx context.Context, sessionID string) error
	// Inspect returns structured read-only state about the session.
	Inspect(ctx context.Context, sessionID string) (*v2.InspectResponse, error)
	// Snapshot returns opaque adapter-defined state.
	Snapshot(ctx context.Context, sessionID string) (*v2.SnapshotResponse, error)
	// Restore re-establishes adapter state from a prior snapshot.
	Restore(ctx context.Context, sessionID string, state []byte, schemaVersion uint32) error
}

type Info struct {
	Name              string
	Version           string
	Capabilities      []string
	SupportedFeatures []string // NEW v2 (D76)
	AdapterInfo       workflow.AdapterInfo
}

type DiscoveryFunc func(name string) (string, error)
type BuiltinFactory func() Handle

type DefaultLoader struct {
	mu            sync.Mutex
	discover      DiscoveryFunc
	builtins      map[string]BuiltinFactory
	active        map[*rpcHandle]struct{}
	cmdCustomizer func(name string, cmd *exec.Cmd)
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

// SetCommandCustomizer sets a function that is called for every
// non-builtin adapter process immediately after the exec.Cmd is
// created and before it is started. Passing nil clears any existing
// customizer.
func (l *DefaultLoader) SetCommandCustomizer(fn func(name string, cmd *exec.Cmd)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cmdCustomizer = fn
}

func (l *DefaultLoader) RegisterBuiltin(name string, factory BuiltinFactory) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if stringsTrim(name) == "" || factory == nil {
		return
	}
	l.builtins[name] = factory
}

func (l *DefaultLoader) Resolve(ctx context.Context, name string) (Handle, error) {
	l.mu.Lock()
	customizer := l.cmdCustomizer
	l.mu.Unlock()
	return l.ResolveWithCustomizer(ctx, name, customizer)
}

// ResolveWithCustomizer is like Resolve but accepts a per-call command
// customizer instead of the shared one set by SetCommandCustomizer.
// This avoids races when multiple sessions open concurrently.
func (l *DefaultLoader) ResolveWithCustomizer(ctx context.Context, name string, customizer func(name string, cmd *exec.Cmd)) (Handle, error) { //nolint:funlen // resolver must handle builtin registry, discovery, launch, handshake, and caching paths
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

	cmd := exec.Command(path)
	// Apply sandbox or other per-adapter command customizations.
	if customizer != nil {
		customizer(name, cmd)
	}

	client := hplugin.NewClient(&hplugin.ClientConfig{
		HandshakeConfig: HandshakeConfig,
		Plugins:         AdapterMap(),
		// Use a process command decoupled from per-step timeout contexts.
		// Session and loader shutdown are the only teardown mechanisms.
		Cmd:              cmd,
		AllowedProtocols: []hplugin.Protocol{hplugin.ProtocolGRPC},
		// 30 s gives adapter binaries enough time to start on loaded CI machines
		// and under the Go race detector, where process scheduling can be delayed
		// significantly. A typical local start takes well under 1 s.
		StartTimeout: 30 * time.Second,
		Logger:       adapterClientLogger(),
		// When a customizer is active (e.g. sandbox env scrub) we must not
		// let go-plugin re-append the host environment after the customizer
		// has filtered it. SkipHostEnv prevents that re-addition.
		SkipHostEnv: customizer != nil,
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

// NewRPCHandle creates a Handle from an existing go-plugin client and its
// dispensed Client interface. This is used by the remote shim (WS20) to wrap
// a reattached adapter into a session-manager-compatible Handle.
func NewRPCHandle(name string, client *hplugin.Client, rpc Client) Handle {
	rp := &rpcHandle{name: name, client: client, rpc: rpc}
	rp.onKill = func() {}
	return rp
}

// ResolveWithRunnerFunc resolves the adapter using a custom go-plugin RunnerFunc
// instead of discovering a local binary. This is used for container-mode adapters
// where the plugin runs inside docker/podman.
func (l *DefaultLoader) ResolveWithRunnerFunc(ctx context.Context, name string, rf func(hclog.Logger, *exec.Cmd, string) (runner.Runner, error)) (Handle, error) {
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
	l.mu.Unlock()

	client := hplugin.NewClient(&hplugin.ClientConfig{
		HandshakeConfig:  HandshakeConfig,
		Plugins:          AdapterMap(),
		AllowedProtocols: []hplugin.Protocol{hplugin.ProtocolGRPC},
		StartTimeout:     30 * time.Second,
		Logger:           adapterClientLogger(),
		SkipHostEnv:      true,
		RunnerFunc:       rf,
	})

	return l.startAdapterClient(name, client)
}

func (l *DefaultLoader) startAdapterClient(name string, client *hplugin.Client) (Handle, error) {
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

	// permMu guards permActive, which tracks session-scoped permission
	// streams started via StartPermissionStream. Execute uses it to
	// decide whether to start a fallback per-Execute permission stream
	// for direct callers (e.g. conformance tests) that bypass SessionManager.
	permMu     sync.Mutex
	permActive map[string]bool
}

func (p *rpcHandle) Info(ctx context.Context) (Info, error) {
	resp, err := p.rpc.Info(ctx, &v2.InfoRequest{})
	if err != nil {
		return Info{}, err
	}
	return Info{
		Name:              resp.GetName(),
		Version:           resp.GetVersion(),
		Capabilities:      append([]string(nil), resp.GetCapabilities()...),
		SupportedFeatures: append([]string(nil), resp.GetSupportedFeatures()...),
		AdapterInfo:       AdapterInfoFromProto(resp),
	}, nil
}

func (p *rpcHandle) OpenSession(ctx context.Context, id string, config, secrets map[string]string) error {
	_, err := p.rpc.OpenSession(ctx, &v2.OpenSessionRequest{SessionId: id, Config: cloneConfig(config), Secrets: cloneConfig(secrets)})
	return err
}

func (p *rpcHandle) StartLogStream(ctx context.Context, sessionID string, sink LogEventSink) (cancel func(), err error) {
	logCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	go func() {
		err := p.rpc.Log(logCtx, &v2.LogRequest{SessionId: sessionID}, sink)
		if err != nil && !isExpectedStreamClose(err) {
			slog.Warn("adapter log stream closed unexpectedly", "adapter", p.name, "session", sessionID, "error", err)
		}
	}()
	return cancel, nil
}

func (p *rpcHandle) StartPermissionStream(ctx context.Context, sessionID string, requests <-chan *v2.PermissionEvent) (cancel func(), err error) {
	p.permMu.Lock()
	if p.permActive == nil {
		p.permActive = make(map[string]bool)
	}
	p.permActive[sessionID] = true
	p.permMu.Unlock()

	permCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	go func() {
		defer func() {
			p.permMu.Lock()
			delete(p.permActive, sessionID)
			p.permMu.Unlock()
		}()
		err := p.rpc.Permissions(permCtx, requests)
		if status.Code(err) == codes.Unimplemented {
			// Drain the requests channel so Evaluate never blocks on a full
			// buffer after the Permissions stream has exited.
			for range requests {
			}
			return
		}
		if err != nil && !isExpectedStreamClose(err, codes.Unimplemented) {
			slog.Warn("adapter permission stream closed unexpectedly", "adapter", p.name, "session", sessionID, "error", err)
		}
	}()
	return cancel, nil
}

// isExpectedStreamClose reports whether err is a benign end-of-stream error
// (nil, EOF, context cancellation, or an optionally supplied gRPC status code).
func isExpectedStreamClose(err error, extra ...codes.Code) bool {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
		return true
	}
	c := status.Code(err)
	if c == codes.Canceled {
		return true
	}
	for _, x := range extra {
		if c == x {
			return true
		}
	}
	return false
}

// Execute streams step execution via the RPC adapter, handling concurrent log streaming,
// event routing, and partial failure recovery.
func (p *rpcHandle) Execute(ctx context.Context, sessionID string, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	req := &v2.ExecuteRequest{
		SessionId:       sessionID,
		StepName:        step.Name,
		Input:           cloneConfig(step.Input),
		SecretInputs:    cloneConfig(step.SecretInputs),
		AllowedOutcomes: collectAllowedOutcomes(step),
	}

	// serialized wraps sink so concurrent Adapter/Log calls from executeCaptureSink
	// and logForwardSink are safe regardless of the sink implementation.
	serialized := &serializedEventSink{inner: sink}

	p.permMu.Lock()
	hasPermStream := p.permActive[sessionID]
	p.permMu.Unlock()

	if !hasPermStream {
		return p.executeWithFallbackStream(ctx, sessionID, step, serialized, req)
	}
	return p.executeWithActiveStream(ctx, step, serialized, req)
}

// executeWithFallbackStream runs Execute with a per-Execute permission stream
// for callers (e.g. conformance tests) that bypass SessionManager.
func (p *rpcHandle) executeWithFallbackStream(ctx context.Context, _ string, step *workflow.StepNode, serialized *serializedEventSink, req *v2.ExecuteRequest) (adapter.Result, error) {
	execCtx, cancelExec := context.WithCancel(ctx)
	defer cancelExec()

	requests := make(chan *v2.PermissionEvent, 16)
	_, cancelPerm, permDone := p.startFallbackPermStream(ctx, requests, cancelExec)
	defer cancelPerm()

	captureSink := &executeCaptureSink{
		sink:        serialized,
		policy:      NewPolicy(step.AllowTools),
		allowTools:  step.AllowTools,
		adapterName: p.name,
		requests:    requests,
		ctx:         execCtx,
	}

	execErr := p.rpc.Execute(execCtx, req, captureSink)

	close(requests)
	cancelPerm()
	permErr := <-permDone

	if execErr != nil {
		if errors.Is(execErr, context.Canceled) && ctx.Err() == nil {
			if !isExpectedStreamClose(permErr) {
				return adapter.Result{Outcome: "failure"}, fmt.Errorf("permissions stream failure aborted execute: %w", permErr)
			}
		}
		return adapter.Result{Outcome: "failure"}, execErr
	}
	if !isExpectedStreamClose(permErr, codes.Unimplemented) {
		return adapter.Result{Outcome: "failure"}, fmt.Errorf("adapter permissions stream: %w", permErr)
	}
	if !captureSink.done {
		return adapter.Result{Outcome: "failure"}, errors.New("adapter execute stream ended without result")
	}
	if captureSink.anyDenied && captureSink.result.Outcome == "success" {
		captureSink.result.Outcome = "needs_review"
	}
	return captureSink.result, nil
}

func (p *rpcHandle) startFallbackPermStream(ctx context.Context, requests chan *v2.PermissionEvent, cancelExec func()) (context.Context, context.CancelFunc, chan error) {
	permCtx, cancelPerm := context.WithCancel(ctx)
	permDone := make(chan error, 1)
	go func() {
		err := p.rpc.Permissions(permCtx, requests)
		permDone <- err
		if status.Code(err) == codes.Unimplemented {
			for range requests {
			}
			return
		}
		if !isExpectedStreamClose(err) {
			cancelExec()
		}
	}()
	return permCtx, cancelPerm, permDone
}

// executeWithActiveStream runs Execute when a session-scoped permission stream
// is already active.
func (p *rpcHandle) executeWithActiveStream(ctx context.Context, step *workflow.StepNode, serialized *serializedEventSink, req *v2.ExecuteRequest) (adapter.Result, error) {
	captureSink := &executeCaptureSink{
		sink:       serialized,
		policy:     NewPolicy(step.AllowTools),
		allowTools: step.AllowTools,
	}

	execErr := p.rpc.Execute(ctx, req, captureSink)

	if execErr != nil {
		return adapter.Result{Outcome: "failure"}, execErr
	}
	if !captureSink.done {
		return adapter.Result{Outcome: "failure"}, errors.New("adapter execute stream ended without result")
	}
	if captureSink.anyDenied && captureSink.result.Outcome == "success" {
		captureSink.result.Outcome = "needs_review"
	}
	return captureSink.result, nil
}

// maxChunkBufBytes is the upper bound for chunk-reassembly buffers in
// executeCaptureSink. Payloads that would exceed this limit are rejected with
// an error to prevent unbounded memory growth from a misbehaving adapter.
const maxChunkBufBytes = 64 * 1024 * 1024 // 64 MiB

// executeCaptureSink implements ExecuteEventSink for use in rpcHandle.Execute.
// It routes AdapterEvent to the upstream EventSink, evaluates the host-side
// permission policy for permission.request events (emitting permission.granted
// or permission.denied and forwarding to the adapter via the Permissions stream),
// translates ToolInvocation to a tool.invocation adapter event, reassembles
// WS02 chunked payloads and outputs before forwarding, and captures the final
// ExecuteResult.
type executeCaptureSink struct {
	sink   adapter.EventSink
	result adapter.Result
	done   bool

	// anyDenied is set true when any permission request was denied this Execute.
	// It triggers the outcome override (success → needs_review) after Execute.
	anyDenied bool

	// Host-side permission policy for this step (fallback when called directly,
	// bypassing SessionManager).
	policy     PermissionPolicy
	allowTools []string // echoed in permission.denied payloads

	// adapterName is used for contextual permission-denial suggestions.
	adapterName string

	// requests and ctx are used in the fallback per-Execute permission stream
	// path (when SessionManager has not started a session-scoped stream).
	// When nil, permission responses are handled by the session-scoped stream.
	requests chan<- *v2.PermissionEvent
	ctx      context.Context

	// Chunk reassembly buffers for the Execute stream.
	// adapterChunkBuf accumulates AdapterEvent.payload_json fragments.
	// resultChunkBuf accumulates ExecuteResult.outputs_json fragments.
	// Chunks within each oneof arrive sequentially (one sequence at a time).
	// adapterChunkNextSeq / resultChunkNextSeq track the expected next seq
	// value (0 means no sequence in progress; >0 means expecting that value).
	adapterChunkBuf     []byte
	adapterChunkKind    string // event_kind carried across chunk fragments
	adapterChunkNextSeq uint32 // expected next adapter chunk seq (0 = idle)
	resultChunkBuf      []byte
	resultOutcome       string // outcome carried across result chunk fragments
	resultChunkNextSeq  uint32 // expected next result chunk seq (0 = idle)
}

func (s *executeCaptureSink) Emit(ev *v2.ExecuteEvent) error {
	if adapterEvt := ev.GetAdapter(); adapterEvt != nil {
		return s.emitAdapter(adapterEvt)
	}
	if toolEvt := ev.GetTool(); toolEvt != nil {
		return s.emitTool(toolEvt)
	}
	if resultEvt := ev.GetResult(); resultEvt != nil {
		return s.emitResult(resultEvt)
	}
	return nil
}

func (s *executeCaptureSink) emitAdapter(adapterEvt *v2.AdapterEvent) error {
	if chunk := adapterEvt.GetChunk(); chunk != nil {
		// Validate and accumulate payload_json fragment; forward when final arrives.
		seq := chunk.GetSeq()
		if seq == 0 {
			// New sequence; reset any prior in-progress buffer.
			s.adapterChunkBuf = nil
			s.adapterChunkKind = adapterEvt.GetEventKind()
			s.adapterChunkNextSeq = 1
		} else if seq != s.adapterChunkNextSeq {
			// Out-of-order chunk; discard buffer and reject.
			expected := s.adapterChunkNextSeq
			s.adapterChunkBuf = nil
			s.adapterChunkNextSeq = 0
			return fmt.Errorf("adapter event chunk out-of-order: seq %d expected %d", seq, expected)
		} else {
			s.adapterChunkNextSeq = seq + 1
		}
		if len(s.adapterChunkBuf)+len(adapterEvt.GetPayloadJson()) > maxChunkBufBytes {
			s.adapterChunkBuf = nil
			s.adapterChunkNextSeq = 0
			return fmt.Errorf("adapter event chunk reassembly: payload exceeds %d bytes", maxChunkBufBytes)
		}
		s.adapterChunkBuf = append(s.adapterChunkBuf, adapterEvt.GetPayloadJson()...)
		if !chunk.GetFinal() {
			return nil
		}
		payload, err := structFromJSON(s.adapterChunkBuf)
		s.adapterChunkBuf = nil
		s.adapterChunkNextSeq = 0
		if err != nil {
			return fmt.Errorf("adapter event chunk reassembly: %w", err)
		}
		return s.emitAdapterEvent(&v2.AdapterEvent{
			EventKind: s.adapterChunkKind,
			Payload:   payload,
		})
	}
	return s.emitAdapterEvent(adapterEvt)
}

// emitTool converts a ToolInvocation proto event into the canonical
// tool.invocation adapter event forwarded to the upstream EventSink.
// The payload shape is {"name": string, "arguments": map} — preserved
// from v1 so existing console and NDJSON consumers do not break.
func (s *executeCaptureSink) emitTool(toolEvt *v2.ToolInvocation) error {
	payload := map[string]any{"name": toolEvt.GetToolName()}
	if args := toolEvt.GetArgs(); args != nil {
		payload["arguments"] = args.AsMap()
	}
	s.sink.Adapter("tool.invocation", payload)
	return nil
}

func (s *executeCaptureSink) emitResult(resultEvt *v2.ExecuteResult) error {
	if chunk := resultEvt.GetChunk(); chunk != nil {
		// Validate and accumulate outputs_json fragment; capture result when final arrives.
		seq := chunk.GetSeq()
		if seq == 0 {
			s.resultChunkBuf = nil
			s.resultOutcome = resultEvt.GetOutcome()
			s.resultChunkNextSeq = 1
		} else if seq != s.resultChunkNextSeq {
			expected := s.resultChunkNextSeq
			s.resultChunkBuf = nil
			s.resultChunkNextSeq = 0
			return fmt.Errorf("execute result chunk out-of-order: seq %d expected %d", seq, expected)
		} else {
			s.resultChunkNextSeq = seq + 1
		}
		if len(s.resultChunkBuf)+len(resultEvt.GetOutputsJson()) > maxChunkBufBytes {
			s.resultChunkBuf = nil
			s.resultChunkNextSeq = 0
			return fmt.Errorf("execute result chunk reassembly: outputs exceed %d bytes", maxChunkBufBytes)
		}
		s.resultChunkBuf = append(s.resultChunkBuf, resultEvt.GetOutputsJson()...)
		if !chunk.GetFinal() {
			return nil
		}
		outputs, err := outputsFromJSON(s.resultChunkBuf)
		s.resultChunkBuf = nil
		s.resultChunkNextSeq = 0
		if err != nil {
			return fmt.Errorf("execute result chunk reassembly: %w", err)
		}
		s.result = adapter.Result{Outcome: s.resultOutcome, Outputs: outputs}
		s.done = true
		return nil
	}
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

// emitAdapterEvent dispatches a fully assembled (non-chunked) AdapterEvent
// to the upstream sink. All events are forwarded directly; permission evaluation
// is handled session-scoped by permissionInterceptSink in SessionManager.Execute.
func (s *executeCaptureSink) emitAdapterEvent(adapterEvt *v2.AdapterEvent) error {
	if adapterEvt.GetEventKind() == "permission.request" {
		if s.requests != nil {
			// Fallback per-Execute path: evaluate locally and forward
			// PermissionEvents to the adapter stream.
			s.handlePermissionRequest(adapterEvt)
			return nil
		}
		// Active stream path: forward to the upstream sink so the
		// session-scoped permissionInterceptSink can evaluate with
		// CombinedPolicy, audit, and stream event dispatch.
	}
	if adapterEvt.GetPayload() != nil {
		s.sink.Adapter(adapterEvt.GetEventKind(), adapterEvt.GetPayload().AsMap())
	} else {
		s.sink.Adapter(adapterEvt.GetEventKind(), nil)
	}
	return nil
}

// handlePermissionRequest evaluates a permission.request AdapterEvent against
// the host-side allow_tools policy. It emits either permission.granted or
// permission.denied to the upstream sink, and forwards the corresponding
// PermissionEvent to the adapter via the bidi Permissions stream when
// requests is non-nil (fallback per-Execute path). A denied request sets
// anyDenied so Execute can override the adapter's "success" outcome to
// "needs_review".
func (s *executeCaptureSink) handlePermissionRequest(adapterEvt *v2.AdapterEvent) {
	var payload map[string]any
	if adapterEvt.GetPayload() != nil {
		payload = adapterEvt.GetPayload().AsMap()
	}

	requestID, _ := payload["request_id"].(string)
	tool, _ := payload["tool"].(string)
	fullCmd, _ := payload["full_command_text"].(string)

	req := PermissionRequest{ID: requestID, Tool: tool}
	if fullCmd != "" {
		req.Details = map[string]string{"full_command_text": fullCmd}
	}

	allow, reason := s.policy.Decide(req)
	if allow {
		s.emitGranted(requestID, tool, reason)
		return
	}
	s.emitDenied(requestID, tool, reason)
}

func (s *executeCaptureSink) emitGranted(requestID, tool, reason string) {
	pattern := strings.TrimPrefix(reason, "matched: ")
	if idx := strings.Index(pattern, " (alias for "); idx >= 0 {
		pattern = pattern[:idx]
	}
	s.sink.Adapter("permission.granted", map[string]any{
		"request_id": requestID,
		"tool":       tool,
		"pattern":    pattern,
	})
	if s.requests == nil {
		return
	}
	select {
	case s.requests <- &v2.PermissionEvent{
		Event: &v2.PermissionEvent_Request{
			Request: &v2.PermissionRequest{RequestId: requestID},
		},
	}:
	case <-s.ctx.Done():
	}
}

func (s *executeCaptureSink) emitDenied(requestID, tool, reason string) {
	s.anyDenied = true
	suggestion := PermissionDenialSuggestion(s.adapterName, tool)
	deniedPayload := map[string]any{
		"request_id": requestID,
		"tool":       tool,
		"reason":     reason,
	}
	if len(s.allowTools) > 0 {
		deniedPayload["allow_tools"] = s.allowTools
	}
	if suggestion != "" {
		deniedPayload["suggestion"] = suggestion
	}
	s.sink.Adapter("permission.denied", deniedPayload)
	if s.requests == nil {
		return
	}
	select {
	case s.requests <- &v2.PermissionEvent{
		Event: &v2.PermissionEvent_Cancel{
			Cancel: &v2.PermissionCancel{RequestId: requestID, Reason: reason},
		},
	}:
	case <-s.ctx.Done():
	}
}

// maxLogLineBufBytes is the upper bound for a single log chunk reassembly
// buffer or individual log line. Log content exceeding this limit is rejected
// to prevent unbounded memory growth from a misbehaving adapter.
const maxLogLineBufBytes = 4 * 1024 * 1024 // 4 MiB per stream

// maxTotalLogBufBytes is the aggregate upper bound across all concurrent chunk
// reassembly buffers within one Log stream. A misbehaving adapter cannot
// exhaust host memory by opening many parallel named streams.
const maxTotalLogBufBytes = 16 * 1024 * 1024 // 16 MiB total

// logForwardSink implements LogEventSink, forwarding log lines to the
// upstream adapter.EventSink.Log. Chunked log lines (LogEvent.chunk != nil)
// are reassembled per stream_name before forwarding, with per-stream and
// aggregate memory caps plus seq-number validation to fail closed on
// out-of-order or corrupt chunk sequences.
type logForwardSink struct {
	sink        adapter.EventSink
	chunkBufs   map[string][]byte // keyed by stream_name
	chunkSeqs   map[string]uint32 // expected next seq per stream (0 = no seq in progress)
	onHeartbeat func()            // called when a heartbeat is received
}

// totalLogBufSize returns the sum of all in-progress chunk buffer lengths.
func totalLogBufSize(m map[string][]byte) int {
	n := 0
	for _, b := range m {
		n += len(b)
	}
	return n
}

func (s *logForwardSink) Emit(ev *v2.LogEvent) error {
	if ev.GetHeartbeat() != nil {
		if s.onHeartbeat != nil {
			s.onHeartbeat()
		}
		return nil
	}
	if chunk := ev.GetChunk(); chunk != nil {
		return s.emitChunk(ev, chunk)
	}
	if len(ev.GetLine()) > maxLogLineBufBytes {
		return fmt.Errorf("log event: stream %q line exceeds %d bytes", ev.GetStreamName(), maxLogLineBufBytes)
	}
	if tsSink, ok := s.sink.(log.TimestampedSink); ok {
		ts := ev.GetTimestamp().AsTime()
		if ts.IsZero() {
			ts = time.Now()
		}
		tsSink.LogAt(ts, ev.GetStreamName(), ev.GetLine())
	} else {
		s.sink.Log(ev.GetStreamName(), ev.GetLine())
	}
	return nil
}

// emitChunk handles a chunked log event: validates seq ordering, enforces memory
// caps, and forwards the reassembled line when the final chunk arrives.
func (s *logForwardSink) emitChunk(ev *v2.LogEvent, chunk *v2.Chunk) error {
	stream := ev.GetStreamName()
	seq := chunk.GetSeq()
	if seq == 0 {
		// New sequence; reset this stream's buffer and seq counter.
		if s.chunkBufs == nil {
			s.chunkBufs = make(map[string][]byte)
		}
		if s.chunkSeqs == nil {
			s.chunkSeqs = make(map[string]uint32)
		}
		s.chunkBufs[stream] = nil
		s.chunkSeqs[stream] = 1
	} else {
		if err := s.validateSeq(stream, seq); err != nil {
			return err
		}
	}
	// Per-stream cap check.
	if len(s.chunkBufs[stream])+len(ev.GetLine()) > maxLogLineBufBytes {
		delete(s.chunkBufs, stream)
		delete(s.chunkSeqs, stream)
		return fmt.Errorf("log chunk reassembly: stream %q exceeds %d bytes", stream, maxLogLineBufBytes)
	}
	// Aggregate cap check across all concurrent buffers.
	if totalLogBufSize(s.chunkBufs)+len(ev.GetLine()) > maxTotalLogBufBytes {
		delete(s.chunkBufs, stream)
		delete(s.chunkSeqs, stream)
		return fmt.Errorf("log chunk reassembly: aggregate buffer exceeds %d bytes", maxTotalLogBufBytes)
	}
	s.chunkBufs[stream] = append(s.chunkBufs[stream], ev.GetLine()...)
	if !chunk.GetFinal() {
		return nil
	}
	line := s.chunkBufs[stream]
	delete(s.chunkBufs, stream)
	delete(s.chunkSeqs, stream)
	if tsSink, ok := s.sink.(log.TimestampedSink); ok {
		ts := ev.GetTimestamp().AsTime()
		if ts.IsZero() {
			ts = time.Now()
		}
		tsSink.LogAt(ts, stream, line)
	} else {
		s.sink.Log(stream, line)
	}
	return nil
}

// validateSeq checks the sequence number for a continuation chunk and
// advances the counter; returns an error and clears state on mismatch.
func (s *logForwardSink) validateSeq(stream string, seq uint32) error {
	expected := s.chunkSeqs[stream]
	if expected == 0 {
		return fmt.Errorf("log chunk: stream %q received seq %d with no sequence in progress", stream, seq)
	}
	if seq != expected {
		delete(s.chunkBufs, stream)
		delete(s.chunkSeqs, stream)
		return fmt.Errorf("log chunk: stream %q out-of-order seq %d (expected %d)", stream, seq, expected)
	}
	s.chunkSeqs[stream] = expected + 1
	return nil
}

// serializedEventSink serializes concurrent calls to adapter.EventSink.Adapter
// and adapter.EventSink.Log behind a mutex. The Execute goroutine (via
// executeCaptureSink) and the Log goroutine (via logForwardSink) both call the
// upstream sink concurrently; wrapping the sink here prevents data races on
// non-thread-safe sink implementations.
type serializedEventSink struct {
	mu    sync.Mutex
	inner adapter.EventSink
}

func (s *serializedEventSink) Log(stream string, chunk []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inner.Log(stream, chunk)
}

func (s *serializedEventSink) Adapter(kind string, data any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inner.Adapter(kind, data)
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

func (p *rpcHandle) Pause(ctx context.Context, sessionID string) error {
	_, err := p.rpc.Pause(ctx, &v2.PauseRequest{SessionId: sessionID})
	return err
}

func (p *rpcHandle) Resume(ctx context.Context, sessionID string) error {
	_, err := p.rpc.Resume(ctx, &v2.ResumeRequest{SessionId: sessionID})
	return err
}

func (p *rpcHandle) Snapshot(ctx context.Context, sessionID string) (*v2.SnapshotResponse, error) {
	return p.rpc.Snapshot(ctx, &v2.SnapshotRequest{SessionId: sessionID})
}

func (p *rpcHandle) Restore(ctx context.Context, sessionID string, state []byte, schemaVersion uint32) error {
	_, err := p.rpc.Restore(ctx, &v2.RestoreRequest{SessionId: sessionID, State: state, SchemaVersion: schemaVersion})
	return err
}

func (p *rpcHandle) Inspect(ctx context.Context, sessionID string) (*v2.InspectResponse, error) {
	return p.rpc.Inspect(ctx, &v2.InspectRequest{SessionId: sessionID})
}

// structFromJSON unmarshals raw JSON bytes into a *structpb.Struct so that
// reassembled AdapterEvent.payload_json chunks can be forwarded as the
// standard payload type.
func structFromJSON(b []byte) (*structpb.Struct, error) {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return structpb.NewStruct(m)
}

// outputsFromJSON unmarshals raw JSON bytes (ExecuteResult.outputs_json)
// into the flat string map used by adapter.Result.Outputs.
func outputsFromJSON(b []byte) (map[string]string, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
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
		ConfigSchema:           protoToConfigSchema(resp.GetConfigSchema()),
		InputSchema:            protoToConfigSchema(resp.GetInputSchema()),
		OutputSchema:           protoToConfigSchema(resp.GetOutputSchema()),
		Capabilities:           append([]string(nil), resp.GetCapabilities()...),
		CompatibleEnvironments: append([]string(nil), resp.GetCompatibleEnvironments()...),
		SupportedFeatures:      append([]string(nil), resp.GetSupportedFeatures()...),
	}
}

func protoToConfigSchema(s *v2.AdapterSchemaProto) map[string]workflow.ConfigField {
	if s == nil || len(s.GetFields()) == 0 {
		return nil
	}
	out := make(map[string]workflow.ConfigField, len(s.GetFields()))
	for k, f := range s.GetFields() {
		out[k] = workflow.ConfigField{
			Required:  f.GetRequired(),
			Type:      protoToConfigFieldType(f.GetType()),
			CtyType:   protoToCtyType(f.GetType()),
			Doc:       f.GetDescription(),
			Sensitive: f.GetSensitive(),
		}
	}
	return out
}

func protoToConfigFieldType(t string) workflow.ConfigFieldType {
	switch t {
	case "number":
		return workflow.ConfigFieldNumber
	case "bool", "boolean": // accept both; "boolean" is the JSON Schema convention
		return workflow.ConfigFieldBool
	case "list_string":
		return workflow.ConfigFieldListString
	default:
		return workflow.ConfigFieldString
	}
}

// protoToCtyType maps an adapter schema field's declared type string to a full
// cty.Type. It is the authoritative type model for OutputSchema fields, driving
// typed coercion of step outputs. "object"/"array" carry no sub-schema on the
// wire, so they map to cty.DynamicPseudoType (decode-anything against JSON). An
// empty or unrecognised type yields cty.NilType (permissive — value preserved as
// a raw string).
func protoToCtyType(t string) cty.Type {
	switch t {
	case "string":
		return cty.String
	case "number":
		return cty.Number
	case "bool", "boolean":
		return cty.Bool
	case "list_string":
		return cty.List(cty.String)
	case "object", "array":
		return cty.DynamicPseudoType
	default:
		return cty.NilType
	}
}
