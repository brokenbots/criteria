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
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brokenbots/criteria/internal/adapter/oci"
	"github.com/brokenbots/criteria/internal/testutil"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	ctypes "github.com/sigstore/cosign/v3/pkg/types"
	"github.com/sigstore/sigstore-go/pkg/util"
)

// defaultPolicy returns a well-formed keyless policy (the PolicyFor default
// shape) suitable for tests that exercise behavior beyond policy validation.
func defaultPolicy(mode VerificationMode) Policy {
	return Policy{
		Mode:            mode,
		TrustedIssuers:  append([]string(nil), DefaultTrustedIssuers...),
		SubjectPatterns: []string{"*"},
	}
}

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

	_, err = Verify(t.Context(), l, "sha256:0000000000000000000000000000000000000000000000000000000000000000", defaultPolicy(ModeStrict))
	if err == nil {
		t.Fatal("expected error for strict mode with no signatures")
	}
}

func TestVerify_ModeWarn_NoSignatures(t *testing.T) {
	l, err := oci.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	oldLog := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(oldLog)

	id, err := Verify(t.Context(), l, "sha256:0000000000000000000000000000000000000000000000000000000000000000", defaultPolicy(ModeWarn))
	if err != nil {
		t.Fatalf("Warn mode should not return error: %v", err)
	}
	if id != nil {
		t.Fatal("Warn mode should return nil identity when no signatures match")
	}

	out := buf.String()
	if out == "" {
		t.Fatal("expected warning log in ModeWarn")
	}
	if !bytes.Contains(buf.Bytes(), []byte("signature verification warning")) {
		t.Fatalf("expected warning log to contain 'signature verification warning', got: %s", out)
	}
}

// TestVerifyKeylessLegacy_FailsClosed asserts the no-bundle keyless path now
// fails closed with an actionable message (WS48): without a transparency-log
// proof, a keyless signature cannot be verified after the Fulcio certificate
// expires, so it must not be accepted.
func TestVerifyKeylessLegacy_FailsClosed(t *testing.T) {
	rec := &signatureRecord{
		payload:      []byte("test payload"),
		signatureB64: base64.StdEncoding.EncodeToString([]byte("sig")),
		certPEM:      "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n",
	}
	_, err := verifyKeylessLegacy(rec)
	if err == nil {
		t.Fatal("expected fail-closed error for a keyless signature with no bundle")
	}
	if !strings.Contains(err.Error(), "transparency-log proof") {
		t.Errorf("error %q does not mention the missing transparency-log proof", err)
	}
}

// TestVerifyKeyless_NoBundleFailsClosed confirms the dispatch: a keyless record
// without a bundle reaches the fail-closed legacy path.
func TestVerifyKeyless_NoBundleFailsClosed(t *testing.T) {
	rec := &signatureRecord{payload: []byte("p"), certPEM: "x"}
	_, err := verifyKeyless(t.Context(), rec, &Policy{Mode: ModeStrict})
	if err == nil || !strings.Contains(err.Error(), "transparency-log proof") {
		t.Fatalf("expected fail-closed error, got %v", err)
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
		{"", "anything", false},
		{"", "", false},
		{"prefix*", "prefix_suffix", true},
		{"*suffix", "prefix_suffix", true},
		{"exact", "exact", true},
		{"exact", "other", false},
		{"prefix*", "other", false},
		{"*suffix", "other", false},
		// Star in the middle: glob semantics, not a literal (path.Match
		// family; '*' also crosses '/' so URL subjects keep their shape).
		{"https://repo-*-prod.example", "https://repo-123-prod.example", true},
		{"https://repo-*-prod.example", "https://repo-123-staging.example", false},
		{"https://github.com/org/*", "https://github.com/org/repo/deep/path", true},
		{"https://github.com/org/*", "https://github.com/other/repo", false},
		{"a*b*c", "a-x-b-y-c", true},
		{"a*b*c", "a-x-b-y-b-c", true},
		{"a*b*c", "a-x-b-c-d", false},
		{"*prod*", "repo-prod-v1", true},
		{"*PROD*", "repo-prod-v1", false},
		{"re?o", "repo", true},
		{"re?o", "rexxo", false},
		// path.Match metacharacters beyond * and ? stay literal here.
		{"[abc]", "[abc]", true},
		{"re[a]o", "repo", false},
		{"pre*fix", "premid*dlefix", true},
		{"pre*fix", "prexfix", true},
	}
	for _, c := range cases {
		got := globMatch(c.pat, c.s)
		if got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.pat, c.s, got, c.want)
		}
	}
}

