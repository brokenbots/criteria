package adapterhost

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/zclconf/go-cty/cty"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapter/environment/sandbox"
	"github.com/brokenbots/criteria/internal/adapter/secrets"
	"github.com/brokenbots/criteria/internal/log"
	"github.com/brokenbots/criteria/internal/tunables"
	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

const (
	OnCrashFail     = "fail"
	OnCrashRespawn  = "respawn"
	OnCrashAbortRun = "abort_run"
)

var (
	ErrSessionAlreadyOpen = errors.New("session already open")
	ErrUnknownSession     = errors.New("unknown session")
)

// FatalRunError signals a non-recoverable adapter failure that should abort
// the workflow run immediately (without applying failure-outcome fallback).
type FatalRunError struct {
	Err error
}

func (e *FatalRunError) Error() string {
	if e == nil || e.Err == nil {
		return "fatal run error"
	}
	return e.Err.Error()
}

func (e *FatalRunError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// SessionCrashError reports that an adapter session crashed and the session
// manager surfaced the crash as a step failure (the default on_crash=fail
// policy, or a respawn policy whose recovery also failed). The engine
// distinguishes it from adapter-reported functional failures via errors.As
// (CRI-130): a session crash means the adapter process is gone and every
// subsequent Execute on the same session returns the crash error again, while
// an adapter-reported failure outcome leaves the session alive.
type SessionCrashError struct {
	Session string
	Err     error
}

func (e *SessionCrashError) Error() string {
	if e == nil {
		return "session crashed"
	}
	return fmt.Sprintf("session %q crashed: %v", e.Session, e.Err)
}

func (e *SessionCrashError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

type SessionManager struct {
	loader Loader
	graph  *workflow.FSMGraph

	// sandboxProbeOverride is a test hook that replaces sandbox.Probe().
	// When nil the real Probe() is used.
	sandboxProbeOverride func() sandbox.Capabilities

	// sandboxShimBin is the path to the binary used as the sandbox pre-exec
	// shim. When empty the current process image (os.Args[0]) is used, matching
	// production criteria CLI behavior. Tests set this to a dedicated helper
	// binary so the shim runs in a minimal process instead of the test binary.
	sandboxShimBin string

	// sandboxProductionShimBin overrides the binary used for the production
	// criteria-as-shim path. Unlike sandboxShimBin, setting this does not
	// enable test-helper relaxations (SkipShimRestrictions remains false).
	sandboxProductionShimBin string

	// lockfile holds the parsed lockfile for container-mode lookups.
	lockfile *lockfile.Lockfile

	// RedactionRegistry masks secret values from all host output streams.
	RedactionRegistry *secrets.Registry

	// Audit receives structured DecisionLogEntry records for every permission
	// decision. If nil, audit logging is a no-op.
	Audit AuditWriter

	// remoteShim is set when the workflow references a remote environment.
	// It provides phone-home adapter handles instead of local binaries.
	remoteShim RemoteShim

	// remoteShimsByEnv maps environment keys ("remote.<name>") to the shim
	// serving that specific environment (CRI-293). Local runs start one shim
	// per remote environment, so two environments that declare the same
	// listen_address each get their own shim. Dispatch falls back to
	// remoteShim when an environment has no dedicated entry.
	remoteShimsByEnv map[string]RemoteShim

	// remoteShimsBorrowed is set when this SessionManager's remote shims were
	// borrowed from a parent SM (BorrowRemoteProvisioningFrom) rather than started by
	// it. A parallel subworkflow iteration receives its own SessionManager for
	// local session isolation, but the remote shim is a single phone-home
	// listener per environment that multiplexes every scope, so the iteration
	// must share the parent's shim instead of starting its own (which would
	// collide on the fixed listen_address). Borrowed shims are owned by the
	// parent: Shutdown must not Stop them, or one finished iteration would tear
	// down the listener the sibling iterations and the root run still use.
	remoteShimsBorrowed bool

	// LifecycleSink receives adapter provisioning events. When a verified-only
	// adapter is promoted to a bound session, "opened" is emitted through this
	// sink so lifecycle observers see the event at the correct phase-2 moment.
	LifecycleSink LifecycleSink

	// deferredRemoteAdapters identifies remote adapters whose eager
	// VerifyGraph handshake should be skipped. This is used when a remote
	// environment enables per_scope_sessions so the per-scope token rotation and
	// provisioning event happen in initScopeAdapters, not during graph
	// verification.
	deferredRemoteAdapters map[string]struct{}

	// CheckpointSave persists a session's checkpoint at step boundaries (and
	// per-turn boundaries for adapters declaring per-turn granularity,
	// CRI-202). The engine wires the state-home store; nil disables
	// checkpointing entirely. Failures are surfaced to execute(), which
	// aborts the run loudly: a step-outcome event is never emitted without
	// its checkpoint.
	CheckpointSave func(sessionID string, snap *SessionSnapshot) error

	// HeartbeatStallThreshold is the duration after which a log-stream heartbeat
	// is considered stalled. If zero, tunables.DefaultHeartbeatStallThreshold
	// (90s) applies. This is primarily a test hook so conformance and
	// regression tests can use a short threshold.
	HeartbeatStallThreshold time.Duration

	// RespawnLogStreamDrainTimeout is the maximum time restartLogStream waits for
	// the previous log-stream watcher to finish after cancellation. If zero, a
	// bound derived from HeartbeatStallThreshold is used (1/3 of the threshold,
	// clamped between 5s and 30s). This is a test hook so the bounded-wait
	// regression test can use a short value.
	RespawnLogStreamDrainTimeout time.Duration

	// PauseToolCallDrainTimeout is the bounded wait the drain-first pause
	// posture (CRI-169, ADR-0004 §11) gives in-flight nested adapter tool
	// calls to settle before canceling them: Session.Pause sets the pause
	// gate (no new nested calls start), waits for the in-flight calls within
	// this window, cancels the stragglers with a typed `canceled` reply, then
	// pauses the stream. If zero, the default 60s is used. This is primarily
	// a test hook so conformance tests can exercise the straggler path in
	// bounded time.
	PauseToolCallDrainTimeout time.Duration

	// StepTimeoutTeardownWindow is how long a CRI-287 step-timeout teardown
	// mark keeps transport-close reclassification active: the teardown
	// cascade closes sibling phone-home transports in the same second as the
	// canceled Execute stream, so follow-on Executes observe the closes
	// within this window of the mark. If zero,
	// tunables.DefaultStepTimeoutTeardownWindow applies. NewSessionManager
	// seeds it from the tunables registry (CRITERIA_STEP_TIMEOUT_TEARDOWN_WINDOW).
	StepTimeoutTeardownWindow time.Duration

	mu       sync.Mutex
	sessions map[string]*Session
	// verified holds adapters that have passed eager verification (phase 1)
	// but have not yet been bound to their working directory (phase 2).
	// Binding happens automatically on the first Execute call.
	verified map[string]*verifiedRecord
	// adapterInfos caches each adapter's declared schema surface
	// (workflow.AdapterInfo) captured from the phase-1 Info handshake. It
	// drives nested tool-call execution (CRI-160): the synthetic callee step's
	// input validation and OutputSchema decode come from here, so a callee
	// verified but never step-targeted still carries its declared types.
	adapterInfos map[string]*workflow.AdapterInfo
	// bindMu serializes the one-time promotion of a verified adapter to a
	// bound session. This prevents concurrent Execute callers from racing to
	// bind the same adapter and seeing ErrSessionAlreadyOpen from the winner.
	bindMu sync.Mutex

	// CRI-287: engineStepTimeoutTeardownAt records when the engine canceled a
	// step because the CRI-275 step ceiling expired. The canceled Execute
	// stream tears down sibling phone-home transports in the same second, so
	// a transport close observed on a later Execute (fresh context) is the
	// consequence of that cancellation, not an adapter death.
	//
	// Invariant: the teardown window is bound to the cascade, not to call
	// success. The window opens when the mark is recorded and stays open for
	// StepTimeoutTeardownWindow of wall-clock time:
	//   - the follow-on steps' Executes — however many of them fail — observe
	//     the cascade-closed transports inside the window, and crash
	//     classification re-enables itself when the window expires even if
	//     every Execute keeps failing;
	//   - a successful Execute on any session does NOT close the window: a
	//     healthy sibling proving its own transport alive is not evidence
	//     that a torn-down sibling has been observed;
	//   - deaths with positive evidence (ProcessExited) and host-initiated
	//     closes (the closing flag) are never downgraded to timeouts.
	//
	// While the window is open, transport-close errors are returned as-is —
	// the step's declared failure/default outcome routing (the checkpoint
	// loop) proceeds — instead of being classified as a session crash.
	engineStepTimeoutTeardownAt atomic.Int64

	// allowedRoots restricts environment working_directory values. Empty means
	// no additional root checks; paths containing ".." are always rejected.
	allowedRoots []string

	// graphAdapters caches every adapter node in the compiled graph tree, keyed
	// by instance ID. It is populated by VerifyGraph and used at resolution time
	// to know whether an adapter is OCI-backed or bound to a remote environment
	// without re-reading workflow files. The owning graph is kept so
	// per-scope environments resolve against the declaring subworkflow body
	// (CRI-269: the root graph's adapter map never contains subworkflow
	// adapters).
	// Locking contract (CRI-50): both maps are guarded by mu. VerifyGraph
	// writes them via cacheGraphAdapterRef and adapter resolution reads them
	// via adapterDeclaration/adapterDir, and nothing serializes those callers
	// at a higher level (the borrow path already took mu for its reads, so
	// mu-guarding the rest adds no deadlock risk). Every access takes mu.
	graphAdapters map[string]graphAdapterRef
	// adapterDirs records the workflow directory each adapter was declared in,
	// keyed by instance ID. Populated by VerifyGraph.
	adapterDirs map[string]string

	// KB-156: tool resources this manager leases from ANOTHER manager's
	// environment, keyed by instance ID -> the owner manager that created and
	// owns the shared session. Nested tool calls for leased names delegate to
	// owner.execute so every caller (root graph, parallel iterations,
	// subworkflows) shares ONE adapter session per environment, instead of
	// the KB-58 copy-per-iteration borrow binding a private session per
	// caller. Guarded by mu.
	leasedToolResources map[string]*SessionManager
	// KB-156: outstanding leases OTHER managers hold against THIS manager's
	// tool-resource sessions, keyed by instance ID -> lease count. While a
	// name's count is non-zero the shared session stays alive: Close refuses
	// to tear it down, and deferred releases (leasing scope unwind) only
	// decrement. The owner scope anchors the session's real teardown. Guarded
	// by mu.
	toolResourceLeases map[string]int
	// borrowedPolicy is the parent graph's policy, set by the engine on
	// borrowed managers so the nested tool-call gates (max_tool_depth,
	// KB-58 max_tool_calls) consult the declaring workflow's values instead
	// of package defaults. Guarded by mu; nil when not borrowed.
	borrowedPolicy *workflow.Policy
	// toolCallBudgets counts reserved nested tool calls per caller session id
	// (KB-58 policy.max_tool_calls). Guarded by mu. Entries live as long as
	// the sessions they count for; the map is cleared at Shutdown with the
	// rest of the session state.
	toolCallBudgets map[string]int
}

// verifiedRecord stores the host-visible state of an adapter that has been
// verified but not yet bound. It contains everything needed to start the
// long-lived session later.
type verifiedRecord struct {
	name             string
	adapter          string
	onCrash          string
	config           map[string]string
	secrets          map[string]string
	secretOriginRefs map[string]secrets.OriginRef
	capabilities     []string
	workingDir       string
	adapterDigest    digest.Digest
	// scopeName is the engine scope that verified this adapter. It is used to
	// attribute the phase-2 "opened" lifecycle event to the same scope as the
	// "verified" event.
	scopeName string
	// scopeInstanceID is a unique identifier for this invocation of the scope.
	// It is used to key per-scope shim sessions when per_scope_sessions is
	// enabled; when empty the legacy adapter-type-only key is used.
	scopeInstanceID string
}

func (m *SessionManager) heartbeatStallThreshold() time.Duration {
	if m.HeartbeatStallThreshold > 0 {
		return m.HeartbeatStallThreshold
	}
	return tunables.DefaultHeartbeatStallThreshold
}

// stepTimeoutTeardownWindow returns the configured CRI-287 teardown window.
func (m *SessionManager) stepTimeoutTeardownWindow() time.Duration {
	if m.StepTimeoutTeardownWindow > 0 {
		return m.StepTimeoutTeardownWindow
	}
	return tunables.DefaultStepTimeoutTeardownWindow
}

func (m *SessionManager) respawnLogStreamDrainTimeout() time.Duration {
	if m.RespawnLogStreamDrainTimeout > 0 {
		return m.RespawnLogStreamDrainTimeout
	}
	return tunables.RespawnLogStreamDrain(m.heartbeatStallThreshold())
}

// RemoteShim is the interface the session manager uses to wait for remote
// adapter connections.
type RemoteShim interface {
	WaitForHandle(ctx context.Context, adapterType, scope string) (Handle, error)
	// WaitForFreshHandle waits for a connection whose handle is not `stale`,
	// used on crash-respawn so the dead handle is never handed back.
	WaitForFreshHandle(ctx context.Context, adapterType, scope string, stale Handle) (Handle, error)
	// RegisterScope registers (or rotates) the accept token for a scope.
	// Only used when per-scope session isolation is enabled.
	RegisterScope(scope, token string)
	// UnregisterScope removes the accept token for a scope so reconnects
	// using the old token are rejected.
	UnregisterScope(scope string)
	// CloseHandle closes the active session for the given adapter type + scope.
	CloseHandle(ctx context.Context, adapterType, scope string) error
	// ListenAddr returns the shim's bound listen address.
	ListenAddr() string
	// Stop tears the shim down: it closes the listener (ending the accept
	// loop), cancels all sessions, and wakes pending waiters with an error.
	// Called by Shutdown so a shim registered with the session manager does
	// not leak its accept goroutine after the run ends.
	Stop(ctx context.Context) error
}

// LifecycleSink receives adapter lifecycle events from the session manager.
type LifecycleSink interface {
	OnAdapterLifecycle(runID, adapter, status, detail string)
}

// SetGraph provides the compiled workflow graph so the session manager
// can look up per-adapter environment policies (e.g. sandbox) at open time.
func (m *SessionManager) SetGraph(g *workflow.FSMGraph) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.graph = g
}

// SetRemoteShim provides the remote shim so the session manager can dispatch
// adapters bound to remote environments to the phone-home listener.
//
// Legacy single-shim form, retained for tests and callers that do not
// know their environment key (the fallback consulted by
// RemoteShimForEnv). Production registration goes through
// SetRemoteShimForEnv (CRI-293: one shim per remote environment).
// Deliberately carries no canonical Deprecated marker: the retained test
// call sites exercise this fallback path and staticcheck SA1019 would
// flag them (CRI-293 review).
func (m *SessionManager) SetRemoteShim(shim RemoteShim) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.remoteShim = shim
}

// SetRemoteShimForEnv registers the shim serving a specific remote
// environment (keyed "remote.<name>", CRI-293). The shim is also recorded as
// the default so legacy callers observe the most recently registered shim.
func (m *SessionManager) SetRemoteShimForEnv(envKey string, shim RemoteShim) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.remoteShimsByEnv == nil {
		m.remoteShimsByEnv = make(map[string]RemoteShim)
	}
	m.remoteShimsByEnv[envKey] = shim
	m.remoteShim = shim
}

// BorrowRemoteProvisioningFrom copies src's remote-provisioning state into m
// WITHOUT taking ownership of the shims. m then serves and registers scopes on
// the same shared shims (one phone-home listener per environment, multiplexed
// across callers), but m.Shutdown will not Stop them — the owning parent SM
// does that at run end. It also copies the VerifyGraph-populated adapter caches
// (graphAdapters, adapterDirs, deferredRemoteAdapters) so isRemoteAdapter on m
// recognises the parent-verified adapters as remote instead of falling through
// to a local OCI resolution (CRI-269 keeps the DECLARING graph in these caches,
// so subworkflow-body adapters resolve correctly).
//
// This is how a parallel subworkflow iteration, which gets its own
// SessionManager for local session isolation, still reaches the remote
// environment: without the shims, provisioning fails with "no remote shim
// registered"; without the caches, the adapter is dispatched locally and not
// found. Each iteration still rotates a DISTINCT scope, so scope isolation and
// per-scope provisioning events are preserved.
func (m *SessionManager) BorrowRemoteProvisioningFrom(src *SessionManager) {
	if src == nil || src == m {
		return
	}
	src.mu.Lock()
	def := src.remoteShim
	byEnv := maps.Clone(src.remoteShimsByEnv)
	graphAdapters := maps.Clone(src.graphAdapters)
	adapterDirs := maps.Clone(src.adapterDirs)
	deferred := maps.Clone(src.deferredRemoteAdapters)
	src.mu.Unlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	if def != nil || len(byEnv) > 0 {
		m.remoteShim = def
		m.remoteShimsByEnv = mergeMapInto(m.remoteShimsByEnv, byEnv)
		m.remoteShimsBorrowed = true
	}
	m.graphAdapters = mergeMapInto(m.graphAdapters, graphAdapters)
	m.adapterDirs = mergeMapInto(m.adapterDirs, adapterDirs)
	m.deferredRemoteAdapters = mergeMapInto(m.deferredRemoteAdapters, deferred)
}

// mergeMapInto copies every entry of src into dst and returns dst, allocating
// dst from src when dst is nil. A nil/empty src leaves dst untouched.
func mergeMapInto[K comparable, V any](dst, src map[K]V) map[K]V {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		return src
	}
	maps.Copy(dst, src)
	return dst
}

// LeaseToolResourcesFrom registers shared-session leases against src for the
// given tool-resource names (KB-156). It replaces the KB-58
// BorrowToolResourceSessionsFrom record copy: instead of deep-copying
// verified records so every parallel iteration lazy-binds its own adapter
// process and initialize handshakes, the nested tool calls for leased names
// delegate to src's session — ONE shared session per environment, created
// once by the host-of-record manager (ADR-0008/0009 one-container-per-
// environment doctrine). Each delegated call runs through the same process,
// so a per-iteration Info surface can no longer clobber itself across
// concurrent callers, and teardown follows lease semantics: the session
// stays anchored to the owner's scope and each leasing scope merely drops
// its lease when it unwinds.
//
// Names m already hosts (bound session or verified record) are skipped — m
// would resolve them locally anyway — as are names m already leases (the
// lease is idempotent; a duplicate registration would leak an owner
// refcount entry). A remote-declared tool resource leases the same way —
// KB-160's host-of-record route: the owner hosts the environment's ONE peer
// phone-home session, so nested calls travel over the peer connection the
// owner opened and the peer opens and serves exactly one shared session for
// the environment. Names src cannot host (no bound session and no verified
// record — including a remote callee whose owner never verified it) are
// skipped so the nested dispatch keeps reporting its
// own typed unknown_adapter resolution error. Ownership resolves through
// src's own leases (pass-through), so a lease never creates an intermediate
// hop.
//
// The owner's adapter infos are installed under the leased names so child-side
// arg validation and nested dispatch resolve without a second Info handshake
// (AdapterInfo carries no secrets). Returns the names actually leased.
// Thread-safe.
func (m *SessionManager) LeaseToolResourcesFrom(src *SessionManager, names []string) []string {
	if src == nil || src == m || len(names) == 0 {
		return nil
	}
	m.mu.Lock()
	leased := make([]string, 0, len(names))
	var refresh []string
	for _, name := range names {
		if !m.leaseEligibleLocked(name) {
			continue
		}
		// Idempotent re-lease: a second lease attempt for a name this manager
		// already leases must NOT bump the owner's refcount again, or the
		// release would underflow and the entry would anchor the owner's
		// teardown forever.
		if held, ok := m.leasedToolResources[name]; ok {
			if held == src {
				refresh = append(refresh, name)
			}
			continue
		}
		if m.leaseToolResourceLocked(src, name) {
			leased = append(leased, name)
		}
	}
	m.mu.Unlock()

	// Install the owner's adapter info surface for the leased names so the
	// child resolves callee capability and declaration checks against what
	// the shared session actually serves. Sources for the surface come from
	// src as-is: for a pass-through lease (src itself leased from a deeper
	// owner) src already carries the ultimate owner's info, so this works
	// regardless of which manager ends up hosting the session.
	// slices.Concat avoids aliasing the returned leased slice (the refresh
	// tail must not collide with a later caller-side append into it).
	if all := slices.Concat(leased, refresh); len(all) > 0 {
		infos := src.snapshotToolResourceInfos(all)
		m.mu.Lock()
		for name, info := range infos {
			if m.adapterInfos == nil {
				m.adapterInfos = make(map[string]*workflow.AdapterInfo)
			}
			m.adapterInfos[name] = info
		}
		m.mu.Unlock()
	}
	return leased
}

