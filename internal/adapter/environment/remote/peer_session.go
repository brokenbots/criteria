package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"

	criteriav1 "github.com/brokenbots/criteria/sdk/pb/criteria/v1"

	"github.com/brokenbots/criteria/internal/adapter"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// This file implements the host side of the peer transport (ADR-0007 Stage
// A, T-06). An adapter running in peer mode phones home over the same mTLS
// connection as the legacy runner, but its identity frame carries role
// "peer": after the shim's standard verification (mTLS, identity pattern,
// lockfile digest, scope token) the connection is handed to the
// peerSessionProvider installed as the shim's PeerAcceptor. The host becomes
// a plain gRPC CLIENT on the already-established connection — no UDS, no
// go-plugin reattach, no second dial — and drives the adapter over the
// adapter v2 AdapterService while the peer pushes its supervision journal
// over PeerService.Supervise on the same connection.
//
// The provider also implements adapterhost.RemoteShim, so the session
// manager dispatch (resolveAdapterHandle / resolveAdapterForRespawn) is
// unchanged: peer sessions are resolved through the same interface the
// legacy byte-bridge sessions use, and a mixed fleet (legacy + peer dials
// for the same adapter type during a rolling upgrade) is supported by
// consulting both the provider's peer registry and the wrapped shim's
// legacy sessions.

const (
	// peerDialTarget is the gRPC target handed to grpc.NewClient. It uses
	// the built-in passthrough scheme so the resolver immediately hands the
	// placeholder address to the context dialer (which returns the held
	// conn); with the default dns scheme the placeholder would fail to
	// resolve before the dialer is ever invoked. The address only appears
	// in logs.
	peerDialTarget = "passthrough:///criteria-peer"

	// PeerService full method names. The wire contract lives in
	// proto/criteria/v1/peer.proto; the generated bindings expose a
	// connect-style client only, so the host invokes the methods on the
	// shared grpc.ClientConn directly.
	peerSuperviseMethod = "/criteria.v1.PeerService/Supervise"
	peerControlMethod   = "/criteria.v1.PeerService/Control"

	// peerSuperviseReplayBackoff paces supervision reconnects after a
	// stream-level reset on a live transport (the journal replays strictly
	// after the last applied event_seq, so replay is cheap and idempotent).
	peerSuperviseReplayBackoff = 500 * time.Millisecond

	// peerKillTimeout bounds the best-effort Kill control round-trip; Kill
	// runs synchronously on session-teardown paths.
	peerKillTimeout = 5 * time.Second

	// peerKillGraceMs is the grace period the peer applies before
	// force-terminating the adapter child on a Kill control.
	peerKillGraceMs = 3000

	// peerLogChannel is the supervision channel whose StreamFlushed events
	// mean "the log stream backlog was drained to the peer journal".
	peerLogChannel = "log"

	// exitReasonProcessExited is the placeholder exit reason a plain Exited
	// journal record carries before any classification. It is deliberately
	// NOT part of the adapterhost CrashReason taxonomy: a peer whose journal
	// only delivered Exited reports no classification, and the host falls
	// through to the ProcessExited evidence path (T-07).
	exitReasonProcessExited = "process_exited"

	// peerAdapterRouteHeader is the gRPC metadata key the host tags each
	// per-session adapter call with on a multi-adapter peer connection (one
	// conn hosting N children, KB-213); the peer-side mux routes the call to
	// the named child. The literal must stay in sync with the peer-side
	// constant peerAdapterRouteHeader in internal/peer/serve.go: the packages
	// cannot import each other (import-boundary lint), so the sync is by
	// comment contract.
	peerAdapterRouteHeader = "x-criteria-adapter"
)

// peerSuperviseStreamDesc describes the PeerService.Supervise server-stream
// for the hand-rolled client invocation (there is no generated grpc-go
// client for PeerService; the connect client cannot share the pre-dialed
// transport).
var peerSuperviseStreamDesc = &grpc.StreamDesc{
	StreamName:    "Supervise",
	ServerStreams: true,
}

// peerSessionProvider receives authenticated peer-role dials from the shim
// and implements adapterhost.RemoteShim on top of them, so a remote adapter
// whose identity frame advertises role "peer" is served without the legacy
// go-plugin reattach path. Legacy (role-absent) dials continue to take the
// shim's byte-bridge path; the provider delegates every RemoteShim method to
// the wrapped shim once its own peer registry has been consulted.
type peerSessionProvider struct {
	shim             *Shim
	perScopeSessions bool

	mu       sync.Mutex
	peers    map[string]*peerSession      // sessionKey → live peer session
	waiters  map[string][]chan waitResult // sessionKey → pending session waits
	stopOnce sync.Once
	stopped  bool

	// declared is the engine-side declared-adapter set for this provider's
	// environment (KB-213): the adapters the peer pod is expected to host.
	// Non-empty enables the host-side fail-closed checks; empty keeps the
	// pre-KB-213 open behavior (undecorated legacy wiring).
	declared declaredAdapters
}

// NewPeerSessionProvider builds the provider serving peer-mode dials for a
// shim. perScopeSessions must match the shim's configuration: it decides the
// session key ("adapterType" in legacy mode, "adapterType\x00scope" with
// per-scope isolation), which must be identical for both registries so a
// mixed fleet keys coherently.
func NewPeerSessionProvider(shim *Shim, perScopeSessions bool) *peerSessionProvider {
	return &peerSessionProvider{
		shim:             shim,
		perScopeSessions: perScopeSessions,
		peers:            make(map[string]*peerSession),
		waiters:          make(map[string][]chan waitResult),
	}
}

// SetDeclaredAdapters installs the environment's declared-adapter set for
// the provider (KB-213). A non-empty set turns on two fail-closed host
// checks:
//
//   - accept time: a peer dial whose hosted child set does not cover every
//     declared adapter is rejected (typed PeerChildSetError) instead of
//     accepted partially — a peer pod that cannot host an adapter the
//     environment uses would otherwise strand that adapter's sessions on
//     scheduling budgets,
//   - wait time: waiting for an adapter outside the declared set fails
//     immediately (typed PeerChildSetError) — no peer will ever host it.
//
// An empty list disables both checks (mixed fleets that predate the
// declared-adapter wiring keep the open behavior).
func (p *peerSessionProvider) SetDeclaredAdapters(names []string) {
	p.declared.set(names)
}

// missingDeclared reports which declared adapters are absent from hosted.
// The declared set governs peer dials only: legacy runner dials never reach
// AcceptPeer, so the mixed fleet stays out of this check's blast radius.
func (p *peerSessionProvider) missingDeclared(hosted []string) []string {
	hostedSet := make(map[string]bool, len(hosted))
	for _, name := range hosted {
		hostedSet[name] = true
	}
	var missing []string
	for _, name := range p.declared.snapshot() {
		if !hostedSet[name] {
			missing = append(missing, name)
		}
	}
	return missing
}

