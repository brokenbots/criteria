package cli

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/brokenbots/criteria/internal/adapter/signing"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

// pinTestLogger swaps the default logger for a sink capturing everything
// policyForPin emits, restoring the previous logger on cleanup.
func pinTestLogger(t *testing.T) *captureLog {
	logSink := &captureLog{}
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logSink, nil)))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })
	return logSink
}

func TestPolicyForPin_KeyRestrictsToPinnedFingerprint(t *testing.T) {
	base := signing.Policy{
		Mode: signing.ModeStrict,
		TrustedKeys: []signing.KeyIdentity{
			{Algorithm: "ed25519", Fingerprint: "aaa", RawKey: []byte("a")},
			{Algorithm: "ed25519", Fingerprint: "bbb", RawKey: []byte("b")},
		},
	}
	pin := &lockfile.LockedSignature{Key: &lockfile.LockedKey{Algorithm: "ed25519", Fingerprint: "bbb"}}

	got, err := policyForPin(&base, pin)
	if err != nil {
		t.Fatalf("policyForPin: %v", err)
	}
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

	got, err := policyForPin(&base, pin)
	if err != nil {
		t.Fatalf("policyForPin: %v", err)
	}
	if len(got.TrustedKeys) != 0 {
		t.Fatalf("expected no trusted keys when pin has no match, got %+v", got.TrustedKeys)
	}
	// With no keys the policy is keyless; a key-signed artifact cannot verify.
	if !got.IsKeyless() {
		t.Error("expected policy to be keyless when no key matches the pin")
	}
}

// TestPolicyForPin_KeyNoMatchLandsOnValidKeylessPolicy covers the tightened
// cross-branch: a key pin with no configured match leaves a policy that is
// keyless-shaped, and — because the base carries a valid keyless identity
// surface — the narrowed policy still validates cleanly.
func TestPolicyForPin_KeyNoMatchLandsOnValidKeylessPolicy(t *testing.T) {
	base := signing.Policy{
		Mode:            signing.ModeStrict,
		TrustedIssuers:  append([]string(nil), signing.DefaultTrustedIssuers...),
		SubjectPatterns: []string{"*"},
		TrustedKeys:     []signing.KeyIdentity{{Fingerprint: "aaa", RawKey: []byte("a")}},
	}
	pin := &lockfile.LockedSignature{Key: &lockfile.LockedKey{Fingerprint: "zzz"}}

	got, err := policyForPin(&base, pin)
	if err != nil {
		t.Fatalf("policyForPin: %v", err)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("narrowed keyless-shaped policy invalid: %v", err)
	}
}

// TestPolicyForPin_KeyNoMatchOnKeyOnlyBaseLeavesKeylessShape documents the
// narrowing/validation split: over a key-mode-only base (no keyless issuer or
// subject configured), a no-match key pin leaves a keyless-shaped policy whose
// identity surface is empty. policyForPin narrows but does not validate the key
// branch; the malformed surface is enforced by Verify's policy gate
// (signing.Validate is called at every verification and at policy build), so
// the artifact can never verify with it.
func TestPolicyForPin_KeyNoMatchOnKeyOnlyBaseLeavesKeylessShape(t *testing.T) {
	base := signing.Policy{
		Mode:        signing.ModeStrict,
		TrustedKeys: []signing.KeyIdentity{{Fingerprint: "aaa", RawKey: []byte("a")}},
	}
	pin := &lockfile.LockedSignature{Key: &lockfile.LockedKey{Fingerprint: "zzz"}}

	got, err := policyForPin(&base, pin)
	if err != nil {
		t.Fatalf("policyForPin: %v", err)
	}
	if !got.IsKeyless() {
		t.Fatalf("expected policy to be keyless-shaped, got %+v", got)
	}
	if len(got.TrustedIssuers) != 0 || len(got.SubjectPatterns) != 0 {
		t.Fatalf("expected empty keyless identity surface, got issuers=%v patterns=%v", got.TrustedIssuers, got.SubjectPatterns)
	}
	if err := got.Validate(); err == nil {
		t.Error("expected invalid keyless-shaped policy; Verify's policy gate is the enforcement point for this shape")
	}
}

