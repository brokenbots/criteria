// Package signing verifies cosign signatures on adapter artifacts stored in
// an OCI layout. It supports both keyless (Sigstore OIDC) and explicit-key
// verification modes.
package signing

import (
	"bytes"
	"context"
	"crypto"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/brokenbots/criteria/internal/adapter/oci"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	ctypes "github.com/sigstore/cosign/v2/pkg/types"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"
	sigcrypto "github.com/sigstore/sigstore/pkg/cryptoutils"
	sigsignature "github.com/sigstore/sigstore/pkg/signature"
)

// VerificationMode controls how signature verification failures are handled.
type VerificationMode string

const (
	ModeOff    VerificationMode = "off"
	ModeWarn   VerificationMode = "warn"
	ModeStrict VerificationMode = "strict"
)

// SignerIdentity records the verified signer of an adapter artifact.
type SignerIdentity struct {
	Keyless *KeylessIdentity `json:"keyless,omitempty"`
	Key     *KeyIdentity     `json:"key,omitempty"`
}

// KeylessIdentity is the OIDC issuer + subject extracted from a Fulcio cert.
type KeylessIdentity struct {
	Issuer  string `json:"issuer"`  // OIDC issuer URL
	Subject string `json:"subject"` // e.g. workflow identity
}

// KeyIdentity describes a verified public key.
type KeyIdentity struct {
	Algorithm   string `json:"algorithm"`   // "ed25519" | "ecdsa-p256" | ...
	Fingerprint string `json:"fingerprint"` // SHA-256 of public key DER
	// RawKey is the DER-encoded public key bytes. Populated during policy
	// resolution from global config; not serialized to JSON.
	RawKey []byte `json:"-"`
}

// Policy defines what signatures are acceptable.
type Policy struct {
	Mode            VerificationMode
	TrustedIssuers  []string      // OIDC issuers accepted for keyless
	SubjectPatterns []string      // glob patterns the subject must match
	TrustedKeys     []KeyIdentity // explicit public keys (key-based verification)
}

// IsKeyless returns true when the policy requires keyless verification (no
// explicit trusted keys).
func (p *Policy) IsKeyless() bool {
	return len(p.TrustedKeys) == 0
}

// Verify checks the cosign signature attached as an OCI referrer to the
// adapter artifact at manifestDigest. Returns the signer identity that
// produced the signature, or an error if no signature satisfies the policy.
//
// In ModeOff:    skips verification, returns nil identity, nil error.
// In ModeWarn:   logs failures but returns nil error and a nil identity.
// In ModeStrict: returns an error on any failure.
func Verify(ctx context.Context, layout *oci.Layout, manifestDigest digest.Digest, policy Policy) (*SignerIdentity, error) {
	if policy.Mode == ModeOff {
		return nil, nil
	}

	sigs, err := findSignatures(layout, manifestDigest)
	if err != nil {
		return handlePolicyMode(policy.Mode, nil, fmt.Errorf("signing: find signatures: %w", err))
	}
	if len(sigs) == 0 {
		return handlePolicyMode(policy.Mode, nil, fmt.Errorf("signing: no cosign signatures found for %s", manifestDigest))
	}

	var lastErr error
	for _, sig := range sigs {
		id, err := verifyOne(ctx, manifestDigest, &sig, &policy)
		if err == nil {
			return id, nil
		}
		lastErr = err
	}

	return handlePolicyMode(policy.Mode, nil, fmt.Errorf("signing: no matching signature: %w", lastErr))
}

func handlePolicyMode(mode VerificationMode, id *SignerIdentity, err error) (*SignerIdentity, error) {
	switch mode {
	case ModeWarn:
		if err != nil {
			slog.Warn("signature verification warning", "mode", mode, "error", err)
		}
		return id, nil
	case ModeStrict:
		return nil, err
	case ModeOff:
		return nil, nil
	default:
		return nil, err
	}
}

// signatureRecord holds the raw data needed to verify a single cosign
// signature found in the OCI layout.
type signatureRecord struct {
	manifestDigest digest.Digest // digest of the signature manifest blob
	payload        []byte        // simplesigning payload (the layer blob)
	signatureB64   string        // base64-encoded signature (from annotation)
	certPEM        string        // PEM certificate (from annotation, keyless only)
	bundleJSON     string        // sigstore bundle JSON (from annotation, optional)
	chainPEM       string        // PEM certificate chain (from annotation, optional)
}

