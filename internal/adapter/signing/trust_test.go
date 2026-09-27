package signing

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFingerprint_MatchesSHA256Hex(t *testing.T) {
	der := []byte("any public key encoding")
	sum := sha256.Sum256(der)
	want := hex.EncodeToString(sum[:])
	if got := Fingerprint(der); got != want {
		t.Errorf("Fingerprint = %q, want %q", got, want)
	}
}

// TestNewTrustedKey_NormalizesToPKIXAndClassifiesAlgorithm pins the
// constructor contract for Policy.TrustedKeys entries: the PEM is decoded,
// normalized to PKIX DER, fingerprinted, and the key algorithm is classified.
func TestNewTrustedKey_NormalizesToPKIXAndClassifiesAlgorithm(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("marshal PKIX: %v", err)
	}
	pemData := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})

	key, err := NewTrustedKey(pemData)
	if err != nil {
		t.Fatalf("NewTrustedKey: %v", err)
	}
	if key.Algorithm != "ed25519" {
		t.Errorf("Algorithm = %q, want ed25519", key.Algorithm)
	}
	if key.Fingerprint != Fingerprint(der) {
		t.Errorf("Fingerprint = %q, want %q", key.Fingerprint, Fingerprint(der))
	}
	if !bytes.Equal(key.RawKey, der) {
		t.Error("RawKey is not the PKIX DER encoding")
	}
}

func TestNewTrustedKey_RejectsInvalidPEM(t *testing.T) {
	if _, err := NewTrustedKey([]byte("not a pem")); err == nil {
		t.Error("NewTrustedKey on invalid PEM = nil error, want error")
	}
}

// TestPublicKeyAlgorithm_ClassifiesCurveVariants pins the algorithm labels
// used by lockfiles and verify-time matching, including the curve variants
// and the unknown fallback.
func TestPublicKeyAlgorithm_ClassifiesCurveVariants(t *testing.T) {
	ecKey := func(curve elliptic.Curve) *ecdsa.PublicKey {
		key, err := ecdsa.GenerateKey(curve, rand.Reader)
		if err != nil {
			t.Fatalf("generate ecdsa key: %v", err)
		}
		return &key.PublicKey
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	cases := []struct {
		name string
		pub  any
		want string
	}{
		{"ed25519", ed25519.PublicKey{}, "ed25519"},
		{"ecdsa-p256", ecKey(elliptic.P256()), "ecdsa-p256"},
		{"ecdsa-p384", ecKey(elliptic.P384()), "ecdsa-p384"},
		{"ecdsa-p521", ecKey(elliptic.P521()), "ecdsa-p521"},
		{"ecdsa-other-curve", &ecdsa.PublicKey{Curve: &elliptic.CurveParams{Name: "custom"}}, "ecdsa"},
		{"rsa", &rsaKey.PublicKey, "rsa"},
		{"unknown", struct{}{}, "unknown"},
	}
	for _, tc := range cases {
		if got := publicKeyAlgorithm(tc.pub); got != tc.want {
			t.Errorf("publicKeyAlgorithm(%s) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestTrustedMaterial_MkdirFailure verifies the cache-priming error path of
// trustedMaterial without touching the network: an unwritable CRITERIA_HOME
// must surface as a mkdir cache error.
func TestTrustedMaterial_MkdirFailure(t *testing.T) {
	if trustedMaterialOverride != nil {
		t.Skip("override active; error path not reachable")
	}
	tmp := t.TempDir()
	blocker := filepath.Join(tmp, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write blocker file: %v", err)
	}
	t.Setenv("CRITERIA_HOME", filepath.Join(blocker, "home"))

	_, err := trustedMaterial(context.Background())
	if err == nil {
		t.Fatal("trustedMaterial with an unwritable CRITERIA_HOME = nil error, want mkdir failure")
	}
	if !strings.Contains(err.Error(), "mkdir cache") {
		t.Errorf("error = %v, want it to mention mkdir cache", err)
	}
}