// key mirrors Shim.sessionKey (identical algorithm, evaluated from the
// provider's own perScopeSessions snapshot).
func (p *peerSessionProvider) key(adapterType, scope string) string {
	if !p.perScopeSessions || scope == "" {
		return adapterType
	}
	return adapterType + "\x00" + scope
}

// AcceptPeer implements PeerAcceptor. It is called by the shim after the
// standard identity verification with ownership of conn: the provider
// becomes the gRPC client on the pre-established connection and starts the
// supervision consumer.
//
// KB-213 fail-closed acceptance: when the environment's declared adapters
// are known, a dial whose child set does not cover them is rejected — a
// peer pod that cannot host a declared adapter is a provisioning error, and
// accepting it would strand the missing adapter's sessions on scheduling
// budgets. The rejection closes the conn (the acceptor owns it until
// success) so the peer's next dial repeats the same typed failure.
func (p *peerSessionProvider) AcceptPeer(ctx context.Context, conn net.Conn, dial *PeerDial) error {
	_ = ctx // ownership of the conn is explicit; session lifetime is managed via close()
	key := p.key(dial.AdapterType, dial.Scope)

	hosted := hostedAdapters(dial)
	if missing := p.missingDeclared(hosted); len(missing) > 0 {
		_ = conn.Close()
		return &PeerChildSetError{
			AdapterType: dial.AdapterType,
			Scope:       dial.Scope,
			Missing:     missing,
			Hosted:      hosted,
		}
	}

	ps, err := newPeerSession(conn, dial, p.peerDied)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("peer transport for adapter %q scope %q: %w", dial.AdapterType, dial.Scope, err)
	}

	// A replacement dial for the same key supersedes the previous session
	// (mirrors the legacy shim storing the new session and tearing the old
	// bridge down).
	replaced, waiters, old := p.storeSession(key, ps)

	// KB-213: drain the session waits for the conn's other hosted adapters
	// too (run-wide multi-adapter conns serve every hosted adapter); each
	// waiter receives the routed handle for its own adapter. Waiter keys
	// follow the registry's key semantics, so in per-scope mode only the
	// dial's own scope drains here (per-scope conns host one child each).
	routed := p.collectRoutedWaiters(ps, hosted, dial.Scope)

	if replaced {
		old.close("replaced by a new peer dial")
	}
	for _, ch := range waiters {
		ch <- waitResult{handle: ps.handle}
	}
	for _, w := range routed {
		w.ch <- waitResult{handle: w.handle}
	}

	go ps.supervise()
	slog.Info("peer adapter connected",
		"adapter", dial.AdapterType,
		"scope", dial.Scope,
		"digest", dial.Digest,
		"hosted", strings.Join(hosted, ","))
	return nil
}

// routedWake is one routed waiter wake: the wait channel and the handle of
// the hosted child it waits for.
type routedWake struct {
	ch     chan waitResult
	handle *peerHandle
}

// storeSession swaps the session into the registry under one p.mu hold,
// snapshotting (and removing) the dial adapter's pending waiters for the
// same critical section AcceptPeer documents. The boolean reports whether a
// previous session was replaced.
func (p *peerSessionProvider) storeSession(key string, ps *peerSession) (replaced bool, waiters []chan waitResult, old *peerSession) {
	p.mu.Lock()
	old, replaced = p.peers[key]
	p.peers[key] = ps
	waiters = p.waiters[key]
	delete(p.waiters, key)
	p.mu.Unlock()
	return replaced, waiters, old
}

// collectRoutedWaiters drains the pending waits of the conn's OTHER hosted
// adapters under the caller's p.mu hold, resolving each to the routed
// per-adapter handle. In per-scope mode only the dial's own scope drains
// here (per-scope conns host one child each).
func (p *peerSessionProvider) collectRoutedWaiters(ps *peerSession, hosted []string, scope string) []routedWake {
	p.mu.Lock()
	defer p.mu.Unlock()
	var routed []routedWake
	for _, hostName := range hosted {
		if hostName == ps.dial.AdapterType {
			continue
		}
		hostKey := p.key(hostName, scope)
		if hostWaiters := p.waiters[hostKey]; len(hostWaiters) > 0 {
			delete(p.waiters, hostKey)
			handle := ps.handleFor(hostName)
			for _, ch := range hostWaiters {
				routed = append(routed, routedWake{ch: ch, handle: handle})
			}
		}
	}
	return routed
}

// peerDied is the onDead hook invoked by a session's supervision consumer
// when its connection dies. A dead peer conn is a dead handle: drop the
// registry entry (if still current) so a respawn dial can take the key, and
// let the peer-side crash reporting (T-07) classify the exit.
func (p *peerSessionProvider) peerDied(ps *peerSession) {
	key := p.key(ps.dial.AdapterType, ps.dial.Scope)
	p.mu.Lock()
	if cur, ok := p.peers[key]; ok && cur == ps {
		delete(p.peers, key)
	}
	p.mu.Unlock()
}

// WaitForHandle blocks until a peer (or legacy) session for the adapter type
// + scope is available.
func (p *peerSessionProvider) WaitForHandle(ctx context.Context, adapterType, scope string) (adapterhost.Handle, error) {
	return p.WaitForFreshHandle(ctx, adapterType, scope, nil)
}

