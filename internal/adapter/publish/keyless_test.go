package publish

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"testing"
	"time"

	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
	sigsignature "github.com/sigstore/sigstore/pkg/signature"

	"github.com/brokenbots/criteria/internal/adapter/oci"
)

// fulcioOIDIssuer is the Fulcio X.509v3 extension OID that carries the OIDC
// issuer URL (1.3.6.1.4.1.57264.1.1).
var fulcioOIDIssuer = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 1}

const (
	testIssuer  = "https://token.actions.githubusercontent.com"
	testSubject = "https://github.com/brokenbots/criteria/.github/workflows/publish.yml@refs/tags/v1.0.0"
)

// mockFulcio is a test CA that mimics Fulcio's /api/v2/signingCert endpoint: it
// issues a short-lived code-signing leaf certificate over the requester's
// public key, signed by an in-memory CA, with the issuer + SAN identity a real
// Fulcio certificate carries.
type mockFulcio struct {
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey
	server *httptest.Server
}

func newMockFulcio(t *testing.T) *mockFulcio {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "mock-fulcio-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, caKey.Public(), caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}

	mf := &mockFulcio{caCert: caCert, caKey: caKey}
	mf.server = httptest.NewServer(http.HandlerFunc(mf.handleSigningCert))
	t.Cleanup(mf.server.Close)
	return mf
}

func (m *mockFulcio) handleSigningCert(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PublicKeyRequest struct {
			PublicKey struct {
				Content string `json:"content"`
			} `json:"publicKey"`
		} `json:"publicKeyRequest"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	block, _ := pem.Decode([]byte(req.PublicKeyRequest.PublicKey.Content))
	if block == nil {
		http.Error(w, "bad public key PEM", http.StatusBadRequest)
		return
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	sanURI, _ := neturl.Parse(testSubject)
	leafTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(10 * time.Minute),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		BasicConstraintsValid: true,
		URIs:                  []*neturl.URL{sanURI},
		ExtraExtensions: []pkix.Extension{{
			Id:    fulcioOIDIssuer,
			Value: []byte(testIssuer),
		}},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, m.caCert, pub, m.caKey)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	leafPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: m.caCert.Raw})

	resp := map[string]any{
		"signedCertificateEmbeddedSct": map[string]any{
			"chain": map[string]any{
				"certificates": []string{string(leafPEM), string(caPEM)},
			},
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// fakeIDToken builds an unsigned JWT whose payload carries a `sub`; Fulcio
// (the real one) verifies the token, but the mock only needs a parseable
// subject for the proof-of-possession step in sign.Fulcio.GetCertificate.
func fakeIDToken() string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"` + testSubject + `","iss":"` + testIssuer + `"}`))
	return header + "." + payload + "."
}

// TestKeylessSigner_RoundTrip proves NewKeylessSigner produces exactly what the
// verifier's keyless-legacy path (signing.verifyKeylessLegacy) accepts: a Fulcio
// leaf certificate that (a) chains to the CA, (b) certifies the key that signed
// the simple-signing payload, and (c) carries the OIDC issuer + subject the
// identity check reads. The three assertions below mirror, one-for-one, the
// checks verifyKeylessLegacy performs.
func TestKeylessSigner_RoundTrip(t *testing.T) {
	mf := newMockFulcio(t)

	signer, err := NewKeylessSigner(context.Background(), KeylessOptions{
		IDToken:   fakeIDToken(),
		FulcioURL: mf.server.URL,
	})
	if err != nil {
		t.Fatalf("NewKeylessSigner: %v", err)
	}

	ref := oci.Reference{Registry: "ghcr.io", Repo: "brokenbots/test", Tag: "v1.0.0"}
	payload := simpleSigningPayload(ref, "sha256:"+
		"abcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabca")

	// No RekorURL is configured, so this offline round-trip produces a bundle
	// without a transparency-log entry; the cert + signature assertions below
	// still hold. Full bundle verification is exercised in TestVerifyBundle_*
	// (offline VirtualSigstore) and the CI keyless integration job.
	res, err := signer.Sign(payload)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	sig := res.Signature
	if res.ChainPEM != "" {
		t.Fatalf("keyless signer should not emit a chain PEM; got %q", res.ChainPEM)
	}
	if res.CertPEM == "" {
		t.Fatal("keyless signer returned an empty certificate")
	}
	if len(res.Bundle) == 0 {
		t.Fatal("keyless signer returned an empty bundle")
	}

	block, _ := pem.Decode([]byte(res.CertPEM))
	if block == nil {
		t.Fatal("certificate is not valid PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}

	// (a) Leaf chains to the issuing CA — mirrors verify.VerifyLeafCertificate.
	roots := x509.NewCertPool()
	roots.AddCert(mf.caCert)
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
	}); err != nil {
		t.Fatalf("leaf certificate does not chain to CA: %v", err)
	}

	// (b) The signature verifies under the certified public key — mirrors
	// verifySignatureWithCert (ECDSA P-256 / SHA-256 over the payload).
	verifier, err := sigsignature.LoadVerifier(cert.PublicKey, crypto.SHA256)
	if err != nil {
		t.Fatalf("load verifier: %v", err)
	}
	if err := verifier.VerifySignature(bytes.NewReader(sig), bytes.NewReader(payload)); err != nil {
		t.Fatalf("signature does not verify under certificate key: %v", err)
	}

	// A tampered payload must NOT verify (guards against a no-op signer).
	if err := verifier.VerifySignature(bytes.NewReader(sig), bytes.NewReader([]byte("other"))); err == nil {
		t.Fatal("signature verified over the wrong payload")
	}

	// (c) Issuer + subject are recoverable — mirrors identityFromCert.
	summary, err := certificate.SummarizeCertificate(cert)
	if err != nil {
		t.Fatalf("summarize certificate: %v", err)
	}
	if summary.Issuer != testIssuer {
		t.Errorf("issuer = %q, want %q", summary.Issuer, testIssuer)
	}
	if summary.SubjectAlternativeName != testSubject {
		t.Errorf("subject = %q, want %q", summary.SubjectAlternativeName, testSubject)
	}
}

// TestKeylessSigner_RequiresToken verifies the signer refuses to proceed
// without an identity token rather than producing an unsigned/garbage result.
func TestKeylessSigner_RequiresToken(t *testing.T) {
	_, err := NewKeylessSigner(context.Background(), KeylessOptions{})
	if err == nil {
		t.Fatal("expected error when IDToken is empty")
	}
}
