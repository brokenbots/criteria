package adapterhost

// tool_call.go — CRI-159: the M4.1 host seam for adapter tool calls
// (ADR-0004 §8), extended in CRI-160 with real nested execution and in
// CRI-161 with async reply correlation. A tool call arrives as a
// permission.request AdapterEvent on the Execute stream carrying
// kind == "adapter_tool" (or a §2 target key). The seam detects those
// payloads, gates them in a fixed order, and answers with a typed
// PermissionEvent.tool_call_result on the caller's Permissions stream keyed
// by request_id:
//
//  1. capability gate (deterministic, no policy call): the caller session's
//     handshake capabilities must include adapter_tools;
//  2. target parse/validate: the strict §2 form
//     adapter.<type>.<name>.tools[.<tool>];
//  3. permission policy: the same allow_tools surface as any tool request
//     (first-match-wins globs over the full target string), with the step's
//     recorded tools grants unioned into the effective allow set (ADR-0004
//     §4: "every entry grants the call"); a policy deny takes the existing
//     permission deny path unchanged;
//  4. graph validation: callee adapter declared (FSMGraph.Adapters) and a
//     named tool present on a callee that declares a static surface;
//  5. self-call rejection (ADR-0004 §10);
//  6. runtime call-chain checks (CRI-162, the enforcement point for
//     policy.max_tool_depth — the compiler only warns): a nested call may
//     not re-enter an adapter already on the caller adapter ref chain
//     (runtime cycle detection, typed cycle_detected) and may not exceed
//     the graph's policy.max_tool_depth (default 8, typed depth_exceeded);
//     each rejected call is audited and the run continues;
//  7. nested execution (CRI-160): the callee runs in its OWN session via a
//     nested SessionManager.Execute issued under the caller's in-flight
//     Execute. The callee session is resolved through the lazy-bind path,
//     governed by the callee's own environment and allow_tools (the declaring
//     workflow's permissions.allow_tools — never the caller's step grants),
//     its outputs are decoded against its own OutputSchema, and the result is
//     returned to the caller as the tool result. The callee's own on_crash
//     governs its session; the caller receives a typed failure (callee_crash)
//     and its outcome routing is unaffected (ADR-0004 §5).
//
// CRI-161: gate 7 dispatches asynchronously. The gates above stay on the
// caller's Execute event loop, but the nested Execute runs on its own
// goroutine, registered in the caller session's pending map keyed by
// request_id (the host-side mirror of the MCP bridge's
// registerPendingPerm/decisionCh pattern). The completing goroutine delivers
// the typed reply — allow + result, or the typed failure — on the caller's
// Permissions stream, so multiple in-flight calls per caller session
// interleave and replies may arrive in any order (the Permissions stream
// contract explicitly allows it; correlation is by request_id). Timeout and
// cancellation propagate into the callee through the caller's Execute
// context; the reply is always delivered (typed callee_timeout / canceled),
// so a caller never wedges waiting for a result.
//
// Plain (non-tool) permission requests never reach this file's decision path:
// the sink routes them to the untouched plain flow, so workflows without tool
// refs execute through identical code paths.
//
// CRI-163: a dispatched call is observable on the run's adapter event stream,
// attributed under the caller step — tool.call when the call starts (payload:
// target, tool, depth, request_id) and tool.call_result when it settles
// (adding outcome and duration; call_error on failure). Audit entries carry
// the nested tool-call layer (DecisionLogEntry.Layer) so caller-layer and
// callee-side decisions are separately distinguishable.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/workflow"
)

// adapterToolsCapability is the well-known AdapterInfo capability that marks
// an adapter as able to issue tool calls (ADR-0004 §9).
const adapterToolsCapability = "adapter_tools"

// Typed call_error codes (ADR-0004 §8 registry; free-form string values on
// ToolCallResult.call_error). malformed_target extends the registry for
// detected tool calls whose target does not parse as the §2 form;
// invalid_args extends it for call arguments that fail the callee's input
// schema (required keys missing, unknown keys on a declared surface);
// paused extends it for calls refused by the CRI-169 pause gate while the
// session's nested tool calls are being drained (ADR-0004 §11).
const (
	callErrorCapabilityMissing = "capability_missing"
	callErrorMalformedTarget   = "malformed_target"
	callErrorUnknownAdapter    = "unknown_adapter"
	callErrorUnknownTool       = "unknown_tool"
	callErrorSelfCall          = "self_call"
	callErrorNotYetSupported   = "not_yet_supported"
	callErrorDepthExceeded     = "depth_exceeded"
	callErrorCycleDetected     = "cycle_detected"
	callErrorCalleeCrash       = "callee_crash"
	callErrorInvalidArgs       = "invalid_args"
	callErrorCalleeTimeout     = "callee_timeout"
	callErrorCanceled          = "canceled"
	callErrorPaused            = "paused"
)

// calleeReportedCallErrorCode is the reserved output key on a failure result
// carrying a callee-reported typed call_error code (CRI-172). Only
// callee-meaningful registry codes are honored (see calleeReportedCallError).
const calleeReportedCallErrorCode = "call_error"

