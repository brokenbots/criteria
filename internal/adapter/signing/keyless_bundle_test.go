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

// TestVerifyBundleEntity_PinNarrowedMatrix drives the KB-144 tightened matrix
// through the real verifier with a lockfile-style narrowed policy (what
// cli.policyForPin produces): glob subjects, wildcard issuers, and malformed
// surfaces all behave fail-closed.
func TestVerifyBundleEntity_PinNarrowedMatrix(t *testing.T) {
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

	subjectGlob := "https://github.com/brokenbots/criteria/*"
	cases := []struct {
		name    string
		issuers []string
		pats    []string
		wantErr bool
	}{
		{name: "pinned repo glob matches", issuers: []string{bundleTestIssuer}, pats: []string{subjectGlob}},
		{name: "pinned repo glob rejects foreign repo", issuers: []string{bundleTestIssuer}, pats: []string{"https://github.com/other/*"}, wantErr: true},
		{name: "exact subject still matches", issuers: []string{bundleTestIssuer}, pats: []string{bundleTestSubject}},
		{name: "wildcard issuer accepts", issuers: []string{WildcardIssuer}, pats: []string{subjectGlob}},
		{name: "wrong issuer rejected with repo glob", issuers: []string{"https://gitlab.example.com"}, pats: []string{subjectGlob}, wantErr: true},
		// A wildcard is explicitly required for allow-any: an empty entry
		// matches nothing and any policy carrying one is malformed.
		{name: "empty subject entry rejects valid subject", issuers: []string{bundleTestIssuer}, pats: []string{"", subjectGlob}, wantErr: true},
		{name: "empty issuer entry rejects", issuers: []string{"", bundleTestIssuer}, pats: []string{subjectGlob}, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			policy := &Policy{Mode: ModeStrict, TrustedIssuers: c.issuers, SubjectPatterns: c.pats}
			if verr := policy.Validate(); verr != nil {
				if !c.wantErr {
					t.Fatalf("policy unexpectedly invalid: %v", verr)
				}
				return
			}
			id, err := verifyBundleEntity(t.Context(), entity, sum[:], policy)
			if c.wantErr && err == nil {
				t.Fatalf("expected rejection")
			}
			if !c.wantErr && err != nil {
				t.Fatalf("verifyBundleEntity: %v", err)
			}
			if !c.wantErr && id.Keyless.Subject != bundleTestSubject {
				t.Fatalf("subject = %q", id.Keyless.Subject)
			}
		})
	}
}
