package cli

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

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	ctypes "github.com/sigstore/cosign/v2/pkg/types"

	"github.com/brokenbots/criteria/internal/adapter/oci"
	"github.com/brokenbots/criteria/internal/adapter/signing"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

// writeKeySignedArtifact builds a key-signed adapter artifact in an on-disk OCI
// layout and returns the layout, the artifact digest, the public-key DER, and
// the canonical fingerprint. It mirrors the cosign signature shape the verifier
// reads (an OCI referrer carrying the simple-signing payload + signature
// annotation).
func writeKeySignedArtifact(t *testing.T) (layout *oci.Layout, dg digest.Digest, pubKeyDER []byte, fingerprint string) {
	t.Helper()
	root := t.TempDir()
	l, err := oci.Open(root)
	if err != nil {
		t.Fatal(err)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}

	artifact := ocispec.Manifest{MediaType: ocispec.MediaTypeImageManifest}
	artifactData, _ := json.Marshal(artifact)
	artifactDigest := digest.FromBytes(artifactData)
	if err := l.WriteBlob(bytes.NewReader(artifactData), artifactDigest); err != nil {
		t.Fatal(err)
	}

	payload := []byte(`{"critical":{"identity":{"docker-reference":"ghcr.io/test"},"image":{"docker-manifest-digest":"` + artifactDigest.String() + `"},"type":"cosign container image signature"},"optional":null}`)
	payloadDigest := digest.FromBytes(payload)
	if err := l.WriteBlob(bytes.NewReader(payload), payloadDigest); err != nil {
		t.Fatal(err)
	}

	sig := ed25519.Sign(priv, payload)
	sigManifest := ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Subject:   &ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: artifactDigest, Size: int64(len(artifactData))},
		Layers: []ocispec.Descriptor{{
			MediaType:   ctypes.SimpleSigningMediaType,
			Digest:      payloadDigest,
			Size:        int64(len(payload)),
			Annotations: map[string]string{"dev.cosignproject.cosign/signature": base64.StdEncoding.EncodeToString(sig)},
		}},
	}
	sigData, _ := json.Marshal(sigManifest)
	sigDigest := digest.FromBytes(sigData)
	if err := l.WriteBlob(bytes.NewReader(sigData), sigDigest); err != nil {
		t.Fatal(err)
	}

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

	return l, artifactDigest, pubDER, signing.Fingerprint(pubDER)
}

// TestVerifyAgainstPin_KeyMode_Succeeds proves the WS47 standalone enforcement:
// a key-signed artifact verifies under strict against the configured trusted key
// and the matching lockfile pin, fully offline.
func TestVerifyAgainstPin_KeyMode_Succeeds(t *testing.T) {
	l, dg, pubDER, fp := writeKeySignedArtifact(t)

	policy := signing.Policy{
		Mode:        signing.ModeStrict,
		TrustedKeys: []signing.KeyIdentity{{Algorithm: "ed25519", Fingerprint: fp, RawKey: pubDER}},
	}
	entry := &lockfile.LockedAdapter{
		Type:      "shell",
		Name:      "default",
		Signature: &lockfile.LockedSignature{Key: &lockfile.LockedKey{Algorithm: "ed25519", Fingerprint: fp}},
	}

	if err := verifyAgainstPin(t.Context(), "shell.default", l, dg, entry, &policy); err != nil {
		t.Fatalf("verifyAgainstPin: %v", err)
	}
}

// TestVerifyAgainstPin_RotatedKey_FailsClosed proves a rotated/wrong pinned
// fingerprint fails closed under strict (the configured key no longer matches
// the pin, so no key remains to verify with).
func TestVerifyAgainstPin_RotatedKey_FailsClosed(t *testing.T) {
	l, dg, pubDER, fp := writeKeySignedArtifact(t)

	policy := signing.Policy{
		Mode:        signing.ModeStrict,
		TrustedKeys: []signing.KeyIdentity{{Algorithm: "ed25519", Fingerprint: fp, RawKey: pubDER}},
	}
	entry := &lockfile.LockedAdapter{
		Type:      "shell",
		Name:      "default",
		Signature: &lockfile.LockedSignature{Key: &lockfile.LockedKey{Algorithm: "ed25519", Fingerprint: "deadbeef"}},
	}

	if err := verifyAgainstPin(t.Context(), "shell.default", l, dg, entry, &policy); err == nil {
		t.Fatal("expected verification to fail closed for a rotated pinned key")
	}
}

// TestVerifyAgainstPin_AllowUnsigned_Skips proves the WS46 override bypasses the
// pin check (ModeOff returns a nil signer).
func TestVerifyAgainstPin_AllowUnsigned_Skips(t *testing.T) {
	l, dg, _, _ := writeKeySignedArtifact(t)

	policy := signing.Policy{Mode: signing.ModeOff}
	entry := &lockfile.LockedAdapter{
		Signature: &lockfile.LockedSignature{Key: &lockfile.LockedKey{Fingerprint: "whatever"}},
	}
	if err := verifyAgainstPin(t.Context(), "shell.default", l, dg, entry, &policy); err != nil {
		t.Fatalf("ModeOff should skip verification, got: %v", err)
	}
}
