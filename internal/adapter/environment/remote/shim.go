// Package remote implements the "remote" environment handler and host-side
// phone-home shim for WS20.
package remote

import (
	"bufio"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	hplugin "github.com/hashicorp/go-plugin"

	"github.com/brokenbots/criteria/internal/adapterhost"
)

// handshakeMessage is the pre-gRPC identity frame sent by the adapter over
// the raw TLS connection before the gRPC client takes over the stream.
type handshakeMessage struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Digest  string `json:"digest"`
	Token   string `json:"token"`
	Scope   string `json:"scope,omitempty"`
	// Role negotiates what the dialing process is: "peer" routes the
	// connection to the PeerAcceptor seam instead of the legacy runner
	// byte-bridge (ADR-0007 D4). Absent or any other value keeps the legacy
	// path. Unknown handshake fields are tolerated by design, so older
	// runners that never send role keep dialing unchanged.
	Role string              `json:"role,omitempty"`
	Peer *PeerClientIdentity `json:"peer,omitempty"`
}

// handshakeRolePeer is the identity-frame role value that routes a dial to
// the PeerAcceptor seam (ADR-0007 D4). Any other value — including absent —
// keeps today's runner byte-bridge path.
const handshakeRolePeer = "peer"

// handshakeFrameCap bounds the identity frame read: the handshake is read
// before authentication, so an unbounded read was an unauthenticated
// memory-DoS (review §4.1). A valid handshake is ~200 bytes; 16 KiB leaves
// ample headroom for future optional fields.
const handshakeFrameCap = 16384

// PeerClientIdentity is the optional `peer` block of the identity frame,
// carried by dials that advertise role "peer" (ADR-0007). It is metadata for
// the peer acceptor; identity verification is unchanged and runs before the
// role branch.
type PeerClientIdentity struct {
	CriteriaVersion string   `json:"criteria_version,omitempty"`
	Capabilities    []string `json:"capabilities,omitempty"`
}

// PeerDial is the authenticated peer-role dial handed to a PeerAcceptor
// (T-06). Every identity field has already been verified by the shim (mTLS,
// identity pattern, lockfile digest, scope token) before AcceptPeer runs.
type PeerDial struct {
	// AdapterType is the verified handshake adapter name. It keys the
	// acceptor's peer registry the same way the shim keys legacy sessions.
	AdapterType string
	// Scope is the verified handshake scope ("scopeName/scopeInstanceID", or
	// "" in legacy mode).
	Scope string
	// Digest is the presented adapter digest (already verified).
	Digest string
	// Peer is the parsed `peer` block of the handshake; may be nil when the
	// dialer omitted it.
	Peer *PeerClientIdentity
}

// PeerAcceptor is the pluggable seam that receives authenticated peer-role
// connections (implemented by the peer runtime, T-06). A dial whose
// handshake advertises role "peer" is handed to AcceptPeer after the
// standard identity verification succeeds; the legacy byte-bridge path is
// not taken.
//
// Ownership: from the moment AcceptPeer is invoked the acceptor owns the
// connection and must close it before returning, whether it succeeds or
// fails.
type PeerAcceptor interface {
	AcceptPeer(ctx context.Context, conn net.Conn, dial PeerDial) error
}

// DigestVerifier checks whether a reported adapter digest is acceptable.
type DigestVerifier interface {
	Verify(adapterType string, digest string) error
}

// ErrScopeNotRegistered marks an adapter dial whose presented scope has no
// accept token registered on this shim (KB-25). It is deliberately distinct
// from a stale-token rejection: a scope that is not registered at all is the
// recoverable shape (the host's ScopeRegistrar may re-register the scope
// from its persisted state so the dialing pod's re-handshake is accepted),
// while a registered scope whose presented token no longer matches is a
// deliberate rotation and is only rejected.
var ErrScopeNotRegistered = errors.New("scope is not registered")

// ScopeNotRegisteredError is the typed form of an unregistered-scope dial
// rejection (KB-25). Its message is byte-identical to the pre-KB-25
// rejection string so existing operator log signatures stay stable.
type ScopeNotRegisteredError struct {
	AdapterType string
	Scope       string
}

func (e *ScopeNotRegisteredError) Error() string {
	return fmt.Sprintf("scope %q is not registered", e.Scope)
}

// Is reports the sentinel so callers can branch on the rejection class
// without unwrapping the concrete type.
func (e *ScopeNotRegisteredError) Is(target error) bool {
	return target == ErrScopeNotRegistered
}

// ScopeRegistrar is the dial-time re-registration seam (KB-25): when the
// shim receives a dial whose digest verifies but whose presented scope has
// no registered accept token, it consults the registrar once with the
// presented identity. A successful registration re-keys the shim's token
// map so the same dial can complete its handshake instead of looping on
// accept rejections; the shim re-checks its own token map afterwards, so a
// registrar that does not actually register the scope cannot turn a
// rejected dial into a session. Returning a non-nil error (or leaving the
// shim without a registrar) keeps the previous reject-only behavior.
//
// Implementations must verify the presented token against their own
// persisted state before re-registering: the shim only hands over dials
// whose digest already verified, but the scope token is the secret that
// distinguishes a rotated survivor from an unauthenticated guess.
type ScopeRegistrar interface {
	RegisterScopeOnDial(adapterType, scope, presentedToken string) error
}

// identityRejectClass classifies why an adapter dial failed identity
// verification, so the surfaced terminal error can name the right diagnosis
// instead of blaming the accept token for every rejection kind.
type identityRejectClass int

const (
	rejectNone identityRejectClass = iota
	rejectDigest
	rejectScopeNotRegistered
	rejectBadToken
)

// verifyFailureState remembers the most recent identity-verification
// rejection for a session key while a session waiter is pending on it. It is
// diagnostics only: the pending wait itself is bounded by the wall-clock
// budget, so no dialer can drive or trip the bound (CRI-137 review hardening).
type verifyFailureState struct {
	lastErr string
	class   identityRejectClass
}

// DefaultVerifyFailureBudget is the authoritative wall-clock bound for a
// pending session wait once the adapter pod has started: if no adapter
// re-handshakes within the budget, the wait fails terminally. It covers the
// stale-pod case (dials keep being rejected) and the dead-Job case (a started
// pod that never dials again); there is deliberately no separate
// rejection-count bound (CRI-137 review: one authoritative bound).
const DefaultVerifyFailureBudget = 5 * time.Minute

// DefaultSchedulingBudget bounds the wait while the adapter pod has NOT yet
// started (KB-70): a burst of per-scope pod creations can keep pods Pending
// well past the handshake budget, and a pod that never started cannot
// handshake, so consuming the handshake budget for it false-positively fails
// the scope. While the pod is still Pending (or unobserved), the handshake
// budget stays frozen and only this, longer, scheduling budget elapses.
const DefaultSchedulingBudget = 15 * time.Minute

