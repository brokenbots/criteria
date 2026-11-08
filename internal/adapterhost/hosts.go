package adapterhost

import (
	"context"
	"errors"
	"maps"

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

// Shim exposes the underlying shim so shutdown collection and the
// legacy single-shim query surfaces keep serving through a host-bound
// shim source (HostShimSource) instead of a parallel registration map.
func (h shimHost) Shim() RemoteShim { return h.shim }

// DefersVerify reports whether the session layer must defer the blocking
// adapter-info verify for adapters served by this host: remote adapters
// that enable per-scope sessions rotate their token at scope entry, so an
// eager VerifyGraph handshake would block before the token exists. The
// capability is discovered structurally (local shims satisfy it via their
// PerScopeSessions getter) so this file never imports a remote binding.
func (h shimHost) DefersVerify() bool {
	if d, ok := h.shim.(interface{ PerScopeSessions() bool }); ok {
		return d.PerScopeSessions()
	}
	return false
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

// containerEnvForAdapter returns the environment key ("container.<name>")
// the adapter declaration is bound to when that environment is a CONTAINER
// environment — the isolation-kind-keyed resolution the container bind
// path dispatches on (the seam's container counterpart of
// remoteEnvForAdapter, which stays remote-only).
func (m *SessionManager) containerEnvForAdapter(instanceID string) (string, bool) {
	adapterNode, graph := m.adapterDeclaration(instanceID)
	if adapterNode == nil || graph == nil {
		return "", false
	}
	envKey, ok := declaredAdapterEnvironmentKey(adapterNode, graph)
	if !ok {
		return "", false
	}
	envNode, ok := graph.Environments[envKey]
	if !ok || envNode.Type != "container" {
		return "", false
	}
	return envKey, true
}

// envHostBoundForEnv returns the dedicated envhost.Host bound to envKey,
// WITHOUT the shimHost fold (the container dispatch lane has no shim
// substrate to fold: an unbound container environment keeps the pre-carve
// BuildContainerRunner dispatch).
//
// m.mu must NOT be held.
func (m *SessionManager) envHostBoundForEnv(envKey string) (envhost.Host, bool) {
	m.mu.Lock()
	host, ok := m.envHostsByEnv[envKey]
	m.mu.Unlock()
	if !ok || host == nil {
		return nil, false
	}
	return host, true
}

// HostShimSource is the optional capability of a bound host to surface the
// adapterhost shim that backs it. Shutdown collection walks bound hosts for
// their shims so stop-time behavior is identical whether a shim arrived as
// a bare registration (SetRemoteShim) or folded behind a host binding;
// hosts without the substrate (container supervisor hosts) contribute no
// shim side.
type HostShimSource interface {
	Shim() RemoteShim
}

// EnvHost returns the envhost.Host bound to envKey without the shim fold
// (nil, false when the environment has no explicit binding). The engine's
// scope registrar and lifecycle resolvers use it to reach the environment's
// host of record directly instead of walking the legacy shim knobs.
func (m *SessionManager) EnvHost(envKey string) (envhost.Host, bool) {
	return m.envHostBoundForEnv(envKey)
}

// ShareEnvHostsFrom copies src's bound envhost.Host entries into m WITHOUT
// taking ownership of the hosts. m then serves and registers scopes on the
// same shared hosts (one host of record per environment, multiplexed across
// callers), but m.Shutdown will not stop them — the owning parent
// SessionManager does that at run end.
//
// This is how a parallel subworkflow iteration, which gets its own
// SessionManager for local session isolation, still reaches the remote
// environment: without the hosts, provisioning fails with "no remote shim
// registered". Each iteration still rotates a DISTINCT scope on the shared
// host, so scope isolation and per-scope provisioning events are preserved.
func (m *SessionManager) ShareEnvHostsFrom(src *SessionManager) {
	if src == nil || src == m {
		return
	}
	src.mu.Lock()
	envHosts := maps.Clone(src.envHostsByEnv)
	src.mu.Unlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(envHosts) > 0 {
		m.envHostsByEnv = mergeMapInto(m.envHostsByEnv, envHosts)
		m.envHostsShared = true
	}
}

// containerEnvHostForAdapter returns the bound envhost.Host serving the
// container environment the adapter declaration resolves to. ok is false
// when the adapter is not container-bound or its environment has no bound
// supervisor host — the caller then keeps its pre-carve dispatch. A
// shimHost bound under a container key (a stray shim registration; the
// container lane has no shim substrate) is refused here: only an explicit
// SetContainerEnvHost bind dispatches container handles.
func (m *SessionManager) containerEnvHostForAdapter(name string) (envhost.Host, bool) {
	envKey, container := m.containerEnvForAdapter(name)
	if !container {
		return nil, false
	}
	return m.containerEnvHostForEnvKey(envKey)
}

// containerEnvHostForEnvKey is the bind lookup for one container
// environment key. A shimHost bound under a container key (a stray shim
// registration; the container lane has no shim substrate) is refused:
// only an explicit SetContainerEnvHost bind dispatches container handles.
//
// m.mu must NOT be held.
func (m *SessionManager) containerEnvHostForEnvKey(envKey string) (envhost.Host, bool) {
	host, ok := m.envHostBoundForEnv(envKey)
	if !ok {
		return nil, false
	}
	if _, isShim := host.(shimHost); isShim {
		return nil, false
	}
	return host, true
}

// EnvHostStopper is the ownership contract for supervisor hosts this
// SessionManager bound through SetEnvHostForEnv and owns at teardown: the
// manager that bound the host stops it at shutdown, so a container host's
// children are never left running past the run. Borrowed hosts
// (BorrowRemoteProvisioningFrom shares the parent's hosts with parallel
// iterations) are owned by the parent and are not stopped by a borrower.
type EnvHostStopper interface {
	StopEnvHost(ctx context.Context)
}

// NewShimHost binds a bare shim as a degenerate host-of-record so an
// environment whose plumbing never materializes a peer provider still
// reaches the seam through the same verbs. Tests and local-mode callers
// use it to route a single shim through the host seam unchanged.
func NewShimHost(shim RemoteShim) envhost.Host { return shimHost{shim: shim} }

// SetContainerEnvHost binds the in-process supervisor host serving a
// container environment (host of record for the seam, keyed "container.<name>").
// Unlike SetEnvHostForEnv it REFUSES to fold through the shim lane: a
// container environment with no bound supervisor keeps the local
// BuildContainerRunner dispatch, so binding is explicit — the engine's
// binder is the only production caller. A nil host binds nothing.
func (m *SessionManager) SetContainerEnvHost(envKey string, host envhost.Host) {
	if host == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setEnvHostForEnvLocked(envKey, host)
}

// envHostForAdapter returns the host serving the environment the adapter
// declaration is bound to, keyed on isolation kind: a REMOTE-bound adapter
// resolves exactly as pre-carve (declared graph -> environment key ->
// per-env shim with default-shim fallback); a CONTAINER-bound adapter
// requires a dedicated bound supervisor host (no shim fold — an unbound
// container environment keeps the local BuildContainerRunner dispatch).
// ok is false when the adapter is not remote/container, or no host serves
// its environment.
func (m *SessionManager) envHostForAdapter(name string) (envhost.Host, bool) {
	envKey, remote := m.remoteEnvForAdapter(name)
	if !remote {
		envKey, container := m.containerEnvForAdapter(name)
		if !container {
			return nil, false
		}
		return m.containerEnvHostForEnvKey(envKey)
	}
	m.mu.Lock()
	host := m.envHostForEnvLocked(envKey)
	m.mu.Unlock()
	if host == nil {
		return nil, false
	}
	return host, true
}

// remoteEnvHostForAdapter is the pre-carve REMOTE-only resolution the
// implicit crash-retry arm keys on: the adapter declaration must bind a
// REMOTE environment AND a host must serve it. A container-bound adapter
// never resolves here — a container session has no pod to recreate, so its
// transport closes follow the declared on_crash policy instead of the
// remote pod-recreate arm.
func (m *SessionManager) remoteEnvHostForAdapter(name string) (envhost.Host, bool) {
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
