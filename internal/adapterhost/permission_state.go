package adapterhost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/workflow"
)

// PermissionStreamer is implemented by handles that support a dedicated
// per-session Permissions bidi stream (v2 adapters). The host starts this
// stream once at session open and cancels it at session close.
type PermissionStreamer interface {
	StartPermissionStream(ctx context.Context, sessionID string, requests <-chan *v2.PermissionEvent) (cancel func(), err error)
}

// AuditWriter receives structured decision log entries.
type AuditWriter interface {
	Write(entry *DecisionLogEntry)
}

// DecisionLogEntry is a single permission decision written to the audit log.
type DecisionLogEntry struct {
	SessionID  string `json:"session_id"`
	RequestID  string `json:"request_id"`
	Tool       string `json:"tool"`
	ArgsDigest string `json:"args_digest"`
	Decision   string `json:"decision"` // "allow" | "deny" | "cancelled"
	Reason     string `json:"reason"`
	// Layer is the nested tool-call layer the decision was made in (CRI-163):
	// 0 for a step-level caller's decisions, 1 for the first nested callee's
	// own decisions, and so on. Together with SessionID it lets audit readers
	// tell the caller's allow/deny decision apart from the callee-side
	// decisions evaluated under the callee's own allow_tools.
	Layer       int       `json:"layer"`
	EvaluatedAt time.Time `json:"evaluated_at"`
}

// permissionState tracks inflight permission requests, recent decisions, and
// drives the session-scoped Permissions bidi stream.
//
// It is created per-session in registerSession and stopped in Close/Shutdown.
type permissionState struct {
	mu        sync.Mutex
	sessionID string
	inflight  map[string]*requestState
	decisions []DecisionLogEntry // recent window for snapshot replay
	policy    PermissionPolicy
	audit     AuditWriter

	// Stream state
	requests chan *v2.PermissionEvent
	cancel   func()
	active   bool

	// pendingToolCalls tracks in-flight adapter tool calls by request_id for
	// reply correlation (CRI-161). Multiple calls per caller session are
	// in flight concurrently, so this is a map, not a queue: replies may
	// arrive in any order (the Permissions stream contract explicitly
	// allows it) and each completing goroutine correlates by request_id.
	pendingToolCalls map[string]*pendingToolCall

	// toolCallPauseGate is the CRI-169 pause gate: when set, nested
	// adapter tool-call dispatch is refused with the typed `paused`
	// call_error until Resume clears it. It is set by beginToolCallPause
	// (Session.Pause drain phase) and atomically by Pause.
	toolCallPauseGate bool
	// pauseDrainWindow is the bounded wait a Session.Pause drain gives
	// in-flight nested tool calls before canceling stragglers. Zero means
	// defaultToolCallPauseWindow.
	pauseDrainWindow time.Duration
}

// defaultToolCallPauseWindow is the drain-first pause window (CRI-169):
// Session.Pause waits this long for in-flight nested tool calls to settle
// before canceling them.
const defaultToolCallPauseWindow = 60 * time.Second

type requestState struct {
	requestID  string
	tool       string
	argsDigest string
	receivedAt time.Time
	decision   string // "allow" | "deny" | "cancelled"
	reason     string
	decidedAt  time.Time
}

// pendingToolCall is one in-flight adapter tool call registered for reply
// correlation (CRI-161), keyed by request_id — the host-side mirror of the
// MCP bridge's registerPendingPerm/decisionCh pending map
// (cmd/criteria-adapter-mcp/bridge.go). The bridge's per-request channel
// exists because the awaiting adapter goroutine blocks on it; on the host the
// completing goroutine delivers the typed reply to the caller's Permissions
// stream directly, so the entry is a completion marker: it is cleared when
// the call settles and drained with an abandonment audit at session close.
type pendingToolCall struct {
	requestID  string
	target     string
	registered time.Time
	// cancel aborts the call's derived context (CRI-169): the pause drain
	// cancels non-draining calls so they settle with a typed `canceled`
	// reply. Nil for legacy registrations without a derived context.
	cancel func()
	// settled is closed when the call's goroutine cleared its pending
	// registration (i.e. the call settled). It lets the pause drain wait
	// for each in-flight call without waiting on per-Execute machinery.
	// Nil when the call was registered without a settle marker.
	settled chan struct{}
	// settledClosed guards the one-shot close of settled under ps.mu.
	settledClosed bool
}