// findSignatures discovers cosign signature manifests in the OCI layout that
// refer to the given manifest digest. It searches:
//
//  1. OCI v1.1 referrers: manifests in index.json whose Subject matches
//     manifestDigest.
//  2. Embedded convention: the artifact manifest itself may contain a layer
//     with title "signatures/cosign.sig" (legacy compat).
func findSignatures(layout *oci.Layout, manifestDigest digest.Digest) ([]signatureRecord, error) {
	var out []signatureRecord

	ix, err := layout.Index()
	if err != nil {
		return nil, fmt.Errorf("read index: %w", err)
	}

	for _, desc := range ix.Manifests {
		m, ok := readManifestBlob(layout, desc.Digest)
		if !ok {
			continue
		}

		if m.Subject != nil && m.Subject.Digest == manifestDigest {
			if rec, ok := recordFromManifest(layout, m, desc.Digest); ok {
				out = append(out, rec)
			}
			continue
		}

		if desc.Digest == manifestDigest {
			out = append(out, embeddedSigs(layout, m)...)
		}
	}

	return out, nil
}

func readManifestBlob(layout *oci.Layout, d digest.Digest) (*ocispec.Manifest, bool) {
	if !layout.HasBlob(d) {
		return nil, false
	}
	data, err := os.ReadFile(layout.BlobPath(d))
	if err != nil {
		return nil, false
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, false
	}
	return &m, true
}

func embeddedSigs(layout *oci.Layout, m *ocispec.Manifest) []signatureRecord {
	var out []signatureRecord
	for _, layer := range m.Layers {
		if title, ok := layer.Annotations[oci.AnnotationTitle]; ok && title == "signatures/cosign.sig" {
			if rec, ok := recordFromEmbedded(layout, &layer); ok {
				out = append(out, rec)
			}
		}
	}
	return out
}

// recordFromManifest extracts a signatureRecord from a cosign signature
// manifest.
func recordFromManifest(layout *oci.Layout, m *ocispec.Manifest, manifestDigest digest.Digest) (signatureRecord, bool) {
	rec := signatureRecord{manifestDigest: manifestDigest}

	anns := mergeAnnotations(m.Annotations, m.Layers)

	rec.signatureB64 = anns["dev.cosignproject.cosign/signature"]
	rec.certPEM = anns["dev.sigstore.cosign/certificate"]
	rec.chainPEM = anns["dev.sigstore.cosign/chain"]
	rec.bundleJSON = anns["dev.sigstore.cosign/bundle"]

	if rec.signatureB64 == "" {
		return signatureRecord{}, false
	}

	rec.payload = readPayload(layout, m)
	return rec, rec.payload != nil
}

func readPayload(layout *oci.Layout, m *ocispec.Manifest) []byte {
	for _, layer := range m.Layers {
		if layer.MediaType == ctypes.SimpleSigningMediaType {
			if layout.HasBlob(layer.Digest) {
				data, err := os.ReadFile(layout.BlobPath(layer.Digest))
				if err == nil {
					return data
				}
			}
			break
		}
	}
	for _, layer := range m.Layers {
		if layout.HasBlob(layer.Digest) {
			data, err := os.ReadFile(layout.BlobPath(layer.Digest))
			if err == nil {
				return data
			}
			break
		}
	}
	return nil
}

func recordFromEmbedded(layout *oci.Layout, layer *ocispec.Descriptor) (signatureRecord, bool) {
	if !layout.HasBlob(layer.Digest) {
		return signatureRecord{}, false
	}
	data, err := os.ReadFile(layout.BlobPath(layer.Digest))
	if err != nil {
		return signatureRecord{}, false
	}
	return signatureRecord{
		manifestDigest: layer.Digest,
		payload:        data,
	}, true
}

func mergeAnnotations(manifestAnns map[string]string, layers []ocispec.Descriptor) map[string]string {
	out := make(map[string]string, len(manifestAnns))
	for k, v := range manifestAnns {
		out[k] = v
	}
	for _, layer := range layers {
		for k, v := range layer.Annotations {
			out[k] = v
		}
	}
	return out
}

func verifyOne(ctx context.Context, manifestDigest digest.Digest, rec *signatureRecord, policy *Policy) (*SignerIdentity, error) {
	_ = manifestDigest // the signature is bound to the artifact via the OCI subject referrer
	// Dispatch on the signature record's shape, not solely on the policy, so a
	// mixed environment (some adapters key-signed, some keyless) verifies under a
	// single policy: a keyless signature carries a Fulcio certificate or a
	// Sigstore bundle; a key-mode signature carries neither and is matched
	// against Policy.TrustedKeys.
	if rec.certPEM != "" || rec.bundleJSON != "" {
		return verifyKeyless(ctx, rec, policy)
	}
	if policy.IsKeyless() {
		// No trusted keys configured and no keyless material: fall through to the
		// keyless path so the failure message is meaningful.
		return verifyKeyless(ctx, rec, policy)
	}
	return verifyKeyBased(rec, policy)
}

