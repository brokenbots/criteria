package signing

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"math/big"
	neturl "net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapter/oci"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	ctypes "github.com/sigstore/cosign/v2/pkg/types"
)

func TestVerify_ModeOff(t *testing.T) {
	l, err := oci.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	id, err := Verify(t.Context(), l, "sha256:0000000000000000000000000000000000000000000000000000000000000000", Policy{Mode: ModeOff})
	if err != nil {
		t.Fatalf("ModeOff should never error: %v", err)
	}
	if id != nil {
		t.Fatal("ModeOff should return nil identity")
	}
}

func TestVerify_ModeStrict_NoSignatures(t *testing.T) {
	l, err := oci.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	_, err = Verify(t.Context(), l, "sha256:0000000000000000000000000000000000000000000000000000000000000000", Policy{Mode: ModeStrict})
	if err == nil {
		t.Fatal("expected error for strict mode with no signatures")
	}
}

func TestVerify_ModeWarn_NoSignatures(t *testing.T) {
	l, err := oci.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	id, err := Verify(t.Context(), l, "sha256:0000000000000000000000000000000000000000000000000000000000000000", Policy{Mode: ModeWarn})
	if err != nil {
		t.Fatalf("Warn mode should not return error: %v", err)
	}
	if id != nil {
		t.Fatal("Warn mode should return nil identity when no signatures match")
	}
}

func TestFindSignatures_SignatureManifest(t *testing.T) {
	root := t.TempDir()
	l, err := oci.Open(root)
	if err != nil {
		t.Fatal(err)
	}

	// Artifact manifest.
	artifact := ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Config: ocispec.Descriptor{
			MediaType: "application/vnd.criteria.adapter.config.v1+json",
			Digest:    digest.FromString("config"),
			Size:      6,
		},
	}
	artifactData, _ := json.Marshal(artifact)
	artifactDigest := digest.FromBytes(artifactData)
	if err := l.WriteBlob(bytes.NewReader(artifactData), artifactDigest); err != nil {
		t.Fatal(err)
	}

	// Simplesigning payload.
	payload := []byte(`{"critical":{"identity":{"docker-reference":"ghcr.io/test"},"image":{"docker-manifest-digest":"` + artifactDigest.String() + `"},"type":"cosign container image signature"},"optional":null}`)
	payloadDigest := digest.FromBytes(payload)
	if err := l.WriteBlob(bytes.NewReader(payload), payloadDigest); err != nil {
		t.Fatal(err)
	}

	// Signature manifest referring to artifact.
	sigManifest := ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Subject: &ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageManifest,
			Digest:    artifactDigest,
			Size:      int64(len(artifactData)),
		},
		Layers: []ocispec.Descriptor{
			{
				MediaType: ctypes.SimpleSigningMediaType,
				Digest:    payloadDigest,
				Size:      int64(len(payload)),
				Annotations: map[string]string{
					"dev.cosignproject.cosign/signature": base64.StdEncoding.EncodeToString([]byte("testsig")),
				},
			},
		},
		Annotations: map[string]string{
			"dev.sigstore.cosign/certificate": "-----BEGIN CERTIFICATE-----\nMIIBkTCCATegAwIBAgIQY\n-----END CERTIFICATE-----\n",
		},
	}
	sigData, _ := json.Marshal(sigManifest)
	sigDigest := digest.FromBytes(sigData)
	if err := l.WriteBlob(bytes.NewReader(sigData), sigDigest); err != nil {
		t.Fatal(err)
	}

	// Write index referencing both manifests.
	ix := ocispec.Index{
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{
			{MediaType: ocispec.MediaTypeImageManifest, Digest: artifactDigest, Size: int64(len(artifactData))},
			{MediaType: ocispec.MediaTypeImageManifest, Digest: sigDigest, Size: int64(len(sigData))},
		},
	}
	ixData, _ := json.Marshal(ix)
	if err := os.WriteFile(filepath.Join(root, "index.json"), ixData, 0o640); err != nil {
		t.Fatal(err)
	}

	sigs, err := findSignatures(l, artifactDigest)
	if err != nil {
		t.Fatalf("findSignatures error: %v", err)
	}
	if len(sigs) != 1 {
		t.Fatalf("expected 1 signature, got %d", len(sigs))
	}
	if sigs[0].signatureB64 == "" {
		t.Fatal("expected signatureB64 to be set")
	}
	if sigs[0].certPEM == "" {
		t.Fatal("expected certPEM to be set")
	}
}

