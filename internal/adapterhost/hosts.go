package adapterhost

import (
	"context"
	"errors"

	"github.com/brokenbots/criteria/internal/envhost"
)

// This file carries the envhost seam consumption glue (ADR-0014 D1 stage
// 1): the shimHost adapter that folds a RemoteShim-only registration into
// the seam, and the host resolution paths the session surfaces dispatch
// through. The remote environment's production binding arrives injected
// (SetEnvHostForEnv, built by the engine as remote.NewHost); a binding that
// predates the seam keeps working through shimHost unchanged.

// shimHost adapts a RemoteShim into envhost.Host so registrations that
// predate the seam dispatch through the same consumption path as bound
// hosts. Every method delegates to the shim exactly where the pre-carve
// session-manager code called it, so behavior — wait semantics, error
// shapes, fallbacks — is byte-identical.
type shimHost struct {
	shim RemoteShim
}

// Ensure waits for each definition's live phone-home handle via the shim,
// in order, stopping at the first failure — the exact verb the remote
// dispatch branch called pre-carve (WaitForHandle per definition).
func (h shimHost) Ensure(ctx context.Context, scope envhost.ScopeRef, defs []envhost.Definition) (envhost.Handles, error) {
	key := scope.Key()
	handles := make(envhost.Handles, len(defs))
	for _, def := range defs {
		handle, err := h.shim.WaitForHandle(ctx, def.Name, key)
		if err != nil {
			return handles, err
		}
		handles[def.Name] = handle
	}
	return handles, nil
}

// Release has no caller while the detached ledger and run-final teardown
// stay on their pre-carve paths; the stage-1 lanes mirror the remote
// binding's so the two bindings stay interchangeable.
func (h shimHost) Release(_ context.Context, scope envhost.ScopeRef, policy envhost.FatePolicy) error {
	if policy.Fate != envhost.FateRelease {
		return nil
	}
	h.shim.UnregisterScope(scope.Key())
	return nil
}

func (h shimHost) Journal(envhost.ScopeRef, string) envhost.JournalReader {
	return shimJournalUnavailable{}
}

// Control maps each arm onto the knob body it replaced: scope-token
// register/unregister, per-definition handle close, listen-address read,
// and the fresh-handle wait for respawn dispatch.
func (h shimHost) Control(ctx context.Context, req *envhost.ControlRequest) (envhost.ControlResponse, error) {
	key := req.Scope.Key()
	switch req.Kind {
	case envhost.ControlRegisterScope:
		h.shim.RegisterScope(key, req.Token)
		return envhost.ControlResponse{}, nil
	case envhost.ControlUnregisterScope:
		h.shim.UnregisterScope(key)
		return envhost.ControlResponse{}, nil
	case envhost.ControlCloseHandle:
		return envhost.ControlResponse{}, h.shim.CloseHandle(ctx, req.Adapter, key)
	case envhost.ControlListenAddress:
		return envhost.ControlResponse{Address: h.shim.ListenAddr()}, nil
	case envhost.ControlEnsureFreshHandle:
		var stale Handle
		if req.Stale != nil {
			st, ok := req.Stale.(Handle)
			if !ok {
				return envhost.ControlResponse{}, errStaleHandleIdentity
			}
			stale = st
		}
		handle, err := h.shim.WaitForFreshHandle(ctx, req.Adapter, key, stale)
		if err != nil {
			return envhost.ControlResponse{}, err
		}
		return envhost.ControlResponse{Handle: handle}, nil
	default:
		return envhost.ControlResponse{}, errUnknownControlArm
	}
}

// shimJournalUnavailable reports the unavailable journal on behalf of
// shim-only registrations (the shim surface predates the journal).
type shimJournalUnavailable struct{}

func (shimJournalUnavailable) Entries(context.Context) ([]envhost.JournalEntry, error) {
	return nil, envhost.ErrJournalUnavailable
}

var (
	// errUnknownControlArm refuses arm values the seam predates.
	errUnknownControlArm = errors.New("adapterhost: unknown envhost control arm")
	// errStaleHandleIdentity refuses a stale handle that is not a live
	// session handle of this session layer.
	errStaleHandleIdentity = errors.New("adapterhost: stale handle is not this session layer's handle identity")
	// errUnsettledHandle reports a host that returned no handle for a
	// definition it engaged.
	errUnsettledHandle = errors.New("adapterhost: envhost host settled no handle for the definition")
	// errForeignHandleIdentity reports a handle the session layer cannot
	// dispatch against.
	errForeignHandleIdentity = errors.New("adapterhost: envhost handle is not this session layer's handle identity")
)

// scopeRefForKey folds a phone-home scope key into a ScopeRef. Parseable
// keys round-trip byte-for-byte through ScopeRef.Key; any other shape
// (none exists in production) keeps its original spelling wholesale so the
// ref renders the exact key downstream.
func scopeRefForKey(key string) envhost.ScopeRef {
	ref, ok := envhost.ParseScopeRef(key)
	if !ok || ref.Key() != key {
		return envhost.ScopeRef{Scope: key}
	}
	return ref
}

