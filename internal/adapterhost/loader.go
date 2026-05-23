package adapterhost

import (
	"context"
	"encoding/json"
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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

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
func (p *rpcHandle) Execute(ctx context.Context, sessionID string, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) { //nolint:funlen,gocyclo // Permissions stream lifecycle, log stream, execute RPC, and result coercion are all required in one place
	req := &v2.ExecuteRequest{
		SessionId:       sessionID,
		StepName:        step.Name,
		Input:           cloneConfig(step.Input),
		AllowedOutcomes: collectAllowedOutcomes(step),
	}

	// Build the host-side permission policy for this step.
	allowTools := step.AllowTools
	if allowTools == nil {
		allowTools = []string{}
	}
	policy := NewPolicyWithAliases(allowTools, adapterPermissionAliases[p.name])

	// execCtx is canceled if the Permissions stream fails unexpectedly, so that
	// Execute is aborted rather than continuing without a functioning decision channel.
	execCtx, cancelExec := context.WithCancel(ctx)
	defer cancelExec()

	// Open the Permissions bidi stream so the adapter can receive grant/deny
	// decisions for each permission.request it emits during Execute.
	requests := make(chan *v2.PermissionEvent, 16)
	permCtx, cancelPerm := context.WithCancel(ctx)
	permDone := make(chan error, 1)
	go func() {
		err := p.rpc.Permissions(permCtx, requests)
		permDone <- err
		// Abort Execute if Permissions failed for a non-expected reason so the
		// adapter is not left blocking on permission decisions that will never arrive.
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) && status.Code(err) != codes.Canceled {
			cancelExec()
		}
	}()

	captureSink := &executeCaptureSink{
		ctx:         ctx,
		sink:        sink,
		policy:      policy,
		allowTools:  allowTools,
		adapterName: p.name,
		requests:    requests,
	}

	// Open the Log stream concurrently with Execute. Adapters may emit log lines
	// at any time during Execute; we must be ready to receive them.
	logCtx, cancelLog := context.WithCancel(ctx)
	logDone := make(chan error, 1)
	go func() {
		logReq := &v2.LogRequest{SessionId: sessionID, StepName: step.Name}
		logDone <- p.rpc.Log(logCtx, logReq, &logForwardSink{sink: sink})
	}()

	execErr := p.rpc.Execute(execCtx, req, captureSink)

	// Signal end of the Permissions stream and wait for the goroutine.
	// Closing requests triggers CloseSend; cancelPerm ensures recvPermissionDecisions
	// exits promptly regardless of whether the adapter has finished sending.
	close(requests)
	cancelPerm()
	permErr := <-permDone

	// Cancel the log stream and wait for the goroutine regardless of execErr.
	cancelLog()
	logErr := <-logDone

	if execErr != nil {
		// If execCtx was canceled by the Permissions goroutine (parent ctx still OK),
		// the Permissions failure is the root cause; surface it instead of
		// context.Canceled from the aborted Execute stream.
		if errors.Is(execErr, context.Canceled) && ctx.Err() == nil {
			if permErr != nil && !errors.Is(permErr, io.EOF) && !errors.Is(permErr, context.Canceled) && status.Code(permErr) != codes.Canceled {
				return adapter.Result{Outcome: "failure"}, fmt.Errorf("permissions stream failure aborted execute: %w", permErr)
			}
		}
		return adapter.Result{Outcome: "failure"}, execErr
	}
	if permErr != nil && !errors.Is(permErr, io.EOF) && !errors.Is(permErr, context.Canceled) && status.Code(permErr) != codes.Canceled {
		return adapter.Result{Outcome: "failure"}, fmt.Errorf("adapter permissions stream: %w", permErr)
	}
	if logErr != nil && !errors.Is(logErr, context.Canceled) && status.Code(logErr) != codes.Canceled {
		return adapter.Result{Outcome: "failure"}, fmt.Errorf("adapter log stream: %w", logErr)
	}
	if !captureSink.done {
		return adapter.Result{Outcome: "failure"}, errors.New("adapter execute stream ended without result")
	}
	// When any permission request was denied and the adapter returned "success",
	// override to "needs_review" so the run is not silently passed with unapproved
	// tool calls (adapters that terminate on first denial — e.g. copilot — return
	// "failure" themselves; this override applies only to permissive adapters).
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
// permission policy for permission.request events (WS16), translates
// ToolInvocation to a tool.invocation adapter event, reassembles WS02 chunked
// payloads and outputs before forwarding, and captures the final ExecuteResult.
type executeCaptureSink struct {
	ctx       context.Context
	sink      adapter.EventSink
	result    adapter.Result
	done      bool
	anyDenied bool // true if any permission request was denied this Execute

	// Host-side permission policy for this step.
	policy      PermissionPolicy
	allowTools  []string // echoed in permission.denied payloads
	adapterName string   // used for alias-aware denial suggestions
	// requests is the send side of the Permissions bidi stream; permission
	// decisions are forwarded to the adapter via this channel.
	requests chan<- *v2.PermissionEvent

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

// emitAdapterEvent dispatches a fully assembled (non-chunked) AdapterEvent.
func (s *executeCaptureSink) emitAdapterEvent(adapterEvt *v2.AdapterEvent) error {
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

// handlePermissionRequest evaluates a permission.request AdapterEvent against
// the host-side policy for this step (WS16). It emits either permission.granted
// or permission.denied to the upstream sink, and forwards the corresponding
// PermissionEvent to the adapter via the bidi Permissions stream.
func (s *executeCaptureSink) handlePermissionRequest(adapterEvt *v2.AdapterEvent) { //nolint:funlen // grant/deny branches each require extracting payload, calling policy, emitting event, and forwarding to stream
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
		// Strip "matched: " prefix to get the raw pattern for the payload.
		pattern := strings.TrimPrefix(reason, "matched: ")
		if idx := strings.Index(pattern, " (alias for "); idx >= 0 {
			pattern = pattern[:idx]
		}
		s.sink.Adapter("permission.granted", map[string]any{
			"request_id": requestID,
			"tool":       tool,
			"pattern":    pattern,
		})
		if s.requests != nil {
			select {
			case s.requests <- &v2.PermissionEvent{
				Event: &v2.PermissionEvent_Request{
					Request: &v2.PermissionRequest{RequestId: requestID},
				},
			}:
			case <-s.ctx.Done():
			}
		}
		return
	}

	// Denied.
	s.anyDenied = true
	deniedPayload := map[string]any{
		"request_id":  requestID,
		"tool":        tool,
		"reason":      reason,
		"allow_tools": s.allowTools,
	}
	if s.adapterName != "" {
		if suggestion := PermissionDenialSuggestion(s.adapterName, tool); suggestion != "" {
			deniedPayload["suggestion"] = suggestion
		}
	}
	s.sink.Adapter("permission.denied", deniedPayload)
	if s.requests != nil {
		select {
		case s.requests <- &v2.PermissionEvent{
			Event: &v2.PermissionEvent_Cancel{
				Cancel: &v2.PermissionCancel{RequestId: requestID},
			},
		}:
		case <-s.ctx.Done():
		}
	}
}

// maxLogLineBufBytes is the upper bound for log chunk reassembly buffers and
// individual log lines. Log lines that would exceed this limit are rejected to
// prevent unbounded memory growth from a misbehaving adapter.
const maxLogLineBufBytes = 4 * 1024 * 1024 // 4 MiB per stream

// logForwardSink implements LogEventSink, forwarding log lines to the
// upstream adapter.EventSink.Log. Chunked log lines (LogEvent.chunk != nil)
// are reassembled per stream_name before forwarding.
type logForwardSink struct {
	sink      adapter.EventSink
	chunkBufs map[string][]byte // keyed by stream_name
}

func (s *logForwardSink) Emit(ev *v2.LogEvent) error {
	if ev.GetHeartbeat() != nil {
		return nil
	}
	if chunk := ev.GetChunk(); chunk != nil {
		stream := ev.GetStreamName()
		if chunk.GetSeq() == 0 {
			if s.chunkBufs == nil {
				s.chunkBufs = make(map[string][]byte)
			}
			s.chunkBufs[stream] = nil
		}
		if len(s.chunkBufs[stream])+len(ev.GetLine()) > maxLogLineBufBytes {
			delete(s.chunkBufs, stream)
			return fmt.Errorf("log chunk reassembly: stream %q exceeds %d bytes", stream, maxLogLineBufBytes)
		}
		s.chunkBufs[stream] = append(s.chunkBufs[stream], ev.GetLine()...)
		if !chunk.GetFinal() {
			return nil
		}
		line := s.chunkBufs[stream]
		delete(s.chunkBufs, stream)
		s.sink.Log(stream, line)
		return nil
	}
	if len(ev.GetLine()) > maxLogLineBufBytes {
		return fmt.Errorf("log event: stream %q line exceeds %d bytes", ev.GetStreamName(), maxLogLineBufBytes)
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