// defaultPodStatePollInterval is how often a pending wait re-observes pod
// state and dial activity while the adapter has not started (no dial seen for
// its key). Once a dial is observed the wait switches to an exact
// handshake-budget timer, so this cadence only adds latency to scheduling.
const defaultPodStatePollInterval = 5 * time.Second

// PodState reports the lifecycle phase of the adapter pod backing a session
// key, as observed by the infrastructure hosting the shim (e.g. an operator
// projecting Kubernetes pod status). The zero-value phase is empty; probes
// that have no observation return ok=false.
type PodState struct {
	Phase string
}

// PodStateProbe is an optional seam letting the host surface adapter pod
// lifecycle phases into the session wait. It is nil in a bare shim; wiring it
// is entirely optional — without it, the wait still distinguishes "pod
// started" from "pod not started" by adapter dial activity (an adapter
// process that is up must have dialed the shim to present an identity frame).
type PodStateProbe interface {
	PodState(adapterType, scope string) (PodState, bool)
}

// podPhaseStarted reports whether the phase names a pod whose processes run.
func podPhaseStarted(phase string) bool {
	return strings.EqualFold(phase, "Running")
}

// podPhaseTerminal reports whether the phase names a pod whose Job finished
// (or died) and can never dial again. Such a wait cannot be rescued by
// waiting longer.
func podPhaseTerminal(phase string) bool {
	return strings.EqualFold(phase, "Succeeded") || strings.EqualFold(phase, "Failed")
}

// Shim listens for inbound adapter connections, terminates mTLS, verifies
// identity, and presents each connection as a local-looking Handle.
type Shim struct {
	listenAddr            string
	tlsConfig             *tls.Config
	acceptToken           string
	clientIdentityPattern string
	clientIdentityRe      *regexp.Regexp
	digestVerifier        DigestVerifier
	insecure              bool
	tlsHandshakeDeadline  time.Duration
	identityDeadline      time.Duration

	mu               sync.Mutex
	sessions         map[string]*session // adapter type → active session (legacy) or adapter type + scope → session
	waiters          map[string][]chan waitResult
	listener         net.Listener
	started          bool
	perScopeSessions bool
	scopeTokens      map[string]string // scope → accept token (only when perScopeSessions is true)

	verifyFailures      map[string]*verifyFailureState // session key → last identity-verification rejection (diagnostics while a waiter is pending)
	verifyFailureBudget time.Duration

	schedulingBudget     time.Duration        // bounds the wait while the adapter pod has not started (KB-70)
	podStatePollInterval time.Duration        // re-observation cadence while not started
	podProbe             PodStateProbe        // optional pod-state seam; nil in a bare shim
	dialActivity         map[string]time.Time // session key → last time an adapter presented an identity frame (pod-started evidence)

	peerAcceptor   PeerAcceptor   // receives authenticated role="peer" dials; nil rejects them
	scopeRegistrar ScopeRegistrar // consulted for unregistered-scope dials (KB-25); nil keeps reject-only
}

type session struct {
	handle     adapterhost.Handle
	cancel     func()
	cancelCtx  context.Context
	socketPath string
}

type waitResult struct {
	handle adapterhost.Handle
	err    error
}

// resolveHandshakeDeadlines defaults the optional TLS and identity handshake
// deadlines.
func resolveHandshakeDeadlines(cfg *Config) (tlsDeadline, identityDeadline time.Duration) {
	tlsDeadline = cfg.TLSHandshakeDeadline
	if tlsDeadline == 0 {
		tlsDeadline = DefaultTLSHandshakeDeadline
	}
	identityDeadline = cfg.IdentityHandshakeDeadline
	if identityDeadline == 0 {
		identityDeadline = DefaultIdentityHandshakeDeadline
	}
	return tlsDeadline, identityDeadline
}

// resolveWaitBudgets defaults the optional KB-70 session-wait budget knobs
// (zero or negative Config values select the defaults).
func resolveWaitBudgets(cfg *Config) (handshakeBudget, schedulingBudget time.Duration) {
	handshakeBudget = cfg.SessionHandshakeBudget
	if handshakeBudget <= 0 {
		handshakeBudget = DefaultVerifyFailureBudget
	}
	schedulingBudget = cfg.SessionSchedulingBudget
	if schedulingBudget <= 0 {
		schedulingBudget = DefaultSchedulingBudget
	}
	return handshakeBudget, schedulingBudget
}

// NewShim builds a Shim from a parsed Config and a digest verifier.
func NewShim(cfg *Config, verifier DigestVerifier) (*Shim, error) {
	if err := validateListenerSecurity(cfg); err != nil {
		return nil, fmt.Errorf("remote shim: %w", err)
	}

	tlsConf, err := buildTLSConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("remote shim: build tls config: %w", err)
	}
	var re *regexp.Regexp
	if cfg.ClientIdentityPattern != "" {
		re, err = regexp.Compile(cfg.ClientIdentityPattern)
		if err != nil {
			return nil, fmt.Errorf("remote shim: invalid client_identity_pattern: %w", err)
		}
	}

	tlsDeadline, identityDeadline := resolveHandshakeDeadlines(cfg)
	handshakeBudget, schedulingBudget := resolveWaitBudgets(cfg)
	if err := validateHandshakeDeadline("tls_handshake_deadline", tlsDeadline); err != nil {
		return nil, fmt.Errorf("remote shim: %w", err)
	}
	if err := validateHandshakeDeadline("identity_handshake_deadline", identityDeadline); err != nil {
		return nil, fmt.Errorf("remote shim: %w", err)
	}

	return &Shim{
		listenAddr:            cfg.ListenAddress,
		tlsConfig:             tlsConf,
		acceptToken:           cfg.AcceptToken,
		clientIdentityPattern: cfg.ClientIdentityPattern,
		clientIdentityRe:      re,
		digestVerifier:        verifier,
		insecure:              cfg.Insecure,
		tlsHandshakeDeadline:  tlsDeadline,
		identityDeadline:      identityDeadline,
		sessions:              make(map[string]*session),
		waiters:               make(map[string][]chan waitResult),
		perScopeSessions:      cfg.PerScopeSessions,
		scopeTokens:           make(map[string]string),
		verifyFailures:        make(map[string]*verifyFailureState),
		verifyFailureBudget:   handshakeBudget,
		schedulingBudget:      schedulingBudget,
		podStatePollInterval:  defaultPodStatePollInterval,
		dialActivity:          make(map[string]time.Time),
	}, nil
}

// phoneHomeKeepAlive is the TCP keepalive applied to accepted phone-home
// connections (CRI-276): accepted conns get no OS-level keepalive by default,
// so a silently-dead peer would go unnoticed on the host side. Dialed conns
// (the pod side) already enable the Go default (15s).
const phoneHomeKeepAlive = 15 * time.Second