// leaseToolResourceLocked registers one new shared-session lease for name:
// it resolves the ultimate owner through src's own leases (pass-through, so
// a lease never creates an intermediate hop), acquires a refcounted lease on
// the owner, and records name -> owner on m. It reports whether the lease
// was registered. Pass-through lock ordering stays one-directional (m.mu ->
// src.mu); no path holds src.mu while taking another manager's mu.
// m.mu must be held.
func (m *SessionManager) leaseToolResourceLocked(src *SessionManager, name string) bool {
	owner := src.leaseOwner(name)
	if owner == nil {
		owner = src
	}
	if owner == m {
		// Out-of-protocol self-owning pass-through that would loop on
		// itself at execute time; leave the name unresolved.
		return false
	}
	if !owner.acquireToolResourceLease(name) {
		return false
	}
	if m.leasedToolResources == nil {
		m.leasedToolResources = make(map[string]*SessionManager)
	}
	m.leasedToolResources[name] = owner
	return true
}

// leaseEligibleLocked reports whether name may receive a shared-session
// lease: no live binding and no verified record of its own. Hostability —
// including for a remote-declared tool resource, whose owner hosts the
// environment's ONE peer phone-home session (KB-160 host-of-record route) —
// is decided on the owner, not here. Unresolvable declarations lease as-is
// — when the owner cannot host the name either, the name is skipped and the
// nested dispatch reports its typed unknown_adapter error.
// m.mu must be held.
func (m *SessionManager) leaseEligibleLocked(name string) bool {
	if _, ok := m.sessions[name]; ok {
		return false
	}
	_, ok := m.verified[name]
	return !ok
}

// leaseOwner returns the manager whose shared session serves the named
// leased tool resource, or nil when name is not leased here (KB-156).
func (m *SessionManager) leaseOwner(name string) *SessionManager {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.leasedToolResources[name]
}

// snapshotToolResourceInfos gathers copies of the manager's adapter infos for
// the given names (KB-156 lease side). AdapterInfo carries no secrets, so
// shallow copies are safe — the KB-58 record copy (config/secrets) is gone:
// leased names execute on the owner's session and never bind locally. The
// manager mutex is held for the whole gather so a concurrent scope change
// cannot split a name across states.
func (m *SessionManager) snapshotToolResourceInfos(names []string) map[string]*workflow.AdapterInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	var infos map[string]*workflow.AdapterInfo
	for _, name := range names {
		info := m.adapterInfos[name]
		if info == nil {
			continue
		}
		if infos == nil {
			infos = make(map[string]*workflow.AdapterInfo, len(names))
		}
		captured := *info
		infos[name] = &captured
	}
	return infos
}

// acquireToolResourceLease increments the shared-session lease count for the
// named tool resource (KB-156). It succeeds only when this manager hosts the
// resource — a bound session or a verified record. Host-local and remote-
// declared tool resources both qualify (KB-160): a remote callee's shared
// session is the environment's ONE peer phone-home session the owner opened,
// so the nested call travels over the peer connection (host-of-record route)
// instead of each caller binding its own copy. Thread-safe.
func (m *SessionManager) acquireToolResourceLease(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, bound := m.sessions[name]; !bound {
		if _, verified := m.verified[name]; !verified {
			return false
		}
	}
	if m.toolResourceLeases == nil {
		m.toolResourceLeases = make(map[string]int)
	}
	m.toolResourceLeases[name]++
	return true
}

// releaseToolResourceLease drops one outstanding shared-session lease
// (KB-156). The session itself is NOT closed here: one caller's release must
// never rip the environment's shared session out from under the other
// callers. Its lifecycle is anchored to the owner's own scope teardown
// (initScopeAdapters/tearDownScopeAdapters), and call nesting guarantees every
// caller's lease is released before the owner scope unwinds — the count
// simply reaches zero when the last caller releases. Thread-safe.
func (m *SessionManager) releaseToolResourceLease(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.toolResourceLeases == nil {
		return
	}
	n := m.toolResourceLeases[name]
	if n <= 1 {
		delete(m.toolResourceLeases, name)
		return
	}
	m.toolResourceLeases[name] = n - 1
}

// ReleaseSharedToolResources drops every shared-session lease this manager
// holds via LeaseToolResourcesFrom (KB-156). Leasing scopes (parallel
// subworkflow iterations) call it on unwind: the owning manager's lease
// count drops per name, and at zero the owner's own scope may tear the
// shared session down. Cached adapter infos installed for the leases are
// discarded too. Idempotent. Thread-safe.
func (m *SessionManager) ReleaseSharedToolResources() {
	m.mu.Lock()
	if len(m.leasedToolResources) == 0 {
		m.mu.Unlock()
		return
	}
	owners := make(map[*SessionManager][]string, len(m.leasedToolResources))
	for name, owner := range m.leasedToolResources {
		owners[owner] = append(owners[owner], name)
		delete(m.leasedToolResources, name)
		// Discard the cached info installed at lease time: after the release
		// this manager no longer resolves the callee locally.
		delete(m.adapterInfos, name)
	}
	m.mu.Unlock()
	for owner, names := range owners {
		for _, name := range names {
			owner.releaseToolResourceLease(name)
		}
	}
}

// SetBorrowedGraphPolicy records the parent graph's policy for m to consult
// when it has no graph of its own (KB-58): the depth gate
// (policy.max_tool_depth) and the call-count gate (policy.max_tool_calls)
// must not fall back to package defaults when a per-workflow value was
// declared. Thread-safe.
func (m *SessionManager) SetBorrowedGraphPolicy(p *workflow.Policy) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.borrowedPolicy = p
}

// BorrowedGraphPolicy returns the parent graph policy recorded via
// SetBorrowedGraphPolicy, or nil when none was borrowed.
func (m *SessionManager) BorrowedGraphPolicy() *workflow.Policy {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.borrowedPolicy
}

// reserveToolCallBudget reserves one nested tool call for the caller session
// against the session's policy.max_tool_calls budget (KB-58), engine-enforced
// at the seam alongside the depth gate so an iterative agent loop cannot run
// unbounded. A maxCalls of 0 or less means no cap (hand-built fixtures with
// no budget configured); otherwise a reservation beyond the cap refuses. The
// caller releases the reservation via releaseToolCallBudget on the
// synchronous gate paths only — an asynchronously dispatched call has been
// executed and keeps consuming budget.
func (m *SessionManager) reserveToolCallBudget(sessionID string, maxCalls int) bool {
	if maxCalls <= 0 {
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.toolCallBudgets == nil {
		m.toolCallBudgets = make(map[string]int)
	}
	if m.toolCallBudgets[sessionID] >= maxCalls {
		return false
	}
	m.toolCallBudgets[sessionID]++
	return true
}

// releaseToolCallBudget gives back one reservation for the caller session
// (a call refused synchronously after its reservation, e.g. invalid
// arguments or a losing pause-gate race).
func (m *SessionManager) releaseToolCallBudget(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if used, ok := m.toolCallBudgets[sessionID]; ok {
		if used <= 1 {
			delete(m.toolCallBudgets, sessionID)
		} else {
			m.toolCallBudgets[sessionID] = used - 1
		}
	}
}

// RemoteShim returns the currently registered remote shim (may be nil).
func (m *SessionManager) RemoteShim() RemoteShim {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.remoteShim
}

// RemoteShimForEnv returns the shim registered for the given environment key,
// falling back to the default shim. Returns nil when neither is registered.
func (m *SessionManager) RemoteShimForEnv(envKey string) RemoteShim {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.remoteShimForEnvLocked(envKey)
}

func (m *SessionManager) remoteShimForEnvLocked(envKey string) RemoteShim {
	if shim, ok := m.remoteShimsByEnv[envKey]; ok {
		return shim
	}
	return m.remoteShim
}

// remoteEnvForAdapter returns the environment key ("remote.<name>") the
// adapter declaration is bound to, resolving against the DECLARING graph so
// subworkflow adapters match the provisioning path (CRI-269). The second
// result is true only when that environment is REMOTE — callers use this to
// route dispatch, and a local environment binding must not be taken as
// remote (pre-refactor semantics).
func (m *SessionManager) remoteEnvForAdapter(instanceID string) (string, bool) {
	adapterNode, graph := m.adapterDeclaration(instanceID)
	if adapterNode == nil || graph == nil {
		return "", false
	}
	if !declaredAdapterEnvironmentIsRemote(adapterNode, graph) {
		return "", false
	}
	return declaredAdapterEnvironmentKey(adapterNode, graph)
}

// declaredAdapterEnvironmentKey resolves an adapter node's environment key
// against its declaring graph: the node's environment override, else the
// graph default. Returns ("", false) when the node or graph is nil or no
// environment is bound.
func declaredAdapterEnvironmentKey(adapterNode *workflow.AdapterNode, graph *workflow.FSMGraph) (string, bool) {
	envKey := adapterNode.Environment
	if envKey == "" {
		envKey = graph.DefaultEnvironment
	}
	if envKey == "" {
		return "", false
	}
	return envKey, true
}

// declaredAdapterEnvironmentIsRemote reports whether the adapter node is
// bound to a remote environment in its declaring graph. Env-less adapters
// (host-local by default) report false.
func declaredAdapterEnvironmentIsRemote(adapterNode *workflow.AdapterNode, graph *workflow.FSMGraph) bool {
	envKey, ok := declaredAdapterEnvironmentKey(adapterNode, graph)
	if !ok {
		return false
	}
	envNode, ok := graph.Environments[envKey]
	return ok && envNode.Type == "remote"
}

// remoteShimForAdapter returns the shim serving the remote environment the
// adapter is bound to. The second return is false when the adapter is not
// remote or no shim is registered for it.
func (m *SessionManager) remoteShimForAdapter(instanceID string) (RemoteShim, bool) {
	envKey, remote := m.remoteEnvForAdapter(instanceID)
	if !remote {
		return nil, false
	}
	m.mu.Lock()
	shim := m.remoteShimForEnvLocked(envKey)
	m.mu.Unlock()
	if shim == nil {
		return nil, false
	}
	return shim, true
}

// RegisterRemoteScope registers a rotated accept token for the given scope
// with the remote shim. It returns an error when no remote shim is registered.
//
// Legacy single-shim form; production callers must use
// RegisterRemoteScopeForEnv so per-environment shims receive their own
// tokens (CRI-293). Kept for tests and unknown-environment fallbacks.
// Deliberately carries no canonical Deprecated marker: a retained test
// call site exercises this fallback and staticcheck SA1019 would flag it
// (CRI-293 review).
func (m *SessionManager) RegisterRemoteScope(scope, token string) error {
	m.mu.Lock()
	shim := m.remoteShim
	m.mu.Unlock()
	if shim == nil {
		return errors.New("no remote shim registered")
	}
	shim.RegisterScope(scope, token)
	return nil
}

// UnregisterRemoteScope removes a scope's accept token so the shim rejects
// future reconnects with the old token.
//
// Deprecated: legacy single-shim form; use UnregisterRemoteScopeForEnv
// (CRI-293).
func (m *SessionManager) UnregisterRemoteScope(scope string) error {
	m.mu.Lock()
	shim := m.remoteShim
	m.mu.Unlock()
	if shim == nil {
		return errors.New("no remote shim registered")
	}
	shim.UnregisterScope(scope)
	return nil
}

// CloseRemoteHandle closes the active remote session for the given adapter
// type and scope. It is a no-op when no remote shim is registered.
//
// Deprecated: legacy single-shim form; use CloseRemoteHandleForEnv
// (CRI-293).
func (m *SessionManager) CloseRemoteHandle(ctx context.Context, adapterType, scope string) error {
	m.mu.Lock()
	shim := m.remoteShim
	m.mu.Unlock()
	if shim == nil {
		return nil
	}
	return shim.CloseHandle(ctx, adapterType, scope)
}

// RemoteListenAddr returns the shim's bound listen address, or "" when no
// remote shim is registered.
//
// Deprecated: legacy single-shim form returning only the most recently
// registered shim's address; production publication must use
// RemoteListenAddrForEnv so every environment receives its own shim's
// address (CRI-293).
func (m *SessionManager) RemoteListenAddr() string {
	m.mu.Lock()
	shim := m.remoteShim
	m.mu.Unlock()
	if shim == nil {
		return ""
	}
	return shim.ListenAddr()
}

// RegisterRemoteScopeForEnv registers a rotated accept token for the given
// scope with the shim serving envKey (CRI-293). It returns an error when no
// shim is registered for the environment.
func (m *SessionManager) RegisterRemoteScopeForEnv(envKey, scope, token string) error {
	m.mu.Lock()
	shim := m.remoteShimForEnvLocked(envKey)
	m.mu.Unlock()
	if shim == nil {
		return errors.New("no remote shim registered")
	}
	shim.RegisterScope(scope, token)
	return nil
}

// UnregisterRemoteScopeForEnv removes a scope's accept token from the shim
// serving envKey so it rejects future reconnects with the old token.
func (m *SessionManager) UnregisterRemoteScopeForEnv(envKey, scope string) error {
	m.mu.Lock()
	shim := m.remoteShimForEnvLocked(envKey)
	m.mu.Unlock()
	if shim == nil {
		return errors.New("no remote shim registered")
	}
	shim.UnregisterScope(scope)
	return nil
}

// CloseRemoteHandleForEnv closes the active remote session for the given
// adapter type and scope on the shim serving envKey. It is a no-op when no
// shim is registered for the environment.
func (m *SessionManager) CloseRemoteHandleForEnv(ctx context.Context, envKey, adapterType, scope string) error {
	m.mu.Lock()
	shim := m.remoteShimForEnvLocked(envKey)
	m.mu.Unlock()
	if shim == nil {
		return nil
	}
	return shim.CloseHandle(ctx, adapterType, scope)
}

// RemoteListenAddrForEnv returns the bound listen address of the shim serving
// envKey, or "" when no shim is registered for it (CRI-293: per-environment
// shim addresses are published to each environment's adapters).
func (m *SessionManager) RemoteListenAddrForEnv(envKey string) string {
	m.mu.Lock()
	shim := m.remoteShimForEnvLocked(envKey)
	m.mu.Unlock()
	if shim == nil {
		return ""
	}
	return shim.ListenAddr()
}

// SetLockfile provides the parsed lockfile so the session manager can
// resolve container images for adapters bound to container environments.
func (m *SessionManager) SetLockfile(lf *lockfile.Lockfile) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lockfile = lf
}

// GetLockfile returns the current lockfile (may be nil).
func (m *SessionManager) GetLockfile() *lockfile.Lockfile {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lockfile
}

// graphAdapterRef identifies a single adapter node in the compiled graph tree
// so VerifyGraph can walk the whole tree and attribute errors to a workflow
// directory.
type graphAdapterRef struct {
	instanceID string
	node       *workflow.AdapterNode
	graph      *workflow.FSMGraph
}

// collectGraphAdapters appends every adapter declared in g (and recursively in
// its subworkflows) to refs. Parent adapters appear before child adapters so the
// cached metadata reflects the first declaration when an instance is
// re-declared in a subworkflow.
func collectGraphAdapters(g *workflow.FSMGraph, refs *[]graphAdapterRef) {
	if g == nil {
		return
	}
	for _, id := range g.AdapterOrder {
		*refs = append(*refs, graphAdapterRef{
			instanceID: id,
			node:       g.Adapters[id],
			graph:      g,
		})
	}
	for _, name := range g.SubworkflowOrder {
		collectGraphAdapters(g.Subworkflows[name].Body, refs)
	}
}

// VerifyGraph eagerly verifies every adapter declared anywhere in the compiled
// graph tree before any step runs. It performs the same handshake and policy
// checks as per-scope Verify but does not store verified records, so session
// binding remains lazy at scope entry. Missing lockfile entries for OCI-backed
// adapters are reported as fatal errors naming the workflow directory, the
// adapter instance, and the remediation command.
func (m *SessionManager) VerifyGraph(ctx context.Context, graph *workflow.FSMGraph, vars map[string]cty.Value) error {
	if graph == nil {
		return nil
	}
	var refs []graphAdapterRef
	collectGraphAdapters(graph, &refs)

	for _, ref := range refs {
		if err := m.verifyGraphAdapter(ctx, graph, ref, vars); err != nil {
			return err
		}
	}
	return nil
}

func (m *SessionManager) verifyGraphAdapter(ctx context.Context, root *workflow.FSMGraph, ref graphAdapterRef, vars map[string]cty.Value) error {
	evalVars := vars
	if ref.graph != root {
		// For subworkflows, use the callee's declared variable defaults at
		// startup. Runtime input bindings are evaluated later at scope entry.
		evalVars = workflow.SeedVarsFromGraph(ref.graph)
	}

	// Re-evaluate adapter config so var.* values are available, but file()
	// content is served from the compile-time cache. This matches what
	// prepareScopeAdapter does at scope entry.
	config := ref.node.Config
	if len(ref.node.ConfigExprs) > 0 {
		opts := workflow.DefaultFunctionOptions(ref.graph.WorkflowDir)
		opts.FileCache = ref.graph.FileCache
		runtimeConfig, err := workflow.ResolveInputExprsWithOpts(ref.node.ConfigExprs, evalVars, opts)
		if err != nil {
			return fmt.Errorf("verify adapter %q in %q: evaluate config: %w", ref.instanceID, ref.graph.WorkflowDir, err)
		}
		config = runtimeConfig
	}

	secretMap, err := m.verifyGraphSecrets(ref, evalVars)
	if err != nil {
		return err
	}

	// The per-instance cache must be populated before any dispatch decision:
	// resolveAdapterHandle consults it for subworkflow adapters, whose
	// declarations never appear in the root graph's adapter map (CRI-269).
	m.cacheGraphAdapterRef(ref)

	// CRI-115: remote adapters with per_scope_sessions rotate their accept token
	// per scope in initScopeAdapters. Eager verification here would block before
	// the token exists, so we defer the adapter-info handshake to scope entry.
	if _, deferVerify := m.deferredRemoteAdapters[ref.instanceID]; !deferVerify {
		if _, err := m.verifyAdapterInfo(ctx, ref.instanceID, ref.node.Type, "", config, secretMap); err != nil {
			return fmt.Errorf("verify adapter %q in %q: %w; run 'criteria adapter lock %s'", ref.instanceID, ref.graph.WorkflowDir, err, ref.graph.WorkflowDir)
		}
	}

	return nil
}

