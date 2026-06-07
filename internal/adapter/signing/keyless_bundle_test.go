package signing

import (
	"crypto/sha256"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/testing/ca"
)

const (
	bundleTestIssuer  = "https://token.actions.githubusercontent.com"
	bundleTestSubject = "https://github.com/brokenbots/criteria/.github/workflows/publish.yml@refs/tags/v1.0.0"
)

// TestVerifyBundleEntity_OfflineRoundTrip verifies a keyless bundle end-to-end
// against an in-memory Sigstore (Fulcio + Rekor + TSA), offline. It proves the
// WS48 verification path: bundle verify + issuer/subject identity extraction.
func TestVerifyBundleEntity_OfflineRoundTrip(t *testing.T) {
	sv, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatal(err)
	}
	oldOverride := trustedMaterialOverride
	trustedMaterialOverride = sv
	t.Cleanup(func() { trustedMaterialOverride = oldOverride })

	payload := []byte(`{"critical":{"identity":{"docker-reference":"ghcr.io/test"}}}`)
	entity, err := sv.Sign(bundleTestSubject, bundleTestIssuer, payload)
	if err != nil {
		t.Fatalf("virtual sigstore sign: %v", err)
	}

	sum := sha256.Sum256(payload)
	policy := &Policy{
		Mode:            ModeStrict,
		TrustedIssuers:  []string{bundleTestIssuer},
		SubjectPatterns: []string{"*"},
	}

	id, err := verifyBundleEntity(t.Context(), entity, sum[:], policy)
	if err != nil {
		t.Fatalf("verifyBundleEntity: %v", err)
	}
	if id == nil || id.Keyless == nil {
		t.Fatal("expected a keyless identity")
	}
	if id.Keyless.Issuer != bundleTestIssuer {
		t.Errorf("issuer = %q, want %q", id.Keyless.Issuer, bundleTestIssuer)
	}
	if id.Keyless.Subject != bundleTestSubject {
		t.Errorf("subject = %q, want %q", id.Keyless.Subject, bundleTestSubject)
	}
}

// TestVerifyBundleEntity_ForeignSubjectRejected proves identity enforcement:
// a verified bundle whose subject is not allowed by the policy is rejected.
func TestVerifyBundleEntity_ForeignSubjectRejected(t *testing.T) {
	sv, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatal(err)
	}
	oldOverride := trustedMaterialOverride
	trustedMaterialOverride = sv
	t.Cleanup(func() { trustedMaterialOverride = oldOverride })

	payload := []byte("payload")
	entity, err := sv.Sign(bundleTestSubject, bundleTestIssuer, payload)
	if err != nil {
		t.Fatalf("virtual sigstore sign: %v", err)
	}

	sum := sha256.Sum256(payload)
	policy := &Policy{
		Mode:            ModeStrict,
		TrustedIssuers:  []string{bundleTestIssuer},
		SubjectPatterns: []string{"https://github.com/other/repo/*"},
	}

	if _, err := verifyBundleEntity(t.Context(), entity, sum[:], policy); err == nil {
		t.Fatal("expected a foreign subject to be rejected")
	}
}