// toolCallTarget is the parsed shape of an adapter tool-call target string
// (ADR-0004 §2): adapter.<type>.<name>.tools[.<tool>].
type toolCallTarget struct {
	AdapterRef string // "<type>.<name>"
	Tool       string // tool name; empty for the bare whole-surface form
}

// String renders the target back to its §2 dotted form.
func (t toolCallTarget) String() string {
	target := "adapter." + t.AdapterRef + ".tools"
	if t.Tool != "" {
		target += "." + t.Tool
	}
	return target
}

// isBarewordLabel reports whether s is a non-empty HCL-style bareword
// identifier: a leading letter or underscore followed by letters, digits,
// underscores, or hyphens. Target labels come from HCL traversal attribute
// names, so incoming targets are held to the same ASCII identifier shape.
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

// splitTargetLabels splits a dotted target into its labels, reporting false
// when any label is not a bareword identifier.
func splitTargetLabels(target string) ([]string, bool) {
	labels := strings.Split(target, ".")
	for _, label := range labels {
		if !isBarewordLabel(label) {
			return nil, false
		}
	}
	return labels, true
}

// parseToolCallTarget parses the strict §2 tool-target form:
// adapter.<type>.<name>.tools[.<tool>].
func parseToolCallTarget(target string) (toolCallTarget, bool) {
	labels, ok := splitTargetLabels(target)
	if !ok || len(labels) < 4 || len(labels) > 5 {
		return toolCallTarget{}, false
	}
	if labels[0] != "adapter" || labels[3] != "tools" {
		return toolCallTarget{}, false
	}
	parsed := toolCallTarget{AdapterRef: labels[1] + "." + labels[2]}
	if len(labels) == 5 {
		parsed.Tool = labels[4]
	}
	return parsed, true
}

// looksLikeToolCallTarget reports whether target parses as the detection
// grammar adapter.<type>.<name>[.tools[.<tool>]]: three bareword labels under
// the adapter root (an adapter-ref mention that is not a valid tool target),
// or the bare/named §2 tool-target forms. Other shapes are not tool calls and
// fall through to the plain permission flow.
func looksLikeToolCallTarget(target string) bool {
	labels, ok := splitTargetLabels(target)
	if !ok {
		return false
	}
	switch len(labels) {
	case 3:
		return labels[0] == "adapter"
	case 4, 5:
		return labels[0] == "adapter" && labels[3] == "tools"
	default:
		return false
	}
}

// toolCallRequestDetected reports whether a permission.request payload is an
// adapter tool call (CRI-159): the payload marks itself with
// kind == "adapter_tool", or carries a target key whose value parses as the
// detection grammar. Payloads without either marker take the plain
// permission flow, byte-identical to before the seam landed.
func toolCallRequestDetected(payload map[string]any) bool {
	if kind, _ := payload["kind"].(string); kind == "adapter_tool" {
		return true
	}
	if target, _ := payload["target"].(string); target != "" {
		return looksLikeToolCallTarget(target)
	}
	return false
}

// toolGrantAllows reports whether the step's recorded tools grants
// (workflow.StepNode.Tools, CRI-157) allow a call to the parsed target.
// Grants are literals, not globs (ADR-0004 §4): a bare …tools grant covers
// the instance's entire surface (both the bare and any named form), while a
// …tools.<tool> grant covers exactly that named call.
func toolGrantAllows(refs []workflow.AdapterToolRef, calleeRef, tool string) bool {
	for _, ref := range refs {
		if ref.CalleeRef != calleeRef {
			continue
		}
		if ref.Tool == "" {
			return true
		}
		if tool != "" && ref.Tool == tool {
			return true
		}
	}
	return false
}

// staticToolErrorCode validates a requested tool against the callee's
// compiled adapter declaration. It returns a call_error code or "" when the
// call names a resolvable tool. Dynamic adapters are extensible: the runtime
// resolves names (CRI-173), so no name check applies. On a static surface a
// named call must reference a declared tool; a bare whole-surface call
// requires the adapter to present a static surface at all. An adapter that
// declares neither tool blocks nor dynamic_tools presents no surface, which
// the CRI-156 compiler only flags advisory for bare refs — the runtime check
// lives here.
func staticToolErrorCode(node *workflow.AdapterNode, named string) string {
	if node.DynamicTools {
		return ""
	}
	if len(node.StaticTools) == 0 {
		return callErrorUnknownTool
	}
	if named == "" {
		return ""
	}
	for _, name := range node.StaticTools {
		if name == named {
			return ""
		}
	}
	return callErrorUnknownTool
}

// sessionDeclaresCapability reports whether the session's cached handshake
// InfoResponse capabilities include capName.
func sessionDeclaresCapability(sess *Session, capName string) bool {
	if sess == nil {
		return false
	}
	for _, cap := range sess.Capabilities {
		if cap == capName {
			return true
		}
	}
	return false
}

// toolCallMatchedPattern extracts the matched pattern or grant from a policy
// reason for the permission.granted payload, mirroring the plain request
// path's "matched: " / alias-suffix handling.
func toolCallMatchedPattern(reason string) string {
	pattern := strings.TrimPrefix(reason, "matched: ")
	pattern = strings.TrimPrefix(pattern, "granted: tools entry ")
	if idx := strings.Index(pattern, " (alias for "); idx >= 0 {
		pattern = pattern[:idx]
	}
	return pattern
}