func (m *SessionManager) cacheGraphAdapterRef(ref graphAdapterRef) {
	// CRI-50: mu-guarded. VerifyGraph runs before the run loop today, but
	// nothing serializes a future caller against concurrent adapter
	// resolution on the same SessionManager (sessions_graph_cache_race_test.go
	// pins exactly that overlap under -race).
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.graphAdapters == nil {
		m.graphAdapters = make(map[string]graphAdapterRef)
	}
	if _, ok := m.graphAdapters[ref.instanceID]; !ok {
		m.graphAdapters[ref.instanceID] = ref
	}
	if m.adapterDirs == nil {
		m.adapterDirs = make(map[string]string)
	}
	if _, ok := m.adapterDirs[ref.instanceID]; !ok {
		m.adapterDirs[ref.instanceID] = ref.graph.WorkflowDir
	}
}

// verifyGraphSecrets evaluates an adapter's secret expressions for VerifyGraph,
// using the compile-time file cache. Returns nil when the adapter declares no
// secrets.
func (m *SessionManager) verifyGraphSecrets(ref graphAdapterRef, vars map[string]cty.Value) (map[string]string, error) {
	if len(ref.node.Secrets) == 0 {
		return nil, nil
	}
	opts := workflow.DefaultFunctionOptions(ref.graph.WorkflowDir)
	opts.FileCache = ref.graph.FileCache
	out, err := workflow.ResolveInputExprsWithOpts(ref.node.Secrets, vars, opts)
	if err != nil {
		return nil, fmt.Errorf("verify adapter %q in %q: evaluate secrets: %w", ref.instanceID, ref.graph.WorkflowDir, err)
	}
	return out, nil
}

// SetAllowedWorkingDirRoots restricts the directories an environment may bind
// to. Empty (the default) disables the additional root check; paths containing
// ".." are always rejected.
func (m *SessionManager) SetAllowedWorkingDirRoots(roots []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.allowedRoots = append([]string(nil), roots...)
}

// ValidateWorkingDirFaceValue rejects a resolved working directory that is
// structurally invalid before the run starts. Paths containing ".." are always
// rejected. When allowed roots are configured, the directory must lie under one
// of them. A missing directory is *not* an error here: it will be deferred to
// session binding, allowing a workflow step to create it first.
func (m *SessionManager) ValidateWorkingDirFaceValue(dir string) error {
	if dir == "" {
		return nil
	}

	for _, part := range strings.Split(filepath.ToSlash(dir), "/") {
		if part == ".." {
			return fmt.Errorf("working directory %q contains \"..\"", dir)
		}
	}

	m.mu.Lock()
	roots := m.allowedRoots
	m.mu.Unlock()
	if len(roots) == 0 {
		return nil
	}

	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve working directory %q: %w", dir, err)
	}
	absDir = filepath.Clean(absDir)

	for _, root := range roots {
		root = filepath.Clean(root)
		if root == "" {
			continue
		}
		if root == string(filepath.Separator) {
			return nil
		}
		if absDir == root || strings.HasPrefix(absDir, root+string(filepath.Separator)) {
			return nil
		}
	}

	return fmt.Errorf("working directory %q is outside allowed roots", dir)
}

type Session struct {
	Name             string
	Adapter          string
	Config           map[string]string
	Secrets          map[string]string            // resolved secret values (WS13)
	SecretOriginRefs map[string]secrets.OriginRef // unevaluated origin refs for snapshot/restore (WS18)
	OnCrash          string
	Capabilities     []string // cached from plug.Info() at Open time
	PermissionState  *permissionState
	handle           Handle
	respawned        bool
	closing          atomic.Bool
	SandboxCleanup   func() // removes transient cgroup dirs, etc.
	// WorkingDir is the resolved environment working_directory (the adapter
	// process launch cwd), evaluated at adapter init. Persisted so respawn and
	// snapshot/restore relaunch the adapter in the same directory.
	WorkingDir string
	// AdapterDigest is the lockfile digest at the time the session was opened.
	AdapterDigest digest.Digest
	// ScopeInstanceID is the per-scope shim session key persisted for respawn
	// and snapshot/restore so the same scope continues to receive the same token.
	ScopeInstanceID string

	// WS15: session-level log stream lifecycle and heartbeat tracking.
	logMu          sync.Mutex
	cancelLog      func()
	logDone        <-chan error
	logHostCancel  chan struct{} // closed by the host before cancelLog(); per-stream
	logStreamAlive atomic.Bool
	hbMonitor      adapter.HeartbeatMonitor

	// CRI-271: lastEventNs records the unix-nano timestamp of the last
	// adapter activity observed for this session (log chunk, log-stream
	// heartbeat, or completed Execute). It powers the
	// idle_since_last_event field in the crash diagnostics so operators can
	// tell a long-silent provider connection from a mid-turn drop. Zero
	// means no activity has been recorded yet.
	lastEventNs atomic.Int64

	// CRI-271: crashed marks a session whose Execute was classified as a
	// session crash. Under the default on_crash=fail policy the session
	// stays registered but dead — every Execute on it replays the crash
	// error — so the engine consults ReopenCrashedSession before follow-on
	// steps. Cleared by a successful respawn/re-open.
	crashed atomic.Bool

	// CRI-271: reopenMu serializes concurrent re-open attempts for this
	// session so a parallel fan-out does not spawn several replacement
	// processes for one crash.
	reopenMu sync.Mutex

	// KB-155: refcounted registry of the per-Execute event sinks bound to
	// this session. When the adapter declares the concurrent_execute
	// capability, sibling executes fan out on the same session and nested
	// siblings sharing a caller's sink collapse onto one entry.
	// Session-level traffic is attributed only when exactly one sink is
	// bound — otherwise it falls back to structured logs rather than being
	// misattributed to an arbitrary execute.
	activeSinksMu sync.Mutex
	activeSinks   map[adapter.EventSink]int

	// KB-155: execTurns serializes executes on sessions that do not declare
	// the concurrent_execute capability: a call acquires the turn before
	// touching session-global state (step policy, sink bindings) and
	// releases it when done. Buffered to one and pre-filled at registration;
	// see acquireExecuteTurn.
	execTurns chan struct{}

	// WS15: MergeBuffer interleaves log and adapter events by timestamp.
	mergeBuf *log.MergeBuffer

	// WS17: session-level pause state for idempotency.
	paused  bool
	pauseMu sync.Mutex

	// CRI-202: the adapter's declared checkpoint-state surface, stamped at
	// session registration from the phase-1 handshake (InfoResponse.state).
	// Zero values mean the adapter declared no checkpointable state.
	stateMode        string
	stateSchema      string
	stateGranularity string

	// ckptMu serializes checkpoint captures for this session so a per-turn
	// save and a step-boundary save never issue concurrent Snapshot RPCs on
	// one adapter handle.
	ckptMu sync.Mutex
	// ckptInFlight marks an in-flight per-turn checkpoint save.
	ckptInFlight atomic.Bool
}

// pauseToolCallDrainTimeout returns the effective drain-first pause window
// for in-flight nested tool calls (CRI-169). Zero means the built-in default
// from the tunables registry.
func (m *SessionManager) pauseToolCallDrainTimeout() time.Duration {
	if m.PauseToolCallDrainTimeout <= 0 {
		return tunables.DefaultPauseToolCallDrainWindow
	}
	return m.PauseToolCallDrainTimeout
}

// Pause halts work on the session without losing state. It implements the
// drain-first pause posture (CRI-169, ADR-0004 §11): before the stream stops,
// the pause gate is set so no new nested tool call starts, then in-flight
// nested tool calls drain within a bounded window; calls that do not drain
// are canceled with a typed `canceled` reply (a wedged straggler that never
// settles is abandoned with a warning and audited at session close). Only
// then is the adapter handle paused and the permission stream stopped.
// Calling Pause on an already-paused session is a no-op.
//
// Rationale (recorded in ADR-0004 §11): nested in-flight Executes crossing a
// snapshot would require callee-session state capture, which is out of scope;
// draining first keeps snapshots to a quiescent permission state.
func (s *Session) Pause(ctx context.Context) error {
	s.pauseMu.Lock()
	defer s.pauseMu.Unlock()
	if s.paused {
		return nil
	}
	ps := s.PermissionState
	if ps != nil {
		// 1. Set the pause gate. New nested tool-call dispatch is refused
		// typed `paused` from here on; the pending set only shrinks.
		ps.beginToolCallPause()
		// 2. Drain in-flight nested calls within the bounded window; the
		// stragglers are canceled (typed `canceled` reply on every path)
		// while the stream is still active.
		if still := ps.awaitToolCallDrain(ps.toolCallDrainWindow()); len(still) > 0 {
			requestIDs := make([]string, 0, len(still))
			for _, call := range still {
				requestIDs = append(requestIDs, call.requestID)
			}
			slog.Warn("pause drain abandoned in-flight tool calls",
				"session", s.Name, "request_ids", requestIDs)
		}
	}
	if err := s.handle.Pause(ctx, s.Name); err != nil {
		if ps != nil {
			// The pause did not take effect at the adapter; lift the gate so
			// the caller is not wedged. Drained calls stay drained.
			ps.resumeToolCalls()
		}
		return err
	}
	// 3. Stop the permission stream. The gate is re-set atomically with the
	// stream flag; tool-call replies keep buffering on the stream channel
	// while paused.
	if ps != nil {
		ps.Pause()
	}
	s.paused = true
	return nil
}

// Resume continues the session from where it was paused.
// It resumes the permission state first, then calls the adapter handle.
// Calling Resume on an already-active session is a no-op.
func (s *Session) Resume(ctx context.Context) error {
	s.pauseMu.Lock()
	defer s.pauseMu.Unlock()
	if !s.paused {
		return nil
	}
	if s.PermissionState != nil {
		s.PermissionState.Resume()
	}
	if err := s.handle.Resume(ctx, s.Name); err != nil {
		return err
	}
	s.paused = false
	return nil
}

// isPaused reports whether the session is currently paused.
func (s *Session) isPaused() bool {
	s.pauseMu.Lock()
	defer s.pauseMu.Unlock()
	return s.paused
}

// Inspect returns a structured read-only view of the session's state.
func (s *Session) Inspect(ctx context.Context) (*v2.InspectResponse, error) {
	return s.handle.Inspect(ctx, s.Name)
}

// noteActivity records that the adapter showed signs of life (CRI-271).
// Called for log chunks, log-stream heartbeats, session open, respawn, and
// completed Executes.
func (s *Session) noteActivity() {
	s.lastEventNs.Store(time.Now().UnixNano())
}

// idleSinceLastEvent reports how long since the adapter's last observable
// activity (CRI-271). The second return is false when nothing has been
// recorded yet, in which case no idle claim can be made.
func (s *Session) idleSinceLastEvent() (time.Duration, bool) {
	last := s.lastEventNs.Load()
	if last == 0 {
		return 0, false
	}
	return time.Since(time.Unix(0, last)), true
}

// LastSessionActivity returns the time of the named session's last observable
// adapter activity (session open, log chunks, log-stream heartbeats,
// respawn, completed Execute — the CRI-271 activity sources). The second
// return is false when the session is unknown or has recorded no activity
// yet. Used by the engine's step stall watchdog (KB-25).
func (m *SessionManager) LastSessionActivity(name string) (time.Time, bool) {
	m.mu.Lock()
	sess, ok := m.sessions[name]
	m.mu.Unlock()
	if !ok {
		return time.Time{}, false
	}
	ns := sess.lastEventNs.Load()
	if ns == 0 {
		return time.Time{}, false
	}
	return time.Unix(0, ns), true
}

// NewSessionManager builds a SessionManager seeding the operator-configurable
// timing tunables from the tunables registry: the CRI-271 heartbeat stall
// threshold (CRITERIA_SESSION_HEARTBEAT_STALL) and the CRI-287 step-timeout
// teardown window (CRITERIA_STEP_TIMEOUT_TEARDOWN_WINDOW). Overrides are
// lenient — an unset, malformed, or non-positive value keeps the built-in
// default.
func NewSessionManager(loader Loader) *SessionManager {
	t := tunables.FromEnv()
	return &SessionManager{
		loader:                    loader,
		sessions:                  map[string]*Session{},
		verified:                  map[string]*verifiedRecord{},
		HeartbeatStallThreshold:   t.HeartbeatStallThreshold,
		StepTimeoutTeardownWindow: t.StepTimeoutTeardownWindow,
	}
}

// SetDeferredRemoteAdapters marks the given remote adapter instance IDs as ones
// whose adapter-info handshake should be skipped during VerifyGraph. This is
// required for remote environments that enable per_scope_sessions, where the
// per-scope accept token is not available until initScopeAdapters.
func (m *SessionManager) SetDeferredRemoteAdapters(instanceIDs []string) {
	if m.deferredRemoteAdapters == nil {
		m.deferredRemoteAdapters = make(map[string]struct{}, len(instanceIDs))
	}
	for _, id := range instanceIDs {
		m.deferredRemoteAdapters[id] = struct{}{}
	}
}

// SetSandboxProbeOverride sets the test hook that replaces sandbox.Probe()
// when evaluating sandbox requirements. It is intended for tests that need to
// simulate a host with missing sandbox primitives.
func (m *SessionManager) SetSandboxProbeOverride(fn func() sandbox.Capabilities) {
	m.sandboxProbeOverride = fn
}

// validateSandboxPrimitivesEagerly runs the host-side sandbox validation for
// instanceID without creating side effects. It resolves the sandbox policy,
// probes the host's sandbox primitives, and calls sandbox.Prepare with
// ValidateOnly set so no transient cgroup directories or other bind-time state
// is allocated. If the adapter is not bound to a sandbox environment or no
// policy exists, it returns nil. In strict mode a missing primitive (or any
// other Prepare validation failure) returns an error so the run fails at
// startup; in permissive mode the failure is logged and the function returns
// nil, matching the bind-time behavior.
func (m *SessionManager) validateSandboxPrimitivesEagerly(instanceID string) error {
	envNode, rp, ok := m.sandboxEnvAndPolicy(instanceID)
	if !ok {
		return nil
	}

	var adapterBinary, adapterType string
	if m.graph != nil {
		if adapterNode, ok := m.graph.Adapters[instanceID]; ok {
			adapterType = adapterNode.Type
			path, discoverErr := m.resolveAdapterBinaryForInstance(instanceID, adapterNode.Type)
			if discoverErr == nil {
				adapterBinary = path
			} else {
				var notFound *ErrAdapterNotFound
				if !errors.As(discoverErr, &notFound) {
					return fmt.Errorf("discover adapter binary: %w", discoverErr)
				}
				// Built-in adapter or missing plugin: leave adapterBinary empty.
				// The primitive-availability check does not depend on it.
			}
		}
	}

	caps := sandbox.Probe()
	if m.sandboxProbeOverride != nil {
		caps = m.sandboxProbeOverride()
	}
	if missing := caps.Missing(); len(missing) > 0 {
		slog.Info("sandbox primitives missing", "missing", missing, "instance", instanceID)
	}

	ctx := sandbox.PrepareContext{
		Policy:        rp,
		Env:           envNode,
		Caps:          caps,
		AdapterBinary: adapterBinary,
		AdapterType:   adapterType,
		ValidateOnly:  true,
	}
	_, err := sandbox.Handler{}.Prepare(ctx)
	if err != nil {
		if rp.PolicyMode == "strict" {
			return fmt.Errorf("sandbox strict mode: %w", err)
		}
		slog.Info("sandbox permissive degradation", "instance", instanceID, "error", err)
	}
	return nil
}

// resolveAdapterBinaryForInstance returns the local adapter binary path for
// the given adapter instance. When the instance is pinned in the lockfile the
// digest-addressed binary is returned; otherwise the caller gets the flat
// install-root binary. A missing binary is reported as *ErrAdapterNotFound so
// callers can decide whether that is fatal.
func (m *SessionManager) resolveAdapterBinaryForInstance(instanceID, adapterType string) (string, error) {
	if a := m.lockedAdapterFor(instanceID); a != nil && a.ResolvedDigest != "" {
		enc := EncodeDigest(digest.Digest(a.ResolvedDigest))
		return DiscoverBinaryAt(adapterType, enc)
	}
	return DiscoverBinary(adapterType)
}

// buildSandboxCustomizer returns a function that applies the sandbox
// configuration to an exec.Cmd, or nil if the adapter is not bound to a
// sandbox environment. The second return value is a cleanup function
// that removes transient resources (e.g. cgroup directories); it may be
// nil. The third return value is an error that is only non-nil when the
// sandbox policy is strict and a required primitive is unavailable; in
// that case the caller must abort the session before Resolve.
func (m *SessionManager) buildSandboxCustomizer(instanceID, workingDir string) (customizer func(name string, cmd *exec.Cmd), cleanup func(), err error) {
	envNode, rp, ok := m.sandboxEnvAndPolicy(instanceID)
	if !ok {
		return nil, nil, nil
	}

	// Resolve the adapter binary path so the sandbox profile can
	// pre-allowlist it at prepare time (avoids mutating the profile
	// inside ApplyToCmd). For lockfile-pinned (OCI) adapters this must use the
	// digest-addressed install path; flat discovery only sees dev/test binaries
	// placed directly in the adapter root.
	var adapterBinary, adapterType string
	if m.graph != nil {
		if adapterNode, ok := m.graph.Adapters[instanceID]; ok {
			adapterType = adapterNode.Type
			path, discoverErr := m.resolveAdapterBinaryForInstance(instanceID, adapterNode.Type)
			if discoverErr == nil {
				adapterBinary = path
			} else {
				var notFound *ErrAdapterNotFound
				if !errors.As(discoverErr, &notFound) {
					return nil, nil, fmt.Errorf("discover adapter binary: %w", discoverErr)
				}
				// Built-in adapter or missing plugin: leave adapterBinary empty.
				// For built-ins the customizer is never invoked; for missing
				// plugins ResolveWithCustomizer will fail later anyway.
			}
		}
	}

	caps := sandbox.Probe()
	if m.sandboxProbeOverride != nil {
		caps = m.sandboxProbeOverride()
	}
	if missing := caps.Missing(); len(missing) > 0 {
		slog.Info("sandbox primitives missing", "missing", missing, "instance", instanceID)
	}

	ctx := sandbox.PrepareContext{
		Policy:        rp,
		Env:           envNode,
		Caps:          caps,
		AdapterBinary: adapterBinary,
		AdapterType:   adapterType,
	}
	prep, err := sandbox.Handler{}.Prepare(ctx)
	if err != nil {
		if rp.PolicyMode == "strict" {
			return nil, nil, fmt.Errorf("sandbox strict mode: %w", err)
		}
		slog.Info("sandbox permissive degradation", "instance", instanceID, "error", err)
		return nil, nil, nil
	}

	shimBin, skipRestrictions := m.selectShimBin()
	customizer, cleanup = makeSandboxCustomizer(&prep, envNode, workingDir, shimBin, skipRestrictions)
	return customizer, cleanup, nil
}

// selectShimBin returns the shim binary path and whether the test-only helper
// relaxations should be applied. The production-shim override takes precedence
// over the test helper override and keeps restrictions enabled.
func (m *SessionManager) selectShimBin() (shimBin string, skipRestrictions bool) {
	if m.sandboxProductionShimBin != "" {
		return m.sandboxProductionShimBin, false
	}
	if m.sandboxShimBin != "" {
		return m.sandboxShimBin, true
	}
	return "", false
}