// WaitForFreshHandle blocks until a handle that is not `stale` is available.
// Resolution order: a live peer session in the provider's registry, then a
// legacy session already stored on the shim (mixed fleet), then a wait
// registered on both registries — a peer dial wakes peer waiters, a legacy
// handshake wakes the shim's waiters. The wait is bounded by the shim's two
// session-wait budgets (handshake + scheduling; KB-70) and shares the shim's
// diagnosis classes (CRI-137) through its awaitWaiter loop.
//
// Each registry resolves atomically: the peek and the waiter registration
// share one critical section — the provider's own under p.mu, the shim's via
// registerFreshWaiter — mirroring Shim.WaitForFreshHandle. Splitting them
// would let a handshake store the session and drain an empty waiter list in
// between, stranding the wait until the budget expired (lost wakeup).
func (p *peerSessionProvider) WaitForFreshHandle(ctx context.Context, adapterType, scope string, stale adapterhost.Handle) (adapterhost.Handle, error) {
	// KB-213 fail-closed waits: with a declared adapter set installed,
	// waiting for an undeclared adapter can never be satisfied (the peer pod
	// contract is "the pod hosts every declared adapter") — fail fast with
	// the typed error instead of burning the scheduling budget.
	if !p.declared.empty() && !p.declared.has(adapterType) {
		return nil, &PeerChildSetError{AdapterType: adapterType, Scope: scope}
	}

	key := p.key(adapterType, scope)

	// Peek + waiter registration share one p.mu critical section: if a fresh
	// peer handle is present, return it (never appending a waiter);
	// otherwise append the waiter under the same lock. AcceptPeer stores the
	// session and drains waiters in one section, so either it sees this
	// waiter, or this peek saw its session.
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return nil, errors.New("remote shim stopped")
	}
	if ps, ok := p.peers[key]; ok && ps.handle != stale {
		p.mu.Unlock()
		return ps.handle, nil
	}

	// KB-213: an exact key only exists for the conn's dial adapter. A
	// wait for a non-dial hosted adapter of a multi-adapter conn has to
	// find the conn in the registry by its hosted set instead — including
	// per-scope conns dialing scoped multi-adapter sets (their keys are
	// prefixed, never the bare wait key). Runs under the caller's p.mu
	// hold.
	if routed := p.findRoutedPeerSession(key, adapterType, scope, stale); routed != nil {
		p.mu.Unlock()
		return routed.handleFor(adapterType), nil
	}

	peerCh := make(chan waitResult, 1)
	p.waiters[key] = append(p.waiters[key], peerCh)
	p.mu.Unlock()

	// Legacy mixed-fleet path: the shim resolves its registry atomically the
	// same way, capturing the verify-failure budget under its own lock.
	legacyHandle, legacyCh, budget := p.shim.registerFreshWaiter(key, stale)
	if legacyHandle != nil {
		// A live legacy session was already available, so the peer waiter
		// registered above is dropped. Both channels are buffered, so a
		// concurrent peer dial's wake never blocks the notifier.
		p.removeWaiter(key, peerCh)
		return legacyHandle, nil
	}

	return p.shim.awaitWaiter(ctx, adapterType, scope, key, peerCh, legacyCh, func() {
		p.removeWaiter(key, peerCh)
		p.shim.removeWaiter(key, legacyCh)
	}, budget)
}

// removeWaiter drops a waiter channel from the provider's waiters map.
func (p *peerSessionProvider) removeWaiter(key string, ch chan waitResult) {
	p.mu.Lock()
	waiters := p.waiters[key]
	for i, w := range waiters {
		if w == ch {
			p.waiters[key] = append(waiters[:i], waiters[i+1:]...)
			break
		}
	}
	if len(p.waiters[key]) == 0 {
		delete(p.waiters, key)
	}
	p.mu.Unlock()
}

// RegisterScope delegates to the shim: accept-token verification stays in
// the shim's handshake path, so peer dials and legacy dials share one token
// store and one rotation story.
func (p *peerSessionProvider) RegisterScope(scope, token string) {
	p.shim.RegisterScope(scope, token)
}

// UnregisterScope delegates to the shim.
func (p *peerSessionProvider) UnregisterScope(scope string) {
	p.shim.UnregisterScope(scope)
}

// CloseHandle tears down the session for the adapter type + scope: a peer
// session closes its adapter session and issues a best-effort kill control
// before releasing the transport (mirroring the legacy shim's teardown
// order); otherwise the legacy shim path applies.
//
// KB-213: on a multi-adapter conn only the requested adapter's child is torn
// down, and the shared conn stays alive for the other hosted adapters — an
// exact-key conn (the dial adapter's own close) still releases the
// transport.
func (p *peerSessionProvider) CloseHandle(ctx context.Context, adapterType, scope string) error {
	key := p.key(adapterType, scope)
	p.mu.Lock()
	ps, ok := p.peers[key]
	delete(p.peers, key)
	exactConn := ok
	if !ok {
		// The adapter may be a hosted non-dial adapter of a multi-adapter
		// conn stored under its dial's key.
		for _, cand := range p.peers {
			if p.key(cand.dial.AdapterType, cand.dial.Scope) == key {
				continue
			}
			if p.perScopeSessions && cand.dial.Scope != scope {
				continue
			}
			if cand.hostsAdapter(adapterType) {
				ps = cand
				ok = true
				break
			}
		}
	}
	p.mu.Unlock()
	if ok {
		// KB-95/KB-96 teardown ordering (ADR-0008): tear the in-flight child
		// run down BEFORE the session closes — the child host's process
		// cleanup after the close must not kill a mid-flight run. The
		// teardown is bounded and best-effort (cancel control, journal
		// evidence wait, then force kill); v0.6.0 has no detach option.
		h := ps.handleFor(adapterType)
		ps.teardownInFlightChildRun(ctx, ps.journalFor(adapterType))
		_ = h.CloseSession(ctx, "")
		h.KillContext(ctx)
		if exactConn {
			ps.close("session closed")
		}
		return nil
	}
	return p.shim.CloseHandle(ctx, adapterType, scope)
}

// ListenAddr delegates to the shim (the phone-home listener is shared).
func (p *peerSessionProvider) ListenAddr() string {
	return p.shim.ListenAddr()
}

// Stop closes every peer session (ending the supervision consumers and the
// gRPC transports), wakes pending peer waiters, and tears the shim down
// (legacy sessions + listener + its waiters). Called by SessionManager
// Shutdown through the RemoteShim interface.
func (p *peerSessionProvider) Stop(ctx context.Context) error {
	p.stopOnce.Do(func() {
		p.mu.Lock()
		p.stopped = true
		peers := p.peers
		p.peers = make(map[string]*peerSession)
		waiters := p.waiters
		p.waiters = make(map[string][]chan waitResult)
		p.mu.Unlock()
		// KB-95/KB-96: tear down any in-flight child run before closing each
		// peer session — same ordering contract as CloseHandle on the
		// run-shutdown teardown path. Every hosted adapter's run is torn
		// down (KB-213): a multi-adapter conn owns several children.
		for _, ps := range peers {
			for _, aj := range ps.hostedJournals() {
				ps.teardownInFlightChildRun(ctx, aj)
			}
			ps.close("shim stopped")
		}
		for _, ws := range waiters {
			for _, ch := range ws {
				ch <- waitResult{err: errors.New("remote shim stopped")}
			}
		}
	})
	return p.shim.Stop(ctx)
}