// evaluateToolCall evaluates an adapter tool-call request against the step's
// effective allow set: the step's recorded tools grants (literals, checked
// first) plus the session's permission policy — the same allow_tools surface
// as any tool request, evaluated on the full target string. It performs the
// same bookkeeping as Evaluate (decision record, PermissionEvent on the
// session stream, audit entry) so the permission decision is logged
// identically for both request shapes. The session policy itself is untouched,
// so concurrent plain requests keep their existing semantics. The layer is
// the nested tool-call layer the call is evaluated in (CRI-163).
func (ps *permissionState) evaluateToolCall(requestID, target string, parsed toolCallTarget, argsDigest, fullCmd string, grants []workflow.AdapterToolRef, layer int) (allow bool, reason string) {
	ps.mu.Lock()
	policy := ps.policy
	ps.mu.Unlock()

	if toolGrantAllows(grants, parsed.AdapterRef, parsed.Tool) {
		allow, reason = true, "granted: tools entry "+parsed.String()
	} else {
		if policy == nil {
			policy = denyAllPolicy{}
		}
		req := PermissionRequest{ID: requestID, Tool: target}
		if fullCmd != "" {
			req.Details = map[string]string{"full_command_text": fullCmd}
		}
		allow, reason = policy.Decide(req)
	}

	decision := ps.recordDecision(requestID, target, argsDigest, allow, reason, layer)
	ps.sendEvent(requestID, allow, reason)
	ps.writeAudit(&decision)

	return allow, reason
}

// sendToolCallResult delivers a typed tool-call failure reply (CRI-152
// PermissionEvent.tool_call_result) on the session Permissions stream,
// non-blocking with drop-on-backlog like sendEvent.
func (ps *permissionState) sendToolCallResult(requestID, callError string) {
	ps.sendToolCallResultEvent(&v2.ToolCallResult{
		RequestId: requestID,
		CallError: callError,
	})
}

// sendToolCallResultEvent delivers an assembled tool-call reply on the
// session Permissions stream, non-blocking with drop-on-backlog like
// sendEvent.
//
// CRI-169: unlike plain permission traffic, tool-call replies are buffered
// while the session is paused (active == false) — the reply is the only
// unblock for the caller's pending correlation map, so dropping it while
// paused would wedge the awaiting caller until resume with no way to learn
// the outcome. Plain permission request/cancel events keep the drop when
// inactive (they are re-presented on resume / restore instead).
func (ps *permissionState) sendToolCallResultEvent(res *v2.ToolCallResult) {
	ps.mu.Lock()
	requests := ps.requests
	ps.mu.Unlock()

	if requests == nil {
		return
	}

	select {
	case requests <- &v2.PermissionEvent{
		Event: &v2.PermissionEvent_ToolCallResult{
			ToolCallResult: res,
		},
	}:
	default:
		// Stream consumer is backlogged; don't block the Execute goroutine.
	}
}

// toolCallPayload holds the §8 request fields extracted from a tool-call
// payload.
type toolCallPayload struct {
	requestID  string
	target     string
	tool       string
	argsDigest string
	fullCmd    string
	// args carries the typed call arguments (§8) that become the callee's
	// input keys. Nil when the caller sent no args.
	args map[string]any
}

// parseToolCallPayload extracts the request id and call fields from a
// detected tool-call payload, reporting false when no request id is
// resolvable.
func parseToolCallPayload(payload map[string]any) (toolCallPayload, bool) {
	requestID, ok := resolvePermissionRequestID(payload)
	if !ok {
		return toolCallPayload{}, false
	}
	p := toolCallPayload{requestID: requestID}
	p.target, _ = payload["target"].(string)
	p.tool, _ = payload["tool"].(string)
	p.argsDigest, _ = payload["args_digest"].(string)
	if p.argsDigest == "" {
		p.argsDigest, _ = payload["argsDigest"].(string)
	}
	p.fullCmd, _ = payload["full_command_text"].(string)
	if args, ok := payload["args"].(map[string]any); ok {
		p.args = args
	}
	return p, true
}

// applyToolCallPolicy evaluates the call against the step's effective allow
// set (ADR-0004 §4: the step's tools grants unioned with the session policy —
// allow_tools semantics unchanged, evaluated on the full target string). On
// grant it emits permission.granted and returns true; on deny it completes
// the existing permission deny path (PermissionEvent.cancel via the
// permissionState, permission.denied, needs_review override) and returns
// false.
func (s *permissionInterceptSink) applyToolCallPolicy(req *toolCallPayload, parsed toolCallTarget) bool {
	grants := []workflow.AdapterToolRef(nil)
	if s.step != nil {
		grants = s.step.Tools
	}
	allow, reason := s.permState.evaluateToolCall(req.requestID, req.target, parsed, req.argsDigest, req.fullCmd, grants, s.nesting.depth)
	if !allow {
		s.anyDenied = true
		deniedTool := req.tool
		if deniedTool == "" {
			deniedTool = req.target
		}
		deniedPayload := map[string]any{
			"request_id": req.requestID,
			"tool":       s.redactEventValue(deniedTool),
			"reason":     reason,
		}
		if suggestion := PermissionDenialSuggestion(s.session.Adapter, deniedTool); suggestion != "" {
			deniedPayload["suggestion"] = suggestion
		}
		s.inner.Adapter("permission.denied", deniedPayload)
		return false
	}

	// The permission surface granted the call; reflect the allow decision on
	// the execute sink exactly as plain requests do. The typed call outcome
	// follows separately on the Permissions stream.
	s.inner.Adapter("permission.granted", map[string]any{
		"request_id": req.requestID,
		"tool":       s.redactEventValue(req.target),
		"pattern":    toolCallMatchedPattern(reason),
	})
	return true
}