// sandboxEnvAndPolicy resolves the sandbox environment node and resolved
// policy for the given adapter instance. It returns false if the adapter
// is not bound to a sandbox environment or no policy is available.
func (m *SessionManager) sandboxEnvAndPolicy(instanceID string) (envNode *workflow.EnvironmentNode, rp *workflow.ResolvedPolicy, ok bool) {
	if m.graph == nil {
		return nil, nil, false
	}
	adapterNode, ok := m.graph.Adapters[instanceID]
	if !ok {
		return nil, nil, false
	}
	envKey := adapterNode.Environment
	if envKey == "" {
		envKey = m.graph.DefaultEnvironment
	}
	if envKey == "" {
		return nil, nil, false
	}
	envNode, ok = m.graph.Environments[envKey]
	if !ok {
		return nil, nil, false
	}
	if envNode.Type != "sandbox" {
		return nil, nil, false
	}
	cacheKey := instanceID + ":" + envKey
	rp, ok = m.graph.ResolvedPolicies[cacheKey]
	if !ok {
		return nil, nil, false
	}
	return envNode, rp, true
}

// buildCommandCustomizer composes the sandbox command customizer (if any) with a
// working-directory customizer derived from the bound environment. The
// environment's working_directory becomes the adapter process launch cwd, which
// shell/copilot adapters inherit as the default directory for their work.
//
// Container environments never reach this path (they launch via a container
// runner) and never carry a working_directory; remote environments apply their
// working_directory on the remote host. So this only adjusts cwd for locally
// launched shell and sandbox adapters.
func (m *SessionManager) buildCommandCustomizer(instanceID, workingDir string) (customizer func(name string, cmd *exec.Cmd), cleanup func(), err error) {
	sandboxCust, cleanup, err := m.buildSandboxCustomizer(instanceID, workingDir)
	if err != nil {
		return nil, nil, err
	}

	if workingDir == "" {
		return sandboxCust, cleanup, nil
	}

	customizer = func(name string, cmd *exec.Cmd) {
		if sandboxCust != nil {
			// Sandbox customizers set cmd.Env (scrubbed) and may set cmd.Dir.
			// Run it first, then override the launch cwd so it wins over the
			// sandbox default (ApplyToCmd only sets Dir when empty; the bwrap
			// path manages the inner cwd via --chdir).
			sandboxCust(name, cmd)
		} else {
			// No sandbox customizer, but providing any customizer flips
			// go-plugin's SkipHostEnv to true (see loader.go). Preserve the host
			// environment ourselves so the adapter keeps PATH and friends;
			// go-plugin still appends its handshake vars.
			cmd.Env = os.Environ()
		}
		cmd.Dir = workingDir
	}
	return customizer, cleanup, nil
}

func (m *SessionManager) Open(ctx context.Context, name, adapterName, onCrash string, config, secrets map[string]string) error {
	return m.OpenWithOriginRefs(ctx, name, adapterName, onCrash, config, secrets, nil, "")
}

// OpenWithOriginRefs opens an adapter session. workingDir is the resolved
// environment working_directory (evaluated at adapter init); it becomes the
// adapter process launch cwd. Pass "" for no working-directory override.
func (m *SessionManager) OpenWithOriginRefs(ctx context.Context, name, adapterName, onCrash string, config, secrets map[string]string, originRefs map[string]secrets.OriginRef, workingDir string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("session name is required")
	}
	if strings.TrimSpace(adapterName) == "" {
		return fmt.Errorf("session %q: adapter name is required", name)
	}

	m.mu.Lock()
	if _, exists := m.sessions[name]; exists {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrSessionAlreadyOpen, name)
	}
	m.mu.Unlock()

	customizer, cleanup, sandboxErr := m.buildCommandCustomizer(name, workingDir)
	if sandboxErr != nil {
		return fmt.Errorf("session %q: %w", name, sandboxErr)
	}

	var plug Handle
	var err error
	plug, err = m.resolveAdapterHandle(ctx, name, adapterName, "", customizer)
	if err != nil {
		if cleanup != nil {
			cleanup()
		}
		return err
	}

	// Cache capabilities so HasCapability can be called without a separate Info RPC.
	// On error, capabilities default to nil — the runtime gate rejects parallel use.
	var caps []string
	if info, infoErr := plug.Info(ctx); infoErr == nil {
		caps = append([]string(nil), info.Capabilities...)
		// CRI-202: capture the declared state surface too — the per-step
		// checkpoint save and the restore schema gate both read it.
		m.cacheAdapterInfo(name, &info.AdapterInfo)
	}

	if err := plug.OpenSession(ctx, name, config, secrets); err != nil {
		plug.Kill()
		if cleanup != nil {
			cleanup()
		}
		return err
	}

	return m.registerSession(ctx, name, adapterName, onCrash, config, secrets, originRefs, caps, plug, cleanup, workingDir, "")
}

// Verify performs phase-1 adapter verification without binding the adapter to
// its working directory. It resolves the adapter binary (or container/remote
// equivalent), validates the protocol handshake via Info, checks the runtime
// config against the adapter's manifest schema, and ensures required secrets
// are present. Throwaway verification handles are killed when the call
// returns; a peer-supervised handle (SupervisedHandle) wraps the peer's one
// live adapter child, so it is left running and the peer keeps owning its
// supervision + crash policy. Verification runs eagerly at scope start so
// broken adapters fail before any step executes.
//
// If a verified or bound record already exists for name (e.g. a parent-scope
// adapter re-declared in a subworkflow), Verify returns ErrSessionAlreadyOpen.
// The caller should treat that as a no-op for re-declared adapters.
func (m *SessionManager) Verify(ctx context.Context, name, adapterName, onCrash string, config, secrets map[string]string, originRefs map[string]secrets.OriginRef, workingDir, scopeName, scopeInstanceID string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("session name is required")
	}
	if strings.TrimSpace(adapterName) == "" {
		return fmt.Errorf("session %q: adapter name is required", name)
	}

	if err := m.checkDuplicateLocked(name); err != nil {
		return err
	}

	caps, err := m.verifyAdapterInfo(ctx, name, adapterName, scopeInstanceID, config, secrets)
	if err != nil {
		return err
	}

	return m.storeVerifiedRecord(name, adapterName, onCrash, config, secrets, originRefs, workingDir, caps, scopeName, scopeInstanceID)
}

// SessionOpen reports whether a session with the given name is already
// bound or verified — or leased from another manager's shared session
// (KB-156). Subworkflow bodies re-declare parent adapters for safety; the
// engine uses this to skip a second per-scope rotation (and its
// provision_wanted emission) for an adapter the parent scope already
// provisioned (CRI-145). A leased tool resource is served by the owner's
// shared session and never opened per-caller, so it counts as open: a
// parallel iteration that re-declares a parent tool resource skips its own
// rotation just like the KB-58 borrow's copied records did.
func (m *SessionManager) SessionOpen(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.sessions[name]; exists {
		return true
	}
	if _, exists := m.verified[name]; exists {
		return true
	}
	_, leased := m.leasedToolResources[name]
	return leased
}

// SessionBound reports whether a session with the given name is bound to a
// live adapter handle. Unlike SessionOpen it excludes verified-but-unbound
// records: CRI-202 resume uses it to decide between replaying a checkpoint
// into an already-open session and re-launching the adapter with prior
// state. A verified record has no handle to restore into, so its checkpoint
// must ride the full Restore launch sequence.
func (m *SessionManager) SessionBound(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, exists := m.sessions[name]
	return exists
}

// checkDuplicateLocked returns ErrSessionAlreadyOpen if the named session is
// already bound or verified. It is the caller's responsibility to hold no lock.
func (m *SessionManager) checkDuplicateLocked(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.sessions[name]; exists {
		return fmt.Errorf("%w: %s", ErrSessionAlreadyOpen, name)
	}
	if _, exists := m.verified[name]; exists {
		return fmt.Errorf("%w: %s", ErrSessionAlreadyOpen, name)
	}
	return nil
}

// verifyAdapterInfo performs the phase-1 adapter handshake in a neutral launch
// directory: it resolves the binary, calls Info, validates config and secrets,
// validates sandbox primitive availability for strict sandbox adapters, and
// kills the temporary handle. It returns the adapter capabilities on success.
func (m *SessionManager) verifyAdapterInfo(ctx context.Context, name, adapterName, scope string, config, secrets map[string]string) ([]string, error) {
	// Phase 1 uses a neutral launch directory. The working-directory-dependent,
	// side-effecting parts of sandbox setup (cgroup directory creation, the bwrap
	// --chdir to the resolved working directory) remain a bind-time concern. The
	// host-side primitive-availability and strict-mode validation can and do run
	// eagerly here so that a strict-sandbox adapter with missing primitives fails
	// before any step executes.
	plug, err := m.resolveAdapterHandle(ctx, name, adapterName, scope, nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		// A peer-supervised handle wraps the peer's one real adapter child,
		// not a throwaway verification handle: killing it would destroy the
		// live child and poison the peer's crash classification
		// (killRequested turns the next genuine crash into a graceful exit).
		// The peer owns its child's lifecycle (supervision + on_crash
		// policy), so phase-1 handshake verification leaves it running.
		if _, supervised := plug.(SupervisedHandle); !supervised {
			plug.Kill()
		}
	}()

	info, infoErr := plug.Info(ctx)
	if infoErr != nil {
		return nil, fmt.Errorf("adapter %q handshake: %w", adapterName, infoErr)
	}

	if stateErr := validateStateHandshake(adapterName, info.AdapterInfo.State); stateErr != nil {
		return nil, stateErr
	}

	if schemaErr := validateConfigAgainstSchema(config, info.AdapterInfo.ConfigSchema); schemaErr != nil {
		return nil, fmt.Errorf("adapter %q config: %w", adapterName, schemaErr)
	}

	if secretErr := validateRequiredSecrets(secrets, info.AdapterInfo.ConfigSchema); secretErr != nil {
		return nil, fmt.Errorf("adapter %q secrets: %w", adapterName, secretErr)
	}

	if sandboxErr := m.validateSandboxPrimitivesEagerly(name); sandboxErr != nil {
		return nil, fmt.Errorf("adapter %q sandbox validation: %w", adapterName, sandboxErr)
	}

	// Cache the declared schema surface for nested tool-call execution
	// (CRI-160). Callees verified but never step-targeted have no compiled
	// step to carry their schema, so the host keeps it here instead.
	m.cacheAdapterInfo(name, &info.AdapterInfo)

	return info.Capabilities, nil
}

// validateStateHandshake checks an adapter's checkpoint-state declaration
// (InfoResponse.state, CRI-201) against the host-side contract. An unknown
// mode fails LOUDLY at the handshake — quietly treating it as mode none
// would silently drop checkpointing for a stateful adapter — and blob/ref
// require a schema version tag so a restore can reject mismatched shapes.
func validateStateHandshake(adapterName string, d *workflow.StateDeclaration) error {
	if d == nil {
		return nil
	}
	if err := d.Validate(); err != nil {
		return fmt.Errorf("adapter %q handshake: state declaration: %w", adapterName, err)
	}
	return nil
}

// cacheAdapterInfo stores the adapter's captured declared surface. It backs
// the phase-1 verify handshake and the snapshot-restore relaunch (so a
// restored session keeps its declared surface without re-verifying).
// Thread-safe.
func (m *SessionManager) cacheAdapterInfo(name string, info *workflow.AdapterInfo) {
	if info == nil {
		return
	}
	captured := *info
	m.mu.Lock()
	if m.adapterInfos == nil {
		m.adapterInfos = make(map[string]*workflow.AdapterInfo)
	}
	m.adapterInfos[name] = &captured
	m.mu.Unlock()
}

// cachedAdapterInfo returns the AdapterInfo captured during the adapter's
// phase-1 handshake or snapshot-restore relaunch, or nil when the adapter
// has no cached surface (directly bound test fixtures). A nil or empty
// InputSchema on the captured surface means the adapter declares no input
// keys (any input key accepted); a non-empty surface is authoritative —
// keys it does not declare must not be delivered to the adapter.
// Thread-safe.
func (m *SessionManager) cachedAdapterInfo(name string) *workflow.AdapterInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.adapterInfos[name]
}

// DeclaredState returns the checkpoint-state declaration captured for the
// named session's adapter during its phase-1 handshake or snapshot-restore
// relaunch (InfoResponse.state, CRI-201). nil means the adapter declared no
// state (mode none): it checkpoints nothing and starts fresh on every
// (re)spawn. Unknown modes never reach here — they fail the handshake
// loudly (validateStateHandshake). Thread-safe.
func (m *SessionManager) DeclaredState(name string) *workflow.StateDeclaration {
	if info := m.cachedAdapterInfo(name); info != nil {
		return info.State
	}
	return nil
}

// storeVerifiedRecord stores a verified adapter record, guarding against races.
func (m *SessionManager) storeVerifiedRecord(name, adapterName, onCrash string, config, secrets map[string]string, originRefs map[string]secrets.OriginRef, workingDir string, capabilities []string, scopeName, scopeInstanceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.sessions[name]; exists {
		return fmt.Errorf("%w: %s", ErrSessionAlreadyOpen, name)
	}
	if _, exists := m.verified[name]; exists {
		return fmt.Errorf("%w: %s", ErrSessionAlreadyOpen, name)
	}

	rec := &verifiedRecord{
		name:             name,
		adapter:          adapterName,
		onCrash:          normalizeOnCrash(onCrash),
		config:           cloneConfig(config),
		secrets:          cloneConfig(secrets),
		secretOriginRefs: cloneOriginRefs(originRefs),
		capabilities:     append([]string(nil), capabilities...),
		workingDir:       workingDir,
		scopeName:        scopeName,
		scopeInstanceID:  scopeInstanceID,
	}
	if a := m.lockedAdapterFor(name); a != nil {
		rec.adapterDigest = digest.Digest(a.ResolvedDigest)
	}
	m.verified[name] = rec
	return nil
}

func (m *SessionManager) resolveAdapterHandle(ctx context.Context, name, adapterName, scope string, customizer func(string, *exec.Cmd)) (Handle, error) {
	// Remote-mode dispatch: if the adapter is bound to a remote environment,
	// wait for the adapter to phone home via the shim serving that
	// environment (CRI-293: per-environment shims).
	if shim, ok := m.remoteShimForAdapter(name); ok {
		return shim.WaitForHandle(ctx, adapterName, scope)
	}

	if dl, ok := m.loader.(*DefaultLoader); ok {
		// Dev-mode dispatch: adapters registered with `criteria adapter dev`
		// are keyed by "<type>.<name>" and take precedence over lockfile pins
		// so local development builds are always used when explicitly bound.
		if devPath, ok := dl.DevBinding(name); ok {
			discover := func(_ string) (string, error) { return devPath, nil }
			return dl.ResolveWithDiscovery(ctx, adapterName, discover, customizer)
		}

		// Container-mode dispatch: if the adapter is bound to a container
		// environment, use a docker/podman runner instead of a local binary.
		runnerFunc, containerErr := adapter.BuildContainerRunner(m.graph, m.lockfile, name)
		if containerErr != nil {
			return nil, containerErr
		}
		if runnerFunc != nil {
			return dl.ResolveWithRunnerFunc(ctx, adapterName, runnerFunc)
		}
		// Digest-addressed dispatch: when the instance is pinned in the
		// lockfile, resolve the exact binary by digest so that two instances of
		// the same adapter type at different versions launch distinct binaries.
		if a := m.lockedAdapterFor(name); a != nil && a.ResolvedDigest != "" {
			enc := EncodeDigest(digest.Digest(a.ResolvedDigest))
			discover := func(t string) (string, error) { return DiscoverBinaryAt(t, enc) }
			h, err := dl.ResolveWithDiscovery(ctx, adapterName, discover, customizer)
			if err != nil {
				dir := m.adapterDir(name)
				return nil, fmt.Errorf("adapter %q in %q (digest %s) could not be resolved: %w; run 'criteria adapter lock %s'", name, dir, a.ResolvedDigest, err, dir)
			}
			return h, nil
		}
		// An OCI-backed adapter without a matching lockfile entry must never
		// fall back to by-name discovery. Surface a clear error that names the
		// workflow directory, the adapter instance, and the remediation command.
		if m.isOCIAdapter(name) {
			dir := m.adapterDir(name)
			return nil, fmt.Errorf("adapter %q in %q is not pinned in the lockfile; run 'criteria adapter lock %s'", name, dir, dir)
		}
		return dl.ResolveWithCustomizer(ctx, adapterName, customizer)
	}
	return m.loader.Resolve(ctx, adapterName)
}

// lockedAdapterFor returns the lockfile entry for the adapter instance keyed by
// the session instanceID ("<type>.<name>"), matching BOTH type and name so that
// multiple versions of the same adapter type resolve to distinct entries.
func (m *SessionManager) lockedAdapterFor(instanceID string) *lockfile.LockedAdapter {
	if m.lockfile == nil {
		return nil
	}
	typ, nm, ok := strings.Cut(instanceID, ".")
	if !ok {
		return nil
	}
	for i := range m.lockfile.Adapters {
		a := &m.lockfile.Adapters[i]
		if a.Type == typ && a.Name == nm {
			return a
		}
	}
	return nil
}

// adapterDeclaration returns the adapter node for instanceID together with the
// graph that declares it. Root-graph declarations win so a re-declared instance
// keeps its parent binding (CRI-145); subworkflow declarations come from the
// per-instance cache populated by VerifyGraph (CRI-269: the root graph's
// adapter map never contains subworkflow adapters). CRI-50: the cache lookup
// takes mu so it cannot race VerifyGraph's cache writes.
func (m *SessionManager) adapterDeclaration(instanceID string) (*workflow.AdapterNode, *workflow.FSMGraph) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.adapterDeclarationLocked(instanceID)
}

// adapterDeclarationLocked is the mu-held core of adapterDeclaration.
func (m *SessionManager) adapterDeclarationLocked(instanceID string) (*workflow.AdapterNode, *workflow.FSMGraph) {
	if m.graph != nil {
		if node, ok := m.graph.Adapters[instanceID]; ok {
			return node, m.graph
		}
	}
	if ref, ok := m.graphAdapters[instanceID]; ok && ref.node != nil && ref.graph != nil {
		return ref.node, ref.graph
	}
	return nil, nil
}

// withRemoteWorkingDir returns the step to hand to the adapter handle,
// injecting the session's resolved environment working_directory as the
// "working_directory" input for remote adapters (CRI-270) that accept the
// key, so the key never reaches an adapter that does not honor it: an
// adapter must declare working_directory on a non-empty input surface, or
// (for a pre-schema binary with no declared surface) be of a type known to
// honor the input contract. A remote adapter process is launched by the
// remote host (e.g. a per-scope operator pod), not by this engine, so
// buildCommandCustomizer's launch-cwd path never applies to it and the
// directory only reaches the adapter through the per-step input key it
// already honors. Injection is additive: a step that declares its own
// working_directory input wins. The compiled step is never mutated —
// a shallow copy carries the augmented input map. Local and container
// adapters keep their customizer/runner cwd behavior and receive no
// injection.
func (m *SessionManager) withRemoteWorkingDir(sess *Session, step *workflow.StepNode) *workflow.StepNode {
	if step == nil || sess.WorkingDir == "" || !m.isRemoteAdapter(sess.Name) {
		return step
	}
	if _, ok := step.Input["working_directory"]; ok {
		return step
	}
	if !m.remoteWorkingDirAccepted(sess) {
		return step
	}
	cp := *step
	cp.Input = make(map[string]string, len(step.Input)+1)
	for k, v := range step.Input {
		cp.Input[k] = v
	}
	cp.Input["working_directory"] = sess.WorkingDir
	return &cp
}

// workingDirInputHonorers lists the adapter types known to honor the
// engine-authored "working_directory" per-step input key even when their
// binary declares no input surface (pre-schema releases; the CRI-270 remote
// delivery reuses the shell adapter's confinement-checked input contract).
// Dynamic-tool adapters (e.g. mcp) are deliberately absent: they declare no
// surface and forward every non-reserved input key onward as a tool
// argument, so an injected undeclared key would corrupt every call.
var workingDirInputHonorers = map[string]bool{
	"shell": true,
}

