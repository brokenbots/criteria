package signing

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/brokenbots/criteria/internal/adapter/oci"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	ctypes "github.com/sigstore/cosign/v3/pkg/types"
)

// openTestLayout opens a fresh OCI layout for a test.
func openTestLayout(t *testing.T) *oci.Layout {
	t.Helper()
	l, err := oci.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// writeArtifactManifest stages a minimal artifact manifest keyed by config
// content (so different test artifacts hash differently) and returns its
// descriptor.
func writeArtifactManifest(t *testing.T, l *oci.Layout, config string) ocispec.Descriptor {
	t.Helper()
	m := ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Config: ocispec.Descriptor{
			MediaType: "application/vnd.criteria.adapter.config.v1+json",
			Digest:    digest.FromString(config),
			Size:      int64(len(config)),
		},
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	dg := digest.FromBytes(data)
	if err := l.WriteBlob(bytes.NewReader(data), dg); err != nil {
		t.Fatal(err)
	}
	return ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    dg,
		Size:      int64(len(data)),
	}
}

// writeSignatureReferrer stages a cosign signature referrer manifest for
// artifactDigest built from exactly the payload bytes given (so digest-binding
// negatives can sign the WRONG payload), the layer media type given, and the
// layer annotations given. Returns the signature manifest descriptor; it is
// NOT indexed — tests assemble the index themselves via writeLayoutIndex.
func writeSignatureReferrer(t *testing.T, l *oci.Layout, artifactDigest digest.Digest, payload []byte, layerMediaType string, anns map[string]string) ocispec.Descriptor {
	t.Helper()
	payloadDigest := digest.FromBytes(payload)
	if err := l.WriteBlob(bytes.NewReader(payload), payloadDigest); err != nil {
		t.Fatal(err)
	}
	m := ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Subject: &ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageManifest,
			Digest:    artifactDigest,
			Size:      0,
		},
		Layers: []ocispec.Descriptor{
			{
				MediaType:   layerMediaType,
				Digest:      payloadDigest,
				Size:        int64(len(payload)),
				Annotations: anns,
			},
		},
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	sigDigest := digest.FromBytes(data)
	if err := l.WriteBlob(bytes.NewReader(data), sigDigest); err != nil {
		t.Fatal(err)
	}
	return ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    sigDigest,
		Size:      int64(len(data)),
	}
}

// writeLayoutIndex writes the index listing the given descriptors.
func writeLayoutIndex(t *testing.T, l *oci.Layout, descs ...ocispec.Descriptor) {
	t.Helper()
	ix := ocispec.Index{
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: descs,
	}
	if err := l.WriteIndex(&ix); err != nil {
		t.Fatal(err)
	}
}

// cosignLayerAnnotations returns the layer annotations for a valid keyed
// signature over sig.
func cosignLayerAnnotations(t *testing.T, sig []byte) map[string]string {
	t.Helper()
	return map[string]string{
		"dev.cosignproject.cosign/signature": base64.StdEncoding.EncodeToString(sig),
	}
}

// trustedKeyPolicy returns a strict single-key policy for the test key.
func trustedKeyPolicy(t *testing.T, priv ed25519.PrivateKey, mode VerificationMode) Policy {
	t.Helper()
	pub := priv.Public().(ed25519.PublicKey)
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return Policy{
		Mode: mode,
		TrustedKeys: []KeyIdentity{
			{
				Algorithm:   "ed25519",
				Fingerprint: fingerprintBytes(pubDER),
				RawKey:      pubDER,
			},
		},
	}
}

// simpleSigningTestPayload mirrors publish.simpleSigningPayload's shape: it
// binds the payload to artifactDigest via critical.image.docker-manifest-digest.
func simpleSigningTestPayload(artifactDigest digest.Digest) []byte {
	return []byte(`{"critical":{"identity":{"docker-reference":"ghcr.io/test"},"image":{"docker-manifest-digest":"` + artifactDigest.String() + `"},"type":"cosign container image signature"},"optional":null}`)
}