// verifyKeyless verifies a keyless cosign signature. It requires the Sigstore
// bundle path: a keyless signature is only durably verifiable via its
// transparency-log proof (the Fulcio certificate is ephemeral, ~10 min). A
// signature with no bundle fails closed (verifyKeylessLegacy).
func verifyKeyless(ctx context.Context, rec *signatureRecord, policy *Policy) (*SignerIdentity, error) {
	if rec.bundleJSON != "" {
		return verifyKeylessBundle(ctx, rec, policy)
	}
	return verifyKeylessLegacy(rec)
}

// verifyKeylessBundle verifies a cosign signature that includes a Sigstore
// bundle. The bundle carries the Fulcio certificate plus a Rekor inclusion proof
// (and/or signed timestamp), letting the verifier check the certificate at the
// log timestamp rather than at time.Now() — so the signature stays verifiable
// after the certificate expires.
func verifyKeylessBundle(ctx context.Context, rec *signatureRecord, policy *Policy) (*SignerIdentity, error) {
	var b bundle.Bundle
	if err := b.UnmarshalJSON([]byte(rec.bundleJSON)); err != nil {
		return nil, fmt.Errorf("unmarshal bundle: %w", err)
	}
	// The bundle's message signature is over the simple-signing payload, which is
	// itself bound to the artifact by the OCI subject referrer (findSignatures).
	payloadDigest := sha256.Sum256(rec.payload)
	return verifyBundleEntity(ctx, &b, payloadDigest[:], policy)
}

// verifyBundleEntity runs the sigstore-go verifier over a signed entity (a real
// bundle in production, a synthetic one in tests), then applies our issuer +
// subject policy to the certificate. Identity matching is done here (not via the
// sigstore-go policy) so it shares one implementation with the legacy path and
// is governed by Policy.TrustedIssuers / Policy.SubjectPatterns.
func verifyBundleEntity(ctx context.Context, entity verify.SignedEntity, artifactSHA256 []byte, policy *Policy) (*SignerIdentity, error) {
	tm, err := trustedMaterial(ctx)
	if err != nil {
		return nil, fmt.Errorf("trusted material: %w", err)
	}

	// Require a transparency-log entry and an observer timestamp: together they
	// fix the cert at the log time, so a keyless signature stays verifiable after
	// the ephemeral Fulcio certificate expires. (Certificate-transparency SCTs
	// are not required here; the Rekor inclusion proof is the trust anchor.)
	v, err := verify.NewVerifier(tm,
		verify.WithTransparencyLog(1),
		verify.WithObserverTimestamps(1),
	)
	if err != nil {
		return nil, fmt.Errorf("create verifier: %w", err)
	}

	policyBuilder := verify.NewPolicy(
		verify.WithArtifactDigest("sha256", artifactSHA256),
		verify.WithoutIdentitiesUnsafe(),
	)

	if _, err := v.Verify(entity, policyBuilder); err != nil {
		return nil, fmt.Errorf("bundle verify: %w", err)
	}

	vc, err := entity.VerificationContent()
	if err != nil {
		return nil, fmt.Errorf("bundle verification content: %w", err)
	}
	cert := vc.Certificate()
	if cert == nil {
		return nil, fmt.Errorf("bundle missing certificate")
	}
	return identityFromCert(cert, policy)
}

// verifyKeylessLegacy is the no-bundle path. A keyless signature without a
// transparency-log proof cannot be verified once the Fulcio certificate expires
// (~10 min), so it fails closed with an actionable message rather than the
// misleading "leaf certificate verification failed" that a time.Now() check
// would produce.
func verifyKeylessLegacy(rec *signatureRecord) (*SignerIdentity, error) {
	if rec.certPEM == "" {
		return nil, fmt.Errorf("keyless signature missing certificate")
	}
	return nil, fmt.Errorf("keyless signature has no transparency-log proof (Sigstore bundle); " +
		"it cannot be verified after the signing certificate expires — re-publish with Rekor enabled, " +
		"or use --allow-unsigned for local development")
}