// Start binds the listener. Called at workflow startup if any remote env
// is referenced; skipped if no remote env is referenced (compile-time fold).
func (s *Shim) Start(ctx context.Context) error {
	// TCP keepalive on accepted phone-home connections keeps the path warm
	// from both ends and detects half-open connections (CRI-276). UDS
	// listeners ignore it.
	lc := net.ListenConfig{KeepAlive: phoneHomeKeepAlive}
	var lis net.Listener
	var err error

	if s.tlsConfig != nil {
		raw, lerr := lc.Listen(ctx, "tcp", s.listenAddr)
		lis, err = tls.NewListener(raw, s.tlsConfig), lerr
	} else {
		// Support both TCP and Unix socket addresses.
		if filepath.IsAbs(s.listenAddr) || s.listenAddr != "" && s.listenAddr[0] == '/' {
			// Try unix socket for absolute paths.
			if err := checkUnixSocketPath(s.listenAddr); err != nil {
				return fmt.Errorf("remote shim: %w", err)
			}
			lis, err = lc.Listen(ctx, "unix", s.listenAddr)
		} else {
			lis, err = lc.Listen(ctx, "tcp", s.listenAddr)
		}
	}
	if err != nil {
		return fmt.Errorf("remote shim: listen %q: %w", s.listenAddr, err)
	}

	s.mu.Lock()
	s.listener = lis
	s.started = true
	s.mu.Unlock()

	if s.insecure && s.tlsConfig == nil && s.acceptToken == "" {
		slog.Warn("remote shim listening without authentication", "addr", lis.Addr().String())
	} else {
		slog.Info("remote shim listening", "addr", lis.Addr().String())
	}

	go s.serve(ctx, lis)
	return nil
}

// checkUnixSocketPath validates that addr fits within the platform's
// sockaddr_un.sun_path limit. net.Listen otherwise fails with an opaque
// "bind: invalid argument"; this returns a clear, actionable error. macOS caps
// sun_path at 104 bytes (103 usable + NUL); Linux/BSD allow 108.
func checkUnixSocketPath(addr string) error {
	maxLen := 107 // Linux/BSD: 108-byte sun_path, 107 usable.
	if runtime.GOOS == "darwin" {
		maxLen = 103 // macOS: 104-byte sun_path, 103 usable.
	}
	if len(addr) > maxLen {
		return fmt.Errorf("unix socket path %q is %d bytes, exceeding the %d-byte limit on %s; use a shorter listen_address (e.g. under /tmp)", addr, len(addr), maxLen, runtime.GOOS)
	}
	return nil
}

// Stop closes the listener and all active sessions.
func (s *Shim) Stop(ctx context.Context) error {
	s.mu.Lock()
	if s.listener != nil {
		_ = s.listener.Close()
	}
	for _, sess := range s.sessions {
		if sess.cancel != nil {
			sess.cancel()
		}
		if sess.handle != nil {
			_ = sess.handle.CloseSession(ctx, "")
			sess.handle.Kill()
		}
	}
	s.sessions = make(map[string]*session)
	// Wake up any waiters with an error.
	for _, waiters := range s.waiters {
		for _, ch := range waiters {
			ch <- waitResult{err: fmt.Errorf("remote shim stopped")}
		}
	}
	s.waiters = make(map[string][]chan waitResult)
	s.mu.Unlock()
	return nil
}

// SetPerScopeSessions enables or disables per-scope session isolation.
// When enabled, the shim keys active sessions and waiters by adapter type
// plus scope, and each scope must register its own accept token before a
// connection is accepted. This is used by the engine to support Phase 2a
// remote-adapter lifecycle events and token rotation.
func (s *Shim) SetPerScopeSessions(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.perScopeSessions = enabled
}

// SetPeerAcceptor installs the seam that receives authenticated peer-role
// connections. When unset, peer-role dials are rejected: a peer conn is a
// gRPC server (the peer is the server on the phone-home conn), so the legacy
// byte-bridge path cannot serve it.
func (s *Shim) SetPeerAcceptor(pa PeerAcceptor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.peerAcceptor = pa
}

// SetPodStateProbe installs an optional seam through which the host surfaces
// adapter pod lifecycle phases into pending session waits (KB-70). When nil,
// the wait falls back to dial activity as the only pod-started signal.
func (s *Shim) SetPodStateProbe(probe PodStateProbe) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.podProbe = probe
}

// noteDialActivity records the most recent time an adapter presented a parsed
// identity frame for a session key. Presenting a frame proves the adapter
// process started, independent of whether the identity later verifies; the
// session wait uses this as its default pod-started signal (KB-70) when no
// pod-state probe is wired.
func (s *Shim) noteDialActivity(adapterType, scope string) {
	key := s.sessionKey(adapterType, scope)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dialActivity == nil {
		s.dialActivity = make(map[string]time.Time)
	}
	s.dialActivity[key] = time.Now()
}

// dialObserved reports whether any identity frame was ever presented for key.
func (s *Shim) dialObserved(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.dialActivity[key]
	return ok
}

// activityObserved reports whether the shim has seen in-band identity
// activity bound to this session key: an identity frame was presented
// (dialObserved) or an identity-verification rejection was attributed to
// waiters of the key. Attributed rejections come from dials of the same
// adapter type and scope-name prefix — including the stale pre-rotation pod
// after a runner restart (CRI-137) — so any of them proves the adapter
// process is up and dialing. The session wait treats them as pod-started
// evidence and runs the handshake budget rather than the scheduling grace
// (KB-70).
func (s *Shim) activityObserved(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.dialActivity[key]; ok {
		return true
	}
	return s.verifyFailures[key] != nil
}

// observePod consults the optional pod-state probe outside s.mu (the probe
// may re-enter the shim). A nil probe or a panic inside it yields no
// observation so a defective seam degrades to the dial-activity signal.
func (s *Shim) observePod(adapterType, scope string) (phase string, ok bool) {
	s.mu.Lock()
	probe := s.podProbe
	s.mu.Unlock()
	if probe == nil {
		return "", false
	}
	defer func() {
		if recover() != nil {
			ok = false
			phase = ""
		}
	}()
	st, observed := probe.PodState(adapterType, scope)
	return st.Phase, observed
}

// SetScopeRegistrar installs the dial-time re-registration seam (KB-25).
// When unset, a dial presenting a valid digest for an unregistered scope is
// rejected with ErrScopeNotRegistered and its connection is closed, exactly
// as before the seam existed.
func (s *Shim) SetScopeRegistrar(r ScopeRegistrar) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scopeRegistrar = r
}

// RegisterScope registers (or updates) the accept token for a given scope.
// It is only consulted when perScopeSessions is enabled.
func (s *Shim) RegisterScope(scope, token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scopeTokens == nil {
		s.scopeTokens = make(map[string]string)
	}
	s.scopeTokens[scope] = token
}

// UnregisterScope removes the accept token for a scope. After this call the
// shim rejects any reconnect using the old token.
func (s *Shim) UnregisterScope(scope string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.scopeTokens, scope)
}