// adapterJournal is the per-adapter supervision view of one peer connection
// (KB-213): with several adapter children sharing one phone-home connection,
// each hosted adapter owns its own slice of the conn's journal — terminal
// process facts, log-drain cursors, liveness, and the KB-95 child-run
// tracker. Events are routed by the supervision event's adapter_type
// attribution; events without attribution (pre-KB-213 peers) land on the
// conn's dial adapter. Every field is guarded by the owning peerSession's mu.
type adapterJournal struct {
	// name / scope identify the track in structured logs: the adapter type
	// it routes, and the conn's dial scope ("" in run-wide mode).
	name  string
	scope string

	exited        bool
	exitReason    string
	exitDetail    string
	lastHeartbeat time.Time
	logFlushed    map[string]uint64

	// childRuns / childWatches are the parent-side child-run tracker for
	// this adapter (KB-95): journal arm truth, plus per-run terminal
	// waiters.
	childRuns    map[string]*childRunRecord
	childWatches map[string][]chan struct{}
}

func newAdapterJournal(name, scope string) *adapterJournal {
	return &adapterJournal{
		name:         name,
		scope:        scope,
		logFlushed:   make(map[string]uint64),
		childRuns:    make(map[string]*childRunRecord),
		childWatches: make(map[string][]chan struct{}),
	}
}

// peerJournals routes one conn's supervision stream into per-adapter
// journals and answers "does this conn host adapter X?" for session
// dispatch. A conn hosts its dial adapter, plus every adapter a multi-child
// dial advertised (KB-213). All methods assume the owning peerSession's mu.
type peerJournals struct {
	dialAdapter string
	dialScope   string
	journals    map[string]*adapterJournal
}

func newPeerJournals(dialAdapter, dialScope string) *peerJournals {
	j := &peerJournals{
		dialAdapter: dialAdapter,
		dialScope:   dialScope,
		journals:    make(map[string]*adapterJournal),
	}
	j.journals[dialAdapter] = newAdapterJournal(dialAdapter, dialScope)
	return j
}

// trackFor returns the journal for adapterType, creating it on first use so
// late (or unattributed) events are never dropped.
func (j *peerJournals) trackFor(adapterType string) *adapterJournal {
	if adapterType == "" {
		adapterType = j.dialAdapter
	}
	aj, ok := j.journals[adapterType]
	if !ok {
		aj = newAdapterJournal(adapterType, j.dialScope)
		j.journals[adapterType] = aj
	}
	return aj
}

// peerSession is one accepted peer connection: the host-side gRPC client
// over the held phone-home net.Conn plus the supervision consumer consuming
// the peer's journal stream, routed into a per-adapter view (adapterJournal
// keyed adapter type) so multi-adapter conns keep every adapter's facts
// separate.
type peerSession struct {
	dial      PeerDial
	conn      net.Conn // the pre-established phone-home connection (owned)
	cc        *grpc.ClientConn
	client    adapterhost.Client
	handle    *peerHandle // the conn's dial-adapter handle (the exact-key handle)
	onDead    func(ps *peerSession)
	closeOnce sync.Once
	// dialed guards the one-shot context dialer: the held conn is handed
	// to grpc exactly once.
	dialed atomic.Bool

	// journals routes the conn's supervision stream by adapter_type: the
	// pre-KB-213 single-child shape (key = dial.AdapterType) is a one-entry
	// map with byte-identical behavior.
	journals *peerJournals

	mu sync.Mutex
	// lastSeq is the conn-level journal replay cursor (shared across the
	// journals: the Supervise stream delivers the union of all hosted
	// adapters' events in one order).
	lastSeq uint64

	// lost records conn-level loss (supervise consumer or an in-flight
	// Execute observing the transport die) — the classification input for
	// the workflow.v1 child-run-loss crash reason (KB-95).
	lost atomic.Bool

	// teardownSettleGrace shrinks the parent's child-run settle budget for
	// tests; zero keeps the production default (peerChildRunTeardownWait).
	// It is set right after session construction, before any teardown can
	// run, so reads need no synchronization beyond construction.
	teardownSettleGrace time.Duration

	// done is closed by close(): it wakes child-run terminal waiters on a
	// lost transport (no terminal can arrive past this point).
	done chan struct{}

	superviseCtx    context.Context
	superviseCancel context.CancelFunc
}