func TestFindSignatures_EmbeddedLayer(t *testing.T) {
	root := t.TempDir()
	l, err := oci.Open(root)
	if err != nil {
		t.Fatal(err)
	}

	// Artifact manifest with embedded signature layer.
	payload := []byte(`{"critical":{"identity":{"docker-reference":"ghcr.io/test"},"image":{"docker-manifest-digest":"sha256:aaaabbbb"},"type":"cosign container image signature"},"optional":null}`)
	payloadDigest := digest.FromBytes(payload)
	if err := l.WriteBlob(bytes.NewReader(payload), payloadDigest); err != nil {
		t.Fatal(err)
	}

	artifact := ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Layers: []ocispec.Descriptor{
			{
				MediaType: ctypes.SimpleSigningMediaType,
				Digest:    payloadDigest,
				Size:      int64(len(payload)),
				Annotations: map[string]string{
					oci.AnnotationTitle: "signatures/cosign.sig",
				},
			},
		},
	}
	artifactData, _ := json.Marshal(artifact)
	artifactDigest := digest.FromBytes(artifactData)
	if err := l.WriteBlob(bytes.NewReader(artifactData), artifactDigest); err != nil {
		t.Fatal(err)
	}

	ix := ocispec.Index{
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{
			{MediaType: ocispec.MediaTypeImageManifest, Digest: artifactDigest, Size: int64(len(artifactData))},
		},
	}
	ixData, _ := json.Marshal(ix)
	if err := os.WriteFile(filepath.Join(root, "index.json"), ixData, 0o640); err != nil {
		t.Fatal(err)
	}

	sigs, err := findSignatures(l, artifactDigest)
	if err != nil {
		t.Fatalf("findSignatures error: %v", err)
	}
	if len(sigs) != 1 {
		t.Fatalf("expected 1 embedded signature, got %d", len(sigs))
	}
}

func TestIdentityFromCert(t *testing.T) {
	cert := makeTestCert(t, "https://token.actions.githubusercontent.com", "https://github.com/brokenbots/criteria/.github/workflows/publish.yml@refs/tags/v1.0.0")

	policy := Policy{
		TrustedIssuers:  []string{"https://token.actions.githubusercontent.com"},
		SubjectPatterns: []string{"https://github.com/brokenbots/*"},
	}

	id, err := identityFromCert(cert, &policy)
	if err != nil {
		t.Fatalf("identityFromCert: %v", err)
	}
	if id.Keyless == nil {
		t.Fatal("expected keyless identity")
	}
	if id.Keyless.Issuer != "https://token.actions.githubusercontent.com" {
		t.Errorf("issuer = %q, want GitHub Actions", id.Keyless.Issuer)
	}
	if id.Keyless.Subject == "" {
		t.Error("expected non-empty subject")
	}
}

func TestIdentityFromCert_UntrustedIssuer(t *testing.T) {
	cert := makeTestCert(t, "https://evil.com", "subject")

	policy := Policy{
		TrustedIssuers: []string{"https://token.actions.githubusercontent.com"},
	}

	_, err := identityFromCert(cert, &policy)
	if err == nil {
		t.Fatal("expected error for untrusted issuer")
	}
}

func TestIdentityFromCert_SubjectMismatch(t *testing.T) {
	cert := makeTestCert(t, "https://token.actions.githubusercontent.com", "https://github.com/other/repo")

	policy := Policy{
		TrustedIssuers:  []string{"https://token.actions.githubusercontent.com"},
		SubjectPatterns: []string{"https://github.com/brokenbots/*"},
	}

	_, err := identityFromCert(cert, &policy)
	if err == nil {
		t.Fatal("expected error for subject mismatch")
	}
}

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pat, s string
		want   bool
	}{
		{"*", "anything", true},
		{"", "anything", true},
		{"prefix*", "prefix_suffix", true},
		{"*suffix", "prefix_suffix", true},
		{"exact", "exact", true},
		{"exact", "other", false},
		{"prefix*", "other", false},
		{"*suffix", "other", false},
	}
	for _, c := range cases {
		got := matchGlob(c.pat, c.s)
		if got != c.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", c.pat, c.s, got, c.want)
		}
	}
}

