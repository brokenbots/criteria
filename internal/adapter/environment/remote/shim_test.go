package remote

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/hcl/v2/hclparse"
	v2 "github.com/brokenbots/criteria/sdk/pb/criteria/v2"
	"github.com/brokenbots/criteria/workflow"
	"google.golang.org/grpc"
)

// --- Helpers ---

func generateTestCerts(t *testing.T) (serverCert, serverKey, clientCert, clientKey, caCert string) {
	t.Helper()
	dir := t.TempDir()

	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	caTemplate := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test CA"},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, &caTemplate, &caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	caCert = filepath.Join(dir, "ca.pem")
	writePEM(t, caCert, "CERTIFICATE", caDER)

	makeCert := func(name string, parent *x509.Certificate, parentKey *rsa.PrivateKey, subject pkix.Name, ips []net.IP, dns []string) (certPath, keyPath string) {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generate key for %s: %v", name, err)
		}
		template := x509.Certificate{
			SerialNumber: big.NewInt(2),
			Subject:      subject,
			NotBefore:    time.Now(),
			NotAfter:     time.Now().Add(time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
			IPAddresses:  ips,
			DNSNames:     dns,
		}
		der, err := x509.CreateCertificate(rand.Reader, &template, parent, &key.PublicKey, parentKey)
		if err != nil {
			t.Fatalf("create cert for %s: %v", name, err)
		}
		certPath = filepath.Join(dir, name+".pem")
		writePEM(t, certPath, "CERTIFICATE", der)
		keyPath = filepath.Join(dir, name+"-key.pem")
		writeKey(t, keyPath, key)
		return certPath, keyPath
	}

	serverCert, serverKey = makeCert("server", &caTemplate, caKey, pkix.Name{CommonName: "criteria-server"}, []net.IP{net.ParseIP("127.0.0.1")}, []string{"localhost"})
	clientCert, clientKey = makeCert("client", &caTemplate, caKey, pkix.Name{CommonName: "criteria-adapter-noop"}, nil, nil)
	return serverCert, serverKey, clientCert, clientKey, caCert
}

func writePEM(t *testing.T, path, typ string, der []byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: typ, Bytes: der}); err != nil {
		t.Fatalf("encode %s: %v", path, err)
	}
}

func writeKey(t *testing.T, path string, key *rsa.PrivateKey) {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: der}); err != nil {
		t.Fatalf("encode %s: %v", path, err)
	}
}

type fixedDigestVerifier struct {
	allowed map[string]string
}

func (v *fixedDigestVerifier) Verify(adapterType, digest string) error {
	if want, ok := v.allowed[adapterType]; ok {
		if digest == want {
			return nil
		}
		return fmt.Errorf("digest mismatch: got %q, want %q", digest, want)
	}
	return fmt.Errorf("unknown adapter type %q", adapterType)
}

// fakeAdapterServer implements a minimal v2.AdapterServiceServer for testing.
type fakeAdapterServer struct {
	v2.UnimplementedAdapterServiceServer
	infoName    string
	infoVersion string
}

func (f *fakeAdapterServer) Info(ctx context.Context, req *v2.InfoRequest) (*v2.InfoResponse, error) {
	return &v2.InfoResponse{Name: f.infoName, Version: f.infoVersion, Capabilities: []string{"execute"}}, nil
}

func (f *fakeAdapterServer) OpenSession(ctx context.Context, req *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	return &v2.OpenSessionResponse{}, nil
}

func (f *fakeAdapterServer) Execute(req *v2.ExecuteRequest, stream v2.AdapterService_ExecuteServer) error {
	_ = stream.Send(&v2.ExecuteEvent{Event: &v2.ExecuteEvent_Result{
		Result: &v2.ExecuteResult{
			Outcome: "success",
			Outputs: map[string]string{"greeting": "hello"},
		},
	}})
	return nil
}

func mustMarshalJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// singleConnListener returns a single connection and then EOFs.
type singleConnListener struct {
	conn net.Conn
	mu   sync.Mutex
	done bool
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done {
		return nil, io.EOF
	}
	l.done = true
	return l.conn, nil
}

func (l *singleConnListener) Close() error   { return nil }
func (l *singleConnListener) Addr() net.Addr { return l.conn.LocalAddr() }