// remoteWorkingDirAccepted reports whether the target adapter accepts the
// engine-authored "working_directory" per-step input key. A non-empty
// declared input surface (cached from the adapter's phase-1 handshake, and
// re-captured at snapshot restore) is authoritative: the key is delivered
// only when the adapter declares it. An undeclared surface (nil/empty
// InputSchema — e.g. a dynamic-tool adapter) receives the key only from an
// adapter type known to honor the input contract, independently of the
// cache.
func (m *SessionManager) remoteWorkingDirAccepted(sess *Session) bool {
	if info := m.cachedAdapterInfo(sess.Name); info != nil && len(info.InputSchema) > 0 {
		_, declared := info.InputSchema["working_directory"]
		return declared
	}
	return workingDirInputHonorers[sess.Adapter]
}

// isRemoteAdapter returns true when the adapter declaration is bound to a
// remote environment (or the declaring graph's default environment is remote).
// The environment resolves against the DECLARING graph so subworkflow adapters
// bound to a per-scope remote env dispatch remotely at VerifyGraph and at bind
// time, matching the provisioning path (CRI-269).
func (m *SessionManager) isRemoteAdapter(instanceID string) bool {
	_, ok := m.remoteEnvForAdapter(instanceID)
	return ok
}

// isOCIAdapter reports whether the named adapter instance declares an OCI
// source. OCI adapters require a matching lockfile entry and may not fall back
// to by-name discovery. The lookup uses the graph tree cached by VerifyGraph so
// subworkflow adapters are covered even though the root graph does not contain
// them.
func (m *SessionManager) isOCIAdapter(instanceID string) bool {
	node, _ := m.adapterDeclaration(instanceID)
	return node != nil && node.Source != ""
}

// adapterDir returns the workflow directory associated with instanceID,
// defaulting to the graph's root directory when no cached directory exists.
// CRI-50: the cache lookup takes mu so it cannot race VerifyGraph's writes.
func (m *SessionManager) adapterDir(instanceID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.adapterDirs != nil {
		if dir, ok := m.adapterDirs[instanceID]; ok {
			return dir
		}
	}
	if m.graph != nil {
		return m.graph.WorkflowDir
	}
	return ""
}

// makeSandboxCustomizer builds the exec.Cmd customizer and cleanup from
// a prepared LinuxPrepared config. It handles both the bubblewrap and
// in-process shim paths. The shimBin argument overrides the binary used as
// the pre-exec shim; when empty the current process image (os.Args[0]) is
// used, which is how the production criteria CLI acts as its own shim.
// When skipRestrictions is true the test-only helper relaxations are
// applied (SkipShimRestrictions=true and namespaces cleared).
func makeSandboxCustomizer(prep *sandbox.LinuxPrepared, envNode *workflow.EnvironmentNode, workingDir, shimBin string, skipRestrictions bool) (customizer func(name string, cmd *exec.Cmd), cleanup func()) {
	cleanup = func() { _ = prep.Cleanup() }
	if bwrapCmd := sandbox.MaybeUseBubblewrap(prep, envNode, workingDir); bwrapCmd != nil {
		return func(_ string, cmd *exec.Cmd) {
			cmd.Path = bwrapCmd.Path
			cmd.Args = bwrapCmd.Args
			cmd.Env = bwrapCmd.Env
			cmd.Dir = bwrapCmd.Dir
			cmd.SysProcAttr = nil
		}, cleanup
	}
	return func(_ string, cmd *exec.Cmd) {
		// The real adapter path is already on cmd.Path at this point (set by
		// the loader from the resolved binary). Linux-only: if the sandbox
		// preparer could not resolve the path earlier, copy it into
		// TargetPath so the shim config is not serialized with an empty
		// executable path. On Darwin this field does not exist; Darwin's
		// ApplyToCmd uses cmd.Path directly.
		seedLinuxTargetPath(prep, cmd.Path)
		// Linux-only test shim relaxations (rlimit NPROC, namespace clearing).
		// No-op on Darwin and non-Linux.
		if skipRestrictions {
			configureLinuxShimForTest(prep, shimBin)
		}
		_ = prep.ApplyToCmd(cmd, shimBin)
		if skipRestrictions {
			finalizeLinuxShimCmd(cmd, shimBin)
		}
	}, cleanup
}

func (m *SessionManager) registerSession(ctx context.Context, name, adapterName, onCrash string, config, secrets map[string]string, originRefs map[string]secrets.OriginRef, caps []string, plug Handle, cleanup func(), workingDir, scopeInstanceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.sessions[name]; exists {
		_ = plug.CloseSession(ctx, name)
		plug.Kill()
		if cleanup != nil {
			cleanup()
		}
		return fmt.Errorf("%w: %s", ErrSessionAlreadyOpen, name)
	}
	sess := &Session{
		Name:             name,
		Adapter:          adapterName,
		Config:           cloneConfig(config),
		Secrets:          cloneConfig(secrets),
		SecretOriginRefs: cloneOriginRefs(originRefs),
		OnCrash:          normalizeOnCrash(onCrash),
		Capabilities:     caps,
		handle:           plug,
		SandboxCleanup:   cleanup,
		WorkingDir:       workingDir,
		ScopeInstanceID:  scopeInstanceID,
	}
	if a := m.lockedAdapterFor(name); a != nil {
		sess.AdapterDigest = digest.Digest(a.ResolvedDigest)
	}
	declared := m.declaredStateLocked(name)
	m.stampStateFields(sess, declared)
	m.sessions[name] = sess
	// KB-155: seed the serialized-execute turn (one token = one turn) and
	// the sink registry before the streams start delivering traffic.
	sess.execTurns = make(chan struct{}, 1)
	sess.execTurns <- struct{}{}
	sess.activeSinks = make(map[adapter.EventSink]int)
	sess.noteActivity()

	m.startPermissionStream(ctx, sess, plug)
	m.startLogStream(ctx, sess, plug)
	m.wireTurnCheckpoint(sess, ctx, declared)
	return nil
}

// bindVerifiedRecord performs phase-2 session binding for a previously verified
// adapter. It launches the adapter in its resolved working directory, opens the
// long-lived session, and starts the permission/log streams. On success the
// verified record is promoted to a bound session.
func (m *SessionManager) bindVerifiedRecord(ctx context.Context, rec *verifiedRecord, stepName string) error {
	customizer, cleanup, sandboxErr := m.buildCommandCustomizer(rec.name, rec.workingDir)
	if sandboxErr != nil {
		return fmt.Errorf("session %q: %w", rec.name, sandboxErr)
	}

	plug, err := m.resolveAdapterHandle(ctx, rec.name, rec.adapter, rec.scopeInstanceID, customizer)
	if err != nil {
		if cleanup != nil {
			cleanup()
		}
		return err
	}

	var caps []string
	if info, infoErr := plug.Info(ctx); infoErr == nil {
		caps = append([]string(nil), info.Capabilities...)
		// CRI-202: capture the declared state surface so per-scope sessions
		// phone-homed at bind time still checkpoint at step boundaries.
		m.cacheAdapterInfo(rec.name, &info.AdapterInfo)
	}

	if err := plug.OpenSession(ctx, rec.name, rec.config, rec.secrets); err != nil {
		plug.Kill()
		if cleanup != nil {
			cleanup()
		}
		return err
	}

	if err := m.registerSession(ctx, rec.name, rec.adapter, rec.onCrash, rec.config, rec.secrets, rec.secretOriginRefs, caps, plug, cleanup, rec.workingDir, rec.scopeInstanceID); err != nil {
		return err
	}

	if m.LifecycleSink != nil {
		m.LifecycleSink.OnAdapterLifecycle(rec.scopeName, rec.name, "opened", stepName)
	}
	return nil
}

// startPermissionStream starts the session-scoped Permissions stream if the
// adapter supports it. An existing PermissionState is preserved (CRI-169
// opportunistic fix, restored in WS18): on the snapshot/restore path the
// session already carries a rehydrated permission state, and replacing it
// here silently discarded the restored decisions. The pause drain window is
// stamped from the manager's configured value at every creation site.
func (m *SessionManager) startPermissionStream(ctx context.Context, sess *Session, plug Handle) {
	if sess.PermissionState == nil {
		sess.PermissionState = NewPermissionState(sess.Name, m.auditWriterForSessions())
	}
	sess.PermissionState.SetPauseDrainWindow(m.pauseToolCallDrainTimeout())
	if streamer, ok := plug.(PermissionStreamer); ok {
		cancel, err := streamer.StartPermissionStream(ctx, sess.Name, sess.PermissionState.Requests())
		if err != nil {
			slog.Warn("adapter permission stream start failed", "session", sess.Name, "err", err)
		} else {
			sess.PermissionState.SetStreamCancel(cancel)
		}
	}
}

// startLogStream starts the dedicated per-session Log stream if the adapter
// supports it.
func (m *SessionManager) startLogStream(ctx context.Context, sess *Session, plug Handle) {
	if starter, ok := plug.(LogStreamStarter); ok {
		m.beginLogStream(ctx, sess, starter)
	}
}

// beginLogStream starts the adapter Log stream for an open session, wires the
// heartbeat monitor, and launches the watcher. On error all stream state is
// cleared.
func (m *SessionManager) beginLogStream(ctx context.Context, sess *Session, starter LogStreamStarter) {
	logAdapterSink := &sessionLogAdapterSink{sess: sess}
	redactedLogSink := m.wrapSink(logAdapterSink)
	sess.mergeBuf = log.NewMergeBuffer(redactedLogSink, tunables.DefaultLogMergeDelay)
	logSink := &logForwardSink{
		sink: sess.mergeBuf,
		onHeartbeat: func() {
			sess.hbMonitor.Record()
			sess.noteActivity()
		},
	}
	cancel, done, err := starter.StartLogStream(ctx, sess.Name, logSink)
	if err != nil {
		slog.Warn("adapter log stream start failed", "session", sess.Name, "err", err)
		sess.mergeBuf.Close()
		sess.mergeBuf = nil
		sess.resetLogStreamState()
		return
	}

	// Each stream generation gets its own host-cancel channel. The host closes
	// it *before* calling cancel() so the watcher can distinguish host-initiated
	// teardown from an adapter that returned early from Log.
	hostCancel := make(chan struct{})
	var closed atomic.Bool
	wrappedCancel := func() {
		if closed.CompareAndSwap(false, true) {
			close(hostCancel)
		}
		if cancel != nil {
			cancel()
		}
	}

	sess.logMu.Lock()
	sess.cancelLog = wrappedCancel
	sess.logDone = done
	sess.logHostCancel = hostCancel
	sess.logMu.Unlock()
	sess.logStreamAlive.Store(true)
	sess.hbMonitor.Record()
	go m.watchLogStream(sess, done, hostCancel)
}

// watchLogStream waits for the adapter's Log stream to end. If it ends while
// the session is still open and the host did not cancel it, the adapter broke
// the contract that the log stream must remain open for the lifetime of the
// session; we log a clear diagnostic and disarm the heartbeat stall detector
// so the session is not falsely declared crashed later.
func (m *SessionManager) watchLogStream(sess *Session, done <-chan error, hostCancel <-chan struct{}) {
	select {
	case <-hostCancel:
		// Host-initiated teardown for this specific stream generation. Drain
		// any terminal error and return without touching logStreamAlive.
		select {
		case <-done:
		default:
		}
		return
	case err := <-done:
		// The stream ended without a host cancellation signal. Re-check
		// hostCancel in case both channels became ready at the same time and
		// the select chose done.
		select {
		case <-hostCancel:
			return
		default:
		}
		if sess.closing.Load() {
			return
		}
		sess.logStreamAlive.Store(false)
		if err != nil {
			slog.Warn("adapter log stream ended unexpectedly; heartbeat stall detector disarmed", "session", sess.Name, "adapter", sess.Adapter, "error", err)
		} else {
			slog.Warn("adapter log stream ended while session was open; heartbeat stall detector disarmed (adapter broke the log-stream contract)", "session", sess.Name, "adapter", sess.Adapter)
		}
	}
}

// SessionSharedError reports that a session close was refused because other
// callers still hold shared-session leases on it (KB-156): one caller's
// teardown must not rip the environment's shared session out from under the
// remaining callers. The close is a no-op; the session stays open until the
// owner scope tears it down once the last lease drops (or Shutdown wins).
type SessionSharedError struct {
	Session string
	Leases  int
}

func (e *SessionSharedError) Error() string {
	return fmt.Sprintf("session %q shared by %d active lease(s); close refused until last lease releases (KB-156)", e.Session, e.Leases)
}

// Close is intentionally idempotent: closing an unknown session is a no-op.
// KB-156: a close for a session with outstanding shared-tool-resource leases
// is refused (SessionSharedError) and the session stays open.
func (m *SessionManager) Close(ctx context.Context, name string) error {
	m.mu.Lock()
	sess, exists := m.sessions[name]
	// KB-156: a tool-resource session shared by outstanding leases must not
	// be torn down out from under its callers. Nested-call lifetimes are
	// strictly shorter than the owner scope's, so this is only reachable via
	// out-of-protocol callers — refuse loudly and keep the session in place.
	if leases := m.toolResourceLeases[name]; exists && leases > 0 {
		m.mu.Unlock()
		slog.Warn("refusing close of shared tool-resource session with active leases",
			"session", name, "leases", leases)
		return &SessionSharedError{Session: name, Leases: leases}
	}
	if exists {
		delete(m.sessions, name)
	}
	// Verified-but-never-bound adapters are tracked for per-scope teardown via
	// initScopeAdapters/tearDownScopeAdapters. Remove the verified record so a
	// later scope reusing the same instance ID can verify and bind afresh.
	delete(m.verified, name)
	m.mu.Unlock()

	if !exists {
		return nil
	}
	sess.closing.Store(true)
	sess.logStreamAlive.Store(false)
	if sess.PermissionState != nil {
		sess.PermissionState.Stop()
	}
	sess.logMu.Lock()
	cancelLog := sess.cancelLog
	sess.logMu.Unlock()
	if cancelLog != nil {
		cancelLog()
	}
	if sess.mergeBuf != nil {
		sess.mergeBuf.Close()
	}
	if sess.SandboxCleanup != nil {
		sess.SandboxCleanup()
	}
	err := sess.handle.CloseSession(ctx, name)
	sess.handle.Kill()
	return err
}

// PauseAll iterates over every open session and calls Pause on each.
// It is reentrant and idempotent: pausing an already-paused session is a no-op.
// Every session is paused even when some fail; the returned error joins the
// per-session failures (the KB-96 pause barrier lands only when ALL sessions
// acked, so the aggregate must report each one, not just the first).
func (m *SessionManager) PauseAll(ctx context.Context) error {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()

	var errs []error
	for _, s := range sessions {
		if err := s.Pause(ctx); err != nil {
			errs = append(errs, fmt.Errorf("session %q: %w", s.Name, err))
		}
	}
	return errors.Join(errs...)
}

// ResumeAll iterates over every open session and calls Resume on each.
// It is reentrant and idempotent: resuming an already-active session is a no-op.
// Mirrors PauseAll: every session is resumed and the returned error joins the
// per-session failures.
func (m *SessionManager) ResumeAll(ctx context.Context) error {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()

	var errs []error
	for _, s := range sessions {
		if err := s.Resume(ctx); err != nil {
			errs = append(errs, fmt.Errorf("session %q: %w", s.Name, err))
		}
	}
	return errors.Join(errs...)
}

// InspectSession returns the inspect response for a single session by name.
func (m *SessionManager) InspectSession(ctx context.Context, name string) (*v2.InspectResponse, error) {
	sess, err := m.lookup(name)
	if err != nil {
		return nil, err
	}
	return sess.Inspect(ctx)
}

func (m *SessionManager) wrapSink(sink adapter.EventSink) adapter.EventSink {
	if m.RedactionRegistry == nil {
		return sink
	}
	return &secrets.RedactingEventSink{Registry: m.RedactionRegistry, Inner: sink}
}

// auditWriterForSessions returns the audit writer the sessions' permission
// states write to: m.Audit wrapped in redaction (CRI-163) when a redaction
// registry is configured, so sensitive values — callee outputs echoed into a
// tool string or an adapter error message — never reach the audit log in
// plaintext.
func (m *SessionManager) auditWriterForSessions() AuditWriter {
	return NewRedactingAuditWriter(m.Audit, m.RedactionRegistry)
}

func (m *SessionManager) registerSensitiveOutputs(result adapter.Result, step *workflow.StepNode) {
	if m.RedactionRegistry == nil {
		return
	}
	for outName, outVal := range result.Outputs {
		if f, ok := step.OutputSchema[outName]; ok && f.Sensitive {
			rendered, err := workflow.RenderOutputValue(outVal)
			if err != nil {
				continue
			}
			m.RedactionRegistry.Register(rendered)
		}
	}
}

// classifySessionCrash names the likely cause of an adapter session crash so
// engine logs answer "which session died and why" (CRI-271). The returned
// string is always one of the exported CrashReason* constants in
// crashreason.go — the single source of truth also consumed by peer
// supervision journal emission (peer.proto CrashClassified.reason, T-05/T-07).
// A supervision-delivered classification comes first (T-07): the peer
// observed the child directly and its CrashClassified.reason is consumed
// verbatim — no new strings, no reinterpretation. The adapter-process
// checks and message heuristics then serve connections without supervision:
// a dead process is the most precise diagnosis, and the message heuristics
// distinguish the transport failure shapes the go-plugin/gRPC stack produces
// (legacy runners and peers without a Supervise stream).
func classifySessionCrash(sess *Session, execErr error) string {
	if sess != nil {
		if reason, ok := SupervisionCrashReason(sess.handle); ok {
			return reason
		}
		if ProcessExited(sess.handle) {
			return CrashReasonProcessExitedEarly
		}
	}
	if execErr == nil {
		return CrashReasonUnknown
	}
	msg := strings.ToLower(execErr.Error())
	switch {
	case strings.Contains(msg, "heartbeat stall"):
		return CrashReasonHeartbeatStall
	case strings.Contains(msg, "transport is closing"),
		strings.Contains(msg, "the client connection is closing"):
		return CrashReasonTransportClosed
	case strings.Contains(msg, "unavailable"):
		return CrashReasonEndpointUnavailable
	case strings.Contains(msg, "broken pipe"):
		return CrashReasonStdioPipeBroken
	case strings.Contains(msg, "eof"):
		return CrashReasonStdioEOF
	case strings.Contains(msg, "terminated"):
		return CrashReasonProcessTerminated
	}
	return CrashReasonUnknownAdapterError
}

// crashDiagnostics builds the shared crash-reason fields for logs and sink
// events (CRI-271): the named cause plus, when known, the idle window since
// the adapter's last event — the "silent gap" between the last streamed
// token and the teardown.
func (s *Session) crashDiagnostics(reason string) []any {
	args := []any{"crash_reason", reason}
	if idle, ok := s.idleSinceLastEvent(); ok {
		args = append(args, "idle_since_last_event", idle.String())
	}
	return args
}

// idleStringOrEmpty renders the idle-since-last-event duration, or "" when
// the session recorded no activity yet (used inside sink event payloads,
// which always carry every key).
func idleStringOrEmpty(sess *Session) string {
	if idle, ok := sess.idleSinceLastEvent(); ok {
		return idle.String()
	}
	return ""
}