// handleToolCallRequest implements the CRI-159 host seam for an adapter tool
// call detected in a permission.request payload. See the file comment for the
// gate order and the stub reply contract.
func (s *permissionInterceptSink) handleToolCallRequest(payload map[string]any) {
	req, ok := parseToolCallPayload(payload)
	if !ok {
		// Without a request id there is nothing to key a typed reply to;
		// reuse the unchanged malformed-request deny path.
		s.anyDenied = true
		s.inner.Adapter("permission.denied", map[string]any{
			"reason": "malformed permission.request payload: missing request_id",
		})
		return
	}

	// Gate 1 (deterministic, before any policy evaluation): the caller
	// session's handshake capabilities must include adapter_tools.
	if !sessionDeclaresCapability(s.session, adapterToolsCapability) {
		s.rejectToolCall(req.requestID, req.target, req.argsDigest, callErrorCapabilityMissing)
		return
	}

	// Gate 2: parse the §2 target form. A detected tool call without a valid
	// tool target cannot be policy-evaluated meaningfully, so the shape check
	// runs before the policy gate.
	parsed, ok := parseToolCallTarget(req.target)
	if !ok {
		s.rejectToolCall(req.requestID, req.target, req.argsDigest, callErrorMalformedTarget)
		return
	}

	// Gate 2.5 (CRI-169, ADR-0004 §11): the pause gate. While the session is
	// pausing (drain-first posture) or paused, new nested tool calls are
	// refused with the typed `paused` call_error instead of starting; the
	// in-flight ones are drained (or canceled) by Session.Pause. The caller
	// re-issues after resume.
	if s.permState.toolCallsPaused() {
		s.rejectToolCall(req.requestID, req.target, req.argsDigest, callErrorPaused)
		return
	}

	// Gate 3: permission policy (deny reuses the unchanged deny path).
	if !s.applyToolCallPolicy(&req, parsed) {
		return
	}

	// Gate 4: graph validation — the callee adapter must be declared, and a
	// referenced static tool must exist when the callee declares a static
	// surface.
	if code := s.validateToolCallGraph(parsed, &req); code != "" {
		s.rejectToolCall(req.requestID, req.target, req.argsDigest, code)
		return
	}

	// Gate 5: self-call (ADR-0004 §10) — callee equals the caller instance.
	if parsed.AdapterRef == s.session.Name {
		s.rejectToolCall(req.requestID, req.target, req.argsDigest, callErrorSelfCall)
		return
	}

	// Gate 7: nested execution (CRI-160/CRI-161). An otherwise-allowed call
	// runs the callee in its own session and replies with the callee's
	// result. Dispatch is asynchronous so in-flight calls interleave; the
	// typed reply is delivered on the caller's Permissions stream by the
	// completing goroutine.
	s.dispatchNestedToolCall(&req, parsed)
}

// nestedToolCallMaxDepth returns the effective policy.max_tool_depth for the
// caller's graph. A compiled graph normalizes the unset default to 8
// (workflow.DefaultPolicy); hand-built graphs may carry 0, so the engine
// default applies there too.
func (s *permissionInterceptSink) nestedToolCallMaxDepth() int {
	if s.graph != nil && s.graph.Policy.MaxToolDepth > 0 {
		return s.graph.Policy.MaxToolDepth
	}
	return workflow.DefaultPolicy.MaxToolDepth
}

// toolCallNesting is the per-call nesting state threaded through nested
// SessionManager.Execute calls (CRI-162): depth counts the nested tool-call
// Executes above the current one (the caller step's own depth baseline is 0,
// compared against policy.max_tool_depth, default 8), and chain is the
// caller adapter refs visited on the call chain, seeded with the executing
// step's own adapter ref. The chain backs runtime cycle detection: a nested
// call whose callee ref already appears on the chain re-enters an adapter on
// the chain.
type toolCallNesting struct {
	depth int
	chain []string
}

// enters reports whether ref is already on the call chain — i.e. the nested
// call would re-enter an adapter on the chain (a runtime cycle).
func (n toolCallNesting) enters(ref string) bool {
	return slices.Contains(n.chain, ref)
}

// descends returns the nesting state for a call to ref: one level deeper,
// with ref appended to a fresh chain. The chain is copied rather than
// appended in place because CRI-161 dispatches sibling calls concurrently —
// sharing a backing array across the completing goroutines would be a data
// hazard.
func (n toolCallNesting) descends(ref string) toolCallNesting {
	chain := make([]string, len(n.chain)+1)
	copy(chain, n.chain)
	chain[len(n.chain)] = ref
	return toolCallNesting{depth: n.depth + 1, chain: chain}
}