// snapshotV1 is the on-disk format for MarshalState / RestoreState.
type snapshotV1 struct {
	Version   int                `json:"version"`
	Inflight  []snapshotRequest  `json:"inflight"`
	Decisions []DecisionLogEntry `json:"decisions"`
}

type snapshotRequest struct {
	RequestID  string    `json:"request_id"`
	Tool       string    `json:"tool"`
	ArgsDigest string    `json:"args_digest"`
	ReceivedAt time.Time `json:"received_at"`
}

// NewPermissionState creates a new permission state tracker for the given session.
func NewPermissionState(sessionID string, audit AuditWriter) *permissionState {
	return &permissionState{
		sessionID: sessionID,
		audit:     audit,
		requests:  make(chan *v2.PermissionEvent, 16),
	}
}

// Requests returns the send-side channel for the Permissions bidi stream.
func (ps *permissionState) Requests() <-chan *v2.PermissionEvent {
	return ps.requests
}

// SetStreamCancel records the cancel function returned by StartPermissionStream.
func (ps *permissionState) SetStreamCancel(cancel func()) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.cancel = cancel
	ps.active = true
}

// SetPolicy updates the policy evaluator used for subsequent Evaluate calls.
func (ps *permissionState) SetPolicy(policy PermissionPolicy) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.policy = policy
}

// Evaluate evaluates a permission request against the current policy, sends the
// corresponding PermissionEvent on the session stream, writes an audit entry,
// and returns the decision.
func (ps *permissionState) Evaluate(requestID, tool, argsDigest, fullCmd string) (allow bool, reason string) {
	return ps.evaluateAtLayer(requestID, tool, argsDigest, fullCmd, 0)
}

// evaluateAtLayer is Evaluate with the nested tool-call layer recorded in the
// audit entry (CRI-163): layer 0 is a step-level caller's decision, deeper
// layers are the callee-side decisions evaluated inside a nested Execute
// under the callee's own allow_tools.
func (ps *permissionState) evaluateAtLayer(requestID, tool, argsDigest, fullCmd string, layer int) (allow bool, reason string) {
	ps.mu.Lock()
	policy := ps.policy
	ps.mu.Unlock()

	req := PermissionRequest{ID: requestID, Tool: tool}
	if fullCmd != "" {
		req.Details = map[string]string{"full_command_text": fullCmd}
	}

	if policy == nil {
		policy = denyAllPolicy{}
	}
	allow, reason = policy.Decide(req)

	decision := ps.recordDecision(requestID, tool, argsDigest, allow, reason, layer)
	ps.sendEvent(requestID, allow, reason)
	ps.writeAudit(&decision)

	return allow, reason
}

// recordDecision updates the inflight map and decisions window under mu.
func (ps *permissionState) recordDecision(requestID, tool, argsDigest string, allow bool, reason string, layer int) DecisionLogEntry {
	now := time.Now()
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.inflight == nil {
		ps.inflight = make(map[string]*requestState)
	}
	rs := &requestState{
		requestID:  requestID,
		tool:       tool,
		argsDigest: argsDigest,
		receivedAt: now,
		decision:   "deny",
		reason:     reason,
		decidedAt:  now,
	}
	if allow {
		rs.decision = "allow"
	}
	ps.inflight[requestID] = rs
	entry := DecisionLogEntry{
		SessionID:   ps.sessionID,
		RequestID:   requestID,
		Tool:        tool,
		ArgsDigest:  argsDigest,
		Decision:    rs.decision,
		Reason:      reason,
		Layer:       layer,
		EvaluatedAt: now,
	}
	ps.decisions = append(ps.decisions, entry)
	const maxDecisions = 1000
	if len(ps.decisions) > maxDecisions {
		ps.decisions = ps.decisions[len(ps.decisions)-maxDecisions:]
	}
	return entry
}