func (m *SessionManager) handleCrash(ctx context.Context, name string, step *workflow.StepNode, sink adapter.EventSink, sess *Session, execErr error, rejection *v2.ExecutionRejection) (adapter.Result, error) {
	reason := classifySessionCrash(sess, execErr)
	sess.crashed.Store(true)
	slog.Warn("adapter session crashed",
		append([]any{"session", sess.Name, "adapter", sess.Adapter, "error", execErr}, sess.crashDiagnostics(reason)...)...)

	// The effective crash policy is the step's (the compiler resolves it as
	// step-overrides-adapter), falling back to the session's adapter-level policy.
	// Without this, an on_crash declared only on the step would be ignored at
	// runtime (sess.OnCrash is captured from the adapter block at open time).
	onCrash := sess.OnCrash
	if step != nil && step.OnCrash != "" {
		onCrash = normalizeOnCrash(step.OnCrash)
	}

	switch onCrash {
	case OnCrashRespawn:
		sink.Adapter("session.respawned", map[string]any{
			"session":               sess.Name,
			"adapter":               sess.Adapter,
			"error":                 execErr.Error(),
			"crash_reason":          reason,
			"idle_since_last_event": idleStringOrEmpty(sess),
		})
		if respawnErr := m.respawn(ctx, sess); respawnErr != nil {
			return m.failResult(sink, sess, fmt.Errorf("respawn after crash failed: %w (original crash: %w)", respawnErr, execErr))
		}
		return m.retryAfterRespawn(ctx, name, step, sink, sess, rejection)
	case OnCrashAbortRun:
		sink.Adapter("session.crash", map[string]any{
			"session":               sess.Name,
			"adapter":               sess.Adapter,
			"policy":                onCrash,
			"error":                 execErr.Error(),
			"crash_reason":          reason,
			"idle_since_last_event": idleStringOrEmpty(sess),
		})
		return adapter.Result{Outcome: "failure"}, &FatalRunError{Err: fmt.Errorf("session %q crashed and on_crash=abort_run", name)}
	default:
		return m.failResult(sink, sess, execErr)
	}
}

// retryAfterRespawn re-executes the step on the freshly respawned session.
// The retried attempt's verdict is contract-validated before it becomes the
// step's result; a contract-invalid retry returns to the engine's attempt
// loop (via OutcomeInvalidError) instead of latching a hard run failure. The
// host-synthesized fallback is exempt: it was validated at synthesis, and
// re-applying the fallback's own schema/require_comment to the empty
// synthesis is unsatisfiable by construction (KB-45).
func (m *SessionManager) retryAfterRespawn(ctx context.Context, name string, step *workflow.StepNode, sink adapter.EventSink, sess *Session, rejection *v2.ExecutionRejection) (adapter.Result, error) {
	retrySink := sink
	if sess.mergeBuf != nil {
		retrySink = sess.mergeBuf
	}
	result, retryErr := sess.handle.Execute(ctx, name, step, retrySink, rejection)
	if retryErr == nil {
		valid := result
		if !result.SynthesizedFallback {
			var issues []string
			valid, issues = evaluateLocalOutcomeContracts(step, result)
			if len(issues) > 0 {
				return adapter.Result{}, &OutcomeInvalidError{Outcome: result.Outcome, Issues: issues}
			}
		}
		m.registerSensitiveOutputs(valid, step)
		return valid, nil
	}
	var invErr *OutcomeInvalidError
	if errors.As(retryErr, &invErr) {
		return adapter.Result{}, invErr
	}
	return m.failResult(sink, sess, retryErr)
}

// lookupOrBind returns the session for name, binding a verified-only adapter
// on first use (ErrUnknownSession from lookup). Any hard lookup error is
// returned as-is.
func (m *SessionManager) lookupOrBind(ctx context.Context, name string, step *workflow.StepNode) (*Session, error) {
	sess, err := m.lookup(name)
	if err == nil {
		return sess, nil
	}
	if !errors.Is(err, ErrUnknownSession) {
		return nil, err
	}
	return m.bindVerifiedAndLookup(ctx, name, step)
}

// bindVerifiedAndLookup promotes a verified-only adapter to a bound session.
// It serializes concurrent attempts so only one caller performs the bind and
// the rest wait and then use the bound session. A binding error is wrapped
// with the adapter name, step name, and working directory so failures at first
// use are actionable.
func (m *SessionManager) bindVerifiedAndLookup(ctx context.Context, name string, step *workflow.StepNode) (*Session, error) {
	m.bindMu.Lock()
	defer m.bindMu.Unlock()

	// Another goroutine may have bound the session while we were waiting.
	if sess, err := m.lookup(name); err == nil {
		return sess, nil
	}

	m.mu.Lock()
	rec := m.verified[name]
	m.mu.Unlock()
	if rec == nil {
		return nil, ErrUnknownSession
	}

	stepName := ""
	if step != nil {
		stepName = step.Name
	}
	if bindErr := m.bindVerifiedRecord(ctx, rec, stepName); bindErr != nil {
		return nil, fmt.Errorf("bind adapter %q for step %q in working directory %q: %w", rec.name, stepName, rec.workingDir, bindErr)
	}

	m.mu.Lock()
	delete(m.verified, name)
	m.mu.Unlock()

	return m.lookup(name)
}

// Execute runs a step against the named adapter session, binding a
// verified-only session lazily on first use.
//
// Locking contract (CRI-160, nested adapter tool calls): Execute holds no
// lock across the adapter call itself — bindMu is taken only inside
// bindVerifiedAndLookup during the bind phase, and mu is taken only for short
// map operations. A nested Execute issued under this one (a callee session
// running while the caller's Execute is in flight) is therefore safe on a
// DIFFERENT session: the nested call serializes on bindMu only if it must
// bind, never while the caller holds it. Callee session != caller session is
// guaranteed by the CRI-159 self-call rejection, so the nested call cannot
// re-enter the caller session's Execute.
//
// Ordering invariant: never block the caller session's stream while holding a
// lock the nested Execute needs — that would deadlock the tool call. The
// nested path (permissionInterceptSink.dispatchNestedToolCall) holds no
// SessionManager locks while dispatching, executes the callee on its own
// goroutine (CRI-161), and replies on the caller's Permissions stream with
// non-blocking channel sends.
//
// CRI-161: nested tool-call executes are asynchronous, so execute waits for
// them (permSink.waitPending) immediately after the adapter call returns —
// before any sink latch is read or the sink unbound — so every nested
// outcome (result delivery, fatal error latch, typed failure) is settled and
// observed before the step completes.
//
// nesting carries the per-call tool-call nesting state (CRI-162): the number
// of nested tool-call Executes above this one (0 for a step-level Execute)
// plus the caller adapter ref chain visited so far, seeded with the
// executing step's own adapter ref.
func (m *SessionManager) Execute(ctx context.Context, name string, step *workflow.StepNode, sink adapter.EventSink, rejection *v2.ExecutionRejection) (adapter.Result, error) {
	// The chain is seeded with the executing step's own adapter ref — the
	// caller's baseline on the call chain.
	seed := name
	if step != nil && step.AdapterRef != "" {
		seed = step.AdapterRef
	}
	return m.execute(ctx, name, step, sink, toolCallNesting{chain: []string{seed}}, rejection)
}

func (m *SessionManager) execute(ctx context.Context, name string, step *workflow.StepNode, sink adapter.EventSink, nesting toolCallNesting, rejection *v2.ExecutionRejection) (adapter.Result, error) {
	// KB-156: a tool resource leased from another manager's environment
	// executes through the owner's shared session — one adapter session per
	// environment, created once by the host-of-record manager. This manager
	// only routes the call; the owner owns the bind, the execute gating, the
	// crash classification (callee crash attribution lands on the shared
	// session, not one caller), and the teardown anchor. Lock ordering stays
	// one-directional: delegation takes no m.mu lock into the owner (the
	// lease map was read under m.mu before this call).
	if owner := m.leaseOwner(name); owner != nil {
		// Mirror a caller-side step-timeout teardown window onto the owner
		// (CRI-287): the engine stamps the window on the manager that ran the
		// canceled step, but a delegated execute classifies on the owner's
		// window. Without the mirror a teardown cascade observed on the
		// shared session would be misclassified as a crash and respawn it
		// out from under the remaining callers.
		owner.mirrorStepTimeoutTeardownWindow(m)
		return owner.execute(ctx, name, step, sink, nesting, rejection)
	}
	sess, err := m.lookupOrBind(ctx, name, step)
	if err != nil {
		return adapter.Result{Outcome: "failure"}, err
	}

	// CRI-270: remotely dispatched adapters never see the launch-cwd
	// customizer, so deliver the session's resolved working_directory
	// through the input contract (per-step input wins).
	step = m.withRemoteWorkingDir(sess, step)

	sink = m.wrapSink(sink)

	// WS15: heartbeat-stall detection. If no heartbeat has been received for
	// longer than the stall threshold while the log stream is still alive,
	// treat the session as crashed before attempting the step. If the log stream
	// ended early (contract violation) watchLogStream has already disarmed the
	// detector, so we never declare a crash on a heartbeat the adapter was not
	// sending.
	if sess.logStreamAlive.Load() && sess.hbMonitor.Stalled(m.heartbeatStallThreshold()) {
		return m.handleCrash(ctx, name, step, sink, sess, fmt.Errorf("heartbeat stall (>%s)", m.heartbeatStallThreshold()), rejection)
	}

	// KB-155: serialize executes on sessions that never declared the
	// concurrent_execute capability. The turn is taken before any
	// session-global state (step policy, sink bindings) is touched so a
	// queued caller can never observe an in-flight sibling's state;
	// multiplexable sessions skip the gate and fan out freely.
	queued, gateErr := m.acquireExecuteTurn(ctx, sess)
	if gateErr != nil {
		return adapter.Result{}, gateErr
	}
	defer m.releaseExecuteTurn(sess)
	// KB-155 abandon policy: a caller whose turn arrives after its own
	// cancellation abandons before any adapter-observable state (the defer
	// handed the turn on) — only when the cancellation is attributable to
	// this call: it either queued behind a sibling or was dispatched while
	// its originating Execute context was still alive (issue-time liveness
	// recorded by startNestedToolCall). A nested follow-up issued after the
	// context already died keeps the CRI-161 no-wedge contract: the callee
	// still runs and delivers its typed reply.
	if ctx.Err() != nil && (queued || nesting.issuedWhileExecAlive) {
		return adapter.Result{}, ctx.Err()
	}

	// KB-155: the execute carries its own step policy so concurrent executes
	// on one multiplexed session each decide under their own allow_tools +
	// environment policy instead of a session-global last-writer snapshot.
	// The session-global snapshot keeps its legacy role for surfaces that
	// evaluate outside an in-flight execute (restore re-present).
	stepPolicy := m.combinedPolicyFor(sess, step)
	m.setStepPolicy(sess, step)

	m.bindActiveSink(sess, sink)
	defer m.unbindActiveSink(sess, sink)

	// KB-155: execute-level adapter events (mcp.progress, mcp.content, ...)
	// flow straight through this execute's (already redaction-wrapped) sink
	// so they keep their per-Execute identity. They are never diverted into
	// the session log merge buf, whose flush routes through
	// sessionLogAdapterSink -> singleActiveSink: that attribution is
	// ambiguous the moment two executes share the session (a
	// concurrent-caller event would be demoted to structured logs, and
	// delivery delayed up to the 500ms merge window), so it can no longer
	// carry execute-level traffic under multiplexing. mergeBuf remains on
	// the log-stream path only.
	permSink := newPermissionInterceptSink(ctx, sink, sess, step, m.graph, m, nesting, stepPolicy)

	result, execErr := sess.handle.Execute(ctx, name, step, permSink, rejection)

	// CRI-161: nested tool calls were dispatched on their own goroutines;
	// wait for them to settle (and deliver their replies) before reading the
	// sink's latches or unbinding the sink. No lock is held here, so the
	// nested goroutine's own manager interactions cannot deadlock against
	// this wait.
	permSink.waitPending()

	return m.finishExecute(ctx, name, step, sess, sink, permSink, result, execErr)
}

// finishExecute applies the post-execute pipeline after waitPending: verdict
// validation, outcome override, and the success postlude.
func (m *SessionManager) finishExecute(ctx context.Context, name string, step *workflow.StepNode, sess *Session, sink adapter.EventSink, permSink *permissionInterceptSink, result adapter.Result, execErr error) (adapter.Result, error) {
	// KB-45: validate the verdict against the step's outcome contracts
	// BEFORE the permission override and any downstream mapping, so a
	// permission-denied success cannot launder an invalid payload. Legacy
	// (contract-less) steps pass through untouched. Only a final verdict is
	// a candidate: when the Execute call itself failed (adapter error or
	// transport death) there is no verdict to validate — the error keeps its
	// crash classification, and the engine's attempt loop resets the repair
	// context on it. A host-synthesized fallback skips this re-validation
	// too: it was validated at synthesis, and re-applying the fallback's
	// own schema/require_comment to the empty synthesis is unsatisfiable by
	// construction.
	if execErr == nil && !result.SynthesizedFallback {
		validated, issues := evaluateLocalOutcomeContracts(step, result)
		if len(issues) > 0 {
			return adapter.Result{}, &OutcomeInvalidError{Outcome: result.Outcome, Issues: issues}
		}
		result = validated
	}

	m.maybeOverrideOutcome(permSink, &result)

	if execErr != nil {
		return m.executeError(ctx, name, step, sess, sink, result, execErr)
	}

	// A completed call proves the session transport was alive (CRI-271).
	// It deliberately does NOT end the CRI-287 step-timeout teardown
	// window: a healthy sibling's success is not evidence that a
	// torn-down sibling has been observed, so the window stays open until
	// it expires (see the engineStepTimeoutTeardownAt comment).
	sess.noteActivity()
	// A nested callee Execute that crashed with on_crash=abort_run latches
	// its fatal error on the sink (CRI-160): the callee's own crash policy
	// governs its session, so the error propagates to the engine instead
	// of the caller's Execute reporting success.
	if fatalErr := permSink.nestedFatal(); fatalErr != nil {
		return result, fatalErr
	}
	m.registerSensitiveOutputs(result, step)
	if err := m.checkpointAfterExecute(ctx, sess); err != nil {
		return result, &FatalRunError{Err: err}
	}
	return result, nil
}

// executeError classifies a failed Execute call: expected closes (an explicit
// Close/Shutdown or a host-canceled context) and plain step errors are
// returned as-is; likely session crashes route to handleCrash (CRI-271).
func (m *SessionManager) executeError(ctx context.Context, name string, step *workflow.StepNode, sess *Session, sink adapter.EventSink, result adapter.Result, execErr error) (adapter.Result, error) {
	// KB-45: a contract-rejected verdict is not a crash or transport
	// failure; it is returned to the engine's attempt loop verbatim before
	// any crash classification can consume it.
	var invErr *OutcomeInvalidError
	if errors.As(execErr, &invErr) {
		return adapter.Result{}, invErr
	}

	// An explicit Close/Shutdown (closing flag) or a host-canceled context
	// (run timeout, user abort) both cause the gRPC stream to produce
	// EOF/broken-pipe errors. Check this before the string heuristic so
	// neither case is misclassified as a crash.
	if sess.closing.Load() || ctx.Err() != nil {
		slog.Debug("adapter stream closed (expected)", "session", sess.Name, "adapter", sess.Adapter)
		return result, execErr
	}

	// CRI-287: a transport close observed while the step-timeout teardown
	// window is open is the consequence of the engine-initiated cancellation
	// (the canceled Execute stream tears down sibling phone-home transports
	// in the same second), not an adapter death. Return the error as-is so
	// the step's declared failure/default outcome routing (the checkpoint
	// loop) proceeds instead of the crash machinery terminating the run. The
	// window is bound to the cascade (see the engineStepTimeoutTeardownAt
	// comment), so crashes outside it keep the hard-failure classification
	// (CRI-271). Deaths with positive evidence are never downgraded for
	// legacy-runner handles: ProcessExited is the verifiable "adapter is
	// dead" signal — the transport-close classification only applies while
	// the process is still running — and a host-initiated close (closing
	// flag) is the expected-close path above; both fall through to crash
	// classification.
	//
	// T-07 peer carve-out: on a peer-supervised handle the ProcessExited
	// fact is supervision-delivered, and the supervised child can die as a
	// direct consequence of the same teardown cascade (the engine's canceled
	// turn takes the child down with it). Inside the open window that
	// ProcessExited is therefore treated like the transport-close evidence
	// above — a teardown consequence, routed as timeout, not a crash. The
	// window bounds the carve-out: after it expires a follow-on Execute on
	// the dead peer handle fails and classifies the crash from the
	// journal-delivered wire fact (SupervisionCrashReason), which then
	// reaches the crash machinery with the full wire-fact reason. The
	// classifySessionCrash call in the diagnostics below feeds evidence
	// fields only: the returned error stays the raw transport error, so the
	// outcome is still routed as a timeout.
	if m.engineStepTimeoutTeardownWindowOpen() &&
		isLikelySessionCrash(sess, execErr) &&
		!sess.closing.Load() &&
		(!ProcessExited(sess.handle) || isPeerSupervised(sess.handle)) {
		slog.Warn("adapter transport closed during engine-initiated step-timeout teardown; routing as timeout, not crash",
			append([]any{"session", sess.Name, "adapter", sess.Adapter}, sess.crashDiagnostics(classifySessionCrash(sess, execErr))...)...)
		return result, execErr
	}

	if !isLikelySessionCrash(sess, execErr) {
		return result, execErr
	}

	return m.handleCrash(ctx, name, step, sink, sess, execErr, nil)
}

// MarkEngineStepTimeoutTeardown records that the engine canceled a step
// because its CRI-275 step ceiling expired (CRI-287). It opens the teardown
// window (see engineStepTimeoutTeardownAt): transport closes observed within
// the window are classified as a timeout teardown, not a session crash, so
// the step's declared failure/default outcome routing (the checkpoint loop)
// wins the race against the transport-close classifier. The engine only marks
// when it actually installed a step ceiling — a parent/run-context deadline
// or subworkflow cancellation never opens the window.
func (m *SessionManager) MarkEngineStepTimeoutTeardown() {
	m.engineStepTimeoutTeardownAt.Store(time.Now().UnixNano())
}

// mirrorStepTimeoutTeardownWindow copies src's CRI-287 step-timeout teardown
// window onto this manager when src's window is open and this manager's is
// not (KB-156). Delegated tool-resource executes classify on the owner's
// window, and the engine stamps the mark on the manager that ran the
// canceled step (a leasing child); mirroring the mark time preserves the
// remaining window so a teardown cascade observed on the shared session
// routes as a timeout, not as a shared-session crash. Mirroring is monotone:
// it can only open a closed owner window, never shrink an open one.
func (m *SessionManager) mirrorStepTimeoutTeardownWindow(src *SessionManager) {
	if src == nil || src == m {
		return
	}
	if m.engineStepTimeoutTeardownWindowOpen() {
		return
	}
	if markedAt := src.engineStepTimeoutTeardownAt.Load(); markedAt != 0 {
		m.engineStepTimeoutTeardownAt.Store(markedAt)
	}
}

// engineStepTimeoutTeardownWindowOpen reports whether a step-timeout teardown
// mark is recorded and still inside the teardown window (CRI-287).
func (m *SessionManager) engineStepTimeoutTeardownWindowOpen() bool {
	markedAt := m.engineStepTimeoutTeardownAt.Load()
	if markedAt == 0 {
		return false
	}
	return time.Since(time.Unix(0, markedAt)) < m.stepTimeoutTeardownWindow()
}

// StepTimeoutTeardownWindowOpen reports whether the CRI-287 step-timeout
// teardown window is currently open (a mark recorded within
// StepTimeoutTeardownWindow). Exported so engine-side probes and run
// diagnostics can observe the window without reaching into the manager.
func (m *SessionManager) StepTimeoutTeardownWindowOpen() bool {
	return m.engineStepTimeoutTeardownWindowOpen()
}