// sessionKey returns the map key used for sessions and waiters. When scope
// isolation is off (legacy behaviour) the key is the adapter type only so
// existing callers and tests see byte-identical behaviour.
func (s *Shim) sessionKey(adapterType, scope string) string {
	if !s.perScopeSessions || scope == "" {
		return adapterType
	}
	return adapterType + "\x00" + scope
}

// ListenAddr returns the shim's bound listen address, or the configured
// listen address if the shim has not started.
func (s *Shim) ListenAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener != nil {
		return s.listener.Addr().String()
	}
	return s.listenAddr
}

func (s *Shim) serve(ctx context.Context, lis net.Listener) {
	for {
		conn, err := lis.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go func(c net.Conn) {
			if err := s.Accept(ctx, c); err != nil {
				slog.Warn("remote shim accept failed", "error", err)
			}
		}(conn)
	}
}

// Accept handles inbound mTLS connections, validates identity + lockfile
// digest, creates a local UDS, spawns the bridge goroutine, and produces
// a Reattach-mode Client for the session layer.
func (s *Shim) Accept(ctx context.Context, conn net.Conn) error {
	if err := s.performHandshake(ctx, conn); err != nil {
		return err
	}

	hs, err := s.readHandshakeMessage(conn)
	if err != nil {
		return err
	}

	// The adapter presented a parsed identity frame, so its process is up:
	// remember this even if identity verification fails below, so pending
	// session waits can tell "pod started but rejected" from "pod never
	// started" (KB-70).
	s.noteDialActivity(hs.Name, hs.Scope)

	if err := s.verifyAdapterIdentity(conn, &hs); err != nil {
		return err
	}

	if hs.Role == handshakeRolePeer {
		return s.acceptPeerConn(ctx, conn, &hs)
	}

	socketPath, lis, err := s.setupUDS(conn)
	if err != nil {
		return err
	}

	res, err := s.bridgeAndDial(ctx, conn, lis, socketPath)
	if err != nil {
		_ = lis.Close()
		_ = os.RemoveAll(filepath.Dir(socketPath))
		return err
	}

	return s.buildAndStoreHandle(ctx, hs.Name, hs.Scope, conn, res.udsConn, lis, socketPath, res.client, res.pluginClient, res.bridgeCancel, res.bridgeCtx, res.bridgeWG)
}

// acceptPeerConn hands an authenticated peer-role connection to the
// configured PeerAcceptor. Auth (mTLS, identity pattern, digest, token) has
// already run; the acceptor receives the verified dial identity it needs to
// supervise the peer. Ownership of conn transfers to the acceptor (see
// PeerAcceptor).
func (s *Shim) acceptPeerConn(ctx context.Context, conn net.Conn, hs *handshakeMessage) error {
	s.mu.Lock()
	acceptor := s.peerAcceptor
	s.mu.Unlock()
	if acceptor == nil {
		_ = conn.Close()
		return fmt.Errorf("peer role dial from %q rejected: no peer acceptor configured", hs.Name)
	}
	dial := PeerDial{AdapterType: hs.Name, Scope: hs.Scope, Digest: hs.Digest, Peer: hs.Peer}
	return acceptor.AcceptPeer(ctx, conn, dial)
}

func (s *Shim) performHandshake(ctx context.Context, conn net.Conn) error {
	var certSubject string
	if tlsConn, ok := conn.(*tls.Conn); ok {
		if err := tlsConn.SetDeadline(time.Now().Add(s.tlsHandshakeDeadline)); err != nil {
			_ = conn.Close()
			return fmt.Errorf("set tls handshake deadline: %w", err)
		}
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			if isDeadlineTimeout(err) {
				return fmt.Errorf("TLS handshake deadline (%s) exceeded; timeout caused rejection", s.tlsHandshakeDeadline)
			}
			return fmt.Errorf("mtls handshake: %w", err)
		}
		_ = tlsConn.SetDeadline(time.Time{})
		state := tlsConn.ConnectionState()
		if len(state.PeerCertificates) > 0 {
			certSubject = state.PeerCertificates[0].Subject.String()
		}
	}
	if err := ValidateClientIdentity(certSubject, s.clientIdentityRe); err != nil {
		_ = conn.Close()
		return err
	}
	return nil
}

func (s *Shim) readHandshakeMessage(conn net.Conn) (handshakeMessage, error) {
	_ = conn.SetReadDeadline(time.Now().Add(s.identityDeadline))
	header, err := readHandshakeFrame(bufio.NewReader(conn), handshakeFrameCap)
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		_ = conn.Close()
		if errors.Is(err, errHandshakeFrameTooLarge) {
			// Distinct diagnostics for the unauthenticated memory-DoS shape
			// (review §4.1): the frame is rejected before any parse, and the
			// oversized connection is closed without buffering the rest.
			slog.Warn(fmt.Sprintf("identity frame exceeds %d bytes", handshakeFrameCap))
			return handshakeMessage{}, fmt.Errorf("identity frame exceeds %d bytes", handshakeFrameCap)
		}
		if isDeadlineTimeout(err) {
			return handshakeMessage{}, fmt.Errorf("identity message deadline (%s) exceeded; timeout caused rejection", s.identityDeadline)
		}
		return handshakeMessage{}, fmt.Errorf("read handshake: %w", err)
	}

	var hs handshakeMessage
	if err := json.Unmarshal(header, &hs); err != nil {
		_ = conn.Close()
		return handshakeMessage{}, fmt.Errorf("unmarshal handshake: %w", err)
	}
	return hs, nil
}

// errHandshakeFrameTooLarge marks a handshake frame that grew past the cap
// before its terminating newline; readHandshakeMessage maps it to the
// distinct oversize diagnostic and connection close.
var errHandshakeFrameTooLarge = errors.New("handshake frame too large")

// readHandshakeFrame reads one '\n'-terminated identity frame from reader,
// aborting once the frame exceeds limit bytes (review §4.1). The returned
// header excludes the newline. Exactly limit bytes are still accepted; only
// longer frames are rejected, so the cap never rejects a frame a compliant
// dialer sends.
func readHandshakeFrame(reader *bufio.Reader, limit int) ([]byte, error) {
	var header []byte
	for {
		b, err := reader.ReadByte()
		if err != nil {
			return nil, err
		}
		if b == '\n' {
			return header, nil
		}
		if len(header) >= limit {
			return nil, errHandshakeFrameTooLarge
		}
		header = append(header, b)
	}
}