func TestPolicyForPin_KeylessNarrowsIdentity(t *testing.T) {
	base := signing.Policy{Mode: signing.ModeStrict, SubjectPatterns: []string{"*"}}
	pin := &lockfile.LockedSignature{Keyless: &lockfile.LockedKeyless{
		Issuer:  "https://token.actions.githubusercontent.com",
		Subject: "https://github.com/acme/repo/.github/workflows/release.yml@refs/heads/main",
	}}

	got, err := policyForPin(&base, pin)
	if err != nil {
		t.Fatalf("policyForPin: %v", err)
	}
	if len(got.TrustedIssuers) != 1 || got.TrustedIssuers[0] != pin.Keyless.Issuer {
		t.Errorf("issuer not narrowed: %+v", got.TrustedIssuers)
	}
	if len(got.SubjectPatterns) != 1 || got.SubjectPatterns[0] != pin.Keyless.Subject {
		t.Errorf("subject not narrowed: %+v", got.SubjectPatterns)
	}
}

// TestPolicyForPin_KeylessPinRequiresIssuerAndSubject asserts the KB-144
// tightening: an empty-issuer (or empty-subject) keyless pin must fail
// validation — never leave the base issuer list (or subject patterns) in
// place, which would make the pin weaker than the lockfile implies.
func TestPolicyForPin_KeylessPinRequiresIssuerAndSubject(t *testing.T) {
	base := signing.Policy{Mode: signing.ModeStrict, SubjectPatterns: []string{"*"}}
	cases := []struct {
		name        string
		issuer      string
		subject     string
		errContains string
	}{
		{name: "empty issuer", issuer: "", subject: "subj", errContains: "issuer"},
		{name: "empty subject", issuer: "https://token.actions.githubusercontent.com", subject: "", errContains: "subject"},
		{name: "whitespace issuer", issuer: "  ", subject: "subj", errContains: "keyless pin is missing"},
		{name: "both empty", issuer: "", subject: "", errContains: "issuer and subject"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pin := &lockfile.LockedSignature{Keyless: &lockfile.LockedKeyless{Issuer: c.issuer, Subject: c.subject}}
			got, err := policyForPin(&base, pin)
			if err == nil {
				t.Fatalf("expected error, got policy %+v", got)
			}
			if !strings.Contains(err.Error(), c.errContains) {
				t.Errorf("error %q does not mention %q", err, c.errContains)
			}
		})
	}
}

// TestPolicyForPin_WildcardIssuerAllowedWithWarning asserts the explicit
// wildcard-issuer pin path: accepted (loud, deliberate relaxation), narrowed
// to the wildcard (never to the base defaults), and emitting the loud warning
// with the pinned identity and the concrete-issuer hint — and nothing about
// the subject, which is concrete here.
func TestPolicyForPin_WildcardIssuerAllowedWithWarning(t *testing.T) {
	base := signing.Policy{Mode: signing.ModeStrict, SubjectPatterns: []string{"*"}}
	pin := &lockfile.LockedSignature{Keyless: &lockfile.LockedKeyless{
		Issuer:  signing.WildcardIssuer,
		Subject: "https://github.com/acme/repo/*",
	}}

	logSink := pinTestLogger(t)
	got, err := policyForPin(&base, pin)
	if err != nil {
		t.Fatalf("wildcard issuer pin must be allowed, got: %v", err)
	}
	if len(got.TrustedIssuers) != 1 || got.TrustedIssuers[0] != signing.WildcardIssuer {
		t.Fatalf("issuer surface = %+v, want the wildcard", got.TrustedIssuers)
	}
	if len(got.SubjectPatterns) != 1 || got.SubjectPatterns[0] != "https://github.com/acme/repo/*" {
		t.Fatalf("subject surface = %+v, want the pinned glob", got.SubjectPatterns)
	}
	log := logSink.String()
	if !strings.Contains(log, "lockfile keyless pin trusts ANY OIDC issuer") {
		t.Errorf("expected a loud wildcard-issuer warning, log:\n%s", log)
	}
	if !strings.Contains(log, "pinned_issuer=*") {
		t.Errorf("warning must name the wildcard pinned issuer, log:\n%s", log)
	}
	if !strings.Contains(log, "prefer a concrete issuer URL") {
		t.Errorf("warning must carry the concrete-issuer hint, log:\n%s", log)
	}
	if strings.Contains(log, "trusts any workflow subject") {
		t.Errorf("warning must stay specific: no subject warning when only the issuer is wildcard, log:\n%s", log)
	}
}