// writeAudit writes a decision entry to the audit writer if configured.
func (ps *permissionState) writeAudit(entry *DecisionLogEntry) {
	if ps.audit == nil {
		return
	}
	ps.audit.Write(entry)
}

// sendEvent dispatches a PermissionEvent to the adapter stream without blocking
// the caller. If the stream is not active the event is silently dropped.
func (ps *permissionState) sendEvent(requestID string, allow bool, reason string) {
	ps.mu.Lock()
	requests := ps.requests
	active := ps.active
	ps.mu.Unlock()

	if !active || requests == nil {
		return
	}

	var ev *v2.PermissionEvent
	if allow {
		ev = &v2.PermissionEvent{
			Event: &v2.PermissionEvent_Request{
				Request: &v2.PermissionRequest{RequestId: requestID},
			},
		}
	} else {
		ev = &v2.PermissionEvent{
			Event: &v2.PermissionEvent_Cancel{
				Cancel: &v2.PermissionCancel{RequestId: requestID, Reason: reason},
			},
		}
	}

	select {
	case requests <- ev:
	default:
		// Stream consumer is backlogged; don't block the Execute goroutine.
	}
}

// registerPendingToolCall records an in-flight adapter tool call keyed by
// request_id. First registration wins: a caller that reuses an in-flight
// request_id violates the correlation contract (unique request ids per call),
// so the duplicate is a no-op — each issued call still gets its own typed
// reply, but the registry keeps one entry per request_id for teardown
// accounting.
//
// CRI-169: the second half of the pause gate lives here. A dispatch that
// passed the gate in handleToolCallRequest but registers after the pause gate
// was set is refused (returns false) — the pending set only ever shrinks once
// the gate is set, so the Session.Pause drain takes a single snapshot. cancel
// is the call's derived-context cancel used by the pause drain; nil keeps a
// legacy registration (no cancelation, no settle marker).
func (ps *permissionState) registerPendingToolCall(requestID, target string, cancel func()) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.pendingToolCalls == nil {
		ps.pendingToolCalls = make(map[string]*pendingToolCall)
	}
	if _, exists := ps.pendingToolCalls[requestID]; exists {
		return true
	}
	if ps.toolCallPauseGate {
		return false
	}
	entry := &pendingToolCall{
		requestID:  requestID,
		target:     target,
		registered: time.Now(),
	}
	if cancel != nil {
		entry.cancel = cancel
		entry.settled = make(chan struct{})
	}
	ps.pendingToolCalls[requestID] = entry
	return true
}

// clearPendingToolCall removes the registration of a settled tool call so
// session teardown does not audit a delivered result as abandoned. The call's
// settled marker (when present) is closed exactly once so the pause drain can
// observe the settle.
func (ps *permissionState) clearPendingToolCall(requestID string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	call, ok := ps.pendingToolCalls[requestID]
	if !ok {
		return
	}
	if call.settled != nil && !call.settledClosed {
		close(call.settled)
		call.settledClosed = true
	}
	delete(ps.pendingToolCalls, requestID)
}

// drainPendingToolCalls removes and returns every still-pending tool-call
// registration. Called at session close: each returned entry is a call whose
// nested execute never settled before the session went away.
func (ps *permissionState) drainPendingToolCalls() []*pendingToolCall {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	drained := make([]*pendingToolCall, 0, len(ps.pendingToolCalls))
	for _, call := range ps.pendingToolCalls {
		drained = append(drained, call)
	}
	ps.pendingToolCalls = nil
	return drained
}

