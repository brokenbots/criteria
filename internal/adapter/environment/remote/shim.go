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
}

// DigestVerifier checks whether a reported adapter digest is acceptable.
type DigestVerifier interface {
	Verify(adapterType string, digest string) error
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
// pending session wait: if no adapter re-handshakes within the budget, the
// wait fails terminally. It covers both the stale-pod case (dials keep being
// rejected) and the dead-Job case (no dials at all); there is deliberately no
// separate rejection-count bound (CRI-137 review: one authoritative bound).
const DefaultVerifyFailureBudget = 5 * time.Minute

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

	tlsDeadline := cfg.TLSHandshakeDeadline
	if tlsDeadline == 0 {
		tlsDeadline = DefaultTLSHandshakeDeadline
	}
	identityDeadline := cfg.IdentityHandshakeDeadline
	if identityDeadline == 0 {
		identityDeadline = DefaultIdentityHandshakeDeadline
	}
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
		verifyFailureBudget:   DefaultVerifyFailureBudget,
	}, nil
}

// Start binds the listener. Called at workflow startup if any remote env
// is referenced; skipped if no remote env is referenced (compile-time fold).
func (s *Shim) Start(ctx context.Context) error {
	var lis net.Listener
	var err error

	if s.tlsConfig != nil {
		lis, err = tls.Listen("tcp", s.listenAddr, s.tlsConfig)
	} else {
		// Support both TCP and Unix socket addresses.
		if filepath.IsAbs(s.listenAddr) || s.listenAddr != "" && s.listenAddr[0] == '/' {
			// Try unix socket for absolute paths.
			if err := checkUnixSocketPath(s.listenAddr); err != nil {
				return fmt.Errorf("remote shim: %w", err)
			}
			lis, err = net.Listen("unix", s.listenAddr)
		} else {
			lis, err = net.Listen("tcp", s.listenAddr)
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

	if err := s.verifyAdapterIdentity(conn, &hs); err != nil {
		return err
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
	reader := bufio.NewReader(conn)
	var header []byte
	for {
		b, err := reader.ReadByte()
		if err != nil {
			_ = conn.Close()
			if isDeadlineTimeout(err) {
				return handshakeMessage{}, fmt.Errorf("identity message deadline (%s) exceeded; timeout caused rejection", s.identityDeadline)
			}
			return handshakeMessage{}, fmt.Errorf("read handshake: %w", err)
		}
		if b == '\n' {
			break
		}
		header = append(header, b)
	}
	_ = conn.SetReadDeadline(time.Time{})

	var hs handshakeMessage
	if err := json.Unmarshal(header, &hs); err != nil {
		_ = conn.Close()
		return handshakeMessage{}, fmt.Errorf("unmarshal handshake: %w", err)
	}
	return hs, nil
}

func (s *Shim) verifyAdapterIdentity(conn net.Conn, hs *handshakeMessage) error {
	class, err := s.checkAdapterIdentity(conn, hs)
	if err != nil {
		s.noteVerifyFailure(hs.Name, hs.Scope, class, err)
		return err
	}
	s.clearVerifyFailure(hs.Name, hs.Scope)
	return nil
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

func (s *Shim) checkAdapterIdentity(conn net.Conn, hs *handshakeMessage) (identityRejectClass, error) {
	if s.digestVerifier != nil {
		if err := s.digestVerifier.Verify(hs.Name, hs.Digest); err != nil {
			_ = conn.Close()
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
			_ = conn.Close()
			return rejectScopeNotRegistered, fmt.Errorf("scope %q is not registered", hs.Scope)
		}
		if subtle.ConstantTimeCompare([]byte(hs.Token), []byte(expectedToken)) != 1 {
			_ = conn.Close()
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
			_ = conn.Close()
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
// The wait is bounded by the verify-failure wall-clock budget: if no adapter
// successfully re-handshakes within the budget, the wait fails with a
// terminal error naming the scope and its accept-token state (CRI-137). This
// covers both a stale adapter pod whose dials keep being rejected on a
// pre-rotation scope key and an adapter Job that is complete or dead and
// never dials again.
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

	budgetTimer := time.NewTimer(budget)
	defer budgetTimer.Stop()

	select {
	case res := <-ch:
		return res.handle, res.err
	case <-budgetTimer.C:
		s.removeWaiter(key, ch)
		return nil, s.waitTimeoutError(adapterType, scope, key, budget)
	case <-ctx.Done():
		// Remove ourselves from waiters on cancellation.
		s.removeWaiter(key, ch)
		return nil, ctx.Err()
	}
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

// waitTimeoutError builds the terminal error for a pending session wait that
// exceeded the wall-clock budget. It names the scope, distinguishes the last
// observed rejection by class (digest mismatch vs accept-token/scope state),
// and calls out the dead-Job shape when no identity handshake was observed at
// all (CRI-137).
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
	}
	return fmt.Errorf("remote adapter %q session wait for scope %q exceeded %s without a successful identity handshake: %s",
		adapterType, scope, budget, detail)
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
