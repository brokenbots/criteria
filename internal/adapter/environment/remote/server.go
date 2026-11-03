// Package remote implements the "remote" environment handler and host-side
// phone-home server for criteria-to-criteria peer adapters.
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
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/brokenbots/criteria/internal/adapterhost"
)

// handshakeMessage is the pre-gRPC identity frame sent by the adapter over
// the raw TLS connection before the gRPC client takes over the stream.
type handshakeMessage struct {
	Name     string                `json:"name"`
	Version  string                `json:"version"`
	Digest   string                `json:"digest"`
	Token    string                `json:"token"`
	Scope    string                `json:"scope,omitempty"`
	Role     string                `json:"role,omitempty"`
	Peer     *PeerClientIdentity   `json:"peer,omitempty"`
	Adapters []PeerAdapterIdentity `json:"adapters,omitempty"`
}

// handshakeRolePeer is the identity-frame role value that routes a dial to
// the PeerAcceptor seam (ADR-0007).
const handshakeRolePeer = "peer"

// PeerClientIdentity carries the criteria-to-criteria peer metadata (ADR-0007).
type PeerClientIdentity struct {
	CriteriaVersion string   `json:"criteria_version"`
	Capabilities    []string `json:"capabilities"`
}

// PeerAdapterIdentity is one hosted child definition advertised by the peer.
type PeerAdapterIdentity struct {
	Name    string `json:"name"`
	Digest  string `json:"digest"`
	Type    string `json:"type,omitempty"`
	Version string `json:"version,omitempty"`
}

// PeerDial holds the verified identity handed to PeerAcceptor.
type PeerDial struct {
	AdapterType string
	Scope       string
	Digest      string
	Adapters    []PeerAdapterIdentity
	Peer        *PeerClientIdentity
}

// PeerAcceptor receives verified role="peer" connections.
type PeerAcceptor interface {
	AcceptPeer(ctx context.Context, conn net.Conn, dial *PeerDial) error
}

// ScopeRegistrar is consulted on dials presenting an unregistered scope.
type ScopeRegistrar interface {
	RegisterScopeOnDial(adapterName, scope, token string) error
}

// DigestVerifier verifies an adapter's pinned content digest.
type DigestVerifier interface {
	Verify(adapterType string, digest string) error
}

type waitResult struct {
	handle adapterhost.Handle
	err    error
}

// ErrScopeNotRegistered marks an adapter dial whose presented scope has no
// accept token registered on this server (KB-25).
var ErrScopeNotRegistered = errors.New("scope is not registered")

// ScopeNotRegisteredError marks an unregistered-scope rejection.
type ScopeNotRegisteredError struct {
	AdapterType string
	Scope       string
}

func (e *ScopeNotRegisteredError) Error() string {
	return fmt.Sprintf("scope %q is not registered", e.Scope)
}

func (e *ScopeNotRegisteredError) Is(target error) bool {
	if target == ErrScopeNotRegistered {
		return true
	}
	t, ok := target.(*ScopeNotRegisteredError)
	return ok && (t.Scope == "" || t.Scope == e.Scope)
}

func hostedAdapters(dial *PeerDial) []string {
	if dial == nil {
		return nil
	}
	if len(dial.Adapters) == 0 {
		return []string{dial.AdapterType}
	}
	out := make([]string, len(dial.Adapters))
	for i, a := range dial.Adapters {
		out[i] = a.Name
	}
	return out
}

type identityRejectClass int

const (
	rejectNone identityRejectClass = iota
	rejectBadToken
	rejectScopeNotRegistered
	rejectDigest
	rejectChildSet
)

type verifyFailureState struct {
	lastErr string
	class   identityRejectClass
}

// PodStateProbe inspects the current Kubernetes pod phase.
type PodStateProbe interface {
	PodState(adapterType, scope string) (PodState, bool)
}

// PodState represents the observed state of an adapter pod.
type PodState struct {
	Phase   string
	Message string
	Reason  string
}

const (
	PodPhasePending   = "Pending"
	PodPhaseRunning   = "Running"
	PodPhaseSucceeded = "Succeeded"
	PodPhaseFailed    = "Failed"
	PodPhaseUnknown   = "Unknown"
)