// Stop closes the request channel and cancels the stream goroutine.
// Pending requests are audit-logged.
func (ps *permissionState) Stop() {
	ps.mu.Lock()
	cancel := ps.cancel
	_ = ps.active
	ps.active = false
	requests := ps.requests
	ps.requests = nil
	inflight := make(map[string]*requestState, len(ps.inflight))
	for k, v := range ps.inflight {
		inflight[k] = v
	}
	ps.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if requests != nil {
		close(requests)
	}

	if len(inflight) > 0 && ps.audit != nil {
		ps.audit.Write(&DecisionLogEntry{
			SessionID:   ps.sessionID,
			RequestID:   "session-close",
			Decision:    "session_closed_with_pending",
			Reason:      fmt.Sprintf("pending: %d", len(inflight)),
			EvaluatedAt: time.Now(),
		})
	}

	// Tool calls still in flight at close (CRI-161): audit each abandonment.
	// No reply is sent — the caller's stream goes away with the session.
	for _, call := range ps.drainPendingToolCalls() {
		if ps.audit != nil {
			ps.audit.Write(&DecisionLogEntry{
				SessionID:   ps.sessionID,
				RequestID:   call.requestID,
				Tool:        call.target,
				Decision:    "cancelled",
				Reason:      "tool call abandoned: session closed while in flight",
				EvaluatedAt: time.Now(),
			})
		}
	}
}

// SetPauseDrainWindow stamps the pause drain window (CRI-169). Called at
// permissionState creation sites from the SessionManager's configured window.
func (ps *permissionState) SetPauseDrainWindow(window time.Duration) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.pauseDrainWindow = window
}

// toolCallDrainWindow returns the effective drain window for Session.Pause.
func (ps *permissionState) toolCallDrainWindow() time.Duration {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.pauseDrainWindow <= 0 {
		return defaultToolCallPauseWindow
	}
	return ps.pauseDrainWindow
}

// drainSettleGrace is the bounded settle grace the pause drain grants each
// canceled straggler's goroutine to deliver its typed reply after its pending
// registration is cleared (clearPendingToolCall closes the settle marker
// before the reply is sent). Clamped to [100ms, 2s], derived from the window
// so a short conformance window keeps a proportionally short grace.
func drainSettleGrace(window time.Duration) time.Duration {
	grace := window / 10
	if grace < 100*time.Millisecond {
		grace = 100 * time.Millisecond
	}
	if grace > 2*time.Second {
		grace = 2 * time.Second
	}
	return grace
}

// beginToolCallPause sets the CRI-169 pause gate: new nested tool-call
// dispatch is refused with the typed `paused` call_error (via
// handleToolCallRequest gate and the registerPendingToolCall re-check) until
// resumeToolCalls clears it. The stream consumer flag is untouched here —
// Session.Pause keeps it live so drain-phase replies (typed `canceled` for
// stragglers) are delivered immediately; PermissionState.Pause stops the
// stream afterwards.
func (ps *permissionState) beginToolCallPause() {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.toolCallPauseGate = true
}

// resumeToolCalls clears the pause gate without touching the stream flag.
func (ps *permissionState) resumeToolCalls() {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.toolCallPauseGate = false
}

// toolCallsPaused reports whether the pause gate is set.
func (ps *permissionState) toolCallsPaused() bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.toolCallPauseGate
}

// snapshotPendingToolCalls returns the currently pending tool-call entries.
// Once the pause gate is set the set only shrinks, so a Session.Pause drain
// snapshots it once.
func (ps *permissionState) snapshotPendingToolCalls() []*pendingToolCall {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	snapshot := make([]*pendingToolCall, 0, len(ps.pendingToolCalls))
	for _, call := range ps.pendingToolCalls {
		snapshot = append(snapshot, call)
	}
	return snapshot
}