// ensureHostHandle ensures the definition's live handle through the
// environment's host (remote dispatch) and recovers the session-layer
// handle type. Both definition identities travel on the Ensure request: the
// wait keys on `name` (the "<type>.<name>" instance identity) and `defType`
// rides along so the host's settled handles can be attributed to the right
// adapter type — keying the wait by the bare type would hand back a sibling
// definition's child. The Ensure error is returned verbatim: shim wait
// errors are surfaced unchanged downstream, exactly as the pre-carve branch
// returned them.
func ensureHostHandle(ctx context.Context, host envhost.Host, name, defType, scope string) (Handle, error) {
	handles, err := host.Ensure(ctx, scopeRefForKey(scope), []envhost.Definition{{Name: name, Type: defType}})
	if err != nil {
		return nil, err
	}
	handle, ok := handles.Get(name)
	if !ok {
		return nil, errUnsettledHandle
	}
	// A settling host that records a nil handle also fails this conversion
	// (intentional stage-1 tightening: pre-carve a nil settle could
	// propagate, but no production binding can settle (nil, nil) today).
	resolved, ok := handle.(Handle)
	if !ok {
		return nil, errForeignHandleIdentity
	}
	return resolved, nil
}

// freshHostHandle waits through the environment's host for a live handle
// that is not the dead one named in the session (respawn = reconnect) and
// recovers the session-layer handle type, mirroring the pre-carve
// WaitForFreshHandle branch.
func freshHostHandle(ctx context.Context, host envhost.Host, name, scope string, stale Handle) (Handle, error) {
	resp, err := host.Control(ctx, &envhost.ControlRequest{
		Scope:   scopeRefForKey(scope),
		Adapter: name,
		Kind:    envhost.ControlEnsureFreshHandle,
		Stale:   stale,
	})
	if err != nil {
		return nil, err
	}
	// A settling host that records a nil handle also fails this conversion
	// (intentional stage-1 tightening; no production binding settles (nil, nil)).
	handle, ok := resp.Handle.(Handle)
	if !ok {
		return nil, errForeignHandleIdentity
	}
	return handle, nil
}

// envHostForEnvLocked returns the envhost.Host serving envKey. The bound
// host wins; otherwise the shim registered for the environment (or the
// legacy default shim) is folded into a shimHost so RemoteShim-only
// registrations dispatch identically. nil mirrors the pre-carve knob
// behavior for an unregistered environment (shim == nil); the caller owns
// the error or no-op lane for nil.
//
// m.mu must be held.
func (m *SessionManager) envHostForEnvLocked(envKey string) envhost.Host {
	if host, ok := m.envHostsByEnv[envKey]; ok {
		return host
	}
	if shim := m.remoteShimForEnvLocked(envKey); shim != nil {
		return shimHost{shim: shim}
	}
	return nil
}

// envHostDefaultLocked resolves the legacy single-shim slot through the
// seam so the legacy knobs delegate through the same path as their ForEnv
// forms.
//
// m.mu must be held.
func (m *SessionManager) envHostDefaultLocked() envhost.Host {
	return m.envHostForEnvLocked("")
}

// envHostForAdapter returns the host serving the remote environment the
// adapter declaration is bound to — the exact resolution the dispatch
// branch used pre-carve (declared graph -> environment key -> per-env shim
// with default-shim fallback). ok is false when the adapter is not remote,
// or no host serves its environment.
func (m *SessionManager) envHostForAdapter(name string) (envhost.Host, bool) {
	envKey, remote := m.remoteEnvForAdapter(name)
	if !remote {
		return nil, false
	}
	m.mu.Lock()
	host := m.envHostForEnvLocked(envKey)
	m.mu.Unlock()
	if host == nil {
		return nil, false
	}
	return host, true
}

// SetEnvHostForEnv binds the envhost.Host serving a specific remote
// environment (keyed "remote.<name>"). Production wiring builds the bound
// host over the environment's shim (remote.NewHost) and pins it after
// SetRemoteShimForEnv registers the shim; the bound host then serves every
// seam verb for that environment while the shim stays registered for the
// query surfaces (RemoteShimForEnv, WrappedShim access). A later
// SetRemoteShimForEnv for the same key re-shadows the binding with a
// shimHost — the latest registration wins, matching the shim map.
func (m *SessionManager) SetEnvHostForEnv(envKey string, host envhost.Host) {
	if host == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setEnvHostForEnvLocked(envKey, host)
}

// setEnvHostForEnvLocked stores the bound host under lock.
//
// m.mu must be held.
func (m *SessionManager) setEnvHostForEnvLocked(envKey string, host envhost.Host) {
	if m.envHostsByEnv == nil {
		m.envHostsByEnv = make(map[string]envhost.Host)
	}
	m.envHostsByEnv[envKey] = host
}