func podPhaseStarted(phase string) bool {
	return phase == PodPhaseRunning || phase == PodPhaseSucceeded || phase == PodPhaseFailed
}

func podPhaseTerminal(phase string) bool {
	return phase == PodPhaseSucceeded || phase == PodPhaseFailed
}

// Server is the host-side phone-home server for remote peer adapters.
// It manages the TCP/mTLS listener, verifies adapter identity frames,
// and routes authenticated peer connections directly to PeerAcceptor.
type Server struct {
	cfg              *Config
	listener         net.Listener
	tlsConfig        *tls.Config
	verifier         DigestVerifier
	clientIdentityRe *regexp.Regexp
	acceptToken      string

	mu               sync.Mutex
	started          bool
	perScopeSessions bool
	scopeTokens      map[string]string

	verifyFailures      map[string]*verifyFailureState
	verifyFailureBudget time.Duration
	waiters             map[string][]chan waitResult

	schedulingBudget     time.Duration
	podStatePollInterval time.Duration
	podProbe             PodStateProbe

	dialActivity map[string]time.Time
	dialCount    map[string]int
	rejectCount  map[string]int
	pendingSink  ScopeSessionSink

	peerAcceptor   PeerAcceptor
	scopeRegistrar ScopeRegistrar

	tlsHandshakeDeadline time.Duration
	identityDeadline     time.Duration
}

// Shim is a type alias for Server to preserve compatibility.
type Shim = Server

// ScopeSessionPending records pending wait signals.
type ScopeSessionPending struct {
	AdapterType string
	Scope       string // full per-scope session key, e.g. "run_handler/<uuid>"
	Dials       int
	Rejections  int
	Waited      time.Duration
	Window      time.Duration
}

// ScopeSessionSink receives scope_session_pending signals.
type ScopeSessionSink interface {
	OnScopeSessionPending(*ScopeSessionPending)
}

// SetScopeSessionSink sets the sink for pending wait events.
func (s *Server) SetScopeSessionSink(sink ScopeSessionSink) {
	s.mu.Lock()
	s.pendingSink = sink
	s.mu.Unlock()
}

func (s *Server) sessionDialStats(key string) (dialed, rejected int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dialCount[key], s.rejectCount[key]
}

func (s *Server) emitScopeSessionPending(adapterType, scope, key string, window, waited time.Duration) {
	dialed, rejected := s.sessionDialStats(key)
	slog.Warn("scope session pending half-window elapsed without live connection",
		"adapter", adapterType,
		"scope", scope,
		"session_key", key,
		"window", window,
		"waited", waited,
		"dials_presented", dialed,
		"dials_rejected", rejected)

	s.mu.Lock()
	sink := s.pendingSink
	s.mu.Unlock()
	if sink != nil {
		sink.OnScopeSessionPending(&ScopeSessionPending{
			AdapterType: adapterType,
			Scope:       scope,
			Dials:       dialed,
			Rejections:  rejected,
			Waited:      waited,
			Window:      window,
		})
	}
}

func resolveHandshakeDeadlines(cfg *Config) (tlsDeadline, identityDeadline time.Duration) {
	tlsDeadline = DefaultTLSHandshakeDeadline
	if cfg != nil && cfg.TLSHandshakeDeadline > 0 {
		tlsDeadline = cfg.TLSHandshakeDeadline
	}
	identityDeadline = DefaultIdentityHandshakeDeadline
	if cfg != nil && cfg.IdentityHandshakeDeadline > 0 {
		identityDeadline = cfg.IdentityHandshakeDeadline
	}
	return tlsDeadline, identityDeadline
}

func resolveWaitBudgets(cfg *Config) (handshakeBudget, schedulingBudget time.Duration) {
	handshakeBudget = DefaultVerifyFailureBudget
	if cfg != nil && cfg.SessionHandshakeBudget > 0 {
		handshakeBudget = cfg.SessionHandshakeBudget
	}
	schedulingBudget = DefaultSchedulingBudget
	if cfg != nil && cfg.SessionSchedulingBudget > 0 {
		schedulingBudget = cfg.SessionSchedulingBudget
	}
	return handshakeBudget, schedulingBudget
}