// TestVerify_DigestBindingRepinsVerification pins the v0.6.0 digest-binding
// contract end to end: a valid signature verifies for the artifact its payload
// names, and the same signature bytes re-attached (via a mutated referrer
// subject in the writable local layout) to a different artifact fail.
func TestVerify_DigestBindingRoundTripAndRepointedFails(t *testing.T) {
	l := openTestLayout(t)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	policy := trustedKeyPolicy(t, priv, ModeStrict)

	artifactA := writeArtifactManifest(t, l, "config-A")
	artifactB := writeArtifactManifest(t, l, "config-B")

	payloadA := simpleSigningTestPayload(artifactA.Digest)
	sigA := ed25519.Sign(priv, payloadA)

	writeLayoutIndex(t, l, artifactA, artifactB,
		writeSignatureReferrer(t, l, artifactA.Digest, payloadA, ctypes.SimpleSigningMediaType, cosignLayerAnnotations(t, sigA)),
		writeSignatureReferrer(t, l, artifactB.Digest, payloadA, ctypes.SimpleSigningMediaType, cosignLayerAnnotations(t, sigA)),
	)

	// Positive: the signature verifies for the artifact its payload binds A to.
	id, err := Verify(t.Context(), l, artifactA.Digest, policy)
	if err != nil {
		t.Fatalf("digest-bound round-trip must verify: %v", err)
	}
	if id == nil || id.Key == nil || id.Key.Fingerprint != policy.TrustedKeys[0].Fingerprint {
		t.Fatalf("unexpected identity: %+v", id)
	}

	// Negative: the same signature bytes attested to artifact B (referring
	// subject re-pointed to B in the writable layout) must fail on the payload
	// digest mismatch, not be accepted via the referrer link alone.
	if _, err := Verify(t.Context(), l, artifactB.Digest, policy); err == nil {
		t.Fatal("signature over digest A must not verify for artifact B")
	}
}

// TestVerify_RepointedReferrerModeWarnRefusesIdentity pins "no mode escape":
// under warn mode the re-pointed signature must not be granted as a verified
// identity (warn downgrades the error, never the authenticity of a foreign
// payload).
func TestVerify_RepointedReferrerModeWarnRefusesIdentity(t *testing.T) {
	l := openTestLayout(t)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	artifactA := writeArtifactManifest(t, l, "config-A")
	artifactB := writeArtifactManifest(t, l, "config-B")
	payloadA := simpleSigningTestPayload(artifactA.Digest)
	sigA := ed25519.Sign(priv, payloadA)

	writeLayoutIndex(t, l, artifactA, artifactB,
		writeSignatureReferrer(t, l, artifactB.Digest, payloadA, ctypes.SimpleSigningMediaType, cosignLayerAnnotations(t, sigA)),
	)

	id, err := Verify(t.Context(), l, artifactB.Digest, trustedKeyPolicy(t, priv, ModeWarn))
	if err != nil {
		t.Fatalf("warn mode must downgrade to no error: %v", err)
	}
	if id != nil {
		t.Fatal("warn mode must not grant the identity of a re-pointed signature")
	}
}

// TestVerify_PayloadMissingDigestFieldFails proves an old signature whose
// payload lacks critical.image.docker-manifest-digest stops verifying (the
// v0.6.0 tightening), even when the signature is over the payload by a
// fully trusted key.
func TestVerify_PayloadMissingDigestFieldFails(t *testing.T) {
	l := openTestLayout(t)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	policy := trustedKeyPolicy(t, priv, ModeStrict)

	artifact := writeArtifactManifest(t, l, "config")
	legacyPayload := []byte(`{"critical":{"identity":{"docker-reference":"ghcr.io/test"},"type":"cosign container image signature"},"optional":null}`)
	sig := ed25519.Sign(priv, legacyPayload)

	writeLayoutIndex(t, l, artifact,
		writeSignatureReferrer(t, l, artifact.Digest, legacyPayload, ctypes.SimpleSigningMediaType, cosignLayerAnnotations(t, sig)),
	)

	_, err = Verify(t.Context(), l, artifact.Digest, policy)
	if err == nil {
		t.Fatal("payload without the digest field must not verify")
	}
	if msg := err.Error(); !bytes.Contains([]byte(msg), []byte("critical.image.docker-manifest-digest")) {
		t.Errorf("error %q does not name the missing digest field", msg)
	}
}

