package peer

// sessionledger.go — ADR-0014 M6 peer session-control arms: the
// (adapter_type, scope) session ledger (Control OpenSession/CloseSession),
// the host-commanded child respawn (Control RespawnChild), and the declared
// idle-reap window (Control IdleReap). Capability gating and the typed
// unimplemented contract live at Server.Control; like every other arm, the
// journal accepts the SessionLedger payload capability-agnostically, so
// ledger truth survives reconnect renegotiation (see journal.go).

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/brokenbots/criteria/internal/adapterhost"
	criteriav1 "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// maxIdleReapAfterMs is the largest after_ms that converts to a
// non-negative time.Duration nanosecond value; anything larger overflows
// and would masquerade as a withdrawal.
const maxIdleReapAfterMs = math.MaxInt64 / int64(time.Millisecond)

// scopeLedger tracks the session engagements the session-control verbs
// carry: ledger truth per (adapter type, scope, session id) triple, held on
// the server because it is journal-side truth in both roles. The table
// drives re-entry semantics: an open of a tracked session re-adopts it
// (never re-provisions), and a detached session stays tracked so a later
// re-entry re-adopts instead of opening fresh. The verbs hold the mutex
// across journal append and table mutation, so concurrent controls of one
// session land one ledger story (two racing opens of a fresh session
// journal opened-then-adopted, not opened-twice).
//
// The table grows with distinct session ids the host controls — one entry
// per tracked engagement, bounded in practice by the host's own session
// id cardinality. Release (non-detach close) is the only shrink path;
// detachment intentionally holds its entry so a later re-entry re-adopts.
// Eviction of abandoned engagements is deferred until the consumer
// milestone that defines a session lifetime (a cap here would evict
// tracked entries and invent adoption semantics the host never declared).
type scopeLedger struct {
	mu sync.Mutex
	// sessions is the tracked-engagement table.
	sessions map[sessionLedgerKey]struct{}
}

// sessionLedgerKey is one tracked engagement.
type sessionLedgerKey struct {
	adapterType string
	scope       string
	sessionID   string
}

func newScopeLedger() *scopeLedger {
	return &scopeLedger{sessions: map[sessionLedgerKey]struct{}{}}
}

// controlSessionLedger serves Control OpenSession/CloseSession (ADR-0014
// M6): the session-ledger verbs are first-class wire verbs, not Execute
// side-effects — the peer journals the ledger fact (opened/closed/adopted/
// detached) and tracks the engagement so open/adopt/close/detach semantics
// stay peer-attested across scope re-entries. The verbs address a hosted
// child's (adapter_type, scope); an unknown adapter rejects with an honest
// ControlResponse, exactly like kill_child addressing a missing child.
func (s *Server) controlSessionLedger(req *criteriav1.ControlRequest) *criteriav1.ControlResponse {
	if open := req.GetOpenSession(); open != nil {
		return s.openLedgerSession(req.GetAdapterType(), req.GetScope(), open.GetSessionId())
	}
	if closeReq := req.GetCloseSession(); closeReq != nil {
		return s.closeLedgerSession(req.GetAdapterType(), req.GetScope(), closeReq)
	}
	return &criteriav1.ControlResponse{Accepted: false, Detail: "unsupported control action"}
}

// resolveLedgerAdapter resolves the ledger attribution adapter: the hosted
// child the host addresses (definition identity; empty = the primary child
// in the legacy single-adapter shape). The serve-adapter role hosts no
// child process — the served workflow adapter is the only subject its
// journal can honestly attribute.
func (s *Server) resolveLedgerAdapter(adapterType string) (string, *criteriav1.ControlResponse) {
	if s.rt == nil {
		if adapterType == "" {
			adapterType = s.cfg.AdapterName
		}
		if adapterType != s.cfg.AdapterName {
			return "", &criteriav1.ControlResponse{
				Accepted: false,
				Detail:   fmt.Sprintf("serve-adapter hosts only %q", s.cfg.AdapterName),
			}
		}
		return adapterType, nil
	}
	resolved, ok := s.rt.hostedChildName(adapterType)
	if ok {
		return resolved, nil
	}
	if adapterType == "" {
		return "", &criteriav1.ControlResponse{Accepted: false, Detail: "no live adapter child"}
	}
	return "", &criteriav1.ControlResponse{Accepted: false, Detail: fmt.Sprintf("no adapter child %q", adapterType)}
}