// combinedPolicyFor builds the step's CombinedPolicy (adapter allow set +
// environment policy) without touching session-global state. It is the
// per-Execute policy source: concurrent executes on one multiplexed session
// each decide under their own step policy (KB-155) instead of a
// last-writer-wins session-global snapshot. The returned policy is non-nil
// (its allow_tools matcher denies when the step declares none — the same
// deny-all semantics the legacy setStepPolicy path had).
func (m *SessionManager) combinedPolicyFor(sess *Session, step *workflow.StepNode) PermissionPolicy {
	var envPolicy *workflow.ResolvedPolicy
	if m.graph != nil && step != nil && step.AdapterRef != "" {
		adapterNode := m.graph.Adapters[step.AdapterRef]
		if adapterNode != nil {
			var envKey string
			if step.Environment != "" {
				envKey = step.Environment
			} else if adapterNode.Environment != "" {
				envKey = adapterNode.Environment
			}
			if envKey != "" && m.graph.ResolvedPolicies != nil {
				policyKey := step.AdapterRef + ":" + envKey
				envPolicy = m.graph.ResolvedPolicies[policyKey]
			}
		}
	}
	return NewCombinedPolicy(sess.Adapter, step.AllowTools, envPolicy)
}

// setStepPolicy builds the step's CombinedPolicy and installs it as the
// session-global snapshot used by legacy surfaces — e.g. the RestoreState
// re-present path after snapshot/restore, which evaluates outside an
// in-flight execute. Live execute decisions carry their own per-Execute
// policies (combinedPolicyFor via the execute's permissionInterceptSink) so
// concurrent executes attribute correctly (KB-155).
func (m *SessionManager) setStepPolicy(sess *Session, step *workflow.StepNode) {
	if m == nil || sess == nil || sess.PermissionState == nil {
		return
	}
	sess.PermissionState.SetPolicy(m.combinedPolicyFor(sess, step))
}

// concurrentExecuteCapability is the well-known adapter capability (declared
// in InfoResponse.capabilities) marking a v2 adapter that accepts multiple
// concurrent in-flight Execute RPCs on one session and correlates replies,
// per-call events, and decisions by request_id (KB-155). The capability
// vocabulary is free-form — hosts ignore unknown values for forward
// compatibility — so hosts without this card keep dispatching concurrent
// executes on such adapters but only see per-session attribution; per-request
// attribution is what KB-155 resolves.
const concurrentExecuteCapability = "concurrent_execute"

// parallelSafeCapability is the adapter side of the long-documented engine
// parallel contract: sessions declaring it authorize a `parallel = [...]`
// step's iterations to Execute concurrently (internal/engine, docs/workflow).
// The turn gate admits it alongside concurrent_execute — serializing those
// iterations would strip the granted concurrency and deadlock barrier-style
// adapters (review B1, KB-155).
const parallelSafeCapability = "parallel_safe"

// workflowV1Capability marks an ADR-0008 criteria-as-adapter session
// (internal/cli serve-adapter role): one child run anchors behind the
// session, so the execute turn gate MUST NOT queue a second concurrent
// Execute — it fails closed instead (KB-95). The child runs the same
// serialized policy on its side (its process-wide one-run guard is the
// final backstop).
const workflowV1Capability = "workflow.v1"

// ErrChildRunInFlight is the typed re-Execute guard (KB-95, ADR-0008):
// Execute on a workflow.v1 session while its child run is still in flight
// fails closed — the caller is never queued behind the in-flight run and no
// second child run is ever started. The session name is actionable for the
// engine's step failure.
type ErrChildRunInFlight struct {
	Session string
}

func (e *ErrChildRunInFlight) Error() string {
	return fmt.Sprintf("workflow.v1 session %q cannot re-execute while its child run is in flight; cancel the child run first (fail closed, no queueing)", e.Session)
}

// sessionSupportsConcurrentExecute reports whether the session's cached
// capabilities declare a multiplexable execute posture (KB-155): either the
// concurrent_execute capability or the engine's parallel_safe contract
// capability. Thread-safe through HasCapability's session/verified-record
// lookup.
func (m *SessionManager) sessionSupportsConcurrentExecute(sess *Session) bool {
	return m.HasCapability(sess.Name, concurrentExecuteCapability) ||
		m.HasCapability(sess.Name, parallelSafeCapability)
}

// acquireExecuteTurn serializes executes on sessions that declare neither
// parallel_safe nor concurrent_execute (KB-155): the turn is taken before
// any session-global state is touched so a queued caller can never observe
// an in-flight sibling's state; multiplexable sessions skip the gate. The
// queued flag
// reports whether the caller had to wait behind a sibling. Returns ctx.Err()
// only when the caller is cancelled while queued — in that state the turn
// was never taken, so the caller must return without releasing (the typed
// execute error mapping turns ctx.Err() into the caller's `canceled` reply).
// Abandonment AFTER a successful take — including the release/cancel race
// where the turn arrives just past the caller's own cancellation — is the
// caller's decision; see the abandon policy in m.execute.
func (m *SessionManager) acquireExecuteTurn(ctx context.Context, sess *Session) (bool, error) {
	if m.sessionSupportsConcurrentExecute(sess) {
		return false, nil
	}
	if sess.execTurns == nil {
		// No gate installed: sessions built only by m.Open are seeded, and
		// releaseExecuteTurn ignores nil as well. A missing channel on a
		// hand-built session must mean "unserialized", never "blocked
		// forever" — receiving on a nil channel would hang the caller.
		return false, nil
	}
	select {
	case <-sess.execTurns:
		// Fast path: the turn was free. Proceed regardless of ctx state.
		return false, nil
	default:
	}
	// KB-95 (ADR-0008) re-Execute guard for workflow.v1 sessions: the turn
	// is busy, which means this session's child run is in flight. Fail
	// closed — the guard caller is never queued and never re-runs the step
	// against the child.
	if m.HasCapability(sess.Name, workflowV1Capability) {
		return false, &ErrChildRunInFlight{Session: sess.Name}
	}
	select {
	case <-sess.execTurns:
		// Queued behind a sibling and the turn is now this caller's: it was
		// taken, so this return never reports an error (a token taken here
		// must be released, which only the caller's defer does). Whether the
		// taken turn is kept is m.execute's abandon decision (queued flag).
		return true, nil
	case <-ctx.Done():
		// Nothing taken: handing back no error would let the caller proceed
		// with a dead context; the turn stays free for the next waiter.
		return true, ctx.Err()
	}
}

// releaseExecuteTurn hands the serialized-execute turn on to the next queued
// caller (no-op for multiplexable sessions). Safe to defer unconditionally:
// callers that never acquired the turn release a free slot instead.
func (m *SessionManager) releaseExecuteTurn(sess *Session) {
	if sess.execTurns == nil {
		return
	}
	select {
	case sess.execTurns <- struct{}{}:
	default:
	}
}

// bindActiveSink registers a per-Execute event sink on the session's sink
// registry (KB-155). The registry is refcounted so nested sibling executes
// that share a caller sink collapse onto one entry; the unbind pairs with
// each bind. Returns nothing; the refcount handles repeated binds.
func (m *SessionManager) bindActiveSink(sess *Session, sink adapter.EventSink) {
	sess.activeSinksMu.Lock()
	defer sess.activeSinksMu.Unlock()
	if sess.activeSinks == nil {
		sess.activeSinks = make(map[adapter.EventSink]int)
	}
	sess.activeSinks[sink]++
}

// unbindActiveSink releases one execute's reference to its sink (KB-155),
// flushing the merge buffer first exactly as the former single-slot unbind
// did. An execute unbinds only the binding its own start path created.
func (m *SessionManager) unbindActiveSink(sess *Session, sink adapter.EventSink) {
	if sess.mergeBuf != nil {
		sess.mergeBuf.Flush()
	}
	sess.activeSinksMu.Lock()
	defer sess.activeSinksMu.Unlock()
	if n, ok := sess.activeSinks[sink]; ok {
		if n <= 1 {
			delete(sess.activeSinks, sink)
		} else {
			sess.activeSinks[sink] = n - 1
		}
	}
}

// singleActiveSink returns the bound sink when exactly one execute is in
// flight on the session (KB-155) — the only case where session-level traffic
// can be attributed without ambiguity. Multi-execute overlaps (concurrent
// executes on a multiplexed adapter) and idle sessions return false, and
// callers must fall back to structured logs: attribution must never guess.
func (s *Session) singleActiveSink() (sink adapter.EventSink, ok bool) {
	s.activeSinksMu.Lock()
	defer s.activeSinksMu.Unlock()
	if len(s.activeSinks) != 1 {
		return nil, false
	}
	for sink := range s.activeSinks {
		return sink, true
	}
	return nil, false
}

func newPermissionInterceptSink(ctx context.Context, inner adapter.EventSink, sess *Session, step *workflow.StepNode, graph *workflow.FSMGraph, mgr *SessionManager, nesting toolCallNesting, stepPolicy PermissionPolicy) *permissionInterceptSink {
	return &permissionInterceptSink{
		inner:      inner,
		permState:  sess.PermissionState,
		session:    sess,
		step:       step,
		graph:      graph,
		mgr:        mgr,
		nesting:    nesting,
		execCtx:    ctx,
		stepPolicy: stepPolicy,
	}
}

func (m *SessionManager) maybeOverrideOutcome(permSink *permissionInterceptSink, result *adapter.Result) {
	if permSink != nil && permSink.lastDecisionDenied && result.Outcome == "success" {
		result.Outcome = "needs_review"
	}
}

// HasCapability reports whether the session identified by name has capName in
// its cached capabilities slice. Returns false if the session is unknown or
// has no capabilities cached. Thread-safe.
func (m *SessionManager) HasCapability(name, capName string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[name]
	if ok {
		for _, c := range sess.Capabilities {
			if c == capName {
				return true
			}
		}
		return false
	}
	rec, ok := m.verified[name]
	if !ok {
		return false
	}
	for _, c := range rec.capabilities {
		if c == capName {
			return true
		}
	}
	return false
}

// AdapterHandle returns the underlying adapter handle for the named session.
// It is used by conformance tests to inspect handle capabilities without
// spawning a throwaway probe process. Returns (nil, false) if the session is
// not found. Thread-safe.
func (m *SessionManager) AdapterHandle(name string) (Handle, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[name]
	if !ok {
		return nil, false
	}
	return sess.handle, true
}

func (m *SessionManager) Shutdown(ctx context.Context) error {
	shims, sessions := m.takeForShutdown()
	errs := make([]error, 0, len(shims)+len(sessions)+1)
	// Stop every phone-home shim first so no new adapter connections are
	// accepted during teardown, pending handle waiters are woken with an
	// error, and no shim accept goroutine outlives the run (CRI-293: one
	// shim per remote environment may be registered).
	for _, shim := range shims {
		if err := shim.Stop(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	for _, sess := range sessions {
		errs = append(errs, m.closeSession(ctx, sess))
	}
	if m.loader != nil {
		if err := m.loader.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// takeForShutdown atomically collects every registered remote shim and
// snapshots+clears the sessions and verification records under the lock.
// The default shim usually aliases one environment's shim; duplicates are
// kept because RemoteShim implementations cannot be assumed comparable and
// Shim.Stop is idempotent, so a second Stop is a no-op.
func (m *SessionManager) takeForShutdown() ([]RemoteShim, []*Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	shims := make([]RemoteShim, 0, len(m.remoteShimsByEnv)+1)
	// Borrowed shims are owned by a parent SM (BorrowRemoteProvisioningFrom); this SM
	// only serves scopes on them, so it must not Stop them here — doing so would
	// tear down the shared phone-home listener the parent run and sibling
	// parallel iterations still depend on.
	if !m.remoteShimsBorrowed {
		if m.remoteShim != nil {
			shims = append(shims, m.remoteShim)
		}
		for _, s := range m.remoteShimsByEnv {
			shims = append(shims, s)
		}
	}
	sessions := make([]*Session, 0, len(m.sessions))
	for name, sess := range m.sessions {
		sessions = append(sessions, sess)
		delete(m.sessions, name)
	}
	for name := range m.verified {
		delete(m.verified, name)
	}
	return shims, sessions
}

// closeSession marks a session closing, cancels its log stream, tears down
// sandbox/merge state and the underlying handle, returning any close error.
func (m *SessionManager) closeSession(ctx context.Context, sess *Session) error {
	sess.closing.Store(true)
	sess.logStreamAlive.Store(false)
	if sess.PermissionState != nil {
		sess.PermissionState.Stop()
	}
	sess.logMu.Lock()
	cancelLog := sess.cancelLog
	sess.logMu.Unlock()
	if cancelLog != nil {
		cancelLog()
	}
	if sess.mergeBuf != nil {
		sess.mergeBuf.Close()
	}
	if sess.SandboxCleanup != nil {
		sess.SandboxCleanup()
	}
	var errs []error
	if err := sess.handle.CloseSession(ctx, sess.Name); err != nil {
		errs = append(errs, err)
	}
	sess.handle.Kill()
	return errors.Join(errs...)
}

// Prompt delivers an agent prompt into the live adapter session for the
// named adapter (ADR-0006 D2/D3/D9). Ordering of checks is the deterministic
// failure taxonomy: capability gate first (UNSUPPORTED_ADAPTER short-circuit,
// no RPC issued), then session resolution (NO_ACTIVE_SESSION), then the
// caller-supplied session-id match (SESSION_MISMATCH), then the Prompt RPC
// (adapter rejection carries the adapter's detail). Returns the adapter
// session id the prompt was delivered to.
func (m *SessionManager) Prompt(ctx context.Context, name string, step *workflow.StepNode, wantSessionID, prompt string) (string, error) {
	if !m.HasCapability(name, PromptCapability) {
		return "", ErrPromptUnsupportedAdapter
	}
	sess, err := m.lookup(name)
	if err != nil {
		if !errors.Is(err, ErrUnknownSession) {
			return "", err
		}
		// A prompt may arrive while the step's first Execute is still in the
		// lazy-bind phase; verified-only sessions bind on first use.
		sess, err = m.bindVerifiedAndLookup(ctx, name, step)
		if err != nil {
			return "", ErrPromptNoActiveSession
		}
	}
	if wantSessionID != "" && sess.Name != wantSessionID {
		return "", ErrPromptSessionMismatch
	}
	capable, ok := sess.handle.(promptCapableHandle)
	if !ok {
		return "", ErrPromptUnsupportedAdapter
	}
	resp, err := capable.Prompt(ctx, &PromptRequest{SessionID: sess.Name, Prompt: prompt})
	if err != nil {
		return "", err
	}
	if !resp.Accepted {
		return sess.Name, &PromptRejectedError{Detail: resp.Detail}
	}
	return sess.Name, nil
}

func (m *SessionManager) lookup(name string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownSession, name)
	}
	return sess, nil
}

func (m *SessionManager) respawn(ctx context.Context, sess *Session) error {
	sess.handle.Kill()
	if sess.SandboxCleanup != nil {
		sess.SandboxCleanup()
	}
	customizer, cleanup, sandboxErr := m.buildCommandCustomizer(sess.Name, sess.WorkingDir)
	if sandboxErr != nil {
		return fmt.Errorf("session %q respawn: %w", sess.Name, sandboxErr)
	}

	plug, err := m.resolveAdapterForRespawn(ctx, sess, customizer)
	if err != nil {
		if cleanup != nil {
			cleanup()
		}
		return err
	}
	if err := plug.OpenSession(ctx, sess.Name, sess.Config, sess.Secrets); err != nil {
		plug.Kill()
		if cleanup != nil {
			cleanup()
		}
		return err
	}
	sess.handle = plug
	sess.SandboxCleanup = cleanup
	sess.respawned = true
	// CRI-271: a fresh process is a fresh activity baseline, and the crash
	// classification no longer applies to the re-opened session.
	sess.noteActivity()
	sess.crashed.Store(false)

	m.restartPermissionStream(ctx, sess, plug)
	m.restartLogStream(ctx, sess, plug)

	return nil
}

func (m *SessionManager) resolveAdapterForRespawn(ctx context.Context, sess *Session, customizer func(string, *exec.Cmd)) (Handle, error) {
	// Remote-mode dispatch: if the adapter is bound to a remote environment,
	// wait for the adapter to phone home via the shim serving that
	// environment (respawn = reconnect; CRI-293: per-environment shims).
	if shim, ok := m.remoteShimForAdapter(sess.Name); ok {
		// Exclude the just-crashed handle so we wait for the replacement
		// connection rather than the dead session still in the shim's map.
		return shim.WaitForFreshHandle(ctx, sess.Adapter, sess.ScopeInstanceID, sess.handle)
	}

	if dl, ok := m.loader.(*DefaultLoader); ok {
		runnerFunc, containerErr := adapter.BuildContainerRunner(m.graph, m.lockfile, sess.Name)
		if containerErr != nil {
			return nil, containerErr
		}
		if runnerFunc != nil {
			return dl.ResolveWithRunnerFunc(ctx, sess.Adapter, runnerFunc)
		}
		return dl.ResolveWithCustomizer(ctx, sess.Adapter, customizer)
	}
	return m.loader.Resolve(ctx, sess.Adapter)
}

func (m *SessionManager) restartPermissionStream(ctx context.Context, sess *Session, plug Handle) {
	if sess.PermissionState == nil {
		return
	}
	sess.PermissionState.Stop()
	sess.PermissionState = NewPermissionState(sess.Name, m.auditWriterForSessions())
	sess.PermissionState.SetPauseDrainWindow(m.pauseToolCallDrainTimeout())
	if streamer, ok := plug.(PermissionStreamer); ok {
		cancel, err := streamer.StartPermissionStream(ctx, sess.Name, sess.PermissionState.Requests())
		if err != nil {
			slog.Warn("adapter permission stream restart failed after respawn", "session", sess.Name, "err", err)
		} else {
			sess.PermissionState.SetStreamCancel(cancel)
		}
	}
}

// restartLogStream cancels the old log stream, closes the old merge buffer, and
// starts a new one for the given handle.
func (m *SessionManager) restartLogStream(ctx context.Context, sess *Session, plug Handle) {
	sess.logMu.Lock()
	oldCancel := sess.cancelLog
	oldDone := sess.logDone
	sess.cancelLog = nil
	sess.logDone = nil
	sess.logHostCancel = nil
	sess.logMu.Unlock()

	if oldCancel != nil {
		// The wrapped cancel closes the stream's host-cancel channel *before*
		// invoking the adapter cancel, so the old watcher can deterministically
		// identify this as host-initiated teardown.
		oldCancel()
	}
	if oldDone != nil {
		// Wait for the previous watcher to finish before reusing the merge buffer
		// and heartbeat state, avoiding races and goroutine leaks. Bound the wait
		// so a broken adapter that never closes its done channel cannot wedge the
		// respawn path and prevent the step retry from starting.
		drainTimeout := m.respawnLogStreamDrainTimeout()
		select {
		case <-oldDone:
		case <-ctx.Done():
		case <-time.After(drainTimeout):
			slog.Warn("adapter log stream did not drain after cancel; continuing respawn to avoid wedge",
				"session", sess.Name, "adapter", sess.Adapter, "drain_timeout", drainTimeout)
		}
	}
	if sess.mergeBuf != nil {
		sess.mergeBuf.Close()
	}

	if starter, ok := plug.(LogStreamStarter); ok {
		m.beginLogStream(ctx, sess, starter)
	} else {
		sess.resetLogStreamState()
		sess.mergeBuf = nil
	}
}

// resetLogStreamState clears the log-stream handles and marks the stream dead.
func (s *Session) resetLogStreamState() {
	s.logMu.Lock()
	s.cancelLog = nil
	s.logDone = nil
	s.logHostCancel = nil
	s.logMu.Unlock()
	s.logStreamAlive.Store(false)
}

func (m *SessionManager) failResult(sink adapter.EventSink, sess *Session, err error) (adapter.Result, error) {
	sink.Adapter("session.crash", map[string]any{
		"session":               sess.Name,
		"adapter":               sess.Adapter,
		"policy":                sess.OnCrash,
		"error":                 err.Error(),
		"crash_reason":          classifySessionCrash(sess, err),
		"idle_since_last_event": idleStringOrEmpty(sess),
	})
	return adapter.Result{Outcome: "failure"}, &SessionCrashError{Session: sess.Name, Err: err}
}

func normalizeOnCrash(v string) string {
	switch strings.TrimSpace(v) {
	case OnCrashRespawn:
		return OnCrashRespawn
	case OnCrashAbortRun:
		return OnCrashAbortRun
	default:
		return OnCrashFail
	}
}

// ReopenCrashedSession replaces the dead process behind a crashed adapter
// session with a fresh one and re-opens the session in place (CRI-271).
//
// Under the default on_crash=fail policy a crashed session stays registered
// but dead: every Execute on the same reference replays the crash error, so
// follow-on steps (bookkeeping such as comment_handler_failed /
// set_review_state) fail on the corpse and bury the run's real state. The
// engine calls this before a follow-on step whose reference is registered as
// crashed, so the step executes on a live session instead.
//
// Semantics: an unknown session is an error; a session that is not marked
// crashed is healthy (already respawned, or never crashed) and is left
// untouched; concurrent callers are serialized per session and the first
// successful re-open wins. The crashing step itself is NOT retried — only
// follow-on work runs on the fresh session.
func (m *SessionManager) ReopenCrashedSession(ctx context.Context, name string) error {
	m.mu.Lock()
	sess, ok := m.sessions[name]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownSession, name)
	}
	if sess.closing.Load() {
		return fmt.Errorf("session %q is closing; cannot re-open", name)
	}
	if !sess.crashed.Load() {
		return nil
	}
	// Single-flight per session: parallel fan-out steps sharing a crashed
	// reference must not spawn several replacement processes.
	sess.reopenMu.Lock()
	defer sess.reopenMu.Unlock()
	if !sess.crashed.Load() {
		return nil
	}
	if err := m.respawn(ctx, sess); err != nil {
		return fmt.Errorf("session %q re-open: %w", name, err)
	}
	slog.Info("adapter session re-opened after crash", "session", name, "adapter", sess.Adapter)
	return nil
}