// TestVerify_ArbitraryBytesPayloadFails proves the readPayload fallback is
// gone: a signature manifest whose layer is not a simple-signing payload is a
// hard error — the old "first blob that exists" path could let a signature
// verify over arbitrary bytes.
func TestVerify_ArbitraryBytesPayloadFails(t *testing.T) {
	l := openTestLayout(t)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	policy := trustedKeyPolicy(t, priv, ModeStrict)

	artifact := writeArtifactManifest(t, l, "config")
	arbitrary := []byte("arbitrary bytes, not a simple-signing payload")
	sig := ed25519.Sign(priv, arbitrary)

	writeLayoutIndex(t, l, artifact,
		writeSignatureReferrer(t, l, artifact.Digest, arbitrary, "application/vnd.oci.image.layer.v1.tar", cosignLayerAnnotations(t, sig)),
	)

	_, err = Verify(t.Context(), l, artifact.Digest, policy)
	if err == nil {
		t.Fatal("a signature manifest with no simple-signing payload layer must not verify")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("payload layer")) {
		t.Errorf("error %q does not name the missing simple-signing payload layer", err.Error())
	}
}

// TestReadPayload_Errors covers the fail-closed branches of readPayload: no
// simple-signing layer at all, and a layer whose blob is absent from the
// layout.
func TestReadPayload_Errors(t *testing.T) {
	l := openTestLayout(t)

	m := ocispec.Manifest{
		Layers: []ocispec.Descriptor{
			{
				MediaType: "application/vnd.oci.image.layer.v1.tar",
				Digest:    digest.FromString("arbitrary"),
			},
		},
	}
	if _, err := readPayload(l, &m); err == nil {
		t.Error("readPayload must error when there is no simple-signing layer")
	}

	m2 := ocispec.Manifest{
		Layers: []ocispec.Descriptor{
			{
				MediaType: ctypes.SimpleSigningMediaType,
				Digest:    digest.FromString("absent"),
			},
		},
	}
	if _, err := readPayload(l, &m2); err == nil {
		t.Error("readPayload must error when the payload blob is absent")
	}
}

// TestSimpleSigningDigest covers the binding-field parser contract: the good
// case, missing field, non-JSON bytes, and a malformed digest string.
func TestSimpleSigningDigest(t *testing.T) {
	want := digest.FromString("artifact")
	got, err := simpleSigningDigest(simpleSigningTestPayload(want))
	if err != nil {
		t.Fatalf("simpleSigningDigest: %v", err)
	}
	if got != want {
		t.Errorf("simpleSigningDigest = %s, want %s", got, want)
	}

	if _, err := simpleSigningDigest([]byte(`{"critical":{"identity":{"docker-reference":"x"}}}`)); err == nil {
		t.Error("expected error for payload missing the digest field")
	}
	if _, err := simpleSigningDigest([]byte("not json")); err == nil {
		t.Error("expected error for payload that is not JSON")
	}
	if _, err := simpleSigningDigest([]byte(`{"critical":{"image":{"docker-manifest-digest":"not-a-digest"}}}`)); err == nil {
		t.Error("expected error for a malformed digest string")
	}
}

// TestFindSignatures_NonCosignReferrerSkipped pins the skip behavior: a
// referrer manifest without the cosign signature annotation (e.g. an SBOM) is
// neither a candidate nor an error.
func TestFindSignatures_NonCosignReferrerSkipped(t *testing.T) {
	l := openTestLayout(t)
	artifact := writeArtifactManifest(t, l, "config")
	sbom := writeSignatureReferrer(t, l, artifact.Digest, []byte("spdx bytes"), "application/vnd.spdx.spdx+json", nil)
	writeLayoutIndex(t, l, artifact, sbom)

	sigs, err := findSignatures(l, artifact.Digest)
	if err != nil {
		t.Fatalf("findSignatures: %v", err)
	}
	if len(sigs) != 0 {
		t.Fatalf("expected 0 signatures, got %d", len(sigs))
	}
}

// TestVerifyOne_DigestBindingPrecedesKeylessDispatch pins that the binding
// check runs before signature-shape dispatch: a keyless-shaped record whose
// payload binds a foreign digest fails on the digest mismatch (not later at
// bundle parsing).
func TestVerifyOne_DigestBindingPrecedesKeylessDispatch(t *testing.T) {
	boundDigest := digest.FromString("artifact-A")
	otherDigest := digest.FromString("artifact-B")
	rec := &signatureRecord{
		payload:      simpleSigningTestPayload(boundDigest),
		signatureB64: base64.StdEncoding.EncodeToString([]byte("sig")),
		bundleJSON:   "{}",
	}
	if _, err := verifyOne(t.Context(), otherDigest, rec, &Policy{Mode: ModeStrict}); err == nil {
		t.Fatal("keyless-shaped payload bound to a foreign digest must fail on binding")
	}
}
