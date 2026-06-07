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
	SessionID   string    `json:"session_id"`
	RequestID   string    `json:"request_id"`
	Tool        string    `json:"tool"`
	ArgsDigest  string    `json:"args_digest"`
	Decision    string    `json:"decision"` // "allow" | "deny" | "cancelled"
	Reason      string    `json:"reason"`
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
}

type requestState struct {
	requestID  string
	tool       string
	argsDigest string
	receivedAt time.Time
	decision   string // "allow" | "deny" | "cancelled"
	reason     string
	decidedAt  time.Time
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

	decision := ps.recordDecision(requestID, tool, argsDigest, allow, reason)
	ps.sendEvent(requestID, allow, reason)
	ps.writeAudit(&decision)

	return allow, reason
}

// recordDecision updates the inflight map and decisions window under mu.
func (ps *permissionState) recordDecision(requestID, tool, argsDigest string, allow bool, reason string) DecisionLogEntry {
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
}

// Pause cancels the stream goroutine's context. The stream is held open at the
// adapter side; no new decisions are dispatched.
func (ps *permissionState) Pause() {
	ps.mu.Lock()
	cancel := ps.cancel
	ps.active = false
	ps.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Resume restarts the consumer goroutine by resetting the active flag.
// The caller (SessionManager) is responsible for spawning a new Permissions
// stream via StartPermissionStream.
func (ps *permissionState) Resume() {
	ps.mu.Lock()
	ps.active = true
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

	// Build a lookup of previously-answered requests.
	answered := make(map[string]DecisionLogEntry, len(snap.Decisions))
	for _, d := range snap.Decisions {
		answered[d.RequestID] = d
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

// permissionInterceptSink wraps an adapter.EventSink and intercepts
// permission.request events. It delegates evaluation to the session's
// PermissionState, emits permission.granted / permission.denied events, and
// tracks whether any request was denied so Execute can override the outcome.
type permissionInterceptSink struct {
	inner     adapter.EventSink
	permState *permissionState
	session   *Session
	anyDenied bool
}

func (s *permissionInterceptSink) Log(stream string, chunk []byte) {
	s.inner.Log(stream, chunk)
}

func (s *permissionInterceptSink) Adapter(kind string, data any) {
	if kind == "permission.request" && s.permState != nil {
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
	requestID, _ := payload["request_id"].(string)
	tool, _ := payload["tool"].(string)
	fullCmd, _ := payload["full_command_text"].(string)

	allow, reason := s.permState.Evaluate(requestID, tool, "", fullCmd)
	if allow {
		// Strip "matched: " prefix to get the raw pattern for the payload.
		pattern := strings.TrimPrefix(reason, "matched: ")
		if idx := strings.Index(pattern, " (alias for "); idx >= 0 {
			pattern = pattern[:idx]
		}
		s.inner.Adapter("permission.granted", map[string]any{
			"request_id": requestID,
			"tool":       tool,
			"pattern":    pattern,
		})
	} else {
		s.anyDenied = true
		suggestion := PermissionDenialSuggestion(s.session.Adapter, tool)
		deniedPayload := map[string]any{
			"request_id": requestID,
			"tool":       tool,
			"reason":     reason,
		}
		if suggestion != "" {
			deniedPayload["suggestion"] = suggestion
		}
		s.inner.Adapter("permission.denied", deniedPayload)
	}
}
