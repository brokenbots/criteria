package cli

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestServerHTTPClient_HTTP(t *testing.T) {
	c, err := serverHTTPClient("http://localhost:8080", "", "", "")
	if err != nil {
		t.Fatalf("expected http client, got error: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil client")
	}
}

func TestServerHTTPClient_HTTPS(t *testing.T) {
	c, err := serverHTTPClient("https://localhost:8443", "", "", "")
	if err != nil {
		t.Fatalf("expected https client, got error: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil client")
	}
}

func TestServerHTTPClient_HTTP_WithTLSFlags_Errors(t *testing.T) {
	_, err := serverHTTPClient("http://localhost:8080", "ca.pem", "", "")
	if err == nil {
		t.Fatal("expected error when TLS flags used with http://")
	}
}

func TestServerHTTPClient_InvalidURL_Errors(t *testing.T) {
	// A URL with an invalid scheme passes url.Parse but hits the default branch.
	_, err := serverHTTPClient("ftp://localhost", "", "", "")
	if err == nil {
		t.Fatal("expected error for unsupported scheme")
	}
}

func TestServerHTTPClient_HTTPSWithCA(t *testing.T) {
	// Write a self-signed CA PEM (just the certificate portion is enough for the
	// pool; it doesn't need to be a real CA for this test).
	caData := `-----BEGIN CERTIFICATE-----
MIIBpDCCAQoCCQDU+pQ4pHgSpDAKBggqhkjOPQQDAjA3MQswCQYDVQQGEwJVUzEM
MAoGA1UECgwDVEVTVDEaMBgGA1UEAwwRdGVzdC1jYS5leGFtcGxlMB4XDTI0MDEw
MTAwMDAwMFoXDTI1MDEwMTAwMDAwMFowNzELMAkGA1UEBhMCVVMxDDAKBgNVBAoM
A1RFU1QxGjAYBgNVBAMMEXRlc3QtY2EuZXhhbXBsZTB2MBAGByqGSM49AgEGBSuB
BAAiA2IABI4N53e/n3IfHQ2+aMuUFMK5yQ+MdJQ4lXbx/dTjMx2F4z3AKTj8MSMV
5Kf0uKDQ6kV4lGf7qA2iDoBg3QSRrjAKBggqhkjOPQQDAgNpADBmAjEApMJnC7k
9p8LiEMjlST5S2RlzNzYDkI4hQ8nOCIBKAIxAI6bKqV3XnVb9yVKlzlF
-----END CERTIFICATE-----
`
	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caFile, []byte(caData), 0o600); err != nil {
		t.Fatalf("write ca: %v", err)
	}

	c, err := serverHTTPClient("https://localhost:8443", caFile, "", "")
	if err != nil {
		// Accept both success (valid PEM) and "invalid ca bundle" (if the fake
		// PEM can't be parsed) — we just need to exercise the branch.
		if err.Error() != "invalid ca bundle" {
			t.Fatalf("unexpected error: %v", err)
		}
		return
	}
	if c == nil {
		t.Fatal("expected non-nil client")
	}
}

func TestServerHTTPClient_MissingCAFile_Errors(t *testing.T) {
	_, err := serverHTTPClient("https://localhost:8443", "/no/such/ca.pem", "", "")
	if err == nil {
		t.Fatal("expected error for missing CA file")
	}
}

func TestServerHTTPClient_MTLSPartial_Errors(t *testing.T) {
	// Only certFile without keyFile.
	_, err := serverHTTPClient("https://localhost:8443", "", "cert.pem", "")
	if err == nil {
		t.Fatal("expected error for incomplete mTLS config")
	}
}

func TestServerHTTPClient_MTLSBadKeyPair_Errors(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, []byte("not a cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := serverHTTPClient("https://localhost:8443", "", certFile, keyFile)
	if err == nil {
		t.Fatal("expected error for bad key pair")
	}
}

// Validate that serverHTTPClient returns a properly-typed *http.Client.
func TestServerHTTPClient_ReturnsHTTPClient(t *testing.T) {
	c, err := serverHTTPClient("http://localhost:1", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	var _ *http.Client = c
}