// dialFakeAdapter connects to the shim, sends the handshake, and serves a
// minimal gRPC adapter on the connection.
func dialFakeAdapter(t *testing.T, addr string, hs handshakeMessage, tlsConf *tls.Config) {
	var conn net.Conn
	var err error
	if tlsConf != nil {
		conn, err = tls.Dial("tcp", addr, tlsConf)
	} else {
		conn, err = net.Dial("tcp", addr)
	}
	if err != nil {
		t.Fatalf("dial shim: %v", err)
	}

	hsBytes, _ := json.Marshal(hs)
	if _, err := conn.Write(append(hsBytes, '\n')); err != nil {
		t.Fatalf("write handshake: %v", err)
	}

	// Serve gRPC on this single connection.
	grpcServer := grpc.NewServer()
	v2.RegisterAdapterServiceServer(grpcServer, &fakeAdapterServer{
		infoName:    hs.Name,
		infoVersion: hs.Version,
	})
	lis := &singleConnListener{conn: conn}
	go func() {
		if err := grpcServer.Serve(lis); err != nil {
			// Expected when connection closes.
		}
	}()
	// Ensure server stops when connection closes.
	t.Cleanup(func() { grpcServer.Stop() })
}

// --- Unit tests ---

func TestParseConfig_Basic(t *testing.T) {
	src := `
listen_address = "0.0.0.0:7778"
accept_token = "secret"
policy_mode = "strict"
mtls {
  server_cert = "/certs/server.pem"
  server_key  = "/certs/key.pem"
  client_ca   = "/certs/ca.pem"
  client_identity_pattern = "CN=adapter-.*"
}
`
	parser := hclparse.NewParser()
	file, diags := parser.ParseHCL([]byte(src), "test.hcl")
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	body := file.Body
	cfg, err := ParseConfig(body)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if cfg.ListenAddress != "0.0.0.0:7778" {
		t.Errorf("listen_address = %q, want 0.0.0.0:7778", cfg.ListenAddress)
	}
	if cfg.AcceptToken != "secret" {
		t.Errorf("accept_token = %q, want secret", cfg.AcceptToken)
	}
	if cfg.PolicyMode != "strict" {
		t.Errorf("policy_mode = %q, want strict", cfg.PolicyMode)
	}
	if cfg.ServerCertPath != "/certs/server.pem" {
		t.Errorf("server_cert = %q", cfg.ServerCertPath)
	}
	if cfg.ServerKeyPath != "/certs/key.pem" {
		t.Errorf("server_key = %q", cfg.ServerKeyPath)
	}
	if cfg.ClientCAPath != "/certs/ca.pem" {
		t.Errorf("client_ca = %q", cfg.ClientCAPath)
	}
	if cfg.ClientIdentityPattern != "CN=adapter-.*" {
		t.Errorf("client_identity_pattern = %q", cfg.ClientIdentityPattern)
	}
}

func TestValidateClientIdentity(t *testing.T) {
	if err := ValidateClientIdentity("CN=criteria-adapter-noop", "CN=criteria-adapter-.*"); err != nil {
		t.Errorf("expected match, got: %v", err)
	}
	if err := ValidateClientIdentity("CN=other", "CN=criteria-adapter-.*"); err == nil {
		t.Error("expected mismatch error")
	}
	if err := ValidateClientIdentity("CN=anything", ""); err != nil {
		t.Errorf("empty pattern should always match: %v", err)
	}
}

func TestBuildTLSConfig(t *testing.T) {
	serverCert, serverKey, _, _, caCert := generateTestCerts(t)
	cfg := &Config{
		ServerCertPath: serverCert,
		ServerKeyPath:  serverKey,
		ClientCAPath:   caCert,
	}
	tlsConf, err := buildTLSConfig(cfg)
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if tlsConf == nil {
		t.Fatal("expected non-nil tls.Config")
	}
	if len(tlsConf.Certificates) != 1 {
		t.Errorf("expected 1 certificate, got %d", len(tlsConf.Certificates))
	}
	if tlsConf.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("expected RequireAndVerifyClientCert")
	}
}

// --- Integration tests ---

