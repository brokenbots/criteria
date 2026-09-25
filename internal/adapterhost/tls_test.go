package adapterhost

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTestTLSFiles writes a self-signed cert (usable as both client
// certificate and CA) into a temp dir and returns the three file paths.
func writeTestTLSFiles(t *testing.T) (certPath, keyPath, caPath string) {
	t.Helper()

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "criteria-peer-tls-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	dir := t.TempDir()
	certPath = filepath.Join(dir, "client.crt")
	keyPath = filepath.Join(dir, "client.key")
	caPath = filepath.Join(dir, "ca.crt")
	for path, blob := range map[string][]byte{
		certPath: certPEM,
		keyPath:  keyPEM,
		caPath:   certPEM,
	} {
		if err := os.WriteFile(path, blob, 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return certPath, keyPath, caPath
}

func TestLoadClientTLS_NoTLSWhenAllPathsEmpty(t *testing.T) {
	cfg, err := LoadClientTLS("", "", "")
	if err != nil {
		t.Fatalf("empty paths: unexpected error %v", err)
	}
	if cfg != nil {
		t.Fatalf("empty paths: want nil config, got %v", cfg)
	}
}

func TestLoadClientTLS_AllOrNothing(t *testing.T) {
	certPath, keyPath, caPath := writeTestTLSFiles(t)

	cases := []struct {
		name             string
		cert, key, ca    string
		wantErrSubstring string
	}{
		{"cert only", certPath, "", "", "mTLS requires"},
		{"key only", "", keyPath, "", "mTLS requires"},
		{"ca only", "", "", caPath, "mTLS requires"},
		{"cert+key missing ca", certPath, keyPath, "", "mTLS requires"},
		{"missing cert file", filepath.Join(t.TempDir(), "nope.crt"), keyPath, caPath, "load client key pair"},
		{"missing key file", certPath, filepath.Join(t.TempDir(), "nope.key"), caPath, "load client key pair"},
		{"missing ca file", certPath, keyPath, filepath.Join(t.TempDir(), "nope.ca"), "read CA bundle"},
		{"junk ca pem", certPath, keyPath, filepath.Join(t.TempDir(), "junk"), "read CA bundle"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadClientTLS(tc.cert, tc.key, tc.ca)
			if err == nil {
				t.Fatalf("want error containing %q, got nil (cfg=%v)", tc.wantErrSubstring, cfg)
			}
			if !strings.Contains(err.Error(), tc.wantErrSubstring) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErrSubstring)
			}
			if cfg != nil {
				t.Fatalf("want nil config on error, got %v", cfg)
			}
		})
	}
}

func TestLoadClientTLS_LoadsConfig(t *testing.T) {
	certPath, keyPath, caPath := writeTestTLSFiles(t)

	cfg, err := LoadClientTLS(certPath, keyPath, caPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg == nil {
		t.Fatal("want TLS config")
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %v, want TLS1.2", cfg.MinVersion)
	}
	if len(cfg.Certificates) != 1 {
		t.Fatalf("got %d client certificates, want 1", len(cfg.Certificates))
	}
	if cfg.RootCAs == nil {
		t.Fatal("want RootCAs pool")
	}
}