func TestVerifyKeyBased(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub := priv.Public().(ed25519.PublicKey)
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}

	payload := []byte("test payload")
	sig := ed25519.Sign(priv, payload)

	rec := signatureRecord{
		payload:      payload,
		signatureB64: base64.StdEncoding.EncodeToString(sig),
	}

	policy := Policy{
		TrustedKeys: []KeyIdentity{
			{
				Algorithm:   "ed25519",
				Fingerprint: fingerprintBytes(pubDER),
				RawKey:      pubDER,
			},
		},
	}

	id, err := verifyKeyBased(&rec, &policy)
	if err != nil {
		t.Fatalf("verifyKeyBased: %v", err)
	}
	if id.Key == nil {
		t.Fatal("expected key identity")
	}
	if id.Key.Fingerprint != fingerprintBytes(pubDER) {
		t.Errorf("fingerprint mismatch")
	}
}

func TestVerifyKeyBased_WrongKey(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, wrongPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub := priv.Public().(ed25519.PublicKey)
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}

	payload := []byte("test payload")
	sig := ed25519.Sign(wrongPriv, payload)

	rec := signatureRecord{
		payload:      payload,
		signatureB64: base64.StdEncoding.EncodeToString(sig),
	}

	policy := Policy{
		TrustedKeys: []KeyIdentity{
			{
				Algorithm:   "ed25519",
				Fingerprint: fingerprintBytes(pubDER),
				RawKey:      pubDER,
			},
		},
	}

	_, err = verifyKeyBased(&rec, &policy)
	if err == nil {
		t.Fatal("expected error when signature does not verify")
	}
}

func TestFingerprintBytes(t *testing.T) {
	data := []byte("hello")
	fp := fingerprintBytes(data)
	if fp == "" {
		t.Fatal("fingerprint should not be empty")
	}
	// SHA-256 of "hello" is known.
	want := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if fp != want {
		t.Errorf("fingerprint = %q, want %q", fp, want)
	}
}

func TestLockfileFields(t *testing.T) {
	id := &SignerIdentity{
		Keyless: &KeylessIdentity{
			Issuer:  "https://github.com",
			Subject: "sub",
		},
	}
	m := LockfileFields(id)
	if m == nil {
		t.Fatal("expected non-nil map")
	}
	if _, ok := m["keyless"]; !ok {
		t.Fatal("expected keyless key")
	}

	id2 := &SignerIdentity{Key: &KeyIdentity{Algorithm: "ed25519", Fingerprint: "abc"}}
	m2 := LockfileFields(id2)
	if _, ok := m2["key"]; !ok {
		t.Fatal("expected key field")
	}

	if LockfileFields(nil) != nil {
		t.Error("nil identity should return nil map")
	}
}

// makeTestCert creates a self-signed certificate with the given issuer and
// subject URI SAN. It is suitable for testing identityFromCert only; real
// Fulcio certificate verification requires a trusted chain.
func makeTestCert(t *testing.T, issuer, subject string) *x509.Certificate {
	t.Helper()

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "test",
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		BasicConstraintsValid: true,
		URIs:                  []*neturl.URL{{Scheme: "https", Host: "example.com", Path: "/"}},
	}

	// We need to add the Fulcio-style issuer extension and set the SAN.
	// sigstore-go's certificate.SummarizeCertificate reads:
	//   - Issuer from the custom OID 1.3.6.1.4.1.57264.1.1
	//   - SubjectAlternativeName from the cert's URIs or email or DNS names
	//
	// We will construct the cert manually with the required extensions.
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	// Add issuer extension.
	issuerExt := pkix.Extension{
		Id:    asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 1},
		Value: []byte(issuer),
	}
	template.ExtraExtensions = append(template.ExtraExtensions, issuerExt)

	// Set URI SAN.
	uri, err := neturl.Parse(subject)
	if err != nil {
		t.Fatal(err)
	}
	template.URIs = []*neturl.URL{uri}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, priv.Public(), priv)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}
