package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
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
func (p *peerSessionProvider) AcceptPeer(ctx context.Context, conn net.Conn, dial PeerDial) error {
	_ = ctx // ownership of the conn is explicit; session lifetime is managed via close()
	key := p.key(dial.AdapterType, dial.Scope)

	ps, err := newPeerSession(conn, dial, p.peerDied)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("peer transport for adapter %q scope %q: %w", dial.AdapterType, dial.Scope, err)
	}

	// A replacement dial for the same key supersedes the previous session
	// (mirrors the legacy shim storing the new session and tearing the old
	// bridge down).
	p.mu.Lock()
	old, ok := p.peers[key]
	p.peers[key] = ps
	waiters := p.waiters[key]
	delete(p.waiters, key)
	p.mu.Unlock()
	if ok {
		old.close("replaced by a new peer dial")
	}
	for _, ch := range waiters {
		ch <- waitResult{handle: ps.handle}
	}

	go ps.supervise()
	slog.Info("peer adapter connected",
		"adapter", dial.AdapterType,
		"scope", dial.Scope,
		"digest", dial.Digest)
	return nil
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
// handshake wakes the shim's waiters, and the wall-clock verify-failure
// budget bounds the whole wait with the shim's own diagnosis classes
// (CRI-137).
func (p *peerSessionProvider) WaitForFreshHandle(ctx context.Context, adapterType, scope string, stale adapterhost.Handle) (adapterhost.Handle, error) {
	key := p.key(adapterType, scope)

	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return nil, errors.New("remote shim stopped")
	}
	ps, ok := p.peers[key]
	p.mu.Unlock()
	if ok && ps.handle != stale {
		return ps.handle, nil
	}

	// Mixed fleet: a legacy (go-plugin reattach) session for this key may
	// already be live on the wrapped shim.
	p.shim.mu.Lock()
	if sess, ok := p.shim.sessions[key]; ok && sess.handle != stale {
		h := sess.handle
		p.shim.mu.Unlock()
		return h, nil
	}
	p.shim.mu.Unlock()

	// Register on both registries: the provider's own waiters (woken by
	// AcceptPeer) and the shim's (woken by a legacy handshake). Both channels
	// are buffered, so a simultaneous wake never blocks either notifier.
	peerCh := make(chan waitResult, 1)
	p.mu.Lock()
	p.waiters[key] = append(p.waiters[key], peerCh)
	p.mu.Unlock()

	legacyCh := make(chan waitResult, 1)
	p.shim.mu.Lock()
	budget := p.shim.verifyFailureBudget
	if budget <= 0 {
		budget = DefaultVerifyFailureBudget
	}
	p.shim.waiters[key] = append(p.shim.waiters[key], legacyCh)
	p.shim.mu.Unlock()

	budgetTimer := time.NewTimer(budget)
	defer budgetTimer.Stop()

	select {
	case res := <-peerCh:
		p.shim.removeWaiter(key, legacyCh)
		return res.handle, res.err
	case res := <-legacyCh:
		p.removeWaiter(key, peerCh)
		return res.handle, res.err
	case <-budgetTimer.C:
		p.removeWaiter(key, peerCh)
		p.shim.removeWaiter(key, legacyCh)
		return nil, p.shim.waitTimeoutError(adapterType, scope, key, budget)
	case <-ctx.Done():
		p.removeWaiter(key, peerCh)
		p.shim.removeWaiter(key, legacyCh)
		return nil, ctx.Err()
	}
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
func (p *peerSessionProvider) CloseHandle(ctx context.Context, adapterType, scope string) error {
	key := p.key(adapterType, scope)
	p.mu.Lock()
	ps, ok := p.peers[key]
	delete(p.peers, key)
	p.mu.Unlock()
	if ok {
		_ = ps.handle.CloseSession(ctx, "")
		ps.handle.Kill()
		ps.close("session closed")
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
		for _, ps := range peers {
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

// peerSession is one accepted peer connection: the host-side gRPC client
// over the held phone-home net.Conn plus the supervision consumer consuming
// the peer's journal stream.
type peerSession struct {
	dial      PeerDial
	conn      net.Conn // the pre-established phone-home connection (owned)
	cc        *grpc.ClientConn
	client    adapterhost.Client
	handle    *peerHandle
	onDead    func(ps *peerSession)
	closeOnce sync.Once

	mu            sync.Mutex
	exited        bool
	exitReason    string
	exitDetail    string
	lastSeq       uint64
	lastHeartbeat time.Time
	logFlushed    map[string]uint64

	superviseCtx    context.Context
	superviseCancel context.CancelFunc
}

// newPeerSession builds the gRPC client over the pre-established connection.
// The context dialer returns the held conn exactly once; after a transport
// loss the dialer refuses further dials — the phone-home conn cannot be
// redialed, so a respawned adapter always presents a fresh handshake and a
// fresh peerSession (no hot reconnect loop, no noopAttachedRunner).
func newPeerSession(conn net.Conn, dial PeerDial, onDead func(ps *peerSession)) (*peerSession, error) {
	ps := &peerSession{
		dial:   dial,
		conn:   conn,
		onDead: onDead,
	}
	dialed := false
	opts := append([]grpc.DialOption{
		grpc.WithContextDialer(func(ctx context.Context, target string) (net.Conn, error) {
			if dialed {
				return nil, errors.New("criteria peer transport: connection already handed out; a lost transport requires the adapter to re-dial")
			}
			dialed = true
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
	ps.logFlushed = make(map[string]uint64)
	ps.superviseCtx, ps.superviseCancel = context.WithCancel(context.Background())
	ps.handle = &peerHandle{ps: ps, name: dial.AdapterType, permActive: make(map[string]bool)}
	return ps, nil
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
		case ps.cc.GetState() != connectivity.Ready:
			// The one-shot dialer makes a lost transport unrecoverable: the
			// conn-level session is dead (a respawn dial creates a fresh
			// peerSession with a fresh journal cursor).
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
// deduped on event_seq (strictly monotonic within one peer conn).
func (ps *peerSession) applySupervisionEvent(ev *criteriav1.SupervisionEvent) {
	ps.mu.Lock()
	if seq := ev.GetEventSeq(); seq != 0 {
		if seq <= ps.lastSeq {
			ps.mu.Unlock()
			return
		}
		ps.lastSeq = seq
	}
	var exited bool
	var reason, detail string
	switch kind := ev.GetKind().(type) {
	case *criteriav1.SupervisionEvent_Exited:
		if !ps.exited {
			exited = true
			ps.exited = true
			ps.exitReason = "process_exited"
			ps.exitDetail = fmt.Sprintf("exit_code=%d signal=%d idle_ms=%d",
				kind.Exited.GetExitCode(), kind.Exited.GetSignal(), kind.Exited.GetIdleMs())
		}
	case *criteriav1.SupervisionEvent_Crash:
		if !ps.exited {
			exited = true
			ps.exited = true
			ps.exitReason = kind.Crash.GetReason()
			ps.exitDetail = kind.Crash.GetDetail()
		}
	case *criteriav1.SupervisionEvent_Flushed:
		if kind.Flushed.GetChannel() != "" {
			ps.logFlushed[kind.Flushed.GetChannel()] = kind.Flushed.GetUpToSeq()
		}
	case *criteriav1.SupervisionEvent_Heartbeat:
		ps.lastHeartbeat = time.Now()
	case *criteriav1.SupervisionEvent_Spawned:
		// Informational; the journal's spawn record for T-07's session
		// records.
	}
	if exited {
		reason, detail = ps.exitReason, ps.exitDetail
	}
	ps.mu.Unlock()

	if exited {
		// Terminal supervision event: the adapter child behind this conn has
		// exited. T-07 wires the session-record handoff (crash classification
		// + respawn) onto this hook; the handle's ProcessExited() already
		// reads the state set here.
		slog.Warn("peer adapter process exited",
			"adapter", ps.dial.AdapterType, "scope", ps.dial.Scope,
			"reason", reason, "detail", detail)
	}
}

// lastEventSeq reports the last applied journal sequence (the replay cursor).
func (ps *peerSession) lastEventSeq() uint64 {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.lastSeq
}

// processExited reports whether the peer journal delivered a terminal
// process event for this session. Connection loss alone does not count: the
// transport may drop for reasons that are not adapter deaths.
func (ps *peerSession) processExited() bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.exited
}

// logDrained reports whether the peer acknowledged draining the log stream
// backlog (StreamFlushed on the "log" channel).
func (ps *peerSession) logDrained() bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	_, ok := ps.logFlushed[peerLogChannel]
	return ok
}

// lastHeartbeatAt reports the last journal heartbeat timestamp (liveness).
func (ps *peerSession) lastHeartbeatAt() time.Time {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.lastHeartbeat
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
// registry (if still current) and release the transport.
func (ps *peerSession) died(cause error) {
	if ps.onDead != nil {
		ps.onDead(ps)
	}
	ps.close(fmt.Sprintf("connection lost: %v", cause))
}

// close releases the session's transport. Idempotent; safe to call from the
// consumer, the provider, or both.
func (ps *peerSession) close(reason string) {
	ps.closeOnce.Do(func() {
		slog.Info("peer adapter session closing", "adapter", ps.dial.AdapterType, "scope", ps.dial.Scope, "reason", reason)
		ps.superviseCancel()
		// cc.Close tears down the transport (closing the phone-home conn);
		// the explicit conn close covers the accept-time failure path.
		_ = ps.cc.Close()
		_ = ps.conn.Close()
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

	mu     sync.Once // guards Kill
	permMu sync.Mutex
	// permActive tracks session-scoped permission streams started via
	// StartPermissionStream; Execute skips its fallback per-Execute
	// permission stream when one is active (identical to rpcHandle).
	permActive map[string]bool
}

// Info returns the adapter's declared surface.
func (h *peerHandle) Info(ctx context.Context) (adapterhost.Info, error) {
	resp, err := h.ps.client.Info(ctx, &v2.InfoRequest{})
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
	_, err := h.ps.client.OpenSession(ctx, &v2.OpenSessionRequest{
		SessionId: id,
		Config:    cloneStringMap(config),
		Secrets:   cloneStringMap(secrets),
	})
	return err
}

// Execute streams one step through the shared host-side execute plumbing
// (fallback permission stream, chunk reassembly, needs_review override) —
// the exact path rpcHandle.Execute uses.
func (h *peerHandle) Execute(ctx context.Context, sessionID string, step *workflow.StepNode, sink adapter.EventSink) (adapter.Result, error) {
	h.permMu.Lock()
	hasPermStream := h.permActive[sessionID]
	h.permMu.Unlock()
	return adapterhost.ExecuteViaClient(ctx, h.ps.client, h.ps.dial.AdapterType, sessionID, hasPermStream, step, sink)
}

// CloseSession closes an adapter session on the peer.
func (h *peerHandle) CloseSession(ctx context.Context, id string) error {
	_, err := h.ps.client.CloseSession(ctx, &v2.CloseSessionRequest{SessionId: id})
	return err
}

// Kill asks the peer to terminate the adapter child (Control{kill_child}).
// The peer reports the actual exit through the supervision journal, which is
// what flips ProcessExited() — there is no host-side process to signal.
func (h *peerHandle) Kill() {
	h.mu.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), peerKillTimeout)
		defer cancel()
		resp, err := h.ps.control(ctx, &criteriav1.ControlRequest{
			AdapterType: h.ps.dial.AdapterType,
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
	})
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
	return h.ps.processExited()
}

// Pause asks the peer adapter to halt work without losing state.
func (h *peerHandle) Pause(ctx context.Context, sessionID string) error {
	_, err := h.ps.client.Pause(ctx, &v2.PauseRequest{SessionId: sessionID})
	return err
}

// Resume asks the peer adapter to continue from where it paused.
func (h *peerHandle) Resume(ctx context.Context, sessionID string) error {
	_, err := h.ps.client.Resume(ctx, &v2.ResumeRequest{SessionId: sessionID})
	return err
}

// Snapshot returns opaque adapter-defined state.
func (h *peerHandle) Snapshot(ctx context.Context, sessionID string) (*v2.SnapshotResponse, error) {
	return h.ps.client.Snapshot(ctx, &v2.SnapshotRequest{SessionId: sessionID})
}

// Restore re-establishes adapter state from a prior snapshot.
func (h *peerHandle) Restore(ctx context.Context, sessionID string, state []byte, schemaVersion uint32) error {
	_, err := h.ps.client.Restore(ctx, &v2.RestoreRequest{SessionId: sessionID, State: state, SchemaVersion: schemaVersion})
	return err
}

// Inspect returns structured read-only state about the session.
func (h *peerHandle) Inspect(ctx context.Context, sessionID string) (*v2.InspectResponse, error) {
	return h.ps.client.Inspect(ctx, &v2.InspectRequest{SessionId: sessionID})
}

// StartLogStream starts the per-session Log server-stream on the peer. The
// stream must live for the whole session (the host's heartbeat contract):
// it runs detached on a cancel-free context and reports its terminal state
// on done (identical to rpcHandle).
func (h *peerHandle) StartLogStream(ctx context.Context, sessionID string, sink adapterhost.LogEventSink) (cancel func(), done <-chan error, err error) {
	logCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	doneCh := make(chan error, 1)
	go func() {
		err := h.ps.client.Log(logCtx, &v2.LogRequest{SessionId: sessionID}, sink)
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
		err := h.ps.client.Permissions(permCtx, requests)
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

// closeOnce serializes a single teardown action (used by peerSession.close).
type closeOnce struct {
	once sync.Once
	done bool
	mu   sync.Mutex
}