// nestedToolCall captures a dispatched adapter tool call: everything the
// completing goroutine needs to run the callee and deliver the typed reply
// (CRI-161). The dispatch path copies these fields before returning to the
// caller's Execute event loop, which immediately moves on to the next
// Permissions event.
type nestedToolCall struct {
	requestID string
	target    string
	// tool is the resolved tool name: the §2 target's tool segment, falling
	// back to the request's tool field when the target omits it (dynamic
	// tool surface) — the same resolution validateToolCallGraph applies.
	tool       string
	argsDigest string
	calleeRef  string
	calleeStep *workflow.StepNode
	nesting    toolCallNesting
	// startedAt is the dispatch time; the tool.call_result event's duration
	// (CRI-163) is measured from it.
	startedAt time.Time
}

// Nested adapter tool-call observability event kinds (CRI-163), attributed
// under the caller step on the run's adapter event stream.
const (
	nestedToolCallEventKind       = "tool.call"
	nestedToolCallResultEventKind = "tool.call_result"
)

// nestedToolCallEventPayload builds the shared CRI-163 event payload: the
// call target, the resolved tool name, the nested call's own depth (1 for a
// step-level caller's first-level nested call), and the request id the typed
// reply is correlated by.
//
// The target and the resolved tool echo adapter-supplied permission.request
// values and may embed values registered in the run's redaction registry, so
// both are masked before emission (CRI-163) — the same treatment the decision
// payloads and audit entries get. depth and request_id are host-computed.
func (s *permissionInterceptSink) nestedToolCallEventPayload(call *nestedToolCall) map[string]any {
	return map[string]any{
		"target":     s.redactEventValue(call.target),
		"tool":       s.redactEventValue(call.tool),
		"depth":      call.nesting.depth,
		"request_id": call.requestID,
	}
}

// emitNestedToolCallEvent emits the tool.call event for a dispatched nested
// adapter tool call, attributed under the caller step via the caller's
// execute sink. It runs synchronously at dispatch, before the callee Execute
// goroutine is spawned, so it strictly precedes every event the callee emits.
func (s *permissionInterceptSink) emitNestedToolCallEvent(call *nestedToolCall) {
	s.inner.Adapter(nestedToolCallEventKind, s.nestedToolCallEventPayload(call))
}

// emitNestedToolCallResultEvent emits the tool.call_result event for a
// settled nested adapter tool call, after the callee Execute returned. On a
// successful call the payload adds outcome (the callee's own outcome) and
// duration (wall-clock from dispatch, integer milliseconds); on a failed call
// outcome is omitted and call_error carries the typed failure code instead.
func (s *permissionInterceptSink) emitNestedToolCallResultEvent(call *nestedToolCall, outcome, callError string) {
	payload := s.nestedToolCallEventPayload(call)
	payload["duration"] = time.Since(call.startedAt).Milliseconds()
	if outcome != "" {
		payload["outcome"] = outcome
	}
	if callError != "" {
		payload["call_error"] = callError
	}
	s.inner.Adapter(nestedToolCallResultEventKind, payload)
}