// NewServer creates a new remote phone-home server.
func NewServer(cfg *Config, verifier DigestVerifier) (*Server, error) {
	if err := validateListenerSecurity(cfg); err != nil {
		return nil, fmt.Errorf("remote shim: %w", err)
	}

	tlsCfg, err := buildTLSConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build tls config: %w", err)
	}

	var identityRe *regexp.Regexp
	if cfg != nil && cfg.ClientIdentityPattern != "" {
		re, err := regexp.Compile(cfg.ClientIdentityPattern)
		if err != nil {
			return nil, fmt.Errorf("compile client identity pattern: %w", err)
		}
		identityRe = re
	}

	tlsDeadline, identityDeadline := resolveHandshakeDeadlines(cfg)
	handshakeBudget, schedulingBudget := resolveWaitBudgets(cfg)

	var acceptToken string
	var perScope bool
	if cfg != nil {
		acceptToken = cfg.AcceptToken
		perScope = cfg.PerScopeSessions
	}

	srv := &Server{
		cfg:                  cfg,
		tlsConfig:            tlsCfg,
		verifier:             verifier,
		clientIdentityRe:     identityRe,
		acceptToken:          acceptToken,
		tlsHandshakeDeadline: tlsDeadline,
		identityDeadline:     identityDeadline,
		perScopeSessions:     perScope,
		scopeTokens:          make(map[string]string),
		verifyFailures:       make(map[string]*verifyFailureState),
		waiters:              make(map[string][]chan waitResult),
		verifyFailureBudget:  handshakeBudget,
		schedulingBudget:     schedulingBudget,
		podStatePollInterval: defaultPodStatePollInterval,
		dialActivity:         make(map[string]time.Time),
		dialCount:            make(map[string]int),
		rejectCount:          make(map[string]int),
	}
	srv.peerAcceptor = NewPeerSessionProvider(srv, perScope)
	return srv, nil
}

// NewShim creates a new Server, preserving the legacy constructor name.
var NewShim = NewServer

const phoneHomeKeepAlive = 15 * time.Second

// Start starts listening on the configured address.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started {
		return errors.New("server already started")
	}

	listenAddr := s.cfg.ListenAddress
	if listenAddr == "" {
		listenAddr = "127.0.0.1:0"
	}

	if err := ValidateListenerSecurity(listenAddr, s.cfg); err != nil {
		return err
	}

	var lis net.Listener
	var err error
	lc := net.ListenConfig{KeepAlive: phoneHomeKeepAlive}

	if s.tlsConfig != nil {
		raw, lerr := lc.Listen(ctx, "tcp", listenAddr)
		lis, err = tls.NewListener(raw, s.tlsConfig), lerr
	} else if isUnixSocketAddr(listenAddr) {
		socketPath := strings.TrimPrefix(listenAddr, "unix://")
		if err := checkUnixSocketPath(socketPath); err != nil {
			return err
		}
		_ = os.Remove(socketPath)
		lis, err = lc.Listen(ctx, "unix", socketPath)
	} else {
		lis, err = lc.Listen(ctx, "tcp", listenAddr)
	}
	if err != nil {
		return fmt.Errorf("listen on %s: %w", listenAddr, err)
	}

	s.listener = lis
	s.started = true

	if s.cfg != nil && s.cfg.Insecure && s.tlsConfig == nil && s.acceptToken == "" {
		slog.Warn("remote shim listening without authentication", "addr", lis.Addr().String())
	} else {
		slog.Info("remote shim listening", "addr", lis.Addr().String())
	}

	go s.serve(ctx, lis)
	return nil
}

func checkUnixSocketPath(addr string) error {
	maxLen := 104
	if len(addr) > maxLen {
		return fmt.Errorf("unix socket path %q exceeds %d characters (%d); use a shorter path", addr, maxLen, len(addr))
	}
	dir := filepath.Dir(addr)
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("unix socket directory %q: %w", dir, err)
	}
	return nil
}

