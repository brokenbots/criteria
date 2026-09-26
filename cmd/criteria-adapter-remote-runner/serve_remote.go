package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"sync"

	"google.golang.org/grpc"

	internaladapterhost "github.com/brokenbots/criteria/internal/adapterhost"
)

// remoteHandshake is the JSON line sent immediately after the transport
// connection is established. It extends the criteria-go-adapter-sdk v0.5.3
// handshake with an optional scope field so the host shim can route parallel
// subworkflow scopes to distinct handles (CRI-115). Older hosts ignore unknown
// JSON fields, keeping this runner backward-compatible.
type remoteHandshake struct {
	Name               string `json:"name"`
	Version            string `json:"version"`
	Digest             string `json:"digest"`
	Token              string `json:"token,omitempty"`
	SDKProtocolVersion int    `json:"sdk_protocol_version"`
	Scope              string `json:"scope,omitempty"`
}

// serveRemoteOnce dials the criteria host, sends the extended identity
// handshake (including scope when configured), and serves the v2 adapter
// contract on the held connection. It returns when the connection closes.
// The contract is served through the shared internal adapterhost bridge
// (ADR-0007 D6), the same implementation the criteria peer uses.
func serveRemoteOnce(ctx context.Context, cfg *remoteConfig, tlsConf *tls.Config, impl internaladapterhost.Client, log *slog.Logger) error {
	conn, err := dialRemote(cfg.Host, tlsConf)
	if err != nil {
		return fmt.Errorf("dial %s: %w", cfg.Host, err)
	}

	if err := sendRemoteHandshake(conn, cfg); err != nil {
		_ = conn.Close()
		return fmt.Errorf("handshake: %w", err)
	}

	// Keepalive policy (CRI-276): the server pings idle phone-home
	// connections (even with no active streams) and tolerates client pings,
	// while no MaxConnectionIdle/Age is set — idleness alone must never close
	// a live session's connection.
	server := grpc.NewServer(internaladapterhost.RemoteKeepaliveServerOptions()...)
	internaladapterhost.RegisterAdapterService(server, impl)

	wrapped := &closeSignalConn{Conn: conn, doneCh: make(chan struct{})}
	lis := newSingleConnListener(wrapped)

	go func() {
		<-wrapped.doneCh
		_ = lis.Close()
	}()

	go func() {
		<-ctx.Done()
		server.Stop()
	}()

	log.Info("remote adapter session connected", "host", cfg.Host)
	return server.Serve(lis)
}

// dialRemote opens a TCP or Unix connection to the host. TLS is applied only
// for TCP addresses when a config is provided.
func dialRemote(host string, tlsConfig *tls.Config) (net.Conn, error) {
	network := "tcp"
	if filepath.IsAbs(host) || (host != "" && host[0] == '/') {
		network = "unix"
	}
	if tlsConfig != nil && network == "tcp" {
		return tls.Dial("tcp", host, tlsConfig)
	}
	return net.Dial(network, host)
}

// sendRemoteHandshake writes the identity handshake line to conn.
func sendRemoteHandshake(conn net.Conn, cfg *remoteConfig) error {
	h := remoteHandshake{
		Name:               cfg.Name,
		Version:            cfg.Version,
		Digest:             cfg.Digest,
		Token:              cfg.Token,
		SDKProtocolVersion: 2,
		Scope:              cfg.Scope,
	}
	data, err := json.Marshal(h)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		return err
	}
	return nil
}

// closeSignalConn wraps a net.Conn and signals via doneCh the first time
// Close() is called. It lets us stop the single-conn listener when the peer
// closes the connection.
type closeSignalConn struct {
	net.Conn
	once   sync.Once
	doneCh chan struct{}
}

func (c *closeSignalConn) Close() error {
	c.once.Do(func() { close(c.doneCh) })
	return c.Conn.Close()
}

// singleConnListener is a net.Listener that returns a pre-opened connection
// on its first Accept() and then blocks until Close() is called. It lets a
// grpc.Server serve on a connection that was dialed outbound (the phone-home
// model).
type singleConnListener struct {
	conn   net.Conn
	mu     sync.Mutex
	used   bool
	closed chan struct{}
}

func newSingleConnListener(conn net.Conn) *singleConnListener {
	return &singleConnListener{
		conn:   conn,
		closed: make(chan struct{}),
	}
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if !l.used {
		l.used = true
		l.mu.Unlock()
		return l.conn, nil
	}
	l.mu.Unlock()
	<-l.closed
	return nil, errors.New("listener closed")
}

func (l *singleConnListener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	select {
	case <-l.closed:
	default:
		close(l.closed)
	}
	return nil
}

func (l *singleConnListener) Addr() net.Addr {
	if l.conn != nil {
		return l.conn.LocalAddr()
	}
	return nil
}