// verifyKeyBased verifies a signature against an explicit set of trusted
// public keys. It matches the signing key by fingerprint and validates the
// signature over the simplesigning payload.
func verifyKeyBased(rec *signatureRecord, policy *Policy) (*SignerIdentity, error) {
	if rec.signatureB64 == "" {
		return nil, fmt.Errorf("missing signature")
	}

	sigBytes, err := base64.StdEncoding.DecodeString(rec.signatureB64)
	if err != nil {
		return nil, fmt.Errorf("decode signature: %w", err)
	}

	for _, trusted := range policy.TrustedKeys {
		if len(trusted.RawKey) == 0 {
			continue
		}
		fp := fingerprintBytes(trusted.RawKey)
		if fp != trusted.Fingerprint {
			continue
		}

		pub, err := sigcrypto.UnmarshalPEMToPublicKey(trusted.RawKey)
		if err != nil {
			pub, err = x509.ParsePKIXPublicKey(trusted.RawKey)
			if err != nil {
				continue
			}
		}

		verifier, err := sigsignature.LoadVerifier(pub, crypto.SHA256)
		if err != nil {
			continue
		}

		if err := verifier.VerifySignature(
			bytes.NewReader(sigBytes),
			bytes.NewReader(rec.payload),
		); err != nil {
			continue
		}

		return &SignerIdentity{
			Key: &KeyIdentity{
				Algorithm:   trusted.Algorithm,
				Fingerprint: fp,
			},
		}, nil
	}

	return nil, fmt.Errorf("no trusted key matched signature")
}

func fingerprintBytes(raw []byte) string {
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

// identityFromCert extracts KeylessIdentity from a Fulcio certificate and
// validates it against the policy.
func identityFromCert(cert *x509.Certificate, policy *Policy) (*SignerIdentity, error) {
	summary, err := certificate.SummarizeCertificate(cert)
	if err != nil {
		return nil, fmt.Errorf("summarize certificate: %w", err)
	}

	issuer := summary.Issuer
	subject := summary.SubjectAlternativeName

	if len(policy.TrustedIssuers) > 0 {
		found := false
		for _, iss := range policy.TrustedIssuers {
			if iss == issuer {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("issuer %q not in trusted issuers", issuer)
		}
	}

	matched := len(policy.SubjectPatterns) == 0
	for _, pat := range policy.SubjectPatterns {
		if matchGlob(pat, subject) {
			matched = true
			break
		}
	}
	if !matched {
		return nil, fmt.Errorf("subject %q does not match any allowed pattern", subject)
	}

	return &SignerIdentity{
		Keyless: &KeylessIdentity{
			Issuer:  issuer,
			Subject: subject,
		},
	}, nil
}

func matchGlob(pattern, s string) bool {
	if pattern == "*" || pattern == "" {
		return true
	}
	if strings.HasPrefix(pattern, "*") {
		return strings.HasSuffix(s, strings.TrimPrefix(pattern, "*"))
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(s, strings.TrimSuffix(pattern, "*"))
	}
	return pattern == s
}

// trustedMaterialOverride is set by integration tests to inject a mock
// Sigstore trusted root. When nil, the production TUF root is used.
var trustedMaterialOverride root.TrustedMaterial

// trustedMaterial returns the Sigstore trusted root used to verify keyless
// bundles.
//
// TUF root policy (decision D-WS48-TUF): the root is fetched via TUF and cached
// on disk at ~/.criteria/cache/sigstore/ (honoring CRITERIA_STATE_DIR); once
// cached, verification reuses it for reproducibility. Refresh happens by
// clearing that cache directory (an explicit `criteria adapter trust refresh`
// command is future work). Air-gapped consumers cannot keyless-verify (the TUF
// root and a was-online-at-sign Rekor entry are required); they use WS47 key
// mode or --allow-unsigned.
func trustedMaterial(_ context.Context) (root.TrustedMaterial, error) {
	if trustedMaterialOverride != nil {
		return trustedMaterialOverride, nil
	}

	cacheDir, err := sigstoreCacheDir()
	if err != nil {
		return nil, fmt.Errorf("cache dir: %w", err)
	}
	if err := os.MkdirAll(cacheDir, 0o750); err != nil {
		return nil, fmt.Errorf("mkdir cache: %w", err)
	}

	opts := tuf.DefaultOptions()
	opts.CachePath = cacheDir

	tr, err := root.NewLiveTrustedRoot(opts)
	if err != nil {
		return nil, fmt.Errorf("live trusted root: %w", err)
	}
	return tr, nil
}

func sigstoreCacheDir() (string, error) {
	base := os.Getenv("CRITERIA_STATE_DIR")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("home dir: %w", err)
		}
		base = filepath.Join(home, ".criteria")
	}
	return filepath.Join(base, "cache", "sigstore"), nil
}

// DefaultTrustedIssuers are the OIDC issuers trusted by default for keyless
// verification.
var DefaultTrustedIssuers = []string{
	"https://token.actions.githubusercontent.com",
	"https://accounts.google.com",
	"https://gitlab.com",
}
