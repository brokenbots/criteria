// toolcall.go — the CRI-165 noop adapter tool-call caller mode, mirrored into
// the in-tree conformance fixture so the adapter-tools examples stay
// self-contained (the standalone criteria-adapter-noop repo is the reference
// implementation; this fixture builds identically-shaped behavior without an
// external dependency).
//
// A step whose input names a tool call (tool_target, tool_name, tool_args) is
// served by the tool-call caller mode: the adapter issues the call as a
// permission.request AdapterEvent (payload kind adapter_tool, CRI-152), waits
// for the correlated PermissionEvent.tool_call_result on the Permissions
// stream, and passes the callee's outcome and outputs through its own
// ExecuteResult under callee.* keys. No LLM is involved; this mode is the
// reusable test double for adapter-tool conformance and examples.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	adapterhost "github.com/brokenbots/criteria-go-adapter-sdk/adapterhost"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Input keys selecting the tool-call modes and their knobs (CRI-165). The
// wire names are the contract shared with the released criteria-adapter-noop;
// example workflows are portable across both.
const (
	inputToolTarget = "tool_target"
	inputToolName   = "tool_name"
	inputToolArgs   = "tool_args"
	inputOutputs    = "outputs"
)

// Output keys carrying the tool-call round-trip result (CRI-165): the callee's
// own outcome and outputs on a completed call, or the typed failure reason
// when the round-trip itself failed. A round-trip failure reports only
// callee.error; a completed call reports outcome+outputs and never callee.error.
const (
	calleeOutcomeKey = "callee.outcome"
	calleeOutputsKey = "callee.outputs"
	calleeErrorKey   = "callee.error"
)

// defaultToolCallTimeout bounds a tool call whose context carries no deadline,
// mirroring the SDK bridge's DefaultToolCallTimeout. The host answers every
// granted call with a typed reply, so the bound only fires against a host
// that predates adapter tools (bare allow-grant, no result). Test code
// overrides it to keep the timeout path fast.
var defaultToolCallTimeout = 60 * time.Second

// toolCallError is the typed failure of an adapter tool call, carrying the
// host's ToolCallResult.call_error code (ADR-0004 §8 registry).
type toolCallError struct{ code string }

func (e *toolCallError) Error() string {
	return fmt.Sprintf("adapter tool call failed: %s", e.code)
}

// pendingToolCall is one in-flight call awaiting its correlated reply.
type pendingToolCall struct {
	done chan struct{}
	res  *v2.ToolCallResult
	// granted records an allow-grant ACK for this id: a bare grant with no
	// typed result within the deadline means the host predates adapter tools.
	granted bool
	// err, set together with done, carries a transport-level failure
	// (denial, stream closed).
	err error
}

// toolCallBridge is the noop fixture's adapter-tools dispatch loop: calls are
// issued as permission.request AdapterEvents on the Execute stream and their
// replies are correlated by request id off the session Permissions stream
// (the only stream tool-call results ride, CRI-152). This is the in-tree
// equivalent of the SDK's ToolCallBridge, scoped to the single-frame replies
// the criteria host sends.
type toolCallBridge struct {
	mu      sync.Mutex
	pending map[string]*pendingToolCall
}

// Permissions runs the dispatch loop: tool-call results, denials, and
// allow-grant ACKs all arrive on the Permissions stream, so pending calls can
// only complete while it runs. Allow-grants are ACKed so the host can retire
// the request; plain permission traffic keeps the auto-allow semantics the
// fixture has always had.
func (b *toolCallBridge) Permissions(ctx context.Context, stream adapterhost.PermissionsStream) error {
	defer b.closeAll()
	for {
		ev, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) || status.Code(err) == codes.Canceled || status.Code(err) == codes.OK {
				return nil
			}
			return err
		}
		switch {
		case ev.GetRequest() != nil:
			id := ev.GetRequest().GetRequestId()
			if id == "" {
				continue
			}
			b.markGranted(id)
			if err := stream.Send(&v2.PermissionDecision{RequestId: id, Decision: "allow"}); err != nil {
				return err
			}
		case ev.GetCancel() != nil:
			cancel := ev.GetCancel()
			reason := "denied"
			if msg := cancel.GetReason(); msg != "" {
				reason = "denied: " + msg
			}
			b.resolve(cancel.GetRequestId(), nil, errors.New(reason))
		case ev.GetToolCallResult() != nil:
			res := ev.GetToolCallResult()
			b.resolve(res.GetRequestId(), res, nil)
		}
	}
}

// register installs the pending entry for requestID.
func (b *toolCallBridge) register(requestID string) *pendingToolCall {
	entry := &pendingToolCall{done: make(chan struct{})}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pending == nil {
		b.pending = map[string]*pendingToolCall{}
	}
	b.pending[requestID] = entry
	return entry
}