// TestPolicyForPin_WildcardSubjectAllowedWithWarning mirrors the wildcard
// issuer case for the subject side: accepted, narrowed to the wildcard, and
// warned with the repo-scoped-glob hint — and no issuer warning when the
// issuer itself is concrete.
func TestPolicyForPin_WildcardSubjectAllowedWithWarning(t *testing.T) {
	base := signing.Policy{Mode: signing.ModeStrict}
	pin := &lockfile.LockedSignature{Keyless: &lockfile.LockedKeyless{
		Issuer:  "https://token.actions.githubusercontent.com",
		Subject: signing.WildcardIssuer,
	}}

	logSink := pinTestLogger(t)
	got, err := policyForPin(&base, pin)
	if err != nil {
		t.Fatalf("wildcard subject pin must be allowed, got: %v", err)
	}
	if len(got.TrustedIssuers) != 1 || got.TrustedIssuers[0] != "https://token.actions.githubusercontent.com" {
		t.Fatalf("issuer surface = %+v, want the pinned issuer", got.TrustedIssuers)
	}
	if len(got.SubjectPatterns) != 1 || got.SubjectPatterns[0] != signing.WildcardIssuer {
		t.Fatalf("subject surface = %+v, want the wildcard", got.SubjectPatterns)
	}
	log := logSink.String()
	if !strings.Contains(log, "lockfile keyless pin trusts any workflow subject") {
		t.Errorf("expected a loud wildcard-subject warning, log:\n%s", log)
	}
	if !strings.Contains(log, "names the adapter's repo") {
		t.Errorf("warning must carry the repo-scoped-glob hint, log:\n%s", log)
	}
	if strings.Contains(log, "trusts ANY OIDC issuer") {
		t.Errorf("warning must stay specific: no issuer warning when only the subject is wildcard, log:\n%s", log)
	}
}

// TestPolicyForPin_ConcretePinEmitsNoWildcardWarning pins the warning to the
// wildcard relaxation only: a fully concrete keyless pin emits neither
// wildcard warning, so the loudness cannot drift into every-day noise.
func TestPolicyForPin_ConcretePinEmitsNoWildcardWarning(t *testing.T) {
	base := signing.Policy{Mode: signing.ModeStrict}
	pin := &lockfile.LockedSignature{Keyless: &lockfile.LockedKeyless{
		Issuer:  "https://token.actions.githubusercontent.com",
		Subject: "https://github.com/acme/repo/*",
	}}

	logSink := pinTestLogger(t)
	got, err := policyForPin(&base, pin)
	if err != nil {
		t.Fatalf("policyForPin: %v", err)
	}
	if len(got.TrustedIssuers) != 1 || got.TrustedIssuers[0] != pin.Keyless.Issuer {
		t.Fatalf("issuer surface = %+v, want the pinned issuer", got.TrustedIssuers)
	}
	log := logSink.String()
	if strings.Contains(log, "trusts ANY OIDC issuer") || strings.Contains(log, "trusts any workflow subject") {
		t.Errorf("concrete pin must not warn, log:\n%s", log)
	}
}