// Stop stops the server.
func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return nil
	}
	s.started = false
	lis := s.listener
	cfg := s.cfg
	for _, waiterList := range s.waiters {
		for _, w := range waiterList {
			select {
			case w <- waitResult{err: errors.New("remote shim stopped")}:
			default:
			}
		}
	}
	s.waiters = make(map[string][]chan waitResult)
	acceptor := s.peerAcceptor
	s.mu.Unlock()

	if p, ok := acceptor.(*peerSessionProvider); ok {
		p.closeActiveSessions(ctx)
	}

	if lis != nil {
		_ = lis.Close()
	}

	if cfg != nil && isUnixSocketAddr(cfg.ListenAddress) {
		_ = os.Remove(strings.TrimPrefix(cfg.ListenAddress, "unix://"))
	}
	return nil
}

// SetPerScopeSessions configures per-scope session isolation.
func (s *Server) SetPerScopeSessions(enabled bool) {
	s.mu.Lock()
	s.perScopeSessions = enabled
	acceptor := s.peerAcceptor
	s.mu.Unlock()

	if p, ok := acceptor.(*peerSessionProvider); ok {
		p.mu.Lock()
		p.perScopeSessions = enabled
		p.mu.Unlock()
	}
}

func (s *Server) removeWaiter(key string, ch chan waitResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ws := s.waiters[key]
	for i, w := range ws {
		if w == ch {
			s.waiters[key] = append(ws[:i], ws[i+1:]...)
			break
		}
	}
	if len(s.waiters[key]) == 0 {
		delete(s.waiters, key)
	}
}

// SetPeerAcceptor sets the receiver for role="peer" connections.
func (s *Server) SetPeerAcceptor(pa PeerAcceptor) {
	s.mu.Lock()
	s.peerAcceptor = pa
	s.mu.Unlock()

	if p, ok := pa.(*peerSessionProvider); ok {
		p.mu.Lock()
		p.shim = s
		p.mu.Unlock()
	}
}

// SetPodStateProbe sets the probe for inspecting pod phases.
func (s *Server) SetPodStateProbe(probe PodStateProbe) {
	s.mu.Lock()
	s.podProbe = probe
	s.mu.Unlock()
}

func (s *Server) noteDialActivity(adapterType, scope string) {
	key := s.sessionKey(adapterType, scope)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dialActivity == nil {
		s.dialActivity = make(map[string]time.Time)
	}
	s.dialActivity[key] = time.Now()
	if s.dialCount == nil {
		s.dialCount = make(map[string]int)
	}
	s.dialCount[key]++
}

func (s *Server) dialObserved(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dialCount[key] > 0
}

func (s *Server) activityObserved(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.dialActivity[key]; ok {
		return true
	}
	return s.verifyFailures[key] != nil
}

func (s *Server) observePod(adapterType, scope string) (phase string, ok bool) {
	s.mu.Lock()
	probe := s.podProbe
	s.mu.Unlock()
	if probe == nil {
		return "", false
	}
	st, ok := probe.PodState(adapterType, scope)
	if !ok || st.Phase == "" {
		return "", false
	}
	return st.Phase, true
}

// SetScopeRegistrar installs a registrar for unregistered-scope dials.
func (s *Server) SetScopeRegistrar(r ScopeRegistrar) {
	s.mu.Lock()
	s.scopeRegistrar = r
	s.mu.Unlock()
}

// RegisterScope registers an accept token for a scope.
func (s *Server) RegisterScope(scope, token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scopeTokens == nil {
		s.scopeTokens = make(map[string]string)
	}
	s.scopeTokens[scope] = token
}

// UnregisterScope removes an accept token for a scope.
func (s *Server) UnregisterScope(scope string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.scopeTokens, scope)
}

func (s *Server) sessionKey(adapterType, scope string) string {
	if s.perScopeSessions && scope != "" {
		return adapterType + "\x00" + scope
	}
	return adapterType
}

// ListenAddr returns the server's listen address.
func (s *Server) ListenAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

func (s *Server) serve(ctx context.Context, lis net.Listener) {
	for {
		conn, err := lis.Accept()
		if err != nil {
			s.mu.Lock()
			started := s.started
			s.mu.Unlock()
			if !started {
				return
			}
			slog.Debug("remote server accept error", "error", err)
			return
		}

		go func(c net.Conn) {
			if err := s.Accept(ctx, c); err != nil {
				slog.Debug("remote server connection rejected", "error", err)
			}
		}(conn)
	}
}