func TestShim_AcceptAndCallInfo(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	verifier := &fixedDigestVerifier{allowed: map[string]string{"noop": "sha256:abcd1234"}}
	shim, err := NewShim(&Config{ListenAddress: "127.0.0.1:0"},
		verifier,
	)
	if err != nil {
		t.Fatalf("NewShim: %v", err)
	}
	if err := shim.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = shim.Stop(ctx) }()

	// Find the actual listen address.
	addr := shim.listener.Addr().String()

	// Dial fake adapter in background.
	go dialFakeAdapter(t, addr, handshakeMessage{
		Name:    "noop",
		Version: "1.0.0",
		Digest:  "sha256:abcd1234",
	}, nil)

	// Wait for handle from session manager perspective.
	handle, err := shim.WaitForHandle(ctx, "noop")
	if err != nil {
		t.Fatalf("WaitForHandle: %v", err)
	}
	defer handle.Kill()

	info, err := handle.Info(ctx)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.Name != "noop" {
		t.Errorf("info.Name = %q, want noop", info.Name)
	}
	if info.Version != "1.0.0" {
		t.Errorf("info.Version = %q, want 1.0.0", info.Version)
	}
}

func TestShim_RejectUnknownDigest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	verifier := &fixedDigestVerifier{allowed: map[string]string{"noop": "sha256:abcd1234"}}
	shim, err := NewShim(&Config{ListenAddress: "127.0.0.1:0"},
		verifier,
	)
	if err != nil {
		t.Fatalf("NewShim: %v", err)
	}
	if err := shim.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = shim.Stop(ctx) }()

	addr := shim.listener.Addr().String()

	// Use a raw net.Conn so we can observe the close.
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	hs := handshakeMessage{Name: "noop", Version: "1.0.0", Digest: "sha256:bad"}
	hsBytes, _ := json.Marshal(hs)
	if _, err := conn.Write(append(hsBytes, '\n')); err != nil {
		t.Fatalf("write handshake: %v", err)
	}

	// Connection should be closed by shim after digest rejection.
	buf := make([]byte, 1)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = conn.Read(buf)
	if err == nil {
		t.Fatal("expected connection to close after bad digest")
	}
}

func TestShim_mTLSAcceptAndReject(t *testing.T) {
	serverCert, serverKey, clientCert, clientKey, caCert := generateTestCerts(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	verifier := &fixedDigestVerifier{allowed: map[string]string{"noop": "sha256:abcd1234"}}
	shim, err := NewShim(&Config{
		ListenAddress:         "127.0.0.1:0",
		ServerCertPath:        serverCert,
		ServerKeyPath:         serverKey,
		ClientCAPath:          caCert,
		ClientIdentityPattern: "CN=criteria-adapter-.*",
	}, verifier)
	if err != nil {
		t.Fatalf("NewShim: %v", err)
	}
	if err := shim.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = shim.Stop(ctx) }()

	addr := shim.listener.Addr().String()

	// Good client with matching CN.
	cert, err := tls.LoadX509KeyPair(clientCert, clientKey)
	if err != nil {
		t.Fatalf("load client cert: %v", err)
	}
	caPool := x509.NewCertPool()
	caPEM, _ := os.ReadFile(caCert)
	caPool.AppendCertsFromPEM(caPEM)
	goodTLS := &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      caPool,
	}

	go dialFakeAdapter(t, addr, handshakeMessage{
		Name:    "noop",
		Version: "1.0.0",
		Digest:  "sha256:abcd1234",
	}, goodTLS)

	handle, err := shim.WaitForHandle(ctx, "noop")
	if err != nil {
		t.Fatalf("WaitForHandle: %v", err)
	}
	defer handle.Kill()

	info, err := handle.Info(ctx)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.Name != "noop" {
		t.Errorf("info.Name = %q, want noop", info.Name)
	}
}