// resolve unblocks the pending call for requestID with res or err; unknown ids
// (e.g. a late reply after the caller timed out) are dropped.
func (b *toolCallBridge) resolve(requestID string, res *v2.ToolCallResult, err error) {
	b.mu.Lock()
	entry, ok := b.pending[requestID]
	if ok {
		delete(b.pending, requestID)
		entry.res = res
		entry.err = err
	}
	b.mu.Unlock()
	if ok {
		close(entry.done)
	}
}

// markGranted records the allow-grant ACK for a pending call.
func (b *toolCallBridge) markGranted(requestID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if entry, ok := b.pending[requestID]; ok {
		entry.granted = true
	}
}

// closeAll resolves every pending call after the Permissions stream ended: no
// reply can ever arrive, so a wedged caller is a bug.
func (b *toolCallBridge) closeAll() {
	b.mu.Lock()
	entries := make([]*pendingToolCall, 0, len(b.pending))
	for id, entry := range b.pending {
		entries = append(entries, entry)
		delete(b.pending, id)
	}
	b.mu.Unlock()
	for _, entry := range entries {
		entry.err = errors.New("permissions stream closed while tool call in flight")
		close(entry.done)
	}
}

// call issues one adapter tool call and blocks for the correlated typed reply.
// A failed round-trip (call_error, denial, transport) surfaces as a
// *toolCallError or plain error; a completed call returns the callee's outcome
// and typed outputs.
func (b *toolCallBridge) call(ctx context.Context, sink adapterhost.ExecuteEventSender, target, tool string, args map[string]any) (string, map[string]any, error) {
	requestID, err := newToolCallRequestID()
	if err != nil {
		return "", nil, err
	}
	digest, err := v2.ArgsDigest(args)
	if err != nil {
		return "", nil, fmt.Errorf("tool call args digest: %w", err)
	}
	argsStruct, err := structpb.NewStruct(args)
	if err != nil {
		return "", nil, fmt.Errorf("tool call args: %w", err)
	}
	payload, err := structpb.NewStruct(map[string]any{
		"kind":        "adapter_tool",
		"request_id":  requestID,
		"target":      target,
		"tool":        tool,
		"args":        argsStruct.AsMap(),
		"args_digest": digest,
	})
	if err != nil {
		return "", nil, fmt.Errorf("build tool-call payload: %w", err)
	}

	entry := b.register(requestID)
	if err := sink.Send(&v2.ExecuteEvent{
		Event: &v2.ExecuteEvent_Adapter{
			Adapter: &v2.AdapterEvent{
				EventKind: "permission.request",
				Payload:   payload,
				EmittedAt: timestamppb.Now(),
			},
		},
	}); err != nil {
		b.resolve(requestID, nil, nil)
		return "", nil, fmt.Errorf("send permission.request for tool call %q: %w", tool, err)
	}

	// A call on a context without a deadline runs under the fixture bound; a
	// caller-provided deadline governs as-is.
	callCtx := ctx
	selfDeadline := false
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, defaultToolCallTimeout)
		defer cancel()
		selfDeadline = true
	}

	select {
	case <-entry.done:
		if entry.err != nil {
			return "", nil, fmt.Errorf("tool call to %s: %w", target, entry.err)
		}
		if code := entry.res.GetCallError(); code != "" {
			return "", nil, &toolCallError{code: code}
		}
		outputs, err := decodeJSONObject(string(entry.res.GetOutputsJson()), "tool_call_result.outputs_json")
		if err != nil {
			return "", nil, err
		}
		return entry.res.GetOutcome(), outputs, nil
	case <-callCtx.Done():
		b.mu.Lock()
		_, stillPending := b.pending[requestID]
		delete(b.pending, requestID)
		granted := entry.granted
		b.mu.Unlock()
		if stillPending && granted && selfDeadline && callCtx.Err() == context.DeadlineExceeded {
			// Bare allow-grant with no typed result: the host predates
			// adapter tools (ADR-0004 §9 versioning matrix).
			return "", nil, &toolCallError{code: "host_unsupported"}
		}
		return "", nil, fmt.Errorf("tool call %q to %s: %w", tool, target, callCtx.Err())
	}
}

// newToolCallRequestID mints a correlation id for one tool call.
func newToolCallRequestID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("mint tool-call request id: %w", err)
	}
	return "noop-tool-" + hex.EncodeToString(buf[:]), nil
}