// newPeerSession builds the gRPC client over the pre-established connection.
// The context dialer returns the held conn exactly once; after a transport
// loss the dialer refuses further dials — the phone-home conn cannot be
// redialed, so a respawned adapter always presents a fresh handshake and a
// fresh peerSession (no hot reconnect loop, no noopAttachedRunner).
func newPeerSession(conn net.Conn, dial *PeerDial, onDead func(ps *peerSession)) (*peerSession, error) {
	ps := &peerSession{
		dial:   *dial,
		conn:   conn,
		onDead: onDead,
	}
	opts := append([]grpc.DialOption{
		grpc.WithContextDialer(func(ctx context.Context, target string) (net.Conn, error) {
			// CAS makes the one-shot handout race-free even under a
			// hypothetical concurrent dial; a lost transport always
			// requires the adapter to re-dial.
			if !ps.dialed.CompareAndSwap(false, true) {
				return nil, errors.New("criteria peer transport: connection already handed out; a lost transport requires the adapter to re-dial")
			}
			return ps.conn, nil
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}, adapterhost.RemoteKeepaliveDialOptions()...)
	cc, err := grpc.NewClient(peerDialTarget, opts...)
	if err != nil {
		return nil, fmt.Errorf("build peer grpc client: %w", err)
	}
	ps.cc = cc
	ps.client = adapterhost.NewClientForConn(cc)
	ps.journals = newPeerJournals(dial.AdapterType, dial.Scope)
	ps.done = make(chan struct{})
	ps.superviseCtx, ps.superviseCancel = context.WithCancel(context.Background())
	ps.handle = &peerHandle{ps: ps, name: dial.AdapterType, permActive: make(map[string]bool)}
	return ps, nil
}

// hostsAdapter reports whether this conn serves adapterType (KB-213): the
// dial adapter always, plus every non-dial adapter the dial's advertised
// child set names. The dial set — not the observed journals — is the
// authority: a conn can host an adapter that has not streamed an event yet.
func (ps *peerSession) hostsAdapter(adapterType string) bool {
	if adapterType == "" {
		adapterType = ps.dial.AdapterType
	}
	if adapterType == ps.dial.AdapterType {
		return true
	}
	for _, name := range peerChildNames(ps.dial.Adapters) {
		if name == adapterType {
			return true
		}
	}
	return false
}

// handleFor returns the routed handle for one hosted adapter: the conn's
// own handle for the dial adapter, or a fresh peerHandle whose requests
// carry the per-adapter route header for the others (KB-213). Adapters the
// conn does not host still get a handle — their requests produce the peer's
// typed no-live-child error, which keeps misdispatch diagnosable instead of
// silently crossing to the dial child.
func (ps *peerSession) handleFor(adapterType string) *peerHandle {
	if adapterType == "" || adapterType == ps.dial.AdapterType {
		return ps.handle
	}
	return &peerHandle{ps: ps, name: adapterType, permActive: make(map[string]bool)}
}

// hostedJournals returns a journal for each hosted adapter (dial-authoritative
// set, sorted names; used by full teardown).
func (ps *peerSession) hostedJournals() []*adapterJournal {
	names := hostedAdapters(&ps.dial)
	ps.mu.Lock()
	defer ps.mu.Unlock()
	journals := make([]*adapterJournal, 0, len(names))
	for _, name := range names {
		journals = append(journals, ps.journals.trackFor(name))
	}
	return journals
}

// journalFor returns the requested adapter's journal (creating lazily).
func (ps *peerSession) journalFor(adapterType string) *adapterJournal {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.journals.trackFor(adapterType)
}

// peerSessionOf extracts the peer session behind a host-side handle, or nil
// for legacy (shim byte-bridge) handles. Session-level freshness checks use
// it: a multi-adapter conn hands out several handles over one session, so
// handle identity cannot express "the same session came back".
// findRoutedPeerSession scans the registry (under the caller's p.mu hold)
// for a session of a DIFFERENT conn hosting the waited adapter, returning
// nil when the exact-key entry or scope mode excludes a hit.
func (p *peerSessionProvider) findRoutedPeerSession(key, adapterType, scope string, stale adapterhost.Handle) *peerSession {
	staleSession := peerSessionOf(stale)
	for _, ps := range p.peers {
		if ps == staleSession {
			continue
		}
		if p.key(ps.dial.AdapterType, ps.dial.Scope) == key {
			// The exact-key entry already handled the dial adapter itself.
			continue
		}
		if p.perScopeSessions && ps.dial.Scope != scope {
			continue
		}
		if ps.hostsAdapter(adapterType) {
			return ps
		}
	}
	return nil
}

func peerSessionOf(h adapterhost.Handle) *peerSession {
	if ph, ok := h.(*peerHandle); ok {
		return ph.ps
	}
	return nil
}

// supervise consumes the peer's supervision journal: reconnects with
// since_event_seq = lastSeq on stream-level resets (dedup drops replays) and
// reports conn-level death to the provider. Terminal events (ProcessExited,
// CrashClassified) mark the session exited — the crash-truth source that
// replaces the go-plugin reaper — while StreamFlushed tracks log drain and
// Heartbeat refreshes the liveness timestamp.
func (ps *peerSession) supervise() {
	ctx := ps.superviseCtx
	for {
		if ctx.Err() != nil {
			return
		}
		err := ps.superviseOnce(ctx)
		switch {
		case err == nil:
			// Stream closed cleanly by the peer; replay from lastSeq.
		case status.Code(err) == codes.Unimplemented:
			// A peer build predating PeerService: the adapter service is
			// still usable, only supervision is unavailable — ProcessExited
			// stays false and callers fall back to error-message heuristics,
			// exactly like a legacy handle.
			slog.Warn("peer adapter does not implement supervision; supervision unavailable",
				"adapter", ps.dial.AdapterType, "scope", ps.dial.Scope)
			return
		case ctx.Err() != nil:
			return
		default:
			// Any non-EOF supervise error is conn-level death: peer-initiated
			// teardown surfaces as a stream error while grpc-go's client state
			// can still read Ready, so gating on cc.GetState() here races the
			// consumer's eviction wait. The one-shot dialer makes a lost
			// transport unrecoverable either way (a respawn dial creates a
			// fresh peerSession with a fresh journal cursor).
			ps.died(err)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(peerSuperviseReplayBackoff):
		}
	}
}

// superviseOnce opens the Supervise stream from the session's last applied
// event_seq and applies every event until the stream ends.
func (ps *peerSession) superviseOnce(ctx context.Context) error {
	ss, err := ps.cc.NewStream(ctx, peerSuperviseStreamDesc, peerSuperviseMethod)
	if err != nil {
		return err
	}
	if err := ss.SendMsg(&criteriav1.SupervisionRequest{SinceEventSeq: ps.lastEventSeq()}); err != nil {
		return err
	}
	if err := ss.CloseSend(); err != nil {
		return err
	}
	for {
		ev := new(criteriav1.SupervisionEvent)
		if err := ss.RecvMsg(ev); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		ps.applySupervisionEvent(ev)
	}
}

// applySupervisionEvent routes one journal event. At-least-once delivery is
// deduped on event_seq (strictly monotonic within one peer conn). The event
// lands on the journal of its adapter_type attribution (KB-213 multi-adapter
// conns); events without attribution belong to the conn's dial adapter.
func (ps *peerSession) applySupervisionEvent(ev *criteriav1.SupervisionEvent) {
	ps.mu.Lock()
	if seq := ev.GetEventSeq(); seq != 0 {
		if seq <= ps.lastSeq {
			ps.mu.Unlock()
			return
		}
		ps.lastSeq = seq
	}
	adapterType := ev.GetAdapterType()
	if adapterType == "" {
		adapterType = ps.dial.AdapterType
	}
	aj := ps.journals.trackFor(adapterType)
	exited, reason, detail := aj.applyProcessTerminalArmLocked(ev)
	var terminalRunID string
	switch kind := ev.GetKind().(type) {
	case *criteriav1.SupervisionEvent_Flushed:
		if kind.Flushed.GetChannel() != "" {
			aj.logFlushed[kind.Flushed.GetChannel()] = kind.Flushed.GetUpToSeq()
		}
	case *criteriav1.SupervisionEvent_Heartbeat:
		aj.lastHeartbeat = time.Now()
	case *criteriav1.SupervisionEvent_Spawned:
		// Informational; the journal's spawn record for T-07's session
		// records.
	case *criteriav1.SupervisionEvent_ChildRunStarted,
		*criteriav1.SupervisionEvent_ChildRunTerminal,
		*criteriav1.SupervisionEvent_ChildRunTeardownPartial:
		// KB-95/KB-96 (ADR-0008 D2): workflow.v1 child-run tracking. Arm
		// routing and the watch broadcast happen through the tracker below
		// (the broadcast runs after this unlock).
		terminalRunID = aj.applyChildRunArmLocked(ev)
	}
	ps.mu.Unlock()

	if terminalRunID != "" {
		ps.noteChildRunTerminal(aj, terminalRunID)
	}

	if exited {
		// Terminal supervision event: the adapter child behind this conn has
		// exited. T-07 wires the session-record handoff (crash classification
		// + respawn) onto this hook; the handle's ProcessExited() already
		// reads the state set here.
		slog.Warn("peer adapter process exited",
			"adapter", adapterType, "scope", ps.dial.Scope,
			"reason", reason, "detail", detail)
	}
}

// applyProcessTerminalArmLocked merges the journal's terminal process-record
// arms (Exited placeholder + CrashClassified) under the session's mu. The
// peer journals a plain Exited record first and then a CrashClassified
// record for the same ungraceful exit (internal/peer/child.go recordExit →
// classifyUnexpectedExit), so a Crash event may arrive after the placeholder
// exit: overwrite it — the journal's classification is the wire fact T-07
// consumes verbatim. CrashClassified is terminal-only (the peer journals it
// exclusively at exit paths), so this cannot resurrect a live child.
func (aj *adapterJournal) applyProcessTerminalArmLocked(ev *criteriav1.SupervisionEvent) (exited bool, reason, detail string) {
	switch kind := ev.GetKind().(type) {
	case *criteriav1.SupervisionEvent_Exited:
		if !aj.exited {
			aj.exited = true
			aj.exitReason = exitReasonProcessExited
			aj.exitDetail = fmt.Sprintf("exit_code=%d signal=%d idle_ms=%d",
				kind.Exited.GetExitCode(), kind.Exited.GetSignal(), kind.Exited.GetIdleMs())
			return true, aj.exitReason, aj.exitDetail
		}
	case *criteriav1.SupervisionEvent_Crash:
		if !aj.exited || aj.exitReason == exitReasonProcessExited {
			aj.exited = true
			aj.exitReason = kind.Crash.GetReason()
			aj.exitDetail = kind.Crash.GetDetail()
			return true, aj.exitReason, aj.exitDetail
		}
	}
	return false, "", ""
}

// lastEventSeq reports the last applied journal sequence (the replay cursor).
func (ps *peerSession) lastEventSeq() uint64 {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.lastSeq
}

// processExited reports whether the peer journal delivered a terminal
// process event for the named adapter. Connection loss alone does not count:
// the transport may drop for reasons that are not adapter deaths.
func (ps *peerSession) processExited(adapterType string) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.journals.trackFor(adapterType).exited
}

// logDrained reports whether the peer acknowledged draining the log stream
// backlog (StreamFlushed on the "log" channel) for the named adapter.
func (ps *peerSession) logDrained(adapterType string) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	_, ok := ps.journals.trackFor(adapterType).logFlushed[peerLogChannel]
	return ok
}