func TestValidateKeylessPolicy(t *testing.T) {
	valid := func() Policy {
		return Policy{
			Mode:            ModeStrict,
			TrustedIssuers:  []string{DefaultTrustedIssuers[0]},
			SubjectPatterns: []string{"*"},
		}
	}

	t.Run("default-shaped policy is valid", func(t *testing.T) {
		p := valid()
		if err := p.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("empty subject pattern entry rejected", func(t *testing.T) {
		p := valid()
		p.SubjectPatterns = []string{"https://github.com/brokenbots/*", ""}
		err := p.Validate()
		if err == nil {
			t.Fatal("expected error for empty subject pattern")
		}
		if !strings.Contains(err.Error(), "empty subject pattern") {
			t.Fatalf("expected 'empty subject pattern' diagnostic, got: %v", err)
		}
	})

	t.Run("empty trusted issuer entry rejected", func(t *testing.T) {
		p := valid()
		p.TrustedIssuers = []string{"", "https://token.actions.githubusercontent.com"}
		err := p.Validate()
		if err == nil {
			t.Fatal("expected error for empty trusted issuer")
		}
	})

	t.Run("no trusted issuers rejected", func(t *testing.T) {
		p := valid()
		p.TrustedIssuers = nil
		err := p.Validate()
		if err == nil {
			t.Fatal("expected error for missing trusted issuers")
		}
	})

	t.Run("no subject patterns rejected", func(t *testing.T) {
		p := valid()
		p.SubjectPatterns = nil
		err := p.Validate()
		if err == nil {
			t.Fatal("expected error for missing subject patterns")
		}
	})

	t.Run("key mode does not require issuer surface", func(t *testing.T) {
		p := Policy{
			Mode:        ModeStrict,
			TrustedKeys: []KeyIdentity{{Algorithm: "ed25519", Fingerprint: "deadbeef"}},
		}
		if err := p.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("warn mode validated like enforce", func(t *testing.T) {
		p := valid()
		p.Mode = ModeWarn
		p.SubjectPatterns = []string{" "}
		if err := p.Validate(); err == nil {
			t.Fatal("expected error for whitespace-only subject pattern in warn mode")
		}
	})

	t.Run("mode off skips identity surface checks", func(t *testing.T) {
		p := valid()
		p.Mode = ModeOff
		p.TrustedIssuers = nil
		p.SubjectPatterns = nil
		if err := p.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})
}

// TestIdentityFromCert_EmptySubjectPatternInert asserts the fail-closed
// matcher invariant: an empty pattern entry matches nothing — it can never
// act as an allow-all — so a policy that relies on one cannot accept a
// subject the real patterns don't cover. Policy.Validate rejects the shape
// outright at build/verify time.
func TestIdentityFromCert_EmptySubjectPatternInert(t *testing.T) {
	cert := makeTestCert(t, "https://token.actions.githubusercontent.com", "https://github.com/other/repo")

	policy := Policy{
		TrustedIssuers:  []string{"https://token.actions.githubusercontent.com"},
		SubjectPatterns: []string{"", "https://github.com/brokenbots/*"},
	}
	if _, err := identityFromCert(cert, &policy); err == nil {
		t.Fatal("expected identityFromCert to reject: empty entry matches nothing, remaining pattern does not cover subject")
	}

	// Policy.Validate is the loud gate for the same shape.
	if err := policy.Validate(); err == nil {
		t.Fatal("expected Validate to reject empty subject pattern entry")
	}

	// And Verify surfaces the malformed shape with a diagnostic before any
	// signature is examined.
	l, err := oci.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, verr := Verify(t.Context(), l, "sha256:0000000000000000000000000000000000000000000000000000000000000000", policy); verr == nil || !strings.Contains(verr.Error(), "invalid verification policy") {
		t.Fatalf("expected Verify to fail on invalid policy, got: %v", verr)
	}
}

func TestIdentityFromCert_IssuerWildcard(t *testing.T) {
	cert := makeTestCert(t, "https://token.actions.githubusercontent.com", "https://github.com/brokenbots/criteria")

	policy := Policy{
		TrustedIssuers:  []string{WildcardIssuer},
		SubjectPatterns: []string{"*"},
	}

	id, err := identityFromCert(cert, &policy)
	if err != nil {
		t.Fatalf("identityFromCert with wildcard issuer: %v", err)
	}
	if id.Keyless == nil {
		t.Fatal("expected keyless identity")
	}
	if id.Keyless.Issuer != "https://token.actions.githubusercontent.com" {
		t.Fatalf("issuer = %q, want the certificate issuer", id.Keyless.Issuer)
	}
}

func TestIdentityFromCert_NoSubjectPatternsFailsClosed(t *testing.T) {
	cert := makeTestCert(t, "https://token.actions.githubusercontent.com", "https://github.com/brokenbots/criteria")

	policy := Policy{
		TrustedIssuers: []string{"https://token.actions.githubusercontent.com"},
	}

	_, err := identityFromCert(cert, &policy)
	if err == nil {
		t.Fatal("expected error: policy without subject patterns must reject, not allow-all")
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

// TestNewTUFFetcher_DedicatedTransportAndUserAgent pins the one-shot TUF
// fetcher contract trustedMaterial relies on: a dedicated transport (not the
// shared http.DefaultClient pools that leak idle HTTP/2 readers) carrying the
// criteria user agent on every request it makes.
func TestNewTUFFetcher_DedicatedTransportAndUserAgent(t *testing.T) {
	testutil.RequireLoopbackTCP(t)
	var gotUA string
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	fetch, transport, err := newTUFFetcher()
	if err != nil {
		t.Fatalf("newTUFFetcher: %v", err)
	}
	defer transport.CloseIdleConnections()

	if !transport.ForceAttemptHTTP2 || transport.TLSHandshakeTimeout == 0 || transport.IdleConnTimeout == 0 {
		t.Errorf("fetcher transport = %+v, want the dedicated hardened transport", transport)
	}
	if transport.Proxy == nil {
		t.Error("fetcher transport has no proxy function, want http.ProxyFromEnvironment")
	}

	// go-tuf's default retry policy is a single attempt, so the 404 stub
	// surfaces as an immediate error rather than a retry loop.
	_, err = fetch.DownloadFile(srv.URL+"/trusted_root.json", 1<<20, time.Second)
	if err == nil {
		t.Fatal("DownloadFile against a 404 stub returned nil error, want failure")
	}
	if gotPath != "/trusted_root.json" {
		t.Errorf("fetcher requested %q, want /trusted_root.json", gotPath)
	}
	if gotUA != util.ConstructUserAgent() {
		t.Errorf("fetcher sent User-Agent %q, want the criteria user agent %q", gotUA, util.ConstructUserAgent())
	}
}