// openLedgerSession journals the open fact and tracks the engagement: a
// fresh session journals opened, a re-opened (or detached, then re-entered)
// one journals adopted — re-entry re-adopts, it never re-provisions.
func (s *Server) openLedgerSession(adapterType, scope, sessionID string) *criteriav1.ControlResponse {
	if sessionID == "" {
		return &criteriav1.ControlResponse{Accepted: false, Detail: "open session: session id is required"}
	}
	adapter, reject := s.resolveLedgerAdapter(adapterType)
	if reject != nil {
		return reject
	}
	key := sessionLedgerKey{adapterType: adapter, scope: scope, sessionID: sessionID}
	l := s.ledger
	l.mu.Lock()
	defer l.mu.Unlock()
	verb := SessionLedgerAdopted
	if _, tracked := l.sessions[key]; !tracked {
		verb = SessionLedgerOpened
	}
	ev := &criteriav1.SupervisionEvent_SessionLedger{SessionLedger: &criteriav1.SessionLedger{Event: verb}}
	if _, err := s.journalFor().Append(ev, adapter, scope, sessionID); err != nil {
		s.log.Error("session ledger append", "event", verb, "error", err)
		return &criteriav1.ControlResponse{Accepted: false, Detail: fmt.Sprintf("session %q ledger append: %v", sessionID, err)}
	}
	l.sessions[key] = struct{}{}
	return &criteriav1.ControlResponse{Accepted: true, Detail: fmt.Sprintf("session %q %s", sessionID, verb)}
}

// closeLedgerSession journals the close fact: detach keeps the engagement
// tracked (a later re-entry re-adopts it), release drops it. An untracked
// session is an honest rejection — the ledger journals facts about the
// engagements it holds.
func (s *Server) closeLedgerSession(adapterType, scope string, closeReq *criteriav1.CloseSession) *criteriav1.ControlResponse {
	if closeReq.GetSessionId() == "" {
		return &criteriav1.ControlResponse{Accepted: false, Detail: "close session: session id is required"}
	}
	adapter, reject := s.resolveLedgerAdapter(adapterType)
	if reject != nil {
		return reject
	}
	key := sessionLedgerKey{adapterType: adapter, scope: scope, sessionID: closeReq.GetSessionId()}
	l := s.ledger
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, tracked := l.sessions[key]; !tracked {
		return &criteriav1.ControlResponse{
			Accepted: false,
			Detail:   fmt.Sprintf("no ledger session %q on %q", closeReq.GetSessionId(), adapter),
		}
	}
	verb := SessionLedgerClosed
	if closeReq.GetDetach() {
		// Detach fate: the engagement outlives the closing scope so a
		// sibling or re-entering scope can adopt it; tracking stays.
		verb = SessionLedgerDetached
	}
	ev := &criteriav1.SupervisionEvent_SessionLedger{SessionLedger: &criteriav1.SessionLedger{
		Event:  verb,
		Reason: closeReq.GetReason(),
	}}
	if _, err := s.journalFor().Append(ev, adapter, scope, closeReq.GetSessionId()); err != nil {
		s.log.Error("session ledger append", "event", verb, "error", err)
		return &criteriav1.ControlResponse{Accepted: false, Detail: fmt.Sprintf("session %q ledger append: %v", closeReq.GetSessionId(), err)}
	}
	// Release drops tracking only after the fact is journaled (mirroring
	// the open order of append-then-track), so a failed append leaves the
	// engagement retryable. Detach keeps tracking either way.
	if verb == SessionLedgerClosed {
		delete(l.sessions, key)
	}
	return &criteriav1.ControlResponse{Accepted: true, Detail: fmt.Sprintf("session %q %s", closeReq.GetSessionId(), verb)}
}

