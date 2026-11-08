package remote

import (
	"context"
	"errors"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/internal/envhost"
)

// hostEnv binds one remote environment under a single envhost.Host
// (ADR-0014 D1 stage 1). It wraps the phone-home server (which serves the
// scope-token store and peer-acceptor surface) together with the peer
// session provider (which serves the peer-backed session lane) and keeps
// every verb's call chain byte-identical to the pre-carve shim knobs: the
// control arms and the ensure path delegate through the provider exactly
// where the session manager's knobs delegated to the shim (the provider
// holds the server), and the listen-address arm reads the server directly —
// the same server the provider forwards to, so the address is identical.
// Events, error shapes, and bookkeeping are unchanged.
type hostEnv struct {
	// srv is the phone-home server serving this environment. It stays
	// pinned on the binding because the host of record IS the environment's
	// server — scope tokens, listen address, and identity verification all
	// live there. The listen-address arm reads it directly; the remaining
	// verbs reach it through the provider (which forwards to this same
	// server) under the ordering the pre-carve knobs relied on.
	srv  *Server
	prov *peerSessionProvider
}

// NewHost returns the envhost.Host binding for a remote environment: the
// phone-home server (`NewShim`) plus the peer session provider serving its
// peer-role dials, merged under one host object. Both arguments must come
// from the same environment wiring. Options install the server's dial-time
// wiring — the registrar for unregistered-scope dials and the pending-wait
// signal sink — which the pre-carve session-manager code reached through
// the (now-unexported) Server setters.
func NewHost(srv *Server, prov *peerSessionProvider, opts ...HostOption) envhost.Host {
	h := &hostEnv{srv: srv, prov: prov}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// HostOption customizes a NewHost binding.
type HostOption func(*hostEnv)

// WithScopeRegistrar installs reg as the binding's dial-time scope
// registrar: unregistered-scope dials whose digest verifies consult it to
// re-register a run's surviving rotated token.
func WithScopeRegistrar(reg ScopeRegistrar) HostOption {
	return func(h *hostEnv) {
		h.srv.setScopeRegistrar(reg)
	}
}

// WithScopeSessionSink installs sink as the binding's scope_session_pending
// signal sink for session waits crossing half their window.
func WithScopeSessionSink(sink ScopeSessionSink) HostOption {
	return func(h *hostEnv) {
		h.srv.setScopeSessionSink(sink)
	}
}

// Shim exposes the environment's phone-home shim: the peer session
// provider the engine handed in, exactly the value the pre-carve
// session-manager shim slot stored. It keeps the query surfaces
// (declared-shim resolution, peer-first session lookup) byte-identical
// now that there is no second shim-registration slot.
func (h *hostEnv) Shim() adapterhost.RemoteShim {
	return h.prov
}

// DefersVerify reports whether this environment defers
// VerifyGraph handshake verification (per-scope session isolation:
// per-scope sessions have no session to verify before a scope entry arms
// it). It is the host-bound replacement of the session manager's
// deferred-adapter bookkeeping.
func (h *hostEnv) DefersVerify() bool {
	return h.srv.perScopeSessions
}

// Ensure engages the scope's definitions and returns their live handles.
// Remote semantics: the adapter phones home to the environment's listener,
// so Ensure blocks until each definition's peer connection is verified and
// stored — the same wait resolveAdapterHandle performed pre-carve. The
// first failing definition stops the engagement.
func (h *hostEnv) Ensure(ctx context.Context, scope envhost.ScopeRef, defs []envhost.Definition) (envhost.Handles, error) {
	key := scope.Key()
	handles := make(envhost.Handles, len(defs))
	for _, def := range defs {
		handle, err := h.prov.WaitForHandle(ctx, def.Name, key)
		if err != nil {
			return handles, err
		}
		handles[def.Name] = handle
	}
	return handles, nil
}

// Release ends the scope's engagement per its declared fate. Stage-1 lanes:
// a release withdraws the scope's accept token (the same unregister the
// run-final teardown performs) — per-definition closes stay on the Control
// arm until the release lane owns them; a detach is a ledger-only no-op
// here because the engagement stays live and the detached-ledger
// bookkeeping remains engine-owned at this stage.
func (h *hostEnv) Release(_ context.Context, scope envhost.ScopeRef, policy envhost.FatePolicy) error {
	if policy.Fate != envhost.FateRelease {
		return nil
	}
	h.prov.UnregisterScope(scope.Key())
	return nil
}

// Journal exposes the scope's supervision truth for one definition. The
// remote binding has no journal of its own yet — supervision truth lives
// in the engine's detached ledger and event stream; the dedicated supervision
// journal arrives with the host-owned control plane.
func (h *hostEnv) Journal(scope envhost.ScopeRef, adapter string) envhost.JournalReader {
	return journalUnavailable{}
}

// Control drives the per-(definition, scope) verbs. Each arm is the exact
// delegate of one pre-carve session-manager knob (Register/Unregister
// scope tokens, per-definition handle close, listen-address read, fresh
// handle wait), so behavior is unchanged.
func (h *hostEnv) Control(ctx context.Context, req *envhost.ControlRequest) (envhost.ControlResponse, error) {
	key := req.Scope.Key()
	switch req.Kind {
	case envhost.ControlRegisterScope:
		h.prov.RegisterScope(key, req.Token)
		return envhost.ControlResponse{}, nil
	case envhost.ControlUnregisterScope:
		h.prov.UnregisterScope(key)
		return envhost.ControlResponse{}, nil
	case envhost.ControlCloseHandle:
		return envhost.ControlResponse{}, h.prov.CloseHandle(ctx, req.Adapter, key)
	case envhost.ControlListenAddress:
		// Read straight off this environment's server: identical to the
		// pre-carve shim.ListenAddr() read the knob performed (the
		// provider delegates its own ListenAddr to this shim).
		return envhost.ControlResponse{Address: h.srv.ListenAddr()}, nil
	case envhost.ControlEnsureFreshHandle:
		var stale adapterhost.Handle
		if req.Stale != nil {
			st, ok := req.Stale.(adapterhost.Handle)
			if !ok {
				return envhost.ControlResponse{}, errStaleHandleIdentity
			}
			stale = st
		}
		handle, err := h.prov.WaitForFreshHandle(ctx, req.Adapter, key, stale)
		if err != nil {
			return envhost.ControlResponse{}, err
		}
		return envhost.ControlResponse{Handle: handle}, nil
	default:
		return envhost.ControlResponse{}, errUnknownControlArm
	}
}

// journalUnavailable is the placeholder reader bindings return while the
// supervision journal is not carved into a given environment host.
type journalUnavailable struct{}

func (journalUnavailable) Entries(context.Context) ([]envhost.JournalEntry, error) {
	return nil, envhost.ErrJournalUnavailable
}

var (
	// errUnknownControlArm refuses arm values the binding predates.
	errUnknownControlArm = errors.New("remote: unknown envhost control arm")
	// errStaleHandleIdentity refuses a stale handle the binding did not mint.
	errStaleHandleIdentity = errors.New("remote: stale handle is not this environment's handle identity")
)