// dispatchNestedToolCall dispatches an allowed adapter tool call to the callee
// adapter in its own session (CRI-160, ADR-0004 §8) and answers the caller
// with the callee's result as the tool result.
//
// CRI-161: the gates stay synchronous on the caller's Execute event loop, but
// the callee Execute itself is dispatched asynchronously — the call is
// registered in the caller session's pending map keyed by request_id and
// executed on its own goroutine, so the caller's Execute keeps reading its
// Permissions stream while calls are in flight. Multiple in-flight calls per
// caller session therefore interleave, and replies are delivered in whatever
// order they complete (the Permissions stream contract explicitly allows any
// order; correlation is by request_id), mirroring the MCP bridge's
// registerPendingPerm/decisionCh pending map
// (cmd/criteria-adapter-mcp/bridge.go).
//
// Locking: the nested Execute runs under the caller's in-flight Execute with
// no SessionManager lock held here (see SessionManager.Execute for the
// contract). SessionManager.execute waits for every dispatched call to settle
// (sink.waitPending) before the step completes.
func (s *permissionInterceptSink) dispatchNestedToolCall(req *toolCallPayload, parsed toolCallTarget) {
	// A sink without a wired manager cannot execute a callee (directly
	// constructed test fixtures). The real host always wires the manager.
	if s.mgr == nil {
		s.rejectToolCall(req.requestID, req.target, req.argsDigest, callErrorNotYetSupported)
		return
	}

	// Runtime call-chain checks (CRI-162, ADR-0004 §6). The compile-time
	// cycle warning stays advisory; this is the enforcement point. Cycle
	// gate first: it is the cheapest and most specific diagnosis — a call
	// that re-enters an adapter already on the caller adapter ref chain
	// fails typed; the run continues.
	if s.nesting.enters(parsed.AdapterRef) {
		s.rejectToolCall(req.requestID, req.target, req.argsDigest, callErrorCycleDetected)
		return
	}

	// Depth gate (ADR-0004 §6): the tool-call stack is bounded by
	// policy.max_tool_depth (default 8). A call that would exceed the bound
	// is a typed failure for the caller; the run continues. The callee ref
	// is appended to the call chain here so the callee's own nested calls
	// see it.
	nesting := s.nesting.descends(parsed.AdapterRef)
	if nesting.depth > s.nestedToolCallMaxDepth() {
		s.rejectToolCall(req.requestID, req.target, req.argsDigest, callErrorDepthExceeded)
		return
	}

	// The callee adapter node: gate 4 already validated the declaration
	// against this graph, so a nil node here means graph validation was
	// skipped (nil graph) and the session lookup will decide resolvability.
	calleeNode := (*workflow.AdapterNode)(nil)
	if s.graph != nil {
		calleeNode = s.graph.Adapters[parsed.AdapterRef]
	}

	// The callee's declared schema surface, captured at its verify-time
	// handshake. Drives input validation and output typing for the synthetic
	// step; nil means permissive (no declared schema).
	calleeInfo := s.mgr.cachedAdapterInfo(parsed.AdapterRef)

	calleeStep, inputErr := syntheticCalleeStep(parsed, calleeNode, s.graph, calleeInfo, req.args)
	if inputErr != nil {
		s.rejectToolCall(req.requestID, req.target, req.argsDigest, callErrorInvalidArgs)
		return
	}

	// Register for reply correlation, then hand the call to its own
	// goroutine so the caller's Execute event loop stays live. Only calls
	// that pass every gate are registered, so teardown never audits a
	// synchronously rejected call as abandoned. See startNestedToolCall
	// for the pause-gate interaction (CRI-169).
	tool := parsed.Tool
	if tool == "" {
		tool = req.tool
	}
	call := &nestedToolCall{
		requestID:  req.requestID,
		target:     req.target,
		tool:       tool,
		argsDigest: req.argsDigest,
		calleeRef:  parsed.AdapterRef,
		calleeStep: calleeStep,
		nesting:    nesting,
		startedAt:  time.Now(),
	}
	s.startNestedToolCall(call)
}

// startNestedToolCall registers the gated call for reply correlation and
// hands it to its own goroutine. CRI-169: the call runs on a derived,
// cancelable context so the pause drain can cancel non-draining calls
// (typed `canceled` reply) without touching the caller's Execute context.
// The registration re-checks the pause gate atomically under the permission
// state's mutex, closing the race between the gate in handleToolCallRequest
// and registration: a call that passes gate 2.5 but registers after the
// pause gate was set is refused typed `paused` (it never starts), so once
// the gate is set the pending set only shrinks.
func (s *permissionInterceptSink) startNestedToolCall(call *nestedToolCall) {
	nestedCtx, nestedCancel := context.WithCancel(s.nestedExecCtx())
	if !s.permState.registerPendingToolCall(call.requestID, call.target, nestedCancel) {
		nestedCancel()
		s.rejectToolCall(call.requestID, call.target, call.argsDigest, callErrorPaused)
		return
	}
	s.emitNestedToolCallEvent(call)
	s.nested.Add(1)
	go s.runNestedToolCall(nestedCtx, nestedCancel, call)
}

// runNestedToolCall executes the nested callee and delivers the typed reply
// on the caller's Permissions stream (CRI-161). It runs on its own goroutine;
// its context is derived from the caller's Execute context (CRI-169 adds the
// per-call cancel used by the pause drain), so the caller's step timeout, run
// cancellation, and a pause drain's cancelation of non-draining calls all
// reach the callee. The reply is delivered on every path — including the
// typed timeout, pause-drain cancellation, and cancellation failures — so the
// caller's pending correlation map always unblocks: a wedged stream is a bug,
// never a timeout mode.
func (s *permissionInterceptSink) runNestedToolCall(nestedCtx context.Context, nestedCancel context.CancelFunc, call *nestedToolCall) {
	// Release both waiters: the SessionManager's waitPending latch and the
	// derived context's resources (vet lostcancel).
	defer nestedCancel()
	defer s.nested.Done()

	result, execErr := s.mgr.execute(nestedCtx, call.calleeRef, call.calleeStep, s.inner, call.nesting)

	// Clear the pending registration before delivering, so a concurrent
	// session teardown never audits a call whose result was already sent.
	s.permState.clearPendingToolCall(call.requestID)

	if execErr != nil {
		s.reportNestedCallFailure(call, execErr)
		return
	}

	// Callee-reported typed failure (CRI-172): a failure result carrying the
	// reserved call_error output is the callee's own typed rejection — e.g.
	// the mcp adapter refusing a tool outside its discovered tools/list
	// surface. Deliver it as the well-known call_error reply, distinguishable
	// from a policy deny and from a callee crash.
	if code := calleeReportedCallError(result); code != "" {
		s.permState.writeAudit(&DecisionLogEntry{
			SessionID:  s.permState.sessionID,
			RequestID:  call.requestID,
			Tool:       call.target,
			ArgsDigest: call.argsDigest,
			Decision:   "deny",
			Reason:     "nested callee rejected call: " + code,
			// The call was evaluated in this sink's own layer; the callee's
			// typed rejection is attributed there, not here.
			Layer:       s.nesting.depth,
			EvaluatedAt: time.Now(),
		})
		s.emitNestedToolCallResultEvent(call, "", code)
		s.permState.sendToolCallResult(call.requestID, code)
		return
	}

	outputsJSON, encErr := encodeToolCallOutputs(result.Outputs)
	if encErr != nil {
		s.reportNestedCallFailure(call, encErr)
		return
	}

	s.emitNestedToolCallResultEvent(call, result.Outcome, "")
	s.permState.sendToolCallResultEvent(&v2.ToolCallResult{
		RequestId:   call.requestID,
		Outcome:     result.Outcome,
		OutputsJson: outputsJSON,
	})
}