func TestShim_ExecuteThroughBridge(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	verifier := &fixedDigestVerifier{allowed: map[string]string{"noop": "sha256:abcd1234"}}
	shim, err := NewShim(&Config{ListenAddress: "127.0.0.1:0"},
		verifier,
	)
	if err != nil {
		t.Fatalf("NewShim: %v", err)
	}
	if err := shim.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = shim.Stop(ctx) }()

	addr := shim.listener.Addr().String()

	go dialFakeAdapter(t, addr, handshakeMessage{
		Name:    "noop",
		Version: "1.0.0",
		Digest:  "sha256:abcd1234",
	}, nil)

	handle, err := shim.WaitForHandle(ctx, "noop")
	if err != nil {
		t.Fatalf("WaitForHandle: %v", err)
	}
	defer handle.Kill()

	if err := handle.OpenSession(ctx, "sess-1", nil, nil); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}

	step := &workflow.StepNode{Name: "step-1", Input: map[string]string{}}
	result, err := handle.Execute(ctx, "sess-1", step, &nopSink{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Outcome != "success" {
		t.Errorf("outcome = %q, want success", result.Outcome)
	}
	if result.Outputs["greeting"] != "hello" {
		t.Errorf("outputs = %v", result.Outputs)
	}
}

type nopSink struct{}

func (n *nopSink) Log(stream string, chunk []byte)       {}
func (n *nopSink) Adapter(kind string, data any)         {}

// TestShim_Reconnect verifies that after a connection is dropped, WaitForHandle
// blocks until a new adapter dials in.
func TestShim_Reconnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	verifier := &fixedDigestVerifier{allowed: map[string]string{"noop": "sha256:abcd1234"}}
	shim, err := NewShim(
		&Config{ListenAddress: "127.0.0.1:0"},
		verifier,
	)
	if err != nil {
		t.Fatalf("NewShim: %v", err)
	}
	if err := shim.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = shim.Stop(ctx) }()

	addr := shim.listener.Addr().String()

	// First connection.
	go dialFakeAdapter(t, addr, handshakeMessage{
		Name:    "noop",
		Version: "1.0.0",
		Digest:  "sha256:abcd1234",
	}, nil)

	h1, err := shim.WaitForHandle(ctx, "noop")
	if err != nil {
		t.Fatalf("first WaitForHandle: %v", err)
	}

	// Kill the first connection (simulates adapter crash / disconnect).
	h1.Kill()

	// Wait a bit for cleanup.
	time.Sleep(200 * time.Millisecond)

	// Second connection in background (simulates respawn reconnect).
	reconnected := make(chan struct{})
	go func() {
		defer close(reconnected)
		dialFakeAdapter(t, addr, handshakeMessage{
			Name:    "noop",
			Version: "2.0.0",
			Digest:  "sha256:abcd1234",
		}, nil)
	}()

	// WaitForHandle should block until the second connection arrives.
	h2, err := shim.WaitForHandle(ctx, "noop")
	if err != nil {
		t.Fatalf("second WaitForHandle: %v", err)
	}
	defer h2.Kill()

	info, err := h2.Info(ctx)
	if err != nil {
		t.Fatalf("Info after reconnect: %v", err)
	}
	if info.Version != "2.0.0" {
		t.Errorf("version after reconnect = %q, want 2.0.0", info.Version)
	}

	select {
	case <-reconnected:
	case <-ctx.Done():
		t.Fatal("timeout waiting for reconnection")
	}
}

// trigger shim startup errors, and that a workflow with a remote env compiles.
// This is a compile-time test using the workflow package.
func TestCompileTimeFold(t *testing.T) {
	src := `
workflow {
  name = "remote-test"
  version = "0.1"
  initial_state = "start"
  target_state = "done"
}
environment "remote" "prod" {
  listen_address = "127.0.0.1:0"
}
adapter "noop" "default" {
  environment = remote.prod
}
step "start" {
  target = adapter.noop.default
  outcome "success" { next = "done" }
}
state "done" { terminal = true }
`
	file, diags := workflow.Parse("test.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}
	graph, diags := workflow.Compile(file, nil)
	if diags.HasErrors() {
		t.Fatalf("compile: %s", diags.Error())
	}
	if len(graph.Environments) != 1 {
		t.Fatalf("expected 1 environment, got %d", len(graph.Environments))
	}
	env := graph.Environments["remote.prod"]
	if env == nil {
		t.Fatal("remote.prod environment not found")
	}
	if env.RawBody == nil {
		t.Fatal("expected RawBody to be preserved for remote environment")
	}
	cfg, err := ParseConfig(env.RawBody)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.ListenAddress != "127.0.0.1:0" {
		t.Errorf("listen_address = %q", cfg.ListenAddress)
	}
}