// lastHeartbeatAt reports the named adapter's last journal heartbeat
// timestamp (liveness).
func (ps *peerSession) lastHeartbeatAt(adapterType string) time.Time {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.journals.trackFor(adapterType).lastHeartbeat
}

// teardownSettleBudget resolves the session's child-run settle budget for
// teardown waits.
func (ps *peerSession) teardownSettleBudget() time.Duration {
	if ps.teardownSettleGrace > 0 {
		return ps.teardownSettleGrace
	}
	return peerChildRunTeardownWait
}

// control issues a PeerService.Control unary call on the shared connection.
func (ps *peerSession) control(ctx context.Context, req *criteriav1.ControlRequest) (*criteriav1.ControlResponse, error) {
	resp := new(criteriav1.ControlResponse)
	if err := ps.cc.Invoke(ctx, peerControlMethod, req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// died marks the conn-level session dead: drop it from the provider's
// registry (if still current) and release the transport. The lost record is
// the KB-95 classification input: the child-run-loss crash reason keys on it.
func (ps *peerSession) died(cause error) {
	ps.lost.Store(true)
	if ps.onDead != nil {
		ps.onDead(ps)
	}
	ps.close(fmt.Sprintf("connection lost: %v", cause))
}

// close releases the session's transport. Idempotent; safe to call from the
// consumer, the provider, or both. Closing the done channel wakes child-run
// terminal waiters: no terminal can arrive past this point.
func (ps *peerSession) close(reason string) {
	ps.closeOnce.Do(func() {
		slog.Info("peer adapter session closing", "adapter", ps.dial.AdapterType, "scope", ps.dial.Scope, "reason", reason)
		ps.superviseCancel()
		// cc.Close tears down the transport (closing the phone-home conn);
		// the explicit conn close covers the accept-time failure path.
		_ = ps.cc.Close()
		_ = ps.conn.Close()
		close(ps.done)
	})
}

// peerHandle implements adapterhost.Handle (plus LogStreamStarter,
// PermissionStreamer and ProcessExitReporter) over a peer session. Every
// method is a direct v2 AdapterService call on the peer client — the same
// contract the go-plugin rpcHandle serves, so SessionManager dispatch and
// the HeartbeatMonitor / permission flows are unchanged.
type peerHandle struct {
	ps   *peerSession
	name string

	killOnce sync.Once // guards Kill
	permMu   sync.Mutex
	// permActive tracks session-scoped permission streams started via
	// StartPermissionStream; Execute skips its fallback per-Execute
	// permission stream when one is active (identical to rpcHandle).
	permActive map[string]bool
}

// routedClient wraps the conn's adapterhost.Client with the per-adapter
// route header (KB-213): on a multi-adapter conn the client-side connMux
// resolves the target child from the header. On single-child conns the mux
// is not installed and the peer ignores unknown metadata, so the header is
// always attached and never changes single-child behavior.
type routedClient struct {
	base  adapterhost.Client
	route string
}

func (rc *routedClient) ctx(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, peerAdapterRouteHeader, rc.route)
}

func (rc *routedClient) Info(ctx context.Context, req *v2.InfoRequest) (*v2.InfoResponse, error) {
	return rc.base.Info(rc.ctx(ctx), req)
}

func (rc *routedClient) OpenSession(ctx context.Context, req *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	return rc.base.OpenSession(rc.ctx(ctx), req)
}

func (rc *routedClient) Execute(ctx context.Context, req *v2.ExecuteRequest, sink adapterhost.ExecuteEventSink) error {
	return rc.base.Execute(rc.ctx(ctx), req, sink)
}

func (rc *routedClient) Log(ctx context.Context, req *v2.LogRequest, sink adapterhost.LogEventSink) error {
	return rc.base.Log(rc.ctx(ctx), req, sink)
}

func (rc *routedClient) Permissions(ctx context.Context, requests <-chan *v2.PermissionEvent) error {
	return rc.base.Permissions(rc.ctx(ctx), requests)
}

func (rc *routedClient) Pause(ctx context.Context, req *v2.PauseRequest) (*v2.PauseResponse, error) {
	return rc.base.Pause(rc.ctx(ctx), req)
}

func (rc *routedClient) Resume(ctx context.Context, req *v2.ResumeRequest) (*v2.ResumeResponse, error) {
	return rc.base.Resume(rc.ctx(ctx), req)
}

func (rc *routedClient) Snapshot(ctx context.Context, req *v2.SnapshotRequest) (*v2.SnapshotResponse, error) {
	return rc.base.Snapshot(rc.ctx(ctx), req)
}

func (rc *routedClient) Restore(ctx context.Context, req *v2.RestoreRequest) (*v2.RestoreResponse, error) {
	return rc.base.Restore(rc.ctx(ctx), req)
}

func (rc *routedClient) Inspect(ctx context.Context, req *v2.InspectRequest) (*v2.InspectResponse, error) {
	return rc.base.Inspect(rc.ctx(ctx), req)
}

func (rc *routedClient) CloseSession(ctx context.Context, req *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
	return rc.base.CloseSession(rc.ctx(ctx), req)
}

func (rc *routedClient) Prompt(ctx context.Context, req *adapterhost.PromptRequest) (*adapterhost.PromptResponse, error) {
	return rc.base.Prompt(rc.ctx(ctx), req)
}

// client returns the handle's routed adapter client: all v2 calls of this
// handle target the handle's own adapter child (KB-213).
func (h *peerHandle) client() adapterhost.Client {
	return &routedClient{base: h.ps.client, route: h.name}
}

// journal returns the handle's adapter journal (creating it lazily when the
// conn has not streamed an event for it yet).
func (h *peerHandle) journal() *adapterJournal {
	h.ps.mu.Lock()
	defer h.ps.mu.Unlock()
	return h.ps.journals.trackFor(h.name)
}

// Info returns the adapter's declared surface.
func (h *peerHandle) Info(ctx context.Context) (adapterhost.Info, error) {
	resp, err := h.client().Info(ctx, &v2.InfoRequest{})
	if err != nil {
		return adapterhost.Info{}, err
	}
	return adapterhost.Info{
		Name:              resp.GetName(),
		Version:           resp.GetVersion(),
		Capabilities:      append([]string(nil), resp.GetCapabilities()...),
		SupportedFeatures: append([]string(nil), resp.GetSupportedFeatures()...),
		Tools:             adapterhost.ToolsFromProto(resp.GetTools()),
		AdapterInfo:       adapterhost.AdapterInfoFromProto(resp),
	}, nil
}

// OpenSession opens an adapter session on the peer.
func (h *peerHandle) OpenSession(ctx context.Context, id string, config, secrets map[string]string) error {
	_, err := h.client().OpenSession(ctx, &v2.OpenSessionRequest{
		SessionId: id,
		Config:    cloneStringMap(config),
		Secrets:   cloneStringMap(secrets),
	})
	return err
}

// Execute streams one step through the shared host-side execute plumbing
// (fallback permission stream, chunk reassembly, needs_review override) —
// the exact path rpcHandle.Execute uses — with the KB-95 child-run mapping
// layered on top (one-shot execute, crash adoption, child-run-loss
// evidence) for workflow.v1 peers.
func (h *peerHandle) Execute(ctx context.Context, sessionID string, step *workflow.StepNode, sink adapter.EventSink, rejection *v2.ExecutionRejection) (adapter.Result, error) {
	h.permMu.Lock()
	hasPermStream := h.permActive[sessionID]
	h.permMu.Unlock()
	return h.executeChildRunAware(ctx, sessionID, hasPermStream, step, sink, rejection)
}

// CloseSession closes an adapter session on the peer.
func (h *peerHandle) CloseSession(ctx context.Context, id string) error {
	_, err := h.client().CloseSession(ctx, &v2.CloseSessionRequest{SessionId: id})
	return err
}

// Kill asks the peer to terminate the adapter child (Control{kill_child}).
// The peer reports the actual exit through the supervision journal, which is
// what flips ProcessExited() — there is no host-side process to signal.
func (h *peerHandle) Kill() {
	h.killOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), peerKillTimeout)
		defer cancel()
		h.killChild(ctx)
	})
}

