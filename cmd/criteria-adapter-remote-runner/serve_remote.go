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

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
	adapterhost "github.com/brokenbots/criteria-go-adapter-sdk/adapterhost"
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
func serveRemoteOnce(ctx context.Context, cfg *remoteConfig, tlsConf *tls.Config, proxy *proxyService, log *slog.Logger) error {
	conn, err := dialRemote(cfg.Host, tlsConf)
	if err != nil {
		return fmt.Errorf("dial %s: %w", cfg.Host, err)
	}

	if err := sendRemoteHandshake(conn, cfg); err != nil {
		_ = conn.Close()
		return fmt.Errorf("handshake: %w", err)
	}

	server := grpc.NewServer()
	v2.RegisterAdapterServiceServer(server, &grpcAdapterServer{impl: proxy})

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

// grpcAdapterServer bridges the generated v2.AdapterServiceServer interface to
// the runner's proxy Service. It mirrors the adapterhost implementation in
// criteria-go-adapter-sdk v0.5.3 and can be retired once the SDK exports a
// scope-aware ServeRemote.
type grpcAdapterServer struct {
	v2.UnimplementedAdapterServiceServer
	impl adapterhost.Service
}

func (s *grpcAdapterServer) Info(ctx context.Context, req *v2.InfoRequest) (*v2.InfoResponse, error) {
	return s.impl.Info(ctx, req)
}

func (s *grpcAdapterServer) OpenSession(ctx context.Context, req *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	return s.impl.OpenSession(ctx, req)
}

func (s *grpcAdapterServer) Execute(req *v2.ExecuteRequest, stream v2.AdapterService_ExecuteServer) error {
	return s.impl.Execute(stream.Context(), req, &grpcExecuteEventServer{stream: stream})
}

func (s *grpcAdapterServer) Log(req *v2.LogRequest, stream v2.AdapterService_LogServer) error {
	sender := &grpcLogEventServer{stream: stream}
	go func() {
		_ = v2.RunHeartbeat(stream.Context(), "log", func(hb *v2.Heartbeat) error {
			return sender.Send(&v2.LogEvent{Heartbeat: hb})
		})
	}()

	errCh := make(chan error, 1)
	go func() { errCh <- s.impl.Log(stream.Context(), req, sender) }()

	err := <-errCh
	if err != nil {
		return err
	}
	<-stream.Context().Done()
	return nil
}

func (s *grpcAdapterServer) Permissions(stream v2.AdapterService_PermissionsServer) error {
	return s.impl.Permissions(stream.Context(), &grpcPermissionsServer{stream: stream})
}

func (s *grpcAdapterServer) CloseSession(ctx context.Context, req *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
	return s.impl.CloseSession(ctx, req)
}

type grpcExecuteEventServer struct {
	mu     sync.Mutex
	stream v2.AdapterService_ExecuteServer
}

func (s *grpcExecuteEventServer) Send(evt *v2.ExecuteEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream.Send(evt)
}

type grpcLogEventServer struct {
	mu     sync.Mutex
	stream v2.AdapterService_LogServer
}

func (s *grpcLogEventServer) Send(evt *v2.LogEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream.Send(evt)
}

type grpcPermissionsServer struct {
	stream v2.AdapterService_PermissionsServer
}

func (s *grpcPermissionsServer) Recv() (*v2.PermissionEvent, error) {
	return s.stream.Recv()
}

func (s *grpcPermissionsServer) Send(dec *v2.PermissionDecision) error {
	return s.stream.Send(dec)
}

func (s *grpcPermissionsServer) Context() context.Context {
	return s.stream.Context()
}
