// criteria-adapter-remote-smoke is a tiny fixture adapter used only by the
// WS22 remote end-to-end smoke test. It reads remote connection config from
// environment variables, phones home to the Criteria host shim, and implements
// echo semantics (returns input as output).
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
)

// remoteHandshakeMessage is the pre-gRPC identity frame sent to the host shim.
type remoteHandshakeMessage struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Digest  string `json:"digest"`
	Token   string `json:"token,omitempty"`
}

// smokeAdapter implements a minimal echo adapter for the remote smoke test.
type smokeAdapter struct {
	v2.UnimplementedAdapterServiceServer
	mu       sync.Mutex
	sessions map[string]struct{}
	name     string
	log      *slog.Logger
}

func (s *smokeAdapter) Info(context.Context, *v2.InfoRequest) (*v2.InfoResponse, error) {
	return &v2.InfoResponse{
		Name:         s.name,
		Version:      "0.1.0",
		Capabilities: []string{"execute", "parallel_safe"},
	}, nil
}

func (s *smokeAdapter) OpenSession(_ context.Context, req *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions == nil {
		s.sessions = map[string]struct{}{}
	}
	s.sessions[req.GetSessionId()] = struct{}{}
	return &v2.OpenSessionResponse{}, nil
}

func (s *smokeAdapter) Execute(req *v2.ExecuteRequest, stream v2.AdapterService_ExecuteServer) error {
	s.mu.Lock()
	_, ok := s.sessions[req.GetSessionId()]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown session %q", req.GetSessionId())
	}

	// Write marker file when Execute starts so the test can synchronise on it.
	if marker := os.Getenv("CRITERIA_REMOTE_STEP_STARTED_FILE"); marker != "" {
		_ = os.WriteFile(marker, []byte("started"), 0o600)
	}

	s.log.Info("step execution started", "session_id", req.GetSessionId())

	ctx := stream.Context()

	// Support delay_ms for crash-recovery testing.
	if rawDelay := req.GetInput()["delay_ms"]; rawDelay != "" {
		delayMS, err := strconv.Atoi(rawDelay)
		if err != nil || delayMS < 0 {
			return fmt.Errorf("invalid delay_ms %q", rawDelay)
		}
		if delayMS > 0 {
			timer := time.NewTimer(time.Duration(delayMS) * time.Millisecond)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}

	// Echo semantics: send every input key back as an output key on the typed
	// outputs_json channel.
	outputs := make(map[string]any, len(req.GetInput()))
	for k, v := range req.GetInput() {
		outputs[k] = v
	}
	ev, err := v2.NewExecuteResultEvent("success", outputs)
	if err != nil {
		return err
	}
	return stream.Send(ev)
}

func (s *smokeAdapter) Log(_ *v2.LogRequest, _ v2.AdapterService_LogServer) error {
	return nil
}

func (s *smokeAdapter) Permissions(_ v2.AdapterService_PermissionsServer) error {
	return nil
}

func (s *smokeAdapter) Pause(context.Context, *v2.PauseRequest) (*v2.PauseResponse, error) {
	return &v2.PauseResponse{}, nil
}

func (s *smokeAdapter) Resume(context.Context, *v2.ResumeRequest) (*v2.ResumeResponse, error) {
	return &v2.ResumeResponse{}, nil
}

func (s *smokeAdapter) Snapshot(context.Context, *v2.SnapshotRequest) (*v2.SnapshotResponse, error) {
	return &v2.SnapshotResponse{}, nil
}

func (s *smokeAdapter) Restore(context.Context, *v2.RestoreRequest) (*v2.RestoreResponse, error) {
	return &v2.RestoreResponse{}, nil
}

func (s *smokeAdapter) Inspect(context.Context, *v2.InspectRequest) (*v2.InspectResponse, error) {
	return &v2.InspectResponse{}, nil
}

func (s *smokeAdapter) CloseSession(_ context.Context, req *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, req.GetSessionId())
	return &v2.CloseSessionResponse{}, nil
}

// singleConnListener hands out exactly one connection. The second Accept blocks
// until that connection is actually closed, then returns io.EOF.
//
// Blocking (rather than returning EOF immediately) is essential: grpc.Server.Serve
// loops on Accept, and if the second Accept returned EOF right away, Serve would
// return while the connection is still live — causing dialAndServe to return and
// the reconnect loop to redial spuriously every backoff interval. Each spurious
// reconnect would make the host shim clobber its own active session (it keys
// sessions by adapter type), killing an in-flight Execute. Blocking until the
// served connection closes means we redial only on a real disconnect.
type singleConnListener struct {
	conn   net.Conn
	mu     sync.Mutex
	served bool
	closed chan struct{}
}