// controlRespawnChild executes Control RespawnChild (ADR-0014 M6): the
// host-commanded replacement of the addressed child with a fresh generation
// spawned verbatim from its preserved boot spec (same binary, same digest
// pin, no re-resolution). A dead generation is settled and respawned
// synchronously so the accepted response speaks for a journaled fact; a
// live generation accepts the decree and displaces asynchronously under the
// declared policy grace — the displacement must land even if the host
// stream ends mid-grace, mirroring kill_child. The host-commanded respawn
// overrides the ensure-path crash no-respawn gate: it is the host's
// recovery decision, not the serve path guessing at a restart.
func (r *peerRuntime) controlRespawnChild(ctx context.Context, req *criteriav1.ControlRequest, respawn *criteriav1.RespawnChild) *criteriav1.ControlResponse {
	name := req.GetAdapterType()
	r.mu.Lock()
	if name == "" {
		if p := r.primaryChildLocked(); p != nil {
			name = p.name
		}
	}
	c := r.children[name]
	spec, specOK := r.bootSpec[name]
	shuttingDown, killInFlight := r.shuttingDown, c != nil && c.killRequested
	live := c != nil && c.handle != nil && !adapterhost.ProcessExited(c.handle)
	r.mu.Unlock()

	if c == nil {
		return r.respawnWithoutChild(ctx, req, name, &spec, specOK, shuttingDown)
	}
	if shuttingDown || !specOK {
		return &criteriav1.ControlResponse{Accepted: false, Detail: fmt.Sprintf("no boot spec for %q: respawn impossible", name)}
	}
	if killInFlight {
		return &criteriav1.ControlResponse{Accepted: false, Detail: fmt.Sprintf("host kill in flight on %q: no respawn", name)}
	}
	if live {
		grace := time.Duration(respawn.GetPolicyGraceMs()) * time.Millisecond
		// The displacement is detached from the answering RPC
		// (kill_child precedent); accepted=true speaks for the decree,
		// not the new pid.
		go r.displaceAfterPolicyGrace(ctx, name, &spec, grace)
		return &criteriav1.ControlResponse{Accepted: true, Detail: fmt.Sprintf("respawning %q after %s policy grace", name, grace)}
	}
	if err := r.displaceAndRespawn(ctx, name, &spec); err != nil {
		return &criteriav1.ControlResponse{Accepted: false, Detail: fmt.Sprintf("adapter %q: %v", name, err)}
	}
	return &criteriav1.ControlResponse{Accepted: true, Detail: fmt.Sprintf("respawned %q", name)}
}

// respawnWithoutChild answers a respawn addressing a child the runtime has
// no live record for. The first branch precedes the generic rejection: a
// hosted definition whose first spawn never landed (or whose failed verify
// deregistered it) still has a manifest addressing a child — booting it is
// the respawn. Anything else (no boot spec, or the peer is shutting down)
// is the honest missing-child rejection.
func (r *peerRuntime) respawnWithoutChild(ctx context.Context, req *criteriav1.ControlRequest, name string, spec *childSpec, specOK, shuttingDown bool) *criteriav1.ControlResponse {
	if specOK && !shuttingDown {
		if r.displaceAndRespawn(ctx, name, spec) == nil {
			return &criteriav1.ControlResponse{Accepted: true, Detail: fmt.Sprintf("respawned %q", name)}
		}
		return &criteriav1.ControlResponse{Accepted: false, Detail: fmt.Sprintf("adapter %q spawn failed (see peer log)", name)}
	}
	if req.GetAdapterType() == "" {
		return &criteriav1.ControlResponse{Accepted: false, Detail: "no live adapter child"}
	}
	return &criteriav1.ControlResponse{Accepted: false, Detail: fmt.Sprintf("no adapter child %q", name)}
}

// displaceAfterPolicyGrace runs the live-generation displacement after the
// declared policy grace. The wait is detached from the answering RPC
// (kill_child precedent): the displacement must land even if the host
// stream ends mid-grace — the WithoutCancel context keeps it running past
// the answering call. Peer shutdown stops the displacement prompt:
// Shutdown kills the children itself and respawns nothing.
func (r *peerRuntime) displaceAfterPolicyGrace(ctx context.Context, name string, spec *childSpec, grace time.Duration) {
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-r.stopWatch:
		return
	}
	if err := r.displaceAndRespawn(context.WithoutCancel(ctx), name, spec); err != nil {
		r.log.Error("peer child displacement failed", "adapter", name, "error", err)
	}
}

// displaceAndRespawn replaces the addressed generation with a fresh spawn
// from the boot spec, serialized per name through respawnMu (the same gate
// the ensure path respawns through). Under that gate it: refuses while
// shutting down or while a Control kill_child holds the current generation
// (the kill decree outranks the respawn decree); kills a still-live
// generation peer-initiated (the watcher records the graceful exit);
// settles an unrecorded natural death's facts into the journal first — a
// successor's Spawned marker must never interleave ahead of the stale
// generation's Exited/CrashClassified facts; and spawns synchronously from
// the boot spec. Returns nil once the fresh generation is live.
func (r *peerRuntime) displaceAndRespawn(ctx context.Context, name string, spec *childSpec) error {
	if m, _ := r.respawnMu.LoadOrStore(name, &sync.Mutex{}); m != nil {
		m.(*sync.Mutex).Lock()
		defer m.(*sync.Mutex).Unlock()
	}
	r.mu.Lock()
	c := r.children[name]
	if r.shuttingDown {
		r.mu.Unlock()
		return fmt.Errorf("peer is shutting down")
	}
	if c != nil && c.killRequested {
		r.mu.Unlock()
		return fmt.Errorf("host kill in flight; the child stays dead")
	}
	var handle adapterhost.Handle
	live := c != nil && c.handle != nil && !adapterhost.ProcessExited(c.handle)
	if live {
		// The flag writes and the handle capture ride the runtime lock —
		// the exit watcher reads peerInitiated under the same lock when it
		// records the displacement's exit fact (graceful, no crash fact).
		c.peerInitiated = true
		c.killRequested = true
		handle = c.handle
	}
	r.mu.Unlock()
	if handle != nil {
		handle.Kill()
	}
	if c != nil {
		if err := r.settleChildExit(c, name); err != nil {
			return err
		}
	}
	if _, ok := r.spawnAndWatch(ctx, spec); !ok {
		return fmt.Errorf("fresh child spawn failed")
	}
	return nil
}

