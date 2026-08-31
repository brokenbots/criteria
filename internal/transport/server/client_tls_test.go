package servertrans

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// generateTestCerts writes a self-signed CA certificate and key to files under
// a temporary directory and returns the file paths.
func generateTestCerts(t *testing.T) (caFile, certFile, keyFile string) {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "criteria-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		IsCA:         true,
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	dir := t.TempDir()
	caFile = filepath.Join(dir, "ca.pem")
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")

	if err := os.WriteFile(caFile, certPEM, 0o600); err != nil {
		t.Fatalf("write ca: %v", err)
	}
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return caFile, certFile, keyFile
}

// TestNewClient_InvalidURL verifies that NewClient rejects malformed and
// non-http(s) URLs.
func TestNewClient_InvalidURL(t *testing.T) {
	_, err := NewClient("://invalid", newTestLogger())
	if err == nil {
		t.Fatal("expected error for invalid URL")
	}

	_, err = NewClient("ftp://localhost:9999", newTestLogger())
	if err == nil {
		t.Fatal("expected error for non-http(s) URL")
	}
}

// TestNewClient_UnknownTLSMode verifies that buildHTTPClient rejects an
// unknown TLS mode.
func TestNewClient_UnknownTLSMode(t *testing.T) {
	_, err := NewClient("http://localhost:9999", newTestLogger(), Options{TLSMode: "bogus"})
	if err == nil {
		t.Fatal("expected error for unknown TLS mode")
	}
}

// TestNewClient_TLSModeIncompatibleWithHTTP verifies that TLS modes requiring
// TLS are rejected for plaintext URLs.
func TestNewClient_TLSModeIncompatibleWithHTTP(t *testing.T) {
	for _, mode := range []TLSMode{TLSEnable, TLSMutual} {
		_, err := NewClient("http://localhost:9999", newTestLogger(), Options{TLSMode: mode})
		if err == nil {
			t.Fatalf("expected error for TLS mode %q over http", mode)
		}
	}
}

// TestNewClient_TLSEnable builds a client with TLS enabled and a CA file.
func TestNewClient_TLSEnable(t *testing.T) {
	caFile, _, _ := generateTestCerts(t)
	c, err := NewClient("https://localhost:9999", newTestLogger(), Options{
		TLSMode: TLSEnable,
		CAFile:  caFile,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.TLSMode() != TLSEnable {
		t.Fatalf("TLSMode: got %q want %q", c.TLSMode(), TLSEnable)
	}
	if c.http.Transport == nil {
		t.Fatal("expected http transport to be configured")
	}
}

// TestNewClient_TLSMutual builds a client with mTLS enabled.
func TestNewClient_TLSMutual(t *testing.T) {
	caFile, certFile, keyFile := generateTestCerts(t)
	c, err := NewClient("https://localhost:9999", newTestLogger(), Options{
		TLSMode:  TLSMutual,
		CAFile:   caFile,
		CertFile: certFile,
		KeyFile:  keyFile,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.TLSMode() != TLSMutual {
		t.Fatalf("TLSMode: got %q want %q", c.TLSMode(), TLSMutual)
	}
}

// TestNewClient_TLSInvalidCAFile verifies that a missing CA file is reported.
func TestNewClient_TLSInvalidCAFile(t *testing.T) {
	_, err := NewClient("https://localhost:9999", newTestLogger(), Options{
		TLSMode: TLSEnable,
		CAFile:  "/does/not/exist",
	})
	if err == nil {
		t.Fatal("expected error for missing CA file")
	}
}

// TestNewClient_TLSInvalidCABundle verifies that an invalid CA bundle is
// rejected.
func TestNewClient_TLSInvalidCABundle(t *testing.T) {
	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caFile, []byte("not a pem"), 0o600); err != nil {
		t.Fatalf("write ca: %v", err)
	}
	_, err := NewClient("https://localhost:9999", newTestLogger(), Options{
		TLSMode: TLSEnable,
		CAFile:  caFile,
	})
	if err == nil {
		t.Fatal("expected error for invalid CA bundle")
	}
}

// TestNewClient_TLSMutualMissingKey verifies that mTLS without a key file is
// rejected.
func TestNewClient_TLSMutualMissingKey(t *testing.T) {
	caFile, certFile, _ := generateTestCerts(t)
	_, err := NewClient("https://localhost:9999", newTestLogger(), Options{
		TLSMode:  TLSMutual,
		CAFile:   caFile,
		CertFile: certFile,
	})
	if err == nil {
		t.Fatal("expected error for mTLS without key file")
	}
}

// TestNewClient_TLSMutualInvalidKeyPair verifies that an invalid cert/key pair
// is rejected.
func TestNewClient_TLSMutualInvalidKeyPair(t *testing.T) {
	caFile, certFile, _ := generateTestCerts(t)
	// Reuse the CA file as the key file; it contains no private key.
	_, err := NewClient("https://localhost:9999", newTestLogger(), Options{
		TLSMode:  TLSMutual,
		CAFile:   caFile,
		CertFile: certFile,
		KeyFile:  caFile,
	})
	if err == nil {
		t.Fatal("expected error for invalid key pair")
	}
}

// TestNewClient_ProtoJSONCodec verifies that the protojson codec option is
// wired through to the connect client.
func TestNewClient_ProtoJSONCodec(t *testing.T) {
	c, err := NewClient("http://localhost:9999", newTestLogger(), Options{Codec: CodecJSON})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.opts.Codec != CodecJSON {
		t.Fatalf("Codec: got %q want %q", c.opts.Codec, CodecJSON)
	}
	if c.grpc == nil {
		t.Fatal("expected grpc client to be configured")
	}
}

// TestNewClient_PlaintextNonLoopbackRejected verifies that plaintext URLs
// pointing at non-loopback hosts are rejected unless TLS is explicitly
// disabled (which itself requires https).
func TestNewClient_PlaintextNonLoopbackRejected(t *testing.T) {
	_, err := NewClient("http://example.com:9999", newTestLogger())
	if err == nil {
		t.Fatal("expected error for plaintext non-loopback URL")
	}
}

// TestNewClient_TLSDisableIncompatibleWithHTTPS verifies that tls=disable is
// rejected for https URLs.
func TestNewClient_TLSDisableIncompatibleWithHTTPS(t *testing.T) {
	_, err := NewClient("https://localhost:9999", newTestLogger(), Options{TLSMode: TLSDisable})
	if err == nil {
		t.Fatal("expected error for tls=disable with https URL")
	}
}