// Accept processes a single phone-home connection.
func (s *Server) Accept(ctx context.Context, conn net.Conn) error {
	if err := s.performHandshake(ctx, conn); err != nil {
		return err
	}

	hs, err := s.readHandshakeMessage(conn)
	if err != nil {
		return err
	}

	s.noteDialActivity(hs.Name, hs.Scope)

	if err := s.verifyAdapterIdentity(conn, &hs); err != nil {
		return err
	}

	if hs.Role != "" && hs.Role != handshakeRolePeer {
		_ = conn.Close()
		return fmt.Errorf("adapter %q: unrecognized handshake role %q", hs.Name, hs.Role)
	}

	return s.acceptPeerConn(ctx, conn, &hs)
}

func (s *Server) acceptPeerConn(ctx context.Context, conn net.Conn, hs *handshakeMessage) error {
	s.mu.Lock()
	acceptor := s.peerAcceptor
	s.mu.Unlock()
	if acceptor == nil {
		_ = conn.Close()
		return fmt.Errorf("peer role dial from %q rejected: no peer acceptor configured", hs.Name)
	}
	dial := PeerDial{AdapterType: hs.Name, Scope: hs.Scope, Digest: hs.Digest, Adapters: hs.Adapters, Peer: hs.Peer}
	return acceptor.AcceptPeer(ctx, conn, &dial)
}

func dialAdapterNames(hs *handshakeMessage) []string {
	if hs == nil {
		return nil
	}
	if len(hs.Adapters) == 0 {
		return []string{hs.Name}
	}
	out := make([]string, len(hs.Adapters))
	for i, a := range hs.Adapters {
		out[i] = a.Name
	}
	return out
}