// settleChildExit guarantees the journal's generation boundary before a
// successor spawn: a dead child whose exit facts the watcher has not yet
// recorded has them recorded here (recordExit classifies exactly as the
// watcher would — a natural death keeps its crash facts), and the wait
// returns only once those facts are actually journaled. Nothing is settled
// for a record without a process.
func (r *peerRuntime) settleChildExit(c *childState, name string) error {
	r.mu.Lock()
	unrecorded := !c.exited && c.handle != nil && adapterhost.ProcessExited(c.handle)
	r.mu.Unlock()
	if unrecorded {
		r.recordExit(c, false)
	}
	return r.waitExitFactsSettled(c, name)
}

// waitExitFactsSettled blocks until the child's exit facts are journaled
// (the settle signal the exit watcher sets after its journal appends), the
// record holds no process to journal facts about, or the bounded wait
// expires — a not-yet-settled death makes the displacement fail so the
// caller can retry with the stale generation's facts already journaled.
func (r *peerRuntime) waitExitFactsSettled(c *childState, name string) error {
	deadline := time.Now().Add(r.shutdownGrace)
	for {
		r.mu.Lock()
		settled := c.handle == nil || c.exitFactsJournaled
		r.mu.Unlock()
		if settled {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("adapter %q: child exit facts were not journaled within %s", name, r.shutdownGrace)
		}
		time.Sleep(r.exitPoll)
	}
}

// controlIdleReap executes Control IdleReap (ADR-0014 M6): the host
// declares, per addressed child, the idle window the peer-side reap policy
// applies. The declaration is recorded policy — no journal fact (the
// journal records lifecycle truth, not declarations) — and the response
// detail carries the effective window verbatim, so re-arming sticks. A
// zero window withdraws the declared policy.
func (r *peerRuntime) controlIdleReap(req *criteriav1.ControlRequest, idle *criteriav1.IdleReap) *criteriav1.ControlResponse {
	name := req.GetAdapterType()
	resolved, ok := r.hostedChildName(name)
	if !ok {
		if name == "" {
			return &criteriav1.ControlResponse{Accepted: false, Detail: "no live adapter child"}
		}
		return &criteriav1.ControlResponse{Accepted: false, Detail: fmt.Sprintf("no adapter child %q", name)}
	}
	// after_ms above the representable duration range would wrap into a
	// negative window and masquerade as a withdrawal — reject on declare
	// instead of silently treating absurd values as policy.
	if afterMs := idle.GetAfterMs(); afterMs > uint64(maxIdleReapAfterMs) {
		return &criteriav1.ControlResponse{
			Accepted: false,
			Detail:   fmt.Sprintf("idle reap after_ms %d exceeds %d", afterMs, uint64(maxIdleReapAfterMs)),
		}
	}
	window := time.Duration(idle.GetAfterMs()) * time.Millisecond
	r.mu.Lock()
	if window > 0 {
		r.idleReapPolicy[resolved] = window
	} else {
		delete(r.idleReapPolicy, resolved)
	}
	r.mu.Unlock()
	if window > 0 {
		return &criteriav1.ControlResponse{Accepted: true, Detail: fmt.Sprintf("idle reap policy for %q declared at %s", resolved, window)}
	}
	return &criteriav1.ControlResponse{Accepted: true, Detail: fmt.Sprintf("idle reap policy for %q withdrawn", resolved)}
}

// idleReapWindow reports the declared idle-reap window for one child
// (ok=false when the child carries no declared policy).
func (r *peerRuntime) idleReapWindow(name string) (time.Duration, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	window, ok := r.idleReapPolicy[name]
	return window, ok
}