func isLikelySessionCrash(sess *Session, err error) bool {
	if err == nil {
		return false
	}
	if sess.closing.Load() {
		// Expected: caller initiated close; any subsequent EOF /
		// transport-closing / broken-pipe is the normal teardown.
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection") ||
		strings.Contains(msg, "transport is closing") ||
		strings.Contains(msg, "unavailable") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "eof") ||
		strings.Contains(msg, "terminated")
}

// sessionLogAdapterSink routes session-level log lines to the active step's
// EventSink when a step is executing, or to structured logs otherwise.
type sessionLogAdapterSink struct {
	sess *Session
}

func (s *sessionLogAdapterSink) Log(stream string, chunk []byte) {
	// Any delivered log chunk is observable adapter activity (CRI-271); the
	// crash diagnostics report the idle window since the last one.
	s.sess.noteActivity()
	// KB-155: session-level traffic is attributed to the executing step sink
	// only when exactly one execute holds the session; otherwise it goes to
	// structured logs so it is never misattributed to an arbitrary sibling.
	if sink, ok := s.sess.singleActiveSink(); ok {
		sink.Log(stream, chunk)
	} else {
		slog.Info("adapter log", "session", s.sess.Name, "stream", stream, "line", string(chunk))
	}
}

func (s *sessionLogAdapterSink) Adapter(kind string, data any) {
	if sink, ok := s.sess.singleActiveSink(); ok {
		sink.Adapter(kind, data)
	} else {
		slog.Info("adapter event", "session", s.sess.Name, "kind", kind, "data", data)
	}
}

func cloneOriginRefs(m map[string]secrets.OriginRef) map[string]secrets.OriginRef {
	if m == nil {
		return nil
	}
	out := make(map[string]secrets.OriginRef, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// validateConfigAgainstSchema performs a runtime check of the resolved config
// against the adapter's declared manifest schema. It only enforces presence of
// required non-sensitive fields; sensitive required values are resolved
// separately and validated by validateRequiredSecrets. The compiler already
// rejects unknown keys and incompatible types at workflow compile time, and the
// manifest schema is intentionally a subset of the HCL type system.
func validateConfigAgainstSchema(config map[string]string, schema map[string]workflow.ConfigField) error {
	if len(schema) == 0 {
		return nil
	}
	var missing []string
	for name, field := range schema {
		if !field.Required || field.Sensitive {
			continue
		}
		val, ok := config[name]
		if !ok || strings.TrimSpace(val) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return fmt.Errorf("missing required config field(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

// validateRequiredSecrets checks that every config field marked Sensitive and
// Required has a non-empty resolved secret value. Non-sensitive required values
// are covered by validateConfigAgainstSchema; this path exists because secrets
// are resolved separately from static config.
func validateRequiredSecrets(secrets map[string]string, schema map[string]workflow.ConfigField) error {
	if len(schema) == 0 {
		return nil
	}
	var missing []string
	for name, field := range schema {
		if !(field.Required && field.Sensitive) {
			continue
		}
		val, ok := secrets[name]
		if !ok || strings.TrimSpace(val) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return fmt.Errorf("missing required secret(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

// SessionSnapshot records the complete host-visible state of a session at a
// point in time, sufficient to restore it on the same or a restarted host.
type SessionSnapshot struct {
	AdapterState     []byte                       `json:"-"` // opaque to host (from adapter via Snapshot RPC)
	SchemaVersion    uint32                       `json:"schema_version"`
	PermissionState  []byte                       `json:"permission_state"`   // from PermissionState.MarshalState() (WS16)
	SecretOriginRefs map[string]secrets.OriginRef `json:"secret_origin_refs"` // from sessions config; values not included
	AdapterDigest    digest.Digest                `json:"adapter_digest"`     // adapter manifest digest at snapshot time
	HostArch         string                       `json:"host_arch"`          // GOOS/GOARCH at snapshot
	WorkingDir       string                       `json:"working_dir"`        // resolved environment working_directory at snapshot
	ScopeInstanceID  string                       `json:"scope_instance_id"`  // shim scope key for remote per-scope sessions
	// CRI-202: the adapter-declared state surface stamped at save time so a
	// restore can reject a mismatched state shape instead of misinterpreting
	// the bytes. Empty on pre-CRI-202 checkpoints; restore treats an empty
	// tag as unverifiable and proceeds with a warning.
	StateSchema string `json:"state_schema,omitempty"`
	// StateMode records the declared mode (blob|ref) the state was saved
	// under; informational for restore diagnostics.
	StateMode string `json:"state_mode,omitempty"`
	// StateDigest is the "sha256:<hex>" digest of AdapterState at save time;
	// the checkpoint store verifies it on restore so a truncated or
	// corrupted blob fails loudly with the session and schema named.
	StateDigest string `json:"state_digest,omitempty"`
	// Granularity records the adapter's declared checkpoint granularity
	// (per-step|per-turn|on-demand) at save time.
	Granularity string    `json:"granularity,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

const currentSnapshotSchemaVersion uint32 = 1

// Snapshot pauses the session, captures adapter state, permission state, and
// host metadata, and returns a SessionSnapshot. The session remains paused after
// the call; the caller must Resume to continue execution.
func (s *Session) Snapshot(ctx context.Context) (*SessionSnapshot, error) {
	if err := s.Pause(ctx); err != nil {
		return nil, fmt.Errorf("pause before snapshot: %w", err)
	}
	return s.captureState(ctx)
}

// captureState captures the adapter's state, permission state, and host
// metadata without pausing the session. Used by Snapshot (after its Pause)
// and by the step/turn-boundary checkpoint saves (CRI-202), which run while
// the session is active: torn-read consistency mid-turn is the adapter's
// declared responsibility — a per-turn declaration promises consistent
// snapshots at turn boundaries.
func (s *Session) captureState(ctx context.Context) (*SessionSnapshot, error) {
	resp, err := s.handle.Snapshot(ctx, s.Name)
	if err != nil {
		return nil, fmt.Errorf("adapter snapshot: %w", err)
	}

	var permState []byte
	if s.PermissionState != nil {
		permState, err = s.PermissionState.MarshalState()
		if err != nil {
			return nil, fmt.Errorf("marshal permission state: %w", err)
		}
	}

	return &SessionSnapshot{
		AdapterState:     resp.State,
		SchemaVersion:    currentSnapshotSchemaVersion,
		PermissionState:  permState,
		SecretOriginRefs: cloneOriginRefs(s.SecretOriginRefs),
		AdapterDigest:    s.AdapterDigest,
		HostArch:         runtime.GOOS + "/" + runtime.GOARCH,
		WorkingDir:       s.WorkingDir,
		ScopeInstanceID:  s.ScopeInstanceID,
		StateSchema:      s.stateSchema,
		StateMode:        s.stateMode,
		Granularity:      s.stateGranularity,
		StateDigest:      ComputeStateDigest(resp.State),
		CreatedAt:        time.Now(),
	}, nil
}

// SnapshotAll iterates over every open session and calls Snapshot on each.
// Errors from individual sessions are collected and returned joined.
func (m *SessionManager) SnapshotAll(ctx context.Context) (map[string]*SessionSnapshot, error) {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()

	out := make(map[string]*SessionSnapshot, len(sessions))
	var errs []error
	for _, s := range sessions {
		snap, err := s.Snapshot(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("session %q: %w", s.Name, err))
			continue
		}
		out[s.Name] = snap
	}
	return out, errors.Join(errs...)
}

// openAndRestoreAdapter resolves the adapter handle, opens a fresh session, and replays
// the saved adapter state. On error it kills the plug and runs the cleanup func.
func (m *SessionManager) openAndRestoreAdapter(ctx context.Context, name, adapterName string, config, resolvedSecrets map[string]string, snap *SessionSnapshot) (Handle, func(), error) {
	customizer, cleanup, sandboxErr := m.buildCommandCustomizer(name, snap.WorkingDir)
	if sandboxErr != nil {
		return nil, nil, fmt.Errorf("session %q: %w", name, sandboxErr)
	}

	plug, err := m.resolveAdapterHandle(ctx, name, adapterName, snap.ScopeInstanceID, customizer)
	if err != nil {
		if cleanup != nil {
			cleanup()
		}
		return nil, nil, err
	}

	if err := plug.OpenSession(ctx, name, config, resolvedSecrets); err != nil {
		plug.Kill()
		if cleanup != nil {
			cleanup()
		}
		return nil, nil, fmt.Errorf("open restored session: %w", err)
	}

	if err := plug.Restore(ctx, name, snap.AdapterState, snap.SchemaVersion); err != nil {
		plug.Kill()
		if cleanup != nil {
			cleanup()
		}
		return nil, nil, fmt.Errorf("adapter restore: %w", err)
	}

	return plug, cleanup, nil
}

func buildRestoredSession(name, adapterName, onCrash string, config, resolvedSecrets map[string]string, originRefs map[string]secrets.OriginRef, caps []string, plug Handle, cleanup func(), permState *permissionState, workingDir, scopeInstanceID string) *Session {
	// execTurns and activeSinks are deliberately nil here: a restored session
	// fails open with the legacy unserialized posture (matching the restored
	// session's pre-KB-155 behavior) until a fresh execute lifecycle binds
	// them.
	return &Session{
		Name:             name,
		Adapter:          adapterName,
		Config:           cloneConfig(config),
		Secrets:          resolvedSecrets,
		SecretOriginRefs: cloneOriginRefs(originRefs),
		OnCrash:          normalizeOnCrash(onCrash),
		Capabilities:     caps,
		handle:           plug,
		SandboxCleanup:   cleanup,
		PermissionState:  permState,
		WorkingDir:       workingDir,
		ScopeInstanceID:  scopeInstanceID,
	}
}

func (m *SessionManager) registerRestoredSession(ctx context.Context, name string, plug Handle, cleanup func(), sess *Session) error {
	m.mu.Lock()
	if _, exists := m.sessions[name]; exists {
		m.mu.Unlock()
		_ = plug.CloseSession(ctx, name)
		plug.Kill()
		if cleanup != nil {
			cleanup()
		}
		return fmt.Errorf("%w: %s", ErrSessionAlreadyOpen, name)
	}
	m.sessions[name] = sess
	m.mu.Unlock()
	return nil
}

// Restore validates a snapshot against cross-host compatibility rules,
// re-resolves secrets, opens a fresh adapter session, replays adapter state,
// restores permission state, and registers the reconstructed session.
func (m *SessionManager) Restore(ctx context.Context, name, adapterName, onCrash string, config map[string]string, envNode *workflow.EnvironmentNode, snap *SessionSnapshot) (*Session, error) {
	if err := m.validateSnapshotCompatibility(name, snap); err != nil {
		return nil, err
	}

	resolvedSecrets, err := m.resolveSnapshotSecrets(ctx, envNode, snap.SecretOriginRefs)
	if err != nil {
		return nil, err
	}

	plug, cleanup, err := m.openAndRestoreAdapter(ctx, name, adapterName, config, resolvedSecrets, snap)
	if err != nil {
		return nil, err
	}

	caps, declared, err := m.validateRelaunchedAdapter(ctx, plug, name, adapterName, snap)
	if err != nil {
		plug.Kill()
		if cleanup != nil {
			cleanup()
		}
		return nil, err
	}

	permState, err := m.restorePermissionState(name, snap.PermissionState)
	if err != nil {
		plug.Kill()
		if cleanup != nil {
			cleanup()
		}
		return nil, err
	}

	sess := buildRestoredSession(name, adapterName, onCrash, config, resolvedSecrets, snap.SecretOriginRefs, caps, plug, cleanup, permState, snap.WorkingDir, snap.ScopeInstanceID)
	m.stampStateFields(sess, declared)
	if err := m.registerRestoredSession(ctx, name, plug, cleanup, sess); err != nil {
		return nil, err
	}

	if permState != nil {
		m.startPermissionStream(ctx, sess, plug)
	}
	m.startLogStream(ctx, sess, plug)
	m.wireTurnCheckpoint(sess, ctx, declared)
	return sess, nil
}

// validateRelaunchedAdapter re-checks the relaunched adapter's declaration
// against the checkpoint stamp and returns the session's capabilities and
// declared state surface. A relaunch may surface a changed or malformed
// declaration (e.g. the adapter binary was swapped mid-run); that fails the
// restore loudly rather than silently downgrading checkpointing. An Info
// failure is tolerated (the session proceeds without a cached declaration),
// as in the fresh-open path.
func (m *SessionManager) validateRelaunchedAdapter(ctx context.Context, plug Handle, name, adapterName string, snap *SessionSnapshot) ([]string, *workflow.StateDeclaration, error) {
	info, infoErr := plug.Info(ctx)
	if infoErr == nil {
		if err := validateStateHandshake(adapterName, info.AdapterInfo.State); err != nil {
			return nil, nil, err
		}
		// CRI-202: re-check the checkpoint's stamped state schema against the
		// relaunched adapter's declaration. Mismatched or dropped state must
		// refuse loudly — never a silent fresh start.
		if err := validateRestoredStateSchema(adapterName, info.AdapterInfo.State, snap); err != nil {
			return nil, nil, err
		}
		// Re-capture the declared surface after a snapshot relaunch so a
		// restored session keeps gating on its declared input contract
		// (CRI-270) without a re-verify round-trip.
		m.cacheAdapterInfo(name, &info.AdapterInfo)
		return append([]string(nil), info.Capabilities...), info.AdapterInfo.State, nil
	}
	// An Info failure is tolerated (the session proceeds without a cached
	// declaration), as in the fresh-open path.
	return nil, nil, nil
}

func (m *SessionManager) restorePermissionState(name string, blob []byte) (*permissionState, error) {
	if len(blob) == 0 {
		return nil, nil
	}
	permState := NewPermissionState(name, m.auditWriterForSessions())
	permState.SetPauseDrainWindow(m.pauseToolCallDrainTimeout())
	if err := permState.RestoreState(blob, nil, m.auditWriterForSessions()); err != nil {
		return nil, fmt.Errorf("restore permission state: %w", err)
	}
	return permState, nil
}

// RestoreIntoLiveSession replays prior adapter state into an already-open
// session (CRI-202). A remote adapter that phone-homed during initialization
// binds its session before the engine's checkpoint-restore pass runs, so the
// pass must be able to restore into a live session instead of reopening one.
// The declared state surface is re-checked against the checkpoint's schema
// tag; permission state and streams are already live and are left untouched.
func (m *SessionManager) RestoreIntoLiveSession(ctx context.Context, name string, snap *SessionSnapshot) error {
	m.mu.Lock()
	sess, ok := m.sessions[name]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("cannot restore checkpoint: session %q is not open", name)
	}
	if err := m.validateSnapshotCompatibility(name, snap); err != nil {
		return err
	}
	info := m.cachedAdapterInfo(name)
	if info != nil {
		if err := validateStateHandshake(name, info.State); err != nil {
			return err
		}
	}
	// nil (no cached handshake info) is legal here: validateRestoredStateSchema
	// then only enforces the schema-vs-no-declaration refusal.
	var declared *workflow.StateDeclaration
	if info != nil {
		declared = info.State
	}
	if err := validateRestoredStateSchema(name, declared, snap); err != nil {
		return err
	}
	if err := sess.handle.Restore(ctx, name, snap.AdapterState, snap.SchemaVersion); err != nil {
		return fmt.Errorf("restore into live session %q: %w", name, err)
	}
	return nil
}

func (m *SessionManager) validateSnapshotCompatibility(adapterKey string, snap *SessionSnapshot) error {
	if snap.SchemaVersion != currentSnapshotSchemaVersion {
		return fmt.Errorf("snapshot schema version %d is not supported (expected %d)", snap.SchemaVersion, currentSnapshotSchemaVersion)
	}

	wantArch := runtime.GOOS + "/" + runtime.GOARCH
	if snap.HostArch != "" && snap.HostArch != wantArch {
		return fmt.Errorf("snapshot host arch %q does not match current host %q", snap.HostArch, wantArch)
	}

	if m.lockfile == nil {
		return nil // nothing to compare against
	}

	var currentDigest string
	for i := range m.lockfile.Adapters {
		a := &m.lockfile.Adapters[i]
		if a.Type+"."+a.Name == adapterKey {
			currentDigest = a.ResolvedDigest
			break
		}
	}
	if currentDigest == "" {
		return nil // adapter not in lockfile; skip digest check
	}
	if snap.AdapterDigest != "" && snap.AdapterDigest.String() != currentDigest {
		return fmt.Errorf("snapshot was taken against adapter %q@%s; current lockfile pins %q@%s. Resume requires the same adapter version", adapterKey, snap.AdapterDigest, adapterKey, currentDigest)
	}
	return nil
}

func (m *SessionManager) resolveSnapshotSecrets(ctx context.Context, envNode *workflow.EnvironmentNode, originRefs map[string]secrets.OriginRef) (map[string]string, error) {
	if len(originRefs) == 0 {
		return nil, nil
	}

	stack, err := secrets.StackFromEnvironment(envNode)
	if err != nil {
		return nil, fmt.Errorf("build secret stack for restore: %w", err)
	}

	resolved := make(map[string]string, len(originRefs))
	for name, ref := range originRefs {
		if ref.Kind == "literal" {
			resolved[name] = ref.Ref
			if m.RedactionRegistry != nil {
				m.RedactionRegistry.Register(ref.Ref)
			}
			continue
		}
		val, err := stack.Resolve(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("missing secret %q: %w", name, err)
		}
		resolved[name] = val
		if m.RedactionRegistry != nil {
			m.RedactionRegistry.Register(val)
		}
	}
	return resolved, nil
}