// reportNestedCallFailure reports a failed nested callee execution to the
// caller: an audit deny entry plus the typed failure reply, so the caller's
// correlation map unblocks. The reply is sent on every path.
//
// Error mapping (typed call_error registry, ADR-0004 §8):
//   - an abort_run crash in the callee's own session aborts the run: the
//     callee's on_crash governs its session, so that policy decision
//     propagates as a fatal run error rather than being swallowed into a
//     tool result. The caller still receives the typed callee_crash reply;
//     the fatal error rides the sink back to SessionManager.execute, which
//     propagates it to the engine once the caller's adapter call returns;
//   - the caller's step deadline (context.DeadlineExceeded) maps to
//     callee_timeout: the callee did not answer within its deadline;
//   - run cancellation (context.Canceled) maps to canceled: the call was
//     abandoned while in flight;
//   - an unknown callee session maps to unknown_adapter;
//   - anything else is callee_crash.
func (s *permissionInterceptSink) reportNestedCallFailure(call *nestedToolCall, execErr error) {
	code := callErrorCalleeCrash
	var fatal *FatalRunError
	switch {
	case errors.As(execErr, &fatal):
		s.setNestedFatalErr(execErr)
	case errors.Is(execErr, context.DeadlineExceeded):
		code = callErrorCalleeTimeout
	case errors.Is(execErr, context.Canceled):
		code = callErrorCanceled
	case errors.Is(execErr, ErrUnknownSession):
		// Unknown at runtime: not in the verified set and not bindable.
		code = callErrorUnknownAdapter
	}
	s.permState.writeAudit(&DecisionLogEntry{
		SessionID:  s.permState.sessionID,
		RequestID:  call.requestID,
		Tool:       call.target,
		ArgsDigest: call.argsDigest,
		Decision:   "deny",
		Reason:     "nested callee execution failed: " + code + ": " + execErr.Error(),
		// The call was evaluated in this sink's own layer; the failure is
		// attributed there, not to the callee's deeper layer.
		Layer:       s.nesting.depth,
		EvaluatedAt: time.Now(),
	})
	s.emitNestedToolCallResultEvent(call, "", code)
	s.permState.sendToolCallResult(call.requestID, code)
}

// calleeReportedCallError recognizes a callee's own typed call_error
// (CRI-172): a failure result whose outputs carry the reserved call_error key
// with a well-known code whose semantics belong to the callee. Only
// callee-meaningful codes are honored — host-computed codes (unknown adapter,
// cycle, depth, self-call, pause, cancellation, ...) stay host-computed so a
// callee cannot forge host gate decisions. Returns "" for any other result.
func calleeReportedCallError(result adapter.Result) string {
	if result.Outcome != "failure" {
		return ""
	}
	raw, ok := result.Outputs[calleeReportedCallErrorCode]
	if !ok || !raw.IsKnown() || raw.IsNull() || !raw.Type().Equals(cty.String) {
		return ""
	}
	code := strings.TrimSpace(raw.AsString())
	switch code {
	case callErrorUnknownTool, callErrorCapabilityMissing, callErrorNotYetSupported:
		return code
	default:
		return ""
	}
}

// nestedExecCtx returns the Execute context this sink serves, so the nested
// callee Execute honors run cancellation. Sinks constructed without a context
// (direct test fixtures) fall back to context.Background().
func (s *permissionInterceptSink) nestedExecCtx() context.Context {
	if s.execCtx == nil {
		return context.Background()
	}
	return s.execCtx
}

// syntheticCalleeStep builds the StepNode that executes a tool-called callee
// (CRI-160). The step carries:
//   - the callee input keys from the call args, rendered to the wire shape
//     (plain strings raw, structured values JSON-encoded) and validated
//     against the callee's declared input schema;
//   - the callee adapter's own environment (AdapterNode.Environment, already
//     resolved to the workflow default at compile time) — never the caller's;
//   - the callee's own allow_tools policy: the declaring workflow's
//     permissions.allow_tools, the same union base any step targeting the
//     callee would receive. The caller's allow_tools gated the call itself
//     (CRI-159 gate 3) and does not carry into the callee session;
//   - the callee's declared OutputSchema so its outputs are typed by the same
//     decode path as normal step outputs;
//   - no OnCrash: the callee's own adapter-level on_crash, captured at bind,
//     governs its session.
//
// owningGraph is the graph that validated the callee (the caller's graph —
// CRI-159 gate 4 restricts callees to adapters declared there), whose
// workflow-level allow_tools governs the callee session. A nil callee node
// (nil graph, gate 4 skipped) yields a permissive step; session resolution
// decides resolvability.
func syntheticCalleeStep(parsed toolCallTarget, calleeNode *workflow.AdapterNode, owningGraph *workflow.FSMGraph, calleeInfo *workflow.AdapterInfo, args map[string]any) (*workflow.StepNode, error) {
	input, err := calleeInputFromArgs(args, calleeInfo)
	if err != nil {
		return nil, err
	}

	step := &workflow.StepNode{
		Name:        "tool." + parsed.String(),
		TargetKind:  workflow.StepTargetAdapter,
		AdapterRef:  parsed.AdapterRef,
		Input:       input,
		Environment: calleeNodeEnvironment(calleeNode),
		AllowTools:  workflowAllowToolsForCallee(owningGraph),
	}
	if calleeInfo != nil {
		step.OutputSchema = calleeInfo.OutputSchema
	}
	return step, nil
}