func (s *Server) performHandshake(ctx context.Context, conn net.Conn) error {
	var certSubject string
	if tlsConn, ok := conn.(*tls.Conn); ok {
		_ = tlsConn.SetDeadline(time.Now().Add(s.tlsHandshakeDeadline))
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

func (s *Server) readHandshakeMessage(conn net.Conn) (handshakeMessage, error) {
	_ = conn.SetReadDeadline(time.Now().Add(s.identityDeadline))
	header, err := readHandshakeFrame(bufio.NewReader(conn), handshakeFrameCap)
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		_ = conn.Close()
		if errors.Is(err, errHandshakeFrameTooLarge) {
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

var errHandshakeFrameTooLarge = errors.New("handshake frame too large")

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

func (s *Server) verifyAdapterIdentity(conn net.Conn, hs *handshakeMessage) error {
	class, err := s.checkAdapterIdentity(hs)
	if err == nil {
		s.clearVerifyFailure(hs.Name, hs.Scope)
		return nil
	}
	if class == rejectScopeNotRegistered && s.tryScopeRecovery(hs) {
		class, err = s.checkAdapterIdentity(hs)
		if err == nil {
			slog.Info("remote server accepted dial after scope re-registration",
				"adapter", hs.Name, "scope", hs.Scope)
			s.clearVerifyFailure(hs.Name, hs.Scope)
			return nil
		}
	}
	_ = conn.Close()
	s.noteVerifyFailure(dialAdapterNames(hs), hs.Scope, class, err)
	return err
}

func (s *Server) checkAdapterIdentity(hs *handshakeMessage) (identityRejectClass, error) {
	if s.verifier != nil && hs.Digest != "" {
		if err := s.verifier.Verify(hs.Name, hs.Digest); err != nil {
			return rejectDigest, fmt.Errorf("digest verification: %w", err)
		}
	}

	if class, err := checkPeerChildSet(s.verifier, hs.Name, hs.Adapters); class != rejectNone {
		return class, err
	}

	s.mu.Lock()
	perScope := s.perScopeSessions
	s.mu.Unlock()

	if perScope {
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

func checkPeerChildSet(verifier DigestVerifier, dialName string, adapters []PeerAdapterIdentity) (identityRejectClass, error) {
	if len(adapters) == 0 {
		return rejectNone, nil
	}
	seen := make(map[string]struct{}, len(adapters))
	selfConsistent := false
	for i := range adapters {
		a := &adapters[i]
		if a.Name == "" {
			return rejectChildSet, fmt.Errorf("peer child set advertises an adapter with an empty name (entry %d)", i)
		}
		if _, dup := seen[a.Name]; dup {
			return rejectChildSet, fmt.Errorf("peer child set advertises adapter %q more than once", a.Name)
		}
		seen[a.Name] = struct{}{}
		if a.Name == dialName {
			selfConsistent = true
		}
		if verifier != nil {
			if err := verifier.Verify(a.Name, a.Digest); err != nil {
				return rejectChildSet, fmt.Errorf("peer child %q digest verification: %w", a.Name, err)
			}
		}
	}
	if !selfConsistent {
		return rejectChildSet, fmt.Errorf("peer identity frame dials as %q but its child set does not include it (hosted: %s)",
			dialName, strings.Join(peerChildNames(adapters), ","))
	}
	return rejectNone, nil
}

func peerChildNames(adapters []PeerAdapterIdentity) []string {
	names := make([]string, 0, len(adapters))
	for i := range adapters {
		names = append(names, adapters[i].Name)
	}
	return names
}

func (s *Server) tryScopeRecovery(hs *handshakeMessage) bool {
	s.mu.Lock()
	registrar := s.scopeRegistrar
	s.mu.Unlock()
	if registrar == nil {
		return false
	}
	if err := registrar.RegisterScopeOnDial(hs.Name, hs.Scope, hs.Token); err != nil {
		slog.Debug("remote server scope re-registration declined the dial",
			"adapter", hs.Name, "scope", hs.Scope, "error", err.Error())
		return false
	}
	return true
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

func (s *Server) noteVerifyFailure(hosts []string, scope string, class identityRejectClass, cause error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, key := range s.failureAttributionKeysLocked(hosts, scope, class) {
		if len(s.waiters[key]) == 0 {
			delete(s.verifyFailures, key)
			continue
		}
		if s.verifyFailures == nil {
			s.verifyFailures = make(map[string]*verifyFailureState)
		}
		if s.rejectCount == nil {
			s.rejectCount = make(map[string]int)
		}
		s.rejectCount[key]++
		s.verifyFailures[key] = &verifyFailureState{lastErr: cause.Error(), class: class}
	}
}

func (s *Server) failureAttributionKeysLocked(hosts []string, scope string, class identityRejectClass) []string {
	var prefixWaiters map[string]bool
	if s.perScopeSessions && class == rejectScopeNotRegistered && scope != "" {
		prefixNow := scopeNamePrefix(scope)
		prefixWaiters = make(map[string]bool, len(s.waiters))
		for waiterKey := range s.waiters {
			waiterType, waiterScope, ok := strings.Cut(waiterKey, "\x00")
			if !ok {
				continue
			}
			if slices.Contains(hosts, waiterType) && scopeNamePrefix(waiterScope) == prefixNow {
				prefixWaiters[waiterKey] = true
			}
		}
	}
	keys := make([]string, 0, len(hosts)+len(prefixWaiters))
	seen := make(map[string]bool, len(hosts)+len(prefixWaiters))
	for _, host := range hosts {
		key := s.sessionKey(host, scope)
		if !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	for waiterKey := range prefixWaiters {
		if !seen[waiterKey] {
			seen[waiterKey] = true
			keys = append(keys, waiterKey)
		}
	}
	return keys
}

func (s *Server) clearVerifyFailure(adapterType, scope string) {
	key := s.sessionKey(adapterType, scope)
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.verifyFailures, key)
}

// WaitForHandle waits for a remote adapter session to become available.
func (s *Server) WaitForHandle(ctx context.Context, adapterName, scope string) (adapterhost.Handle, error) {
	return s.WaitForFreshHandle(ctx, adapterName, scope, nil)
}

// WaitForFreshHandle waits for a connection whose handle is not stale.
func (s *Server) WaitForFreshHandle(ctx context.Context, adapterName, scope string, stale adapterhost.Handle) (adapterhost.Handle, error) {
	s.mu.Lock()
	acceptor := s.peerAcceptor
	s.mu.Unlock()
	if rs, ok := acceptor.(adapterhost.RemoteShim); ok {
		return rs.WaitForFreshHandle(ctx, adapterName, scope, stale)
	}
	key := s.sessionKey(adapterName, scope)
	ch := make(chan waitResult, 1)
	s.mu.Lock()
	budget := s.verifyFailureBudget
	s.mu.Unlock()
	return s.awaitWaiter(ctx, adapterName, scope, key, ch, func() {}, budget)
}

// CloseHandle closes the active session for the given adapter definition identity and scope.
func (s *Server) CloseHandle(ctx context.Context, adapterName, scope string) error {
	s.mu.Lock()
	acceptor := s.peerAcceptor
	s.mu.Unlock()
	if rs, ok := acceptor.(adapterhost.RemoteShim); ok {
		return rs.CloseHandle(ctx, adapterName, scope)
	}
	return nil
}

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

// buildTLSConfig compiles TLS settings from config.
func buildTLSConfig(cfg *Config) (*tls.Config, error) {
	if cfg == nil {
		return nil, nil
	}
	if cfg.ServerCertPath == "" && cfg.ServerKeyPath == "" && cfg.ClientCAPath == "" {
		return nil, nil
	}
	if cfg.ServerCertPath == "" || cfg.ServerKeyPath == "" {
		return nil, errors.New("both server_cert and server_key must be specified")
	}

	cert, err := tls.LoadX509KeyPair(cfg.ServerCertPath, cfg.ServerKeyPath)
	if err != nil {
		return nil, fmt.Errorf("load server key pair: %w", err)
	}

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	}

	if cfg.ClientCAPath != "" {
		caData, err := os.ReadFile(cfg.ClientCAPath)
		if err != nil {
			return nil, fmt.Errorf("read client CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caData) {
			return nil, errors.New("failed to parse client CA PEM")
		}
		tlsCfg.ClientCAs = pool
		tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
	}

	return tlsCfg, nil
}

// validateListenerSecurity rejects non-loopback TCP listeners that are not
// protected by mTLS, accept_token, or the explicit insecure opt-in.
func validateListenerSecurity(cfg *Config) error {
	if cfg == nil {
		return nil
	}
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
	hasToken := cfg.AcceptToken != ""
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

// ValidateListenerSecurity validates security constraints on the listener address.
func ValidateListenerSecurity(addr string, cfg *Config) error {
	if cfg == nil {
		return nil
	}
	c := *cfg
	c.ListenAddress = addr
	return validateListenerSecurity(&c)
}

// Scheduling and waiter budgets
const (
	defaultPodStatePollInterval = 100 * time.Millisecond
	defaultPodStatePollTimeout  = 500 * time.Millisecond
	handshakeFrameCap           = 16 * 1024
	DefaultVerifyFailureBudget  = 6 * time.Second
	DefaultSchedulingBudget     = 30 * time.Second
)

type waiterState struct {
	started           bool
	schedDeadline     time.Time
	handshakeDeadline time.Time
}

func (s *Server) waiterBudgets() (schedulingBudget, poll time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	poll = s.podStatePollInterval
	if poll <= 0 {
		poll = defaultPodStatePollInterval
	}
	schedulingBudget = s.schedulingBudget
	if schedulingBudget <= 0 {
		schedulingBudget = DefaultSchedulingBudget
	}
	return schedulingBudget, poll
}

func (s *Server) startEvidence(phase string, phaseKnown bool, key string) bool {
	return s.activityObserved(key) || (phaseKnown && podPhaseStarted(phase))
}

func (s *Server) podTerminalFailure(adapterType, scope, key, phase string, phaseKnown bool) error {
	if phaseKnown && podPhaseTerminal(phase) && !s.dialObserved(key) {
		return s.podTerminalError(adapterType, scope, phase)
	}
	return nil
}

func (s *Server) handleWaiterWake(adapterType, scope, key string, handshakeBudget, schedulingBudget time.Duration, st *waiterState) (done bool, err error) {
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

func (s *Server) awaitWaiter(ctx context.Context, adapterType, scope, key string, ch <-chan waitResult, deregister func(), handshakeBudget time.Duration) (adapterhost.Handle, error) {
	schedulingBudget, poll := s.waiterBudgets()

	phase, phaseKnown := s.observePod(adapterType, scope)
	if terr := s.podTerminalFailure(adapterType, scope, key, phase, phaseKnown); terr != nil {
		deregister()
		return nil, terr
	}
	st := waiterState{schedDeadline: time.Now().Add(schedulingBudget)}
	if st.started = s.startEvidence(phase, phaseKnown, key); st.started {
		st.handshakeDeadline = time.Now().Add(handshakeBudget)
	}

	w := newSessionWaitWindow(ctx, schedulingBudget)
	pendingEmitted := false
	nextWake := func(st *waiterState) time.Duration {
		d := waitPollDuration(poll, st.started, st.handshakeDeadline, st.schedDeadline)
		if !pendingEmitted {
			d = min(d, time.Until(w.pendingDeadline))
		}
		return d
	}

	timer := time.NewTimer(nextWake(&st))
	defer timer.Stop()
	for {
		select {
		case res := <-ch:
			deregister()
			return res.handle, res.err
		case <-ctx.Done():
			deregister()
			return nil, ctx.Err()
		case <-timer.C:
			if !pendingEmitted && !time.Now().Before(w.pendingDeadline) {
				pendingEmitted = true
				s.emitScopeSessionPending(adapterType, scope, key, w.window, time.Since(w.start))
			}
			done, err := s.handleWaiterWake(adapterType, scope, key, handshakeBudget, schedulingBudget, &st)
			if done {
				deregister()
				return nil, err
			}
			timer.Reset(nextWake(&st))
		}
	}
}

func (s *Server) waitTimeoutError(adapterType, scope, key string, budget time.Duration) error {
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
		case rejectChildSet:
			// A peer whose advertised child set failed verification is
			// mis-declared (KB-213); the message already names the offending
			// child so blame the declaration, not the token.
			detail += "; peer child set failed verification (KB-213)"
		case rejectScopeNotRegistered, rejectBadToken:
			detail += "; stale adapter pod holding a pre-rotation accept token? (CRI-137)"
		}
	} else if phase, ok := s.observePod(adapterType, scope); ok && podPhaseStarted(phase) {
		detail = fmt.Sprintf("no identity handshake observed at all for scope %q; adapter pod observed Running but never dialed this shim — adapter Job may be complete or dead (CRI-137)", scope)
	}
	return fmt.Errorf("remote adapter %q session wait for scope %q exceeded %s without a successful identity handshake: %s",
		adapterType, scope, budget, detail)
}

func (s *Server) schedulingTimeoutError(adapterType, scope, key string, schedulingBudget time.Duration, phase string, phaseKnown bool) error {
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

func (s *Server) podTerminalError(adapterType, scope, phase string) error {
	return fmt.Errorf("remote adapter %q session wait for scope %q failed without a successful identity handshake: adapter pod phase is %q — the adapter Job ended without dialing this shim (CRI-137)",
		adapterType, scope, phase)
}

type sessionWaitWindow struct {
	start           time.Time
	window          time.Duration
	pendingDeadline time.Time
}

func newSessionWaitWindow(ctx context.Context, fallbackWindow time.Duration) sessionWaitWindow {
	start := time.Now()
	window := fallbackWindow
	if dl, ok := ctx.Deadline(); ok {
		if ctxWindow := time.Until(dl); ctxWindow > 0 {
			window = ctxWindow.Round(time.Millisecond)
		}
	}
	return sessionWaitWindow{
		start:           start,
		window:          window,
		pendingDeadline: start.Add(window / 2),
	}
}

func waitPollDuration(poll time.Duration, started bool, handshakeDeadline, schedDeadline time.Time) time.Duration {
	d := poll
	if started {
		rem := time.Until(handshakeDeadline)
		if rem > 0 && rem < d {
			d = rem
		}
	} else {
		rem := time.Until(schedDeadline)
		if rem > 0 && rem < d {
			d = rem
		}
	}
	if d <= 0 {
		return time.Millisecond
	}
	return d
}