// KillContext is the context-carrying variant used by teardown paths that
// already hold a context (peerSessionProvider.CloseHandle); it shares Kill's
// once-only guard.
func (h *peerHandle) KillContext(ctx context.Context) {
	h.killOnce.Do(func() { h.killChild(ctx) })
}

func (h *peerHandle) killChild(ctx context.Context) {
	resp, err := h.ps.control(ctx, &criteriav1.ControlRequest{
		AdapterType: h.name,
		Scope:       h.ps.dial.Scope,
		GraceMs:     peerKillGraceMs,
		Kind:        &criteriav1.ControlRequest_KillChild{KillChild: &criteriav1.KillChild{}},
	})
	if err != nil {
		slog.Warn("peer adapter kill_child control failed", "adapter", h.name, "error", err)
		return
	}
	if !resp.GetAccepted() {
		slog.Warn("peer adapter rejected kill_child control", "adapter", h.name, "detail", resp.GetDetail())
	}
}

// ProcessExited implements ProcessExitReporter from the supervision journal:
// true once the peer delivered ProcessExited or CrashClassified for this
// handle's (adapter_type, scope). This makes classifySessionCrash's precise
// branch (CrashReasonProcessExitedEarly) reachable for remote adapters —
// the role the go-plugin reaper plays for local adapters.
func (h *peerHandle) ProcessExited() bool {
	if h == nil || h.ps == nil {
		return false
	}
	return h.ps.processExited(h.name)
}

// SupervisionCrashReason implements adapterhost.SupervisedHandle (T-07): it
// returns the crash classification the supervision journal delivered
// (CrashClassified.reason verbatim — a peer-emitted CrashReason* wire fact,
// never a new string). ok=false when the journal only delivered the plain
// Exited placeholder: the host then classifies from the ProcessExited
// evidence (and, for legacy runners and peers whose Supervise stream is
// unavailable, from the string heuristics) exactly as before.
func (h *peerHandle) SupervisionCrashReason() (string, bool) {
	if h == nil || h.ps == nil {
		return "", false
	}
	ps := h.ps
	ps.mu.Lock()
	defer ps.mu.Unlock()
	aj := ps.journals.trackFor(h.name)
	if aj.exited {
		if aj.exitReason == exitReasonProcessExited {
			return "", false
		}
		return aj.exitReason, true
	}
	// KB-95 (ADR-0008): a transport loss while a child run was tracked in
	// flight is the crash fact for the step — the child's verdict did not
	// land before the connection died (the classifier prefers this over the
	// transport heuristics, without overriding the journal's own
	// CrashClassified evidence above).
	if ps.lost.Load() {
		if rec := aj.childRunInFlightLocked(); rec != nil {
			return adapterhost.CrashReasonChildRunLost, true
		}
	}
	return "", false
}

