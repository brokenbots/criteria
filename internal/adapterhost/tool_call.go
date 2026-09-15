package adapterhost

// tool_call.go — CRI-159: the M4.1 host seam for adapter tool calls
// (ADR-0004 §8). A tool call arrives as a permission.request AdapterEvent on
// the Execute stream carrying kind == "adapter_tool" (or a §2 target key).
// The seam detects those payloads, gates them in a fixed order, and answers
// with a typed PermissionEvent.tool_call_result on the caller's Permissions
// stream keyed by request_id:
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
//  6. stub reply: an otherwise-allowed call receives call_error
//     not_yet_supported — real nested execution lands in CRI-160.
//
// Plain (non-tool) permission requests never reach this file's decision path:
// the sink routes them to the untouched plain flow, so workflows without tool
// refs execute through identical code paths.

import (
	"strings"
	"time"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/workflow"
)

// adapterToolsCapability is the well-known AdapterInfo capability that marks
// an adapter as able to issue tool calls (ADR-0004 §9).
const adapterToolsCapability = "adapter_tools"

// Typed call_error codes (ADR-0004 §8 registry; free-form string values on
// ToolCallResult.call_error). malformed_target extends the registry for
// detected tool calls whose target does not parse as the §2 form.
const (
	callErrorCapabilityMissing = "capability_missing"
	callErrorMalformedTarget   = "malformed_target"
	callErrorUnknownAdapter    = "unknown_adapter"
	callErrorUnknownTool       = "unknown_tool"
	callErrorSelfCall          = "self_call"
	callErrorNotYetSupported   = "not_yet_supported"
)

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
// so concurrent plain requests keep their existing semantics.
func (ps *permissionState) evaluateToolCall(requestID, target string, parsed toolCallTarget, argsDigest, fullCmd string, grants []workflow.AdapterToolRef) (allow bool, reason string) {
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

	decision := ps.recordDecision(requestID, target, argsDigest, allow, reason)
	ps.sendEvent(requestID, allow, reason)
	ps.writeAudit(&decision)

	return allow, reason
}

// sendToolCallResult delivers a typed tool-call reply (CRI-152
// PermissionEvent.tool_call_result) on the session Permissions stream,
// non-blocking with drop-on-backlog like sendEvent.
func (ps *permissionState) sendToolCallResult(requestID, callError string) {
	ps.mu.Lock()
	requests := ps.requests
	active := ps.active
	ps.mu.Unlock()

	if !active || requests == nil {
		return
	}

	select {
	case requests <- &v2.PermissionEvent{
		Event: &v2.PermissionEvent_ToolCallResult{
			ToolCallResult: &v2.ToolCallResult{
				RequestId: requestID,
				CallError: callError,
			},
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
	allow, reason := s.permState.evaluateToolCall(req.requestID, req.target, parsed, req.argsDigest, req.fullCmd, grants)
	if !allow {
		s.anyDenied = true
		deniedTool := req.tool
		if deniedTool == "" {
			deniedTool = req.target
		}
		deniedPayload := map[string]any{
			"request_id": req.requestID,
			"tool":       deniedTool,
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
		"tool":       req.target,
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

	// Stub reply for an otherwise-allowed call: the seam is exercised end to
	// end (request in, typed reply out, caller unblocked) before real nested
	// execution lands in CRI-160.
	s.rejectToolCall(req.requestID, req.target, req.argsDigest, callErrorNotYetSupported)
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
		EvaluatedAt: time.Now(),
	})
	s.permState.sendToolCallResult(requestID, code)
}
