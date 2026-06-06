package cli

import (
	"testing"

	"github.com/brokenbots/criteria/internal/adapter/signing"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

func TestPolicyForPin_KeyRestrictsToPinnedFingerprint(t *testing.T) {
	base := signing.Policy{
		Mode: signing.ModeStrict,
		TrustedKeys: []signing.KeyIdentity{
			{Algorithm: "ed25519", Fingerprint: "aaa", RawKey: []byte("a")},
			{Algorithm: "ed25519", Fingerprint: "bbb", RawKey: []byte("b")},
		},
	}
	pin := &lockfile.LockedSignature{Key: &lockfile.LockedKey{Algorithm: "ed25519", Fingerprint: "bbb"}}

	got := policyForPin(base, pin)
	if len(got.TrustedKeys) != 1 || got.TrustedKeys[0].Fingerprint != "bbb" {
		t.Fatalf("expected only the pinned key bbb, got %+v", got.TrustedKeys)
	}
	// Base must be untouched.
	if len(base.TrustedKeys) != 2 {
		t.Errorf("base policy mutated: %+v", base.TrustedKeys)
	}
}

func TestPolicyForPin_KeyNoMatchFailsClosed(t *testing.T) {
	base := signing.Policy{
		Mode:        signing.ModeStrict,
		TrustedKeys: []signing.KeyIdentity{{Fingerprint: "aaa", RawKey: []byte("a")}},
	}
	pin := &lockfile.LockedSignature{Key: &lockfile.LockedKey{Fingerprint: "zzz"}}

	got := policyForPin(base, pin)
	if len(got.TrustedKeys) != 0 {
		t.Fatalf("expected no trusted keys when pin has no match, got %+v", got.TrustedKeys)
	}
	// With no keys the policy is keyless; a key-signed artifact cannot verify.
	if !got.IsKeyless() {
		t.Error("expected policy to be keyless when no key matches the pin")
	}
}

func TestPolicyForPin_KeylessNarrowsIdentity(t *testing.T) {
	base := signing.Policy{Mode: signing.ModeStrict, SubjectPatterns: []string{"*"}}
	pin := &lockfile.LockedSignature{Keyless: &lockfile.LockedKeyless{
		Issuer:  "https://token.actions.githubusercontent.com",
		Subject: "https://github.com/acme/repo/.github/workflows/release.yml@refs/heads/main",
	}}

	got := policyForPin(base, pin)
	if len(got.TrustedIssuers) != 1 || got.TrustedIssuers[0] != pin.Keyless.Issuer {
		t.Errorf("issuer not narrowed: %+v", got.TrustedIssuers)
	}
	if len(got.SubjectPatterns) != 1 || got.SubjectPatterns[0] != pin.Keyless.Subject {
		t.Errorf("subject not narrowed: %+v", got.SubjectPatterns)
	}
}

func TestPolicyForPin_NilPinReturnsBase(t *testing.T) {
	base := signing.Policy{Mode: signing.ModeWarn}
	if got := policyForPin(base, nil); got.Mode != signing.ModeWarn {
		t.Errorf("expected base returned unchanged, got %+v", got)
	}
}

func TestAssertSignerMatchesPin(t *testing.T) {
	keyPin := &lockfile.LockedSignature{Key: &lockfile.LockedKey{Algorithm: "ed25519", Fingerprint: "fp1"}}
	keylessPin := &lockfile.LockedSignature{Keyless: &lockfile.LockedKeyless{Issuer: "iss", Subject: "sub"}}

	tests := []struct {
		name    string
		signer  *signing.SignerIdentity
		pin     *lockfile.LockedSignature
		wantErr bool
	}{
		{name: "nil signer skips", signer: nil, pin: keyPin},
		{name: "nil pin skips", signer: &signing.SignerIdentity{Key: &signing.KeyIdentity{Fingerprint: "fp1"}}, pin: nil},
		{name: "key matches", signer: &signing.SignerIdentity{Key: &signing.KeyIdentity{Fingerprint: "fp1"}}, pin: keyPin},
		{name: "key mismatch", signer: &signing.SignerIdentity{Key: &signing.KeyIdentity{Fingerprint: "other"}}, pin: keyPin, wantErr: true},
		{name: "key signer but keyless pin", signer: &signing.SignerIdentity{Key: &signing.KeyIdentity{Fingerprint: "fp1"}}, pin: keylessPin, wantErr: true},
		{name: "keyless matches", signer: &signing.SignerIdentity{Keyless: &signing.KeylessIdentity{Issuer: "iss", Subject: "sub"}}, pin: keylessPin},
		{name: "keyless subject mismatch", signer: &signing.SignerIdentity{Keyless: &signing.KeylessIdentity{Issuer: "iss", Subject: "evil"}}, pin: keylessPin, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := assertSignerMatchesPin("type.name", tt.signer, tt.pin)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}