// Prompt delivers a mid-turn agent message into the live adapter session on
// the peer (ADR-0006 D3) through the same promptwire encoding rpcHandle
// uses. Capability gating stays in the SessionManager: adapters without
// supports_prompt never reach this method, so the peer path keeps the exact
// ADR-0006 failure taxonomy.
func (h *peerHandle) Prompt(ctx context.Context, req *adapterhost.PromptRequest) (*adapterhost.PromptResponse, error) {
	return h.client().Prompt(ctx, req)
}

// workflowV1IdleChildRun reports whether this peer's workflow.v1 child run
// tracker shows no in-flight run: the peer's child runs (at most one live
// serve-adapter run per workflow client) have all settled on the journal,
// so control verbs that target a live run are no-ops on it.
func (h *peerHandle) workflowV1IdleChildRun() bool {
	if h.ps == nil || !h.ps.hasPeerCapability(peerWorkflowV1Capability) {
		return false
	}
	_, inFlight := h.ps.childRunInFlight(h.name)
	return !inFlight
}

// Pause asks the peer adapter to halt work without losing state.
//
// KB-96 (ADR-0008 D2): for a workflow.v1 peer with no in-flight child run
// the pause is an idempotent ack WITHOUT a control round-trip — the child
// has nothing to park and its settled runs already persisted their own
// checkpoints (the child is its state's host of record), so the parent's
// pause barrier counts the session as acked instead of fail-closing on the
// child's typed no-live-run error. A live child run parks through the real
// Pause RPC (the child engine's boundary checkpoint).
func (h *peerHandle) Pause(ctx context.Context, sessionID string) error {
	if h.workflowV1IdleChildRun() {
		return nil
	}
	_, err := h.client().Pause(ctx, &v2.PauseRequest{SessionId: sessionID})
	return err
}

// Resume asks the peer adapter to continue from where it paused. A parked
// child run resumes through the real Resume RPC; a workflow.v1 peer with
// nothing in flight (its run settled or was cancelled while the parent
// considered it parked) acks idempotently without a control round-trip.
func (h *peerHandle) Resume(ctx context.Context, sessionID string) error {
	if h.workflowV1IdleChildRun() {
		return nil
	}
	_, err := h.client().Resume(ctx, &v2.ResumeRequest{SessionId: sessionID})
	return err
}

// Snapshot returns opaque adapter-defined state.
func (h *peerHandle) Snapshot(ctx context.Context, sessionID string) (*v2.SnapshotResponse, error) {
	return h.client().Snapshot(ctx, &v2.SnapshotRequest{SessionId: sessionID})
}

// Restore re-establishes adapter state from a prior snapshot.
func (h *peerHandle) Restore(ctx context.Context, sessionID string, state []byte, schemaVersion uint32) error {
	_, err := h.client().Restore(ctx, &v2.RestoreRequest{SessionId: sessionID, State: state, SchemaVersion: schemaVersion})
	return err
}

// Inspect returns structured read-only state about the session.
func (h *peerHandle) Inspect(ctx context.Context, sessionID string) (*v2.InspectResponse, error) {
	return h.client().Inspect(ctx, &v2.InspectRequest{SessionId: sessionID})
}

// StartLogStream starts the per-session Log server-stream on the peer. The
// stream must live for the whole session (the host's heartbeat contract):
// it runs detached on a cancel-free context and reports its terminal state
// on done (identical to rpcHandle).
func (h *peerHandle) StartLogStream(ctx context.Context, sessionID string, sink adapterhost.LogEventSink) (cancel func(), done <-chan error, err error) {
	logCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	doneCh := make(chan error, 1)
	go func() {
		err := h.client().Log(logCtx, &v2.LogRequest{SessionId: sessionID}, sink)
		if err != nil && !isExpectedStreamClose(err) {
			slog.Warn("peer adapter log stream closed unexpectedly", "adapter", h.name, "session", sessionID, "error", err)
		}
		doneCh <- err
		close(doneCh)
	}()
	return cancel, doneCh, nil
}

// StartPermissionStream starts the bidi Permissions RPC on the peer,
// tracking the active session so Execute skips its fallback stream
// (identical to rpcHandle; the HeartbeatMonitor and permission flows are
// unchanged).
func (h *peerHandle) StartPermissionStream(ctx context.Context, sessionID string, requests <-chan *v2.PermissionEvent) (cancel func(), err error) {
	h.permMu.Lock()
	h.permActive[sessionID] = true
	h.permMu.Unlock()

	permCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	go func() {
		defer func() {
			h.permMu.Lock()
			delete(h.permActive, sessionID)
			h.permMu.Unlock()
		}()
		err := h.client().Permissions(permCtx, requests)
		if status.Code(err) == codes.Unimplemented {
			// Drain the requests channel so Evaluate never blocks on a full
			// buffer after the Permissions stream has exited.
			for range requests {
			}
			return
		}
		if err != nil && !isExpectedStreamClose(err, codes.Unimplemented) {
			slog.Warn("peer adapter permission stream closed unexpectedly", "adapter", h.name, "session", sessionID, "error", err)
		}
	}()
	return cancel, nil
}

// cloneStringMap returns a defensive copy of a config map (the adapterhost
// cloneConfig equivalent for the peer path).
func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// isExpectedStreamClose mirrors the adapterhost helper's contract for the
// peer path: nil, EOF, context cancellation, and any of the supplied codes
// are benign stream endings. It is defined here (rather than imported)
// because adapterhost keeps it unexported.
func isExpectedStreamClose(err error, extra ...codes.Code) bool {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
		return true
	}
	c := status.Code(err)
	if c == codes.Canceled {
		return true
	}
	for _, x := range extra {
		if c == x {
			return true
		}
	}
	return false
}

// declaredAdapters is the provider's snapshot of the adapters an
// environment declares (KB-213) — the adapters its peer pod is expected to
// host. It is immutable once set and only ever swapped whole, so readers
// work on stable snapshots. An empty set disables the host's fail-closed
// checks (pre-KB-213 wiring and mixed fleets).
type declaredAdapters struct {
	mu     sync.RWMutex
	values map[string]bool
}

// set swaps the snapshot. An empty input clears it.
func (d *declaredAdapters) set(names []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(names) == 0 {
		d.values = nil
		return
	}
	values := make(map[string]bool, len(names))
	for _, name := range names {
		values[name] = true
	}
	d.values = values
}

func (d *declaredAdapters) empty() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.values) == 0
}

func (d *declaredAdapters) has(name string) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.values[name]
}

// snapshot returns the sorted declared names (stable for diagnostics).
func (d *declaredAdapters) snapshot() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	names := make([]string, 0, len(d.values))
	for name := range d.values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