func (s *Shim) verifyAdapterIdentity(conn net.Conn, hs *handshakeMessage) error {
	class, err := s.checkAdapterIdentity(hs)
	if err == nil {
		s.clearVerifyFailure(hs.Name, hs.Scope)
		return nil
	}
	// KB-25: an unregistered-scope dial (digest verified, but no accept
	// token registered for the scope) is the recoverable shape — after a
	// token rotation or runner restart a pod can keep dialing a scope key
	// this shim has no token for. Give the installed registrar one chance to
	// re-register the scope from its persisted state, then re-run the token
	// check on the same connection instead of closing it and leaving the
	// dialer in a silent rejection loop. A stale-token dial (registered
	// scope, mismatching token) never reaches the registrar: rotation is
	// deliberate and must not resurrect a pre-rotation pod.
	if class == rejectScopeNotRegistered && s.tryScopeRecovery(hs) {
		class, err = s.checkAdapterIdentity(hs)
		if err == nil {
			slog.Info("remote shim accepted dial after scope re-registration",
				"adapter", hs.Name, "scope", hs.Scope)
			s.clearVerifyFailure(hs.Name, hs.Scope)
			return nil
		}
	}
	// Final rejection: close the connection exactly once. checkAdapterIdentity
	// is a pure check (it no longer closes the dial itself) so the recovery
	// re-check could run with the connection still open.
	_ = conn.Close()
	s.noteVerifyFailure(hs.Name, hs.Scope, class, err)
	return err
}

// tryScopeRecovery consults the installed registrar for a rejected
// unregistered-scope dial. It returns true only when the registrar itself
// reported success; the caller then re-checks the shim's own token map
// before the dial is accepted.
func (s *Shim) tryScopeRecovery(hs *handshakeMessage) bool {
	s.mu.Lock()
	registrar := s.scopeRegistrar
	s.mu.Unlock()
	if registrar == nil {
		return false
	}
	if err := registrar.RegisterScopeOnDial(hs.Name, hs.Scope, hs.Token); err != nil {
		slog.Debug("remote shim scope re-registration declined the dial",
			"adapter", hs.Name, "scope", hs.Scope, "error", err.Error())
		return false
	}
	return true
}

// noteVerifyFailure records an identity-verification rejection as diagnostics
// for pending session waiters. The pending wait itself is bounded by the
// wall-clock budget in WaitForFreshHandle, so these records only shape the
// terminal error an operator sees when the budget expires (stale adapter pod
// holding a pre-rotation accept token, digest mismatch, or no dials at all —
// CRI-137). Failures observed while nobody is waiting are not recorded.
//
// Attribution is scoped to the pending waiter's expected identity: a dial
// whose presented scope is not registered (the stale-pod-on-old-key shape
// after a runner restart) is attributed only to waiters of the same adapter
// type whose scope shares the dial's scope name prefix. An unrelated adapter
// type — or an unrelated scope name — can never have its wait poisoned by
// another dialer's rejections.
func (s *Shim) noteVerifyFailure(adapterType, scope string, class identityRejectClass, cause error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, key := range s.failureAttributionKeys(adapterType, scope, class) {
		if len(s.waiters[key]) == 0 {
			delete(s.verifyFailures, key)
			continue
		}
		if s.verifyFailures == nil {
			s.verifyFailures = make(map[string]*verifyFailureState)
		}
		s.verifyFailures[key] = &verifyFailureState{lastErr: cause.Error(), class: class}
	}
}

// failureAttributionKeys returns the session keys a rejected dial may enrich
// with its last-failure diagnostics. A dial that failed for its own exact
// session key only records there. A dial rejected because its presented scope
// is not registered (stale pre-rotation key) additionally reaches pending
// waiters of the same adapter type whose scope shares the dial's scope-name
// prefix; in per-scope mode an unregistered scope can never match a waiter's
// expected identity exactly, so this prefix affinity is the only way the
// stale-pod diagnosis reaches the pending wait's terminal error.
func (s *Shim) failureAttributionKeys(adapterType, scope string, class identityRejectClass) []string {
	key := s.sessionKey(adapterType, scope)
	keys := []string{key}
	if s.perScopeSessions && class == rejectScopeNotRegistered && scope != "" {
		for waiterKey := range s.waiters {
			if waiterKey == key {
				continue
			}
			waiterType, waiterScope, ok := strings.Cut(waiterKey, "\x00")
			if !ok || waiterType != adapterType {
				continue
			}
			if scopeNamePrefix(waiterScope) != scopeNamePrefix(scope) {
				continue
			}
			keys = append(keys, waiterKey)
		}
	}
	return keys
}

// scopeNamePrefix splits a "<scopeName>/<scopeInstanceID>" session scope key
// into its workflow-level scope name. Both the dialer's stale key and the
// waiter's current key share the same scope name across a runner restart.
func scopeNamePrefix(scope string) string {
	if i := strings.LastIndex(scope, "/"); i >= 0 {
		return scope[:i]
	}
	return scope
}

// clearVerifyFailure resets the last-failure tracker for a session key after
// a successful identity verification.
func (s *Shim) clearVerifyFailure(adapterType, scope string) {
	key := s.sessionKey(adapterType, scope)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.verifyFailures != nil {
		delete(s.verifyFailures, key)
	}
}

// checkAdapterIdentity is a pure identity check: it classifies the dial and
// returns the rejection error without touching the connection, so the
// caller can retry after registrar recovery (KB-25) with the connection
// still open and close it only on the final rejection.
func (s *Shim) checkAdapterIdentity(hs *handshakeMessage) (identityRejectClass, error) {
	if s.digestVerifier != nil {
		if err := s.digestVerifier.Verify(hs.Name, hs.Digest); err != nil {
			return rejectDigest, fmt.Errorf("digest verification: %w", err)
		}
	}

	s.mu.Lock()
	perScope := s.perScopeSessions
	s.mu.Unlock()

	if perScope {
		// In per-scope mode each scope must have registered its own token.
		// An empty scope is rejected: when isolation is enabled every adapter
		// must present a valid, registered scope.
		s.mu.Lock()
		expectedToken, ok := s.scopeTokens[hs.Scope]
		s.mu.Unlock()
		if !ok {
			return rejectScopeNotRegistered, &ScopeNotRegisteredError{AdapterType: hs.Name, Scope: hs.Scope}
		}
		if subtle.ConstantTimeCompare([]byte(hs.Token), []byte(expectedToken)) != 1 {
			return rejectBadToken, fmt.Errorf("accept_token verification failed for scope %q", hs.Scope)
		}
		return rejectNone, nil
	}

	// Legacy run-wide token verification.
	s.mu.Lock()
	expectedToken := s.acceptToken
	s.mu.Unlock()
	if expectedToken != "" {
		if subtle.ConstantTimeCompare([]byte(hs.Token), []byte(expectedToken)) != 1 {
			return rejectBadToken, fmt.Errorf("accept_token verification failed")
		}
	}
	return rejectNone, nil
}