// TestPolicyForPin_PatternSubjectMatchesWorkflowURL asserts a narrowed pin with
// a glob subject behaves like verification expects: a workflow URL subject
// falls within the pinned glob, an out-of-repo subject does not.
func TestPolicyForPin_PatternSubjectMatchesWorkflowURL(t *testing.T) {
	base := signing.Policy{Mode: signing.ModeStrict, SubjectPatterns: []string{"*"}}
	pin := &lockfile.LockedSignature{Keyless: &lockfile.LockedKeyless{
		Issuer:  "https://token.actions.githubusercontent.com",
		Subject: "https://github.com/acme/repo/*",
	}}

	got, err := policyForPin(&base, pin)
	if err != nil {
		t.Fatalf("policyForPin: %v", err)
	}
	if !signing.SubjectMatches(got.SubjectPatterns[0], "https://github.com/acme/repo/.github/workflows/ci.yml@refs/heads/main") {
		t.Error("workflow URL subject should match the pinned glob")
	}
	if signing.SubjectMatches(got.SubjectPatterns[0], "https://github.com/evil/malware/.github/workflows/ci.yml@refs/heads/main") {
		t.Error("foreign subject should not match the pinned glob")
	}
	if signing.SubjectMatches("https://token.actions.githubusercontent.com/*", "https://token.actions.example.com") {
		t.Error("issuer glob must not be a loose prefix match")
	}
}

func TestPolicyForPin_NilPinReturnsBase(t *testing.T) {
	base := signing.Policy{Mode: signing.ModeWarn}
	got, err := policyForPin(&base, nil)
	if err != nil {
		t.Fatalf("policyForPin: %v", err)
	}
	if got.Mode != signing.ModeWarn {
		t.Errorf("expected base returned unchanged, got %+v", got)
	}
}

func TestAssertSignerMatchesPin(t *testing.T) {
	keyPin := &lockfile.LockedSignature{Key: &lockfile.LockedKey{Algorithm: "ed25519", Fingerprint: "fp1"}}
	keylessPin := &lockfile.LockedSignature{Keyless: &lockfile.LockedKeyless{Issuer: "iss", Subject: "sub"}}

	tests := []struct {
		name        string
		signer      *signing.SignerIdentity
		pin         *lockfile.LockedSignature
		mode        signing.VerificationMode
		wantErr     bool
		errContains []string
	}{
		// ModeOff (explicit off, or the unsigned override lowering warn/unset)
		// deliberately skips verification, so a nil signer leaves the pin
		// unasserted.
		{name: "nil signer skips under mode off", signer: nil, pin: keyPin, mode: signing.ModeOff},
		{
			// A pinned signer must never be bypassed by a warn-mode failure
			// downgraded to a proceed (signer == nil).
			name:        "nil signer fails closed under warn",
			signer:      nil,
			pin:         keyPin,
			mode:        signing.ModeWarn,
			wantErr:     true,
			errContains: []string{"pins a signer", "verification produced none"},
		},
		{
			// Defensive: strict failures error inside signing.Verify and never
			// reach this call with a nil signer, but the guard must hold.
			name:    "nil signer fails closed under strict",
			signer:  nil,
			pin:     keyPin,
			mode:    signing.ModeStrict,
			wantErr: true,
		},
		{name: "nil pin skips", signer: &signing.SignerIdentity{Key: &signing.KeyIdentity{Fingerprint: "fp1"}}, pin: nil, mode: signing.ModeWarn},
		{name: "key matches", signer: &signing.SignerIdentity{Key: &signing.KeyIdentity{Fingerprint: "fp1"}}, pin: keyPin, mode: signing.ModeWarn},
		{name: "key mismatch", signer: &signing.SignerIdentity{Key: &signing.KeyIdentity{Fingerprint: "other"}}, pin: keyPin, mode: signing.ModeWarn, wantErr: true},
		{name: "key signer but keyless pin", signer: &signing.SignerIdentity{Key: &signing.KeyIdentity{Fingerprint: "fp1"}}, pin: keylessPin, mode: signing.ModeWarn, wantErr: true},
		{name: "keyless matches", signer: &signing.SignerIdentity{Keyless: &signing.KeylessIdentity{Issuer: "iss", Subject: "sub"}}, pin: keylessPin, mode: signing.ModeWarn},
		{name: "keyless subject mismatch", signer: &signing.SignerIdentity{Keyless: &signing.KeylessIdentity{Issuer: "iss", Subject: "evil"}}, pin: keylessPin, mode: signing.ModeWarn, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := assertSignerMatchesPin("type.name", tt.signer, tt.pin, tt.mode)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if err != nil {
				for _, sub := range tt.errContains {
					if !strings.Contains(err.Error(), sub) {
						t.Errorf("error %q does not contain %q", err.Error(), sub)
					}
				}
			}
		})
	}
}