func newSingleConnListener(conn net.Conn) *singleConnListener {
	l := &singleConnListener{closed: make(chan struct{})}
	l.conn = &notifyConn{Conn: conn, onClose: func() { close(l.closed) }}
	return l
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if !l.served {
		l.served = true
		c := l.conn
		l.mu.Unlock()
		return c, nil
	}
	l.mu.Unlock()
	<-l.closed // block until the served connection is closed
	return nil, io.EOF
}

func (l *singleConnListener) Close() error   { return nil }
func (l *singleConnListener) Addr() net.Addr { return l.conn.LocalAddr() }

// notifyConn signals (once) when the connection is closed, so the listener knows
// when to release the blocked Accept.
type notifyConn struct {
	net.Conn
	once    sync.Once
	onClose func()
}

func (c *notifyConn) Close() error {
	c.once.Do(c.onClose)
	return c.Conn.Close()
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	host := os.Getenv("CRITERIA_REMOTE_HOST")
	if host == "" {
		log.Error("missing CRITERIA_REMOTE_HOST")
		os.Exit(1)
	}
	token := os.Getenv("CRITERIA_REMOTE_TOKEN")
	digest := os.Getenv("CRITERIA_REMOTE_DIGEST")
	if digest == "" {
		digest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	}
	name := os.Getenv("CRITERIA_ADAPTER_NAME")
	if name == "" {
		name = "remote-smoke"
	}

	tlsCertPath := os.Getenv("CRITERIA_REMOTE_TLS_CERT")
	tlsKeyPath := os.Getenv("CRITERIA_REMOTE_TLS_KEY")
	tlsCAPath := os.Getenv("CRITERIA_REMOTE_CA")

	var tlsConf *tls.Config
	if tlsCertPath != "" && tlsKeyPath != "" && tlsCAPath != "" {
		var err error
		tlsConf, err = buildTLSConfig(tlsCertPath, tlsKeyPath, tlsCAPath)
		if err != nil {
			log.Error("build tls config", "error", err)
			os.Exit(1)
		}
	}

	// Stop phoning home on SIGTERM/SIGINT. When a deployment deletes this pod
	// (e.g. the crash-recovery test), Kubernetes sends SIGTERM with a grace
	// period; without this, the terminating pod would keep reconnecting and
	// clobber the replacement pod's session on the host shim.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	for ctx.Err() == nil {
		if err := dialAndServe(ctx, host, token, digest, name, tlsConf, log); err != nil {
			log.Error("connection lost", "error", err)
		}
		// Back-off before reconnecting so crash-looping doesn't hammer the host.
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
	log.Info("shutting down on signal")
}

func dialAndServe(ctx context.Context, host, token, digest, name string, tlsConf *tls.Config, log *slog.Logger) error {
	var conn net.Conn
	var err error
	if tlsConf != nil {
		conn, err = tls.Dial("tcp", host, tlsConf)
	} else {
		conn, err = net.Dial("tcp", host)
	}
	if err != nil {
		return fmt.Errorf("dial host: %w", err)
	}

	hs := remoteHandshakeMessage{
		Name:    name,
		Version: "0.1.0",
		Digest:  digest,
		Token:   token,
	}
	hsBytes, _ := json.Marshal(hs)
	if _, err := conn.Write(append(hsBytes, '\n')); err != nil {
		_ = conn.Close()
		return fmt.Errorf("write handshake: %w", err)
	}

	log.Info("handshake sent", "host", host, "name", hs.Name)

	// Serve gRPC directly on this single connection.
	// The shim reads the handshake line, validates it, then bridges the
	// remaining bytes to a local UDS where the go-plugin client connects.
	grpcServer := grpc.NewServer()
	v2.RegisterAdapterServiceServer(grpcServer, &smokeAdapter{sessions: map[string]struct{}{}, name: name, log: log})

	// Stop the server (closing the connection) when the process is signalled,
	// so the reconnect loop exits instead of redialing during pod termination.
	go func() {
		<-ctx.Done()
		grpcServer.Stop()
	}()

	lis := newSingleConnListener(conn)
	log.Info("serving gRPC", "host", host)
	return grpcServer.Serve(lis)
}

func buildTLSConfig(certPath, keyPath, caPath string) (*tls.Config, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("read client cert: %w", err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read client key: %w", err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("load client key pair: %w", err)
	}

	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read CA: %w", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("parse CA")
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      caPool,
	}, nil
}