func (s *Shim) setupUDS(conn net.Conn) (string, net.Listener, error) {
	dir, err := os.MkdirTemp("", "criteria-remote-*")
	if err != nil {
		_ = conn.Close()
		return "", nil, fmt.Errorf("create socket dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		_ = conn.Close()
		return "", nil, fmt.Errorf("chmod socket dir: %w", err)
	}
	socketPath := filepath.Join(dir, "adapter.sock")

	lis, err := net.Listen("unix", socketPath)
	if err != nil {
		_ = os.RemoveAll(dir)
		_ = conn.Close()
		return "", nil, fmt.Errorf("listen uds: %w", err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = lis.Close()
		_ = os.RemoveAll(dir)
		_ = conn.Close()
		return "", nil, fmt.Errorf("chmod socket: %w", err)
	}
	return socketPath, lis, nil
}

type bridgeResult struct {
	client       adapterhost.Client
	pluginClient *hplugin.Client
	bridgeCancel func()
	bridgeCtx    context.Context
	bridgeWG     *sync.WaitGroup
	udsConn      net.Conn
}

//nolint:funlen // split from Accept to reduce cognitive complexity; linear sequence required
func (s *Shim) bridgeAndDial(
	ctx context.Context,
	conn net.Conn,
	lis net.Listener,
	socketPath string,
) (*bridgeResult, error) {
	udsConnCh := make(chan net.Conn, 1)
	udsErrCh := make(chan error, 1)
	go func() {
		c, err := lis.Accept()
		if err != nil {
			udsErrCh <- err
			return
		}
		udsConnCh <- c
		for {
			extra, err := lis.Accept()
			if err != nil {
				return
			}
			_ = extra.Close()
		}
	}()

	clientCh := make(chan struct {
		client adapterhost.Client
		plugin *hplugin.Client
		err    error
	}, 1)
	go func() {
		c, p, err := adapterhost.LocalSocketDialer(ctx, socketPath)
		clientCh <- struct {
			client adapterhost.Client
			plugin *hplugin.Client
			err    error
		}{client: c, plugin: p, err: err}
	}()

	var udsConn net.Conn
	select {
	case c := <-udsConnCh:
		udsConn = c
	case err := <-udsErrCh:
		_ = conn.Close()
		return nil, fmt.Errorf("uds accept: %w", err)
	case <-ctx.Done():
		_ = conn.Close()
		return nil, ctx.Err()
	}

	bridgeCtx, bridgeCancel := context.WithCancel(context.Background())
	var bridgeWG sync.WaitGroup
	bridgeWG.Add(2)
	go func() {
		defer bridgeWG.Done()
		_, _ = io.Copy(conn, udsConn)
		bridgeCancel()
	}()
	go func() {
		defer bridgeWG.Done()
		_, _ = io.Copy(udsConn, conn)
		bridgeCancel()
	}()

	var client adapterhost.Client
	var pluginClient *hplugin.Client
	select {
	case res := <-clientCh:
		if res.err != nil {
			bridgeCancel()
			bridgeWG.Wait()
			_ = udsConn.Close()
			_ = conn.Close()
			return nil, fmt.Errorf("local socket dialer: %w", res.err)
		}
		client = res.client
		pluginClient = res.plugin
	case <-ctx.Done():
		bridgeCancel()
		bridgeWG.Wait()
		_ = udsConn.Close()
		_ = conn.Close()
		return nil, ctx.Err()
	}

	return &bridgeResult{
		client:       client,
		pluginClient: pluginClient,
		bridgeCancel: bridgeCancel,
		bridgeCtx:    bridgeCtx,
		bridgeWG:     &bridgeWG,
		udsConn:      udsConn,
	}, nil
}

//nolint:funlen // split from Accept to reduce cognitive complexity; sequential teardown required
func (s *Shim) buildAndStoreHandle(
	ctx context.Context,
	adapterName string,
	scope string,
	conn net.Conn,
	udsConn net.Conn,
	lis net.Listener,
	socketPath string,
	client adapterhost.Client,
	pluginClient *hplugin.Client,
	bridgeCancel func(),
	bridgeCtx context.Context,
	bridgeWG *sync.WaitGroup,
) error {
	handle := makeHandle(adapterName, client, pluginClient, func() {
		bridgeCancel()
		_ = conn.Close()
		_ = udsConn.Close()
		bridgeWG.Wait()
		_ = lis.Close()
		_ = os.RemoveAll(filepath.Dir(socketPath))
	})

	s.mu.Lock()
	key := s.sessionKey(adapterName, scope)
	var old *session
	if existing, ok := s.sessions[key]; ok {
		old = existing
	}

	sess := &session{
		handle:     handle,
		cancel:     bridgeCancel,
		cancelCtx:  bridgeCtx,
		socketPath: socketPath,
	}
	s.sessions[key] = sess

	if waiters, ok := s.waiters[key]; ok {
		for _, ch := range waiters {
			ch <- waitResult{handle: handle}
		}
		delete(s.waiters, key)
	}
	s.mu.Unlock()

	if old != nil {
		if old.cancel != nil {
			old.cancel()
		}
		if old.handle != nil {
			_ = old.handle.CloseSession(ctx, "")
			old.handle.Kill()
		}
		_ = os.RemoveAll(filepath.Dir(old.socketPath))
	}

	go func() {
		<-bridgeCtx.Done()
		_ = conn.Close()
		_ = udsConn.Close()
		bridgeWG.Wait()
		_ = lis.Close()
		pluginClient.Kill()
		_ = os.RemoveAll(filepath.Dir(socketPath))

		s.mu.Lock()
		if cur, ok := s.sessions[key]; ok && cur.handle == handle {
			delete(s.sessions, key)
		}
		s.mu.Unlock()
	}()

	return nil
}

// WaitForHandle blocks until a remote adapter of the given type connects.
func (s *Shim) WaitForHandle(ctx context.Context, adapterType, scope string) (adapterhost.Handle, error) {
	return s.WaitForFreshHandle(ctx, adapterType, scope, nil)
}

// WaitForFreshHandle blocks until a remote adapter of the given type connects,
// returning a handle that is not `stale`. On a crash-respawn the just-crashed
// handle may still be the current session entry (its bridge-teardown runs
// asynchronously), so callers pass the dead handle as `stale` to ensure they
// wait for a genuinely new connection rather than receiving the dead one back.
//
// The wait is bounded by two wall-clock budgets (KB-70). The handshake budget
// (DefaultVerifyFailureBudget) runs only while the adapter has started — the
// shim observed an identity frame for the key or attributed an identity
// rejection to it (a dialing pod, even a stale pre-rotation one — CRI-137),
// or the pod-state probe reports Running — and fails terminally with the
// CRI-137 diagnosis classes when it expires. The scheduling budget
// (DefaultSchedulingBudget) runs while the adapter pod has not started: a
// burst of per-scope pod creations can keep a pod Pending well past the
// handshake budget, and a pod that never started cannot handshake, so the
// handshake budget stays frozen until it does. If the scheduling budget
// expires first, the wait fails naming the pod's observed phase instead of
// the dead-Job verdict.
func (s *Shim) WaitForFreshHandle(ctx context.Context, adapterType, scope string, stale adapterhost.Handle) (adapterhost.Handle, error) {
	key := s.sessionKey(adapterType, scope)
	s.mu.Lock()
	if sess, ok := s.sessions[key]; ok && sess.handle != stale {
		s.mu.Unlock()
		return sess.handle, nil
	}
	ch := make(chan waitResult, 1)
	s.waiters[key] = append(s.waiters[key], ch)
	budget := s.verifyFailureBudget
	if budget <= 0 {
		budget = DefaultVerifyFailureBudget
	}
	s.mu.Unlock()

	return s.awaitWaiter(ctx, adapterType, scope, key, ch, nil, func() { s.removeWaiter(key, ch) }, budget)
}

// waiterState tracks the KB-70 two-budget wait across wake-ups: the adapter
// start evidence, the armed handshake deadline (start time + budget), and
// the scheduling deadline.
type waiterState struct {
	started           bool
	handshakeDeadline time.Time
	schedDeadline     time.Time
}

// startEvidence reports whether the adapter session for key has provably
// started: an identity frame was presented for the key (a dial, even one
// attributed to a verify failure — a stale pre-rotation pod dials and is
// rejected, CRI-137), or the pod-state probe reports a started phase. Until
// this returns true the wait is governed by the scheduling budget, not the
// handshake budget (KB-70).
func (s *Shim) startEvidence(phase string, phaseKnown bool, key string) bool {
	return s.activityObserved(key) || (phaseKnown && podPhaseStarted(phase))
}

// podTerminalFailure returns the terminal wait error for a probe-observed
// finished pod that never presented an identity frame for key on this shim
// (KB-70): Succeeded means the Job ran to completion without phone-homing,
// Failed means it died first. nil when the pod state is unknown or still
// live, or when dials were observed.
func (s *Shim) podTerminalFailure(adapterType, scope, key, phase string, phaseKnown bool) error {
	if phaseKnown && podPhaseTerminal(phase) && !s.dialObserved(key) {
		return s.podTerminalError(adapterType, scope, phase)
	}
	return nil
}

// handleWaiterWake evaluates one timer wake-up. While the adapter has not
// started it re-observes the pod, arms the handshake budget on the first
// start evidence, and fails on scheduling-budget expiry naming the observed
// pod phase; once started it fails at the handshake deadline. done=true
// carries the terminal wait error; otherwise the caller re-arms the wake
// timer from the returned state.
func (s *Shim) handleWaiterWake(adapterType, scope, key string, handshakeBudget, schedulingBudget time.Duration, st *waiterState) (done bool, err error) {
	now := time.Now()
	if st.started {
		if !now.Before(st.handshakeDeadline) {
			return true, s.waitTimeoutError(adapterType, scope, key, handshakeBudget)
		}
		return false, nil
	}
	phase, phaseKnown := s.observePod(adapterType, scope)
	if terr := s.podTerminalFailure(adapterType, scope, key, phase, phaseKnown); terr != nil {
		return true, terr
	}
	if s.startEvidence(phase, phaseKnown, key) {
		st.started = true
		st.handshakeDeadline = now.Add(handshakeBudget)
		return false, nil
	}
	if !now.Before(st.schedDeadline) {
		return true, s.schedulingTimeoutError(adapterType, scope, key, schedulingBudget, phase, phaseKnown)
	}
	return false, nil
}

// awaitWaiter blocks on one or two waiter result channels until the session
// resolves, the context is cancelled, or one of the two session-wait budgets
// expires (KB-70). A nil channel never resolves: the legacy wait passes nil
// for its single registry and the peer wait passes both of its registries.
//
// See WaitForFreshHandle for the budget semantics. deregister removes the
// caller's waiter registrations on every exit path — including success, where
// removing an already-drained channel is a no-op — so a woken wait never
// leaves a stale channel in a registry.
func (s *Shim) awaitWaiter(ctx context.Context, adapterType, scope, key string, primary, secondary <-chan waitResult, deregister func(), handshakeBudget time.Duration) (adapterhost.Handle, error) {
	s.mu.Lock()
	schedulingBudget := s.schedulingBudget
	poll := s.podStatePollInterval
	s.mu.Unlock()
	if schedulingBudget <= 0 {
		schedulingBudget = DefaultSchedulingBudget
	}
	if poll <= 0 {
		poll = defaultPodStatePollInterval
	}

	// Initial observation, so a pod that is already Running (or already
	// dialing) starts its handshake budget now rather than at the first tick.
	phase, phaseKnown := s.observePod(adapterType, scope)
	if terr := s.podTerminalFailure(adapterType, scope, key, phase, phaseKnown); terr != nil {
		deregister()
		return nil, terr
	}
	st := waiterState{schedDeadline: time.Now().Add(schedulingBudget)}
	if st.started = s.startEvidence(phase, phaseKnown, key); st.started {
		st.handshakeDeadline = time.Now().Add(handshakeBudget)
	}

	timer := time.NewTimer(waitPollDuration(poll, st.started, st.handshakeDeadline, st.schedDeadline))
	defer timer.Stop()
	for {
		select {
		case res := <-primary:
			deregister()
			return res.handle, res.err
		case res := <-secondary:
			deregister()
			return res.handle, res.err
		case <-ctx.Done():
			deregister()
			return nil, ctx.Err()
		case <-timer.C:
			done, err := s.handleWaiterWake(adapterType, scope, key, handshakeBudget, schedulingBudget, &st)
			if done {
				deregister()
				return nil, err
			}
			timer.Reset(waitPollDuration(poll, st.started, st.handshakeDeadline, st.schedDeadline))
		}
	}
}

// waitPollDuration picks the next wake-up: immediately at the handshake
// deadline once the adapter started, otherwise the closer of the next poll
// and the scheduling deadline. A non-positive wake-up fires on Reset.
func waitPollDuration(poll time.Duration, started bool, handshakeDeadline, schedDeadline time.Time) time.Duration {
	if started {
		return time.Until(handshakeDeadline)
	}
	return min(poll, time.Until(schedDeadline))
}

// registerFreshWaiter atomically resolves a fresh legacy session or
// registers a waiter: the peek and the append share one s.mu critical
// section so a handshake that stores a session between the two cannot drain
// an empty waiter list and strand the wait (lost wakeup). It returns
// (handle, nil, 0) when a live session for key is already present, and
// (nil, ch, budget) when a waiter was registered; the verify-failure budget
// is captured under the same lock, the way WaitForFreshHandle does.
func (s *Shim) registerFreshWaiter(key string, stale adapterhost.Handle) (adapterhost.Handle, chan waitResult, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[key]; ok && sess.handle != stale {
		return sess.handle, nil, 0
	}
	ch := make(chan waitResult, 1)
	s.waiters[key] = append(s.waiters[key], ch)
	budget := s.verifyFailureBudget
	if budget <= 0 {
		budget = DefaultVerifyFailureBudget
	}
	return nil, ch, budget
}

// removeWaiter drops a registered waiter channel from the waiters map.
func (s *Shim) removeWaiter(key string, ch chan waitResult) {
	s.mu.Lock()
	waiters := s.waiters[key]
	for i, w := range waiters {
		if w == ch {
			s.waiters[key] = append(waiters[:i], waiters[i+1:]...)
			break
		}
	}
	if len(s.waiters[key]) == 0 {
		delete(s.waiters, key)
	}
	s.mu.Unlock()
}

// waitTimeoutError builds the terminal error for a pending session wait whose
// handshake budget expired with the adapter started (KB-70). It names the
// scope, distinguishes the last observed rejection by class (digest mismatch
// vs accept-token/scope state), and calls out the dead-Job shape when no
// identity handshake was observed at all (CRI-137). A pod that never started
// does not reach this error: it is failed by the scheduling budget instead
// (schedulingTimeoutError), whose verdict never claims the Job is dead.
func (s *Shim) waitTimeoutError(adapterType, scope, key string, budget time.Duration) error {
	s.mu.Lock()
	st := s.verifyFailures[key]
	delete(s.verifyFailures, key)
	s.mu.Unlock()

	detail := fmt.Sprintf("no identity handshake observed at all for scope %q; adapter Job may be complete or dead (CRI-137)", scope)
	if st != nil {
		detail = fmt.Sprintf("last identity rejection for scope %q: %s", scope, st.lastErr)
		switch st.class {
		case rejectDigest:
			// Digest failures are their own diagnosis; do not blame the
			// accept token for them.
			detail += "; digest verification failed — stale or wrong adapter build? (CRI-137)"
		case rejectScopeNotRegistered, rejectBadToken:
			detail += "; stale adapter pod holding a pre-rotation accept token? (CRI-137)"
		}
	} else if phase, ok := s.observePod(adapterType, scope); ok && podPhaseStarted(phase) {
		// The probe watched the pod Run while it presented no identity frame:
		// the processes are up but the Job is not phone-homing.
		detail = fmt.Sprintf("no identity handshake observed at all for scope %q; adapter pod observed Running but never dialed this shim — adapter Job may be complete or dead (CRI-137)", scope)
	}
	return fmt.Errorf("remote adapter %q session wait for scope %q exceeded %s without a successful identity handshake: %s",
		adapterType, scope, budget, detail)
}

// schedulingTimeoutError builds the terminal error for a wait whose adapter
// pod never started within the scheduling budget (KB-70). Unlike
// waitTimeoutError it must not claim the adapter Job "may be complete or
// dead": a pod that never became Running had no live Job to die mid-run —
// the burst scheduling latency beat the handshake budget and the verdict
// must say so.
func (s *Shim) schedulingTimeoutError(adapterType, scope, key string, schedulingBudget time.Duration, phase string, phaseKnown bool) error {
	s.mu.Lock()
	delete(s.verifyFailures, key)
	s.mu.Unlock()

	detail := "no identity handshake observed at all for that scope; the adapter pod was never observed Running during the wait (KB-70)"
	if phaseKnown {
		detail += fmt.Sprintf("; last observed pod phase: %q", phase)
	} else {
		detail += "; no pod-state probe is wired, so the pod phase is unknown — the adapter likely never scheduled or started dialing"
	}
	return fmt.Errorf("remote adapter %q session wait for scope %q exceeded %s waiting for the adapter pod to start: %s",
		adapterType, scope, schedulingBudget, detail)
}

// podTerminalError fails a wait promptly when the pod-state probe reports a
// terminal phase: the Job finished or died without ever dialing this shim, so
// no amount of further waiting can complete the handshake (CRI-137).
func (s *Shim) podTerminalError(adapterType, scope, phase string) error {
	return fmt.Errorf("remote adapter %q session wait for scope %q failed without a successful identity handshake: adapter pod phase is %q — the adapter Job ended without dialing this shim (CRI-137)",
		adapterType, scope, phase)
}

// CloseHandle removes a session for the given adapter type + scope.
func (s *Shim) CloseHandle(ctx context.Context, adapterType, scope string) error {
	key := s.sessionKey(adapterType, scope)
	s.mu.Lock()
	sess, ok := s.sessions[key]
	if !ok {
		s.mu.Unlock()
		return nil
	}
	delete(s.sessions, key)
	s.mu.Unlock()
	if sess.cancel != nil {
		sess.cancel()
	}
	if sess.handle != nil {
		_ = sess.handle.CloseSession(ctx, "")
		sess.handle.Kill()
	}
	if sess.socketPath != "" {
		_ = os.RemoveAll(filepath.Dir(sess.socketPath))
	}
	return nil
}

// isDeadlineTimeout reports whether err was caused by a net.Conn deadline.
// It is used to turn low-level i/o timeout errors into clear handshake
// diagnostics that name the configured deadline.
func isDeadlineTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	return false
}

// buildTLSConfig assembles a server-side TLS config from the Config.
func buildTLSConfig(cfg *Config) (*tls.Config, error) {
	if cfg.ServerCertPath == "" || cfg.ServerKeyPath == "" || cfg.ClientCAPath == "" {
		return nil, nil
	}

	certPEM, err := os.ReadFile(cfg.ServerCertPath)
	if err != nil {
		return nil, fmt.Errorf("read server cert: %w", err)
	}
	keyPEM, err := os.ReadFile(cfg.ServerKeyPath)
	if err != nil {
		return nil, fmt.Errorf("read server key: %w", err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("load server key pair: %w", err)
	}

	caPEM, err := os.ReadFile(cfg.ClientCAPath)
	if err != nil {
		return nil, fmt.Errorf("read client CA: %w", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("parse client CA")
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    caPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}, nil
}

// validateListenerSecurity rejects non-loopback TCP listeners that are not
// protected by mTLS, accept_token, or the explicit insecure opt-in.
func validateListenerSecurity(cfg *Config) error {
	if isUnixSocketAddr(cfg.ListenAddress) {
		return nil
	}
	host, _, err := net.SplitHostPort(cfg.ListenAddress)
	if err != nil {
		return fmt.Errorf("listen_address %q: %w", cfg.ListenAddress, err)
	}
	if isLoopbackHost(host) {
		return nil
	}

	hasMTLS := cfg.ServerCertPath != "" && cfg.ServerKeyPath != "" && cfg.ClientCAPath != ""
	hasToken := cfg.AcceptToken != "" || cfg.PerScopeSessions
	if hasMTLS || hasToken || cfg.Insecure {
		return nil
	}

	return fmt.Errorf("listen_address %q binds to a non-loopback address and has no authentication; configure mtls, accept_token, or set insecure = true to opt out", cfg.ListenAddress)
}

// isUnixSocketAddr reports whether addr is intended as a Unix socket path.
func isUnixSocketAddr(addr string) bool {
	return filepath.IsAbs(addr) || (addr != "" && addr[0] == '/')
}

// isLoopbackHost reports whether host is a loopback-only host name or address.
// An empty host (e.g. ":7778") binds all interfaces and is therefore not
// loopback. Hostnames are resolved and considered loopback only if every
// resolved IP is loopback. IPv6 literals from net.SplitHostPort include
// brackets, which are stripped before parsing.
func isLoopbackHost(host string) bool {
	host = strings.Trim(host, "[]")
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}

	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return false
	}
	for _, ip := range ips {
		if !ip.IsLoopback() {
			return false
		}
	}
	return true
}