// calleeNodeEnvironment resolves the callee adapter's own environment.
func calleeNodeEnvironment(calleeNode *workflow.AdapterNode) string {
	if calleeNode == nil {
		return ""
	}
	return calleeNode.Environment
}

// workflowAllowToolsForCallee returns the callee's own allow_tools: the
// declaring workflow's permissions.allow_tools.
func workflowAllowToolsForCallee(owningGraph *workflow.FSMGraph) []string {
	if owningGraph == nil {
		return nil
	}
	return owningGraph.WorkflowAllowTools()
}

// calleeInputFromArgs renders the §8 typed call arguments as the callee's
// wire input map (map<string,string>, the same shape a step's input{} block
// produces): string values pass through raw; every other JSON type is
// encoded. When the callee declares an input schema, the args are validated
// against it — required keys must be present and non-empty, and on a declared
// surface no undeclared keys may appear (the compiler enforces the same
// posture for static input{} blocks).
func calleeInputFromArgs(args map[string]any, calleeInfo *workflow.AdapterInfo) (map[string]string, error) {
	input := make(map[string]string, len(args))
	for key, val := range args {
		if s, ok := val.(string); ok {
			input[key] = s
			continue
		}
		encoded, err := json.Marshal(val)
		if err != nil {
			return nil, fmt.Errorf("render call argument %q: %w", key, err)
		}
		input[key] = string(encoded)
	}

	if calleeInfo == nil || len(calleeInfo.InputSchema) == 0 {
		return input, nil
	}
	for key := range input {
		if _, declared := calleeInfo.InputSchema[key]; !declared {
			return nil, fmt.Errorf("undeclared input key %q for callee", key)
		}
	}
	var missing []string
	for name, field := range calleeInfo.InputSchema {
		if !field.Required {
			continue
		}
		val, ok := input[name]
		if !ok || strings.TrimSpace(val) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("missing required input key(s): %s", strings.Join(missing, ", "))
	}
	return input, nil
}

// encodeToolCallOutputs serializes the callee's decoded typed outputs as the
// native-JSON outputs_json of a successful tool result (ADR-0004 §5: the
// callee's ExecuteResult flows back to the caller as the tool result).
func encodeToolCallOutputs(outputs map[string]cty.Value) ([]byte, error) {
	if len(outputs) == 0 {
		return []byte("{}"), nil
	}
	vals := make(map[string]cty.Value, len(outputs))
	types := make(map[string]cty.Type, len(outputs))
	for k, v := range outputs {
		vals[k] = v
		types[k] = v.Type()
	}
	return ctyjson.Marshal(cty.ObjectVal(vals), cty.Object(types))
}

// validateToolCallGraph validates the parsed callee against the compiled
// graph (ADR-0004 §7): the callee adapter must be declared and, when it
// presents a static surface, the referenced tool must exist. It returns a
// call_error code or "" when validation passes; a nil graph skips validation.
func (s *permissionInterceptSink) validateToolCallGraph(parsed toolCallTarget, req *toolCallPayload) string {
	if s.graph == nil {
		return ""
	}
	node := s.graph.Adapters[parsed.AdapterRef]
	if node == nil {
		return callErrorUnknownAdapter
	}
	named := parsed.Tool
	if named == "" {
		named = req.tool
	}
	return staticToolErrorCode(node, named)
}

// rejectToolCall answers a tool call with a typed call_error reply: an audit
// entry for the enforcement event and a PermissionEvent.tool_call_result on
// the caller's Permissions stream, keyed by request_id. No permission
// decision is recorded — these gates run before or beside the policy call —
// and the caller's outcome routing is unaffected (ADR-0004 §5: a failed tool
// call is data for the caller, not a run failure).
func (s *permissionInterceptSink) rejectToolCall(requestID, target, argsDigest, code string) {
	s.permState.writeAudit(&DecisionLogEntry{
		SessionID:   s.permState.sessionID,
		RequestID:   requestID,
		Tool:        target,
		ArgsDigest:  argsDigest,
		Decision:    "deny",
		Reason:      "adapter tool call rejected: " + code,
		Layer:       s.nesting.depth,
		EvaluatedAt: time.Now(),
	})
	s.permState.sendToolCallResult(requestID, code)
}