// executeToolCall serves the tool-call caller mode: the named target+tool+args
// are issued as one call and the callee's result passes through under callee.*
// keys. A failed round-trip (denied, unknown adapter, callee crash, ...) is
// reported as the failure outcome with callee.error, not an Execute error: the
// step ran and produced a result.
func (s *noopService) executeToolCall(ctx context.Context, input map[string]string, sink adapterhost.ExecuteEventSender) error {
	target, tool, args, err := toolCallFromInput(input)
	if err != nil {
		return err
	}
	calleeOutcome, calleeOutputs, err := s.bridge().call(ctx, sink, target, tool, args)
	if err != nil {
		outputs, encErr := encodeOutputs(map[string]any{calleeErrorKey: err.Error()})
		if encErr != nil {
			return encErr
		}
		return sendResult(sink, &v2.ExecuteResult{Outcome: "failure", OutputsJson: outputs})
	}
	if calleeOutputs == nil {
		calleeOutputs = map[string]any{}
	}
	outputs, err := encodeOutputs(map[string]any{
		calleeOutcomeKey: calleeOutcome,
		calleeOutputsKey: calleeOutputs,
	})
	if err != nil {
		return err
	}
	return sendResult(sink, &v2.ExecuteResult{Outcome: "success", OutputsJson: outputs})
}

// toolCallFromInput validates the tool-call inputs and renders the §8 call
// arguments. The target's tool segment wins; tool_name names the tool for a
// bare target (adapter.<type>.<name>.tools) and must agree with the segment
// when both are present.
func toolCallFromInput(input map[string]string) (target, tool string, args map[string]any, err error) {
	target, tool, err = parseToolTarget(input[inputToolTarget], input[inputToolName])
	if err != nil {
		return "", "", nil, err
	}
	args = map[string]any{}
	if raw := input[inputToolArgs]; raw != "" {
		if args, err = decodeJSONObject(raw, inputToolArgs); err != nil {
			return "", "", nil, err
		}
	}
	return target, tool, args, nil
}

// parseToolTarget parses the strict §2 tool-target form, mirroring the
// engine's grammar so malformed targets fail before any wire traffic.
// toolName names the tool for a bare target and must agree with the segment
// when both are present.
func parseToolTarget(raw, toolName string) (target, tool string, err error) {
	labels := splitToolTargetLabels(raw)
	if len(labels) != 4 && len(labels) != 5 {
		return "", "", fmt.Errorf("invalid tool_target %q: want adapter.<type>.<name>.tools[.<tool>]", raw)
	}
	for _, label := range labels {
		if !isBarewordLabel(label) {
			return "", "", fmt.Errorf("invalid tool_target %q: label %q is not a bareword", raw, label)
		}
	}
	if labels[0] != "adapter" || labels[3] != "tools" {
		return "", "", fmt.Errorf("invalid tool_target %q: want adapter.<type>.<name>.tools[.<tool>]", raw)
	}
	if name := toolName; name != "" {
		switch {
		case len(labels) == 4:
			labels = append(labels, name)
		case labels[4] != name:
			return "", "", fmt.Errorf("tool_name %q conflicts with tool target %q", name, raw)
		}
	}
	if len(labels) == 4 {
		return "", "", fmt.Errorf("bare tool target %q requires tool_name", raw)
	}
	return raw, labels[4], nil
}

// splitToolTargetLabels splits the dotted §2 target form.
func splitToolTargetLabels(raw string) []string {
	var labels []string
	start := 0
	for i := 0; i <= len(raw); i++ {
		if i == len(raw) || raw[i] == '.' {
			labels = append(labels, raw[start:i])
			start = i + 1
		}
	}
	return labels
}

// isBarewordLabel mirrors the engine's bareword identifier: a leading letter
// or underscore followed by letters, digits, underscores, or hyphens.
func isBarewordLabel(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
			// always allowed
		case c >= '0' && c <= '9', c == '-':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// executeNoopPassthrough serves the data-ish callee mode: a success result
// whose typed outputs are the JSON object carried by the outputs input — the
// callee-side double a caller noop reads back through callee.outputs.
func executeNoopPassthrough(input map[string]string, sink adapterhost.ExecuteEventSender) error {
	outputs, err := decodeJSONObject(input[inputOutputs], inputOutputs)
	if err != nil {
		return err
	}
	encoded, err := encodeOutputs(outputs)
	if err != nil {
		return fmt.Errorf("encode %s: %w", inputOutputs, err)
	}
	return sendResult(sink, &v2.ExecuteResult{Outcome: "success", OutputsJson: encoded})
}

// decodeJSONObject decodes a JSON-object input value, rejecting every other
// JSON shape.
func decodeJSONObject(raw, name string) (map[string]any, error) {
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", name, err)
	}
	if obj == nil {
		return nil, fmt.Errorf("invalid %s: want a JSON object", name)
	}
	return obj, nil
}

// encodeOutputs renders an ExecuteResult.outputs_json payload.
func encodeOutputs(outputs map[string]any) ([]byte, error) {
	return json.Marshal(outputs)
}

// sendResult emits a terminal ExecuteResult event.
func sendResult(sink adapterhost.ExecuteEventSender, result *v2.ExecuteResult) error {
	return sink.Send(&v2.ExecuteEvent{Event: &v2.ExecuteEvent_Result{Result: result}})
}
