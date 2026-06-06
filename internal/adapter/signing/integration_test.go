package signing

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/brokenbots/criteria/internal/adapter/oci"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	ctypes "github.com/sigstore/cosign/v3/pkg/types"
)

// TestIntegration_KeyBased verifies a real artifact+signature round-trip
// through the OCI layout using an explicit trusted key.
func TestIntegration_KeyBased(t *testing.T) {
	root := t.TempDir()
	l, err := oci.Open(root)
	if err != nil {
		t.Fatal(err)
	}

	// Generate a test Ed25519 key.
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub := priv.Public().(ed25519.PublicKey)
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	fp := fingerprintBytes(pubDER)

	// Artifact manifest (the thing being signed).
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

	// Sign the payload.
	sig := ed25519.Sign(priv, payload)

	// Signature manifest.
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
					"dev.cosignproject.cosign/signature": base64.StdEncoding.EncodeToString(sig),
				},
			},
		},
	}
	sigData, _ := json.Marshal(sigManifest)
	sigDigest := digest.FromBytes(sigData)
	if err := l.WriteBlob(bytes.NewReader(sigData), sigDigest); err != nil {
		t.Fatal(err)
	}

	// Write index.
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

	// Verify with the trusted key.
	policy := Policy{
		Mode: ModeStrict,
		TrustedKeys: []KeyIdentity{
			{
				Algorithm:   "ed25519",
				Fingerprint: fp,
				RawKey:      pubDER,
			},
		},
	}

	id, err := Verify(t.Context(), l, artifactDigest, policy)
	if err != nil {
		t.Fatalf("Verify failed: %v", err)
	}
	if id == nil || id.Key == nil {
		t.Fatal("expected key identity")
	}
	if id.Key.Fingerprint != fp {
		t.Errorf("fingerprint = %q, want %q", id.Key.Fingerprint, fp)
	}
	if id.Key.Algorithm != "ed25519" {
		t.Errorf("algorithm = %q, want ed25519", id.Key.Algorithm)
	}
}

// TestIntegration_KeylessFixture verifies a real cosigned artifact pulled from
// the Criteria test registry. Skipped until the fixture is published.
func TestIntegration_KeylessFixture(t *testing.T) {
	t.Skip("CI fixture ghcr.io/criteria-test/signed-fixture:1.0.0 not yet published; enable after WS06 CI setup")

	// When enabled, this test should:
	// 1. Pull the artifact into a temporary OCI layout (using oci.Layout or
	//    an equivalent from WS04).
	// 2. Call Verify(ctx, layout, manifestDigest, Policy{Mode: ModeStrict}).
	// 3. Assert that the returned SignerIdentity matches the expected issuer
	//    and subject for the fixture.
}
