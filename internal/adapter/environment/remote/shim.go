// Package remote implements the "remote" environment handler and host-side
// phone-home shim for WS20.
package remote

import (
	"bufio"
	"context"
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
}

// DigestVerifier checks whether a reported adapter digest is acceptable.
type DigestVerifier interface {
	Verify(adapterType string, digest string) error
}

// Shim listens for inbound adapter connections, terminates mTLS, verifies
// identity, and presents each connection as a local-looking Handle.
type Shim struct {
	listenAddr            string
	tlsConfig             *tls.Config
	acceptToken           string
	clientIdentityPattern string
	digestVerifier        DigestVerifier

	mu       sync.Mutex
	sessions map[string]*session // adapter type → active session
	waiters  map[string][]chan waitResult
	listener net.Listener
	started  bool
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
	tlsConf, err := buildTLSConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("remote shim: build tls config: %w", err)
	}
	return &Shim{
		listenAddr:            cfg.ListenAddress,
		tlsConfig:             tlsConf,
		acceptToken:           cfg.AcceptToken,
		clientIdentityPattern: cfg.ClientIdentityPattern,
		digestVerifier:        verifier,
		sessions:              make(map[string]*session),
		waiters:               make(map[string][]chan waitResult),
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
			// Try unix socket for absolute paths
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

	slog.Info("remote shim listening", "addr", lis.Addr().String())

	go s.serve(ctx, lis)
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
	_, err := s.performHandshake(ctx, conn)
	if err != nil {
		return err
	}

	hs, err := s.readHandshakeMessage(conn)
	if err != nil {
		return err
	}

	if err := s.verifyAdapterIdentity(conn, hs); err != nil {
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

	return s.buildAndStoreHandle(ctx, hs.Name, conn, res.udsConn, lis, socketPath, res.client, res.pluginClient, res.bridgeCancel, res.bridgeCtx, res.bridgeWG)
}

func (s *Shim) performHandshake(ctx context.Context, conn net.Conn) (string, error) {
	var certSubject string
	if tlsConn, ok := conn.(*tls.Conn); ok {
		if err := tlsConn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
			_ = conn.Close()
			return "", fmt.Errorf("set handshake deadline: %w", err)
		}
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return "", fmt.Errorf("mtls handshake: %w", err)
		}
		_ = tlsConn.SetDeadline(time.Time{})
		state := tlsConn.ConnectionState()
		if len(state.PeerCertificates) > 0 {
			certSubject = state.PeerCertificates[0].Subject.String()
		}
	}
	if err := ValidateClientIdentity(certSubject, s.clientIdentityPattern); err != nil {
		_ = conn.Close()
		return "", err
	}
	return certSubject, nil
}

func (s *Shim) readHandshakeMessage(conn net.Conn) (handshakeMessage, error) {
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(conn)
	var header []byte
	for {
		b, err := reader.ReadByte()
		if err != nil {
			_ = conn.Close()
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

func (s *Shim) verifyAdapterIdentity(conn net.Conn, hs handshakeMessage) error {
	if s.digestVerifier != nil {
		if err := s.digestVerifier.Verify(hs.Name, hs.Digest); err != nil {
			_ = conn.Close()
			return fmt.Errorf("digest verification: %w", err)
		}
	}
	if s.acceptToken != "" {
		if hs.Token != s.acceptToken {
			_ = conn.Close()
			return fmt.Errorf("accept_token verification failed")
		}
	}
	return nil
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
	if old, ok := s.sessions[adapterName]; ok {
		s.mu.Unlock()
		if old.cancel != nil {
			old.cancel()
		}
		if old.handle != nil {
			_ = old.handle.CloseSession(ctx, "")
			old.handle.Kill()
		}
		_ = os.RemoveAll(filepath.Dir(old.socketPath))
		s.mu.Lock()
	}

	sess := &session{
		handle:     handle,
		cancel:     bridgeCancel,
		cancelCtx:  bridgeCtx,
		socketPath: socketPath,
	}
	s.sessions[adapterName] = sess

	if waiters, ok := s.waiters[adapterName]; ok {
		for _, ch := range waiters {
			ch <- waitResult{handle: handle}
		}
		delete(s.waiters, adapterName)
	}
	s.mu.Unlock()

	go func() {
		<-bridgeCtx.Done()
		_ = conn.Close()
		_ = udsConn.Close()
		bridgeWG.Wait()
		_ = lis.Close()
		pluginClient.Kill()
		_ = os.RemoveAll(filepath.Dir(socketPath))

		s.mu.Lock()
		if cur, ok := s.sessions[adapterName]; ok && cur.handle == handle {
			delete(s.sessions, adapterName)
		}
		s.mu.Unlock()
	}()

	return nil
}

// WaitForHandle blocks until a remote adapter of the given type connects.
func (s *Shim) WaitForHandle(ctx context.Context, adapterType string) (adapterhost.Handle, error) {
	s.mu.Lock()
	if sess, ok := s.sessions[adapterType]; ok {
		s.mu.Unlock()
		return sess.handle, nil
	}
	ch := make(chan waitResult, 1)
	s.waiters[adapterType] = append(s.waiters[adapterType], ch)
	s.mu.Unlock()

	select {
	case res := <-ch:
		return res.handle, res.err
	case <-ctx.Done():
		// Remove ourselves from waiters on cancellation.
		s.mu.Lock()
		waiters := s.waiters[adapterType]
		for i, w := range waiters {
			if w == ch {
				s.waiters[adapterType] = append(waiters[:i], waiters[i+1:]...)
				break
			}
		}
		if len(s.waiters[adapterType]) == 0 {
			delete(s.waiters, adapterType)
		}
		s.mu.Unlock()
		return nil, ctx.Err()
	}
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