// awaitToolCallDrain is the Session.Pause drain (CRI-169 drain-first
// posture): it waits up to window for every pending nested tool call to
// settle (bounded wait), cancels the stragglers, then grants them a bounded
// settle grace to deliver their typed replies. It returns the calls still
// pending after the grace — the wedged stragglers the pause abandons (the
// session-close abandonment audit reports them later; Stop must keep that
// contract).
func (ps *permissionState) awaitToolCallDrain(window time.Duration) []*pendingToolCall {
	deadline := time.Now().Add(window)
	grace := drainSettleGrace(window)

	// Wait for the in-flight calls to settle within the bounded window.
	// The set only shrinks while the pause gate is set, so the snapshot
	// taken at entry is complete.
	for _, call := range ps.snapshotPendingToolCalls() {
		if call.settled == nil {
			continue
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		select {
		case <-call.settled:
		case <-time.After(remaining):
		}
	}

	// Cancel whatever did not drain in the window: the call's goroutine
	// observes a canceled context, maps it to the typed `canceled`
	// call_error, and clears its registration.
	pending := ps.snapshotPendingToolCalls()
	stragglers := make([]*pendingToolCall, 0, len(pending))
	for _, call := range pending {
		if call.cancel != nil {
			call.cancel()
		}
		stragglers = append(stragglers, call)
	}
	if len(stragglers) == 0 {
		return nil
	}

	// Bounded settle grace: the canceled goroutine still has to run its
	// failure-report path (clear registration, deliver the typed reply).
	graceDeadline := time.Now().Add(grace)
	for _, call := range stragglers {
		if call.settled == nil {
			continue
		}
		remaining := time.Until(graceDeadline)
		if remaining <= 0 {
			break
		}
		select {
		case <-call.settled:
		case <-time.After(remaining):
		}
	}

	return ps.snapshotPendingToolCalls()
}

// Pause cancels the stream goroutine's context. The stream is held open at the
// adapter side; no new decisions are dispatched. The CRI-169 pause gate is
// set atomically on the same mutex, so no nested tool call can be registered
// between the gate and the stream flag.
func (ps *permissionState) Pause() {
	ps.mu.Lock()
	ps.toolCallPauseGate = true
	cancel := ps.cancel
	ps.active = false
	ps.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Resume restarts the consumer goroutine by resetting the active flag and
// clearing the pause gate atomically. The caller (SessionManager) is
// responsible for spawning a new Permissions stream via StartPermissionStream.
func (ps *permissionState) Resume() {
	ps.mu.Lock()
	ps.active = true
	ps.toolCallPauseGate = false
	ps.mu.Unlock()
}

// MarshalState serialises the inflight queue and a window of recent decisions
// into a JSON blob suitable for embedding in the Snapshot output.
func (ps *permissionState) MarshalState() ([]byte, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	snap := snapshotV1{
		Version:   1,
		Decisions: append([]DecisionLogEntry(nil), ps.decisions...),
	}
	for _, rs := range ps.inflight {
		snap.Inflight = append(snap.Inflight, snapshotRequest{
			RequestID:  rs.requestID,
			Tool:       rs.tool,
			ArgsDigest: rs.argsDigest,
			ReceivedAt: rs.receivedAt,
		})
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	if err := enc.Encode(snap); err != nil {
		return nil, fmt.Errorf("marshal permission state: %w", err)
	}
	return buf.Bytes(), nil
}

// RestoreState rehydrates from a blob; previously-answered requests are
// re-answered from the decision log; unanswered are re-presented to policy.
func (ps *permissionState) RestoreState(data []byte, policy PermissionPolicy, audit AuditWriter) error {
	var snap snapshotV1
	if err := json.Unmarshal(data, &snap); err != nil {
		return fmt.Errorf("unmarshal permission state: %w", err)
	}
	if snap.Version != 1 {
		return fmt.Errorf("unsupported permission state version %d", snap.Version)
	}

	ps.mu.Lock()
	ps.policy = policy
	ps.audit = audit
	ps.inflight = make(map[string]*requestState)
	ps.decisions = append([]DecisionLogEntry(nil), snap.Decisions...)
	ps.pendingToolCalls = nil

	// Build a lookup of previously-answered requests.
	answered := make(map[string]DecisionLogEntry, len(snap.Decisions))
	for i := range snap.Decisions {
		answered[snap.Decisions[i].RequestID] = snap.Decisions[i]
	}
	ps.mu.Unlock()

	for _, req := range snap.Inflight {
		if dec, ok := answered[req.RequestID]; ok {
			// Previously answered — replay deterministically.
			ps.sendEvent(req.RequestID, dec.Decision == "allow", dec.Reason)
			if audit != nil {
				audit.Write(&DecisionLogEntry{
					SessionID:   ps.sessionID,
					RequestID:   req.RequestID,
					Tool:        req.Tool,
					ArgsDigest:  req.ArgsDigest,
					Decision:    dec.Decision,
					Reason:      "restored: " + dec.Reason,
					EvaluatedAt: time.Now(),
				})
			}
		} else {
			// Unanswered — re-present to policy.
			ps.Evaluate(req.RequestID, req.Tool, req.ArgsDigest, "")
		}
	}

	return nil
}

// resolvePermissionRequestID extracts the permission request ID from an
// adapter payload, preferring the snake_case "request_id" key and falling back
// to camelCase "requestId" for SDKs that emit that shape. It returns the
// resolved ID and true when one of the keys is present and non-empty.
func resolvePermissionRequestID(payload map[string]any) (string, bool) {
	if id, ok := payload["request_id"].(string); ok && id != "" {
		return id, true
	}
	if id, ok := payload["requestId"].(string); ok && id != "" {
		return id, true
	}
	return "", false
}

// permissionInterceptSink wraps an adapter.EventSink and intercepts
// permission.request events. It delegates evaluation to the session's
// PermissionState, emits permission.granted / permission.denied events, and
// tracks whether any request was denied so Execute can override the outcome.
// Adapter tool-call requests (CRI-159) are routed to the typed reply path in
// tool_call.go; plain requests keep the untouched plain flow.
type permissionInterceptSink struct {
	inner     adapter.EventSink
	permState *permissionState
	session   *Session
	// step is the step being executed; its recorded tools grants (CRI-157)
	// are unioned into the effective allow set for tool-call policy. Nil for
	// plain permission-only use (then no grant matching applies).
	step *workflow.StepNode
	// graph is the compiled workflow graph used for tool-call validation
	// (callee declared, static tool surface). Nil skips graph validation.
	graph *workflow.FSMGraph
	// mgr is the owning SessionManager; nested adapter tool calls (CRI-160)
	// resolve and execute the callee through it. Nil means nested execution
	// is not wired (directly constructed sinks), and allowed calls fall back
	// to the typed not_yet_supported reply.
	mgr *SessionManager
	// nesting is the per-call tool-call nesting state (CRI-162): the depth of
	// nested tool-call Executes above the Execute this sink serves (0 for a
	// step-level Execute) plus the caller adapter ref chain, checked for
	// runtime cycles before dispatching a nested call and compared against
	// the graph's policy.max_tool_depth.
	nesting toolCallNesting
	// execCtx is the Execute context the sink serves. A nested callee Execute
	// runs under it so run cancellation (timeout, user abort) reaches the
	// callee too.
	execCtx context.Context
	// nested counts in-flight nested tool-call executes (CRI-161). They run
	// on their own goroutines so the caller's Execute event loop keeps
	// reading its Permissions stream while calls are in flight (replies may
	// arrive in any order). SessionManager.execute waits on it right after
	// the adapter call returns, before reading the sink's latches.
	nested sync.WaitGroup
	// mu guards the nested-execution latches below: dispatch is asynchronous
	// (CRI-161), so completing goroutines can set them while SessionManager
	// reads.
	mu sync.Mutex
	// fatalErr latches a FatalRunError raised by a nested callee Execute
	// (callee on_crash=abort_run, CRI-160). SessionManager.execute reads it
	// after the adapter call returns and propagates the error so the run
	// aborts.
	nestedFatalErr error
	// anyDenied is set and read on the Execute goroutine only (the sink is
	// not shared); nested completers touch only setNestedFatalErr and the
	// permission state.
	anyDenied bool
}

// setNestedFatalErr records a fatal run error raised by a nested callee
// Execute so SessionManager.execute can propagate it.
func (s *permissionInterceptSink) setNestedFatalErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nestedFatalErr = err
}

// nestedFatal returns the latched fatal run error, if any.
func (s *permissionInterceptSink) nestedFatal() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nestedFatalErr
}

// waitPending blocks until every nested tool-call dispatched by this sink has
// settled and delivered its reply (CRI-161). Called by SessionManager.execute
// immediately after the adapter call returns so the run observes every
// nested outcome before the step completes.
func (s *permissionInterceptSink) waitPending() {
	s.nested.Wait()
}

func (s *permissionInterceptSink) Log(stream string, chunk []byte) {
	s.inner.Log(stream, chunk)
}

// redactEventValue masks registered sensitive values out of an adapter-provided
// payload field before it is re-emitted as an event (CRI-163). Decision
// payloads echo adapter-supplied strings such as tool arguments, which may
// carry values the redaction registry has registered for this run.
func (s *permissionInterceptSink) redactEventValue(v string) string {
	if s.mgr == nil || s.mgr.RedactionRegistry == nil {
		return v
	}
	return s.mgr.RedactionRegistry.Redact(v)
}

func (s *permissionInterceptSink) Adapter(kind string, data any) {
	if kind == "permission.request" && s.permState != nil {
		if payload, ok := data.(map[string]any); ok && toolCallRequestDetected(payload) {
			s.handleToolCallRequest(payload)
			return
		}
		s.handlePermissionRequest(data)
		return
	}
	s.inner.Adapter(kind, data)
}

func (s *permissionInterceptSink) handlePermissionRequest(data any) {
	payload, ok := data.(map[string]any)
	if !ok {
		// Malformed payload — treat as deny.
		s.anyDenied = true
		s.inner.Adapter("permission.denied", map[string]any{
			"reason": "malformed permission.request payload",
		})
		return
	}
	requestID, ok := resolvePermissionRequestID(payload)
	if !ok {
		s.anyDenied = true
		s.inner.Adapter("permission.denied", map[string]any{
			"reason": "malformed permission.request payload: missing request_id",
		})
		return
	}
	tool, _ := payload["tool"].(string)
	fullCmd, _ := payload["full_command_text"].(string)

	// The layer is the caller Execute this sink serves: a step-level caller
	// records layer 0, a nested callee records its own deeper layer, so the
	// audit log distinguishes the two decision surfaces.
	allow, reason := s.permState.evaluateAtLayer(requestID, tool, "", fullCmd, s.nesting.depth)
	if allow {
		// Strip "matched: " prefix to get the raw pattern for the payload.
		pattern := strings.TrimPrefix(reason, "matched: ")
		if idx := strings.Index(pattern, " (alias for "); idx >= 0 {
			pattern = pattern[:idx]
		}
		s.inner.Adapter("permission.granted", map[string]any{
			"request_id": requestID,
			"tool":       s.redactEventValue(tool),
			"pattern":    pattern,
		})
	} else {
		s.anyDenied = true
		suggestion := PermissionDenialSuggestion(s.session.Adapter, tool)
		deniedPayload := map[string]any{
			"request_id": requestID,
			"tool":       s.redactEventValue(tool),
			"reason":     reason,
		}
		if suggestion != "" {
			deniedPayload["suggestion"] = suggestion
		}
		s.inner.Adapter("permission.denied", deniedPayload)
	}
}
