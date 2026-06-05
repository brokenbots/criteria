package publish

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/brokenbots/criteria/internal/adapter/oci"
	"github.com/brokenbots/criteria/internal/adapter/signing"
)

// TestSign_KeyMode_RoundTripVerifies proves the publish-side signing builders
// produce a cosign signature manifest that the verifier accepts — the contract
// between `criteria adapter publish --sign` and `signing.Verify`. Runs fully
// in-process (key mode, no registry, no OIDC).
func TestSign_KeyMode_RoundTripVerifies(t *testing.T) {
	root := t.TempDir()
	l, err := oci.Open(root)
	if err != nil {
		t.Fatalf("open layout: %v", err)
	}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(priv.Public())
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(pubDER)
	fp := hex.EncodeToString(sum[:])

	// Stage a minimal artifact manifest — the thing being signed.
	artifact := ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    ocispec.Descriptor{MediaType: mediaTypeAdapterConfig, Digest: digest.FromString("{}"), Size: 2},
	}
	artifactData, _ := json.Marshal(artifact)
	artifactDigest := digest.FromBytes(artifactData)
	if err := l.WriteBlob(bytes.NewReader(artifactData), artifactDigest); err != nil {
		t.Fatal(err)
	}
	artifactDesc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: artifactDigest, Size: int64(len(artifactData))}

	ref := oci.Reference{Registry: "ghcr.io", Repo: "criteria-adapters/test", Tag: "v1.0.0"}

	// Sign via the publish builders.
	payload := simpleSigningPayload(ref, artifactDigest)
	sig, certPEM, chainPEM, err := KeySigner{Priv: priv}.Sign(payload)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if certPEM != "" || chainPEM != "" {
		t.Fatalf("key signer must not emit cert/chain, got cert=%q chain=%q", certPEM, chainPEM)
	}
	manifestJSON, payloadBytes := buildSignatureManifest(&artifactDesc, payload, sig, certPEM, chainPEM)

	payloadDigest := digest.FromBytes(payloadBytes)
	sigDigest := digest.FromBytes(manifestJSON)
	if err := l.WriteBlob(bytes.NewReader(payloadBytes), payloadDigest); err != nil {
		t.Fatal(err)
	}
	if err := l.WriteBlob(bytes.NewReader(manifestJSON), sigDigest); err != nil {
		t.Fatal(err)
	}

	// Index references both the artifact and its signature manifest (referrer).
	ix := ocispec.Index{
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{
			{MediaType: ocispec.MediaTypeImageManifest, Digest: artifactDigest, Size: int64(len(artifactData))},
			{MediaType: ocispec.MediaTypeImageManifest, Digest: sigDigest, Size: int64(len(manifestJSON))},
		},
	}
	ixData, _ := json.Marshal(ix)
	if err := os.WriteFile(filepath.Join(root, "index.json"), ixData, 0o640); err != nil {
		t.Fatal(err)
	}

	policy := signing.Policy{
		Mode:        signing.ModeStrict,
		TrustedKeys: []signing.KeyIdentity{{Algorithm: "ed25519", Fingerprint: fp, RawKey: pubDER}},
	}
	id, err := signing.Verify(context.Background(), l, artifactDigest, policy)
	if err != nil {
		t.Fatalf("verify rejected publish-produced signature: %v", err)
	}
	if id == nil || id.Key == nil {
		t.Fatal("expected a key identity from verify")
	}
	if id.Key.Fingerprint != fp {
		t.Errorf("fingerprint = %q, want %q", id.Key.Fingerprint, fp)
	}

	// A different key must NOT verify (guards against an accidental no-op signer).
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	otherDER, _ := x509.MarshalPKIXPublicKey(otherPriv.Public())
	otherSum := sha256.Sum256(otherDER)
	wrongPolicy := signing.Policy{
		Mode:        signing.ModeStrict,
		TrustedKeys: []signing.KeyIdentity{{Algorithm: "ed25519", Fingerprint: hex.EncodeToString(otherSum[:]), RawKey: otherDER}},
	}
	if _, err := signing.Verify(context.Background(), l, artifactDigest, wrongPolicy); err == nil {
		t.Fatal("verify accepted a signature under the wrong trusted key")
	}
}

// TestSimpleSigningPayload_BindsDigest checks the payload binds the exact
// artifact digest and repository reference cosign expects.
func TestSimpleSigningPayload_BindsDigest(t *testing.T) {
	ref := oci.Reference{Registry: "ghcr.io", Repo: "org/name", Tag: "v1"}
	dg := digest.FromString("artifact")
	payload := simpleSigningPayload(ref, dg)

	if !strings.Contains(string(payload), dg.String()) {
		t.Errorf("payload missing artifact digest: %s", payload)
	}
	if !strings.Contains(string(payload), "ghcr.io/org/name") {
		t.Errorf("payload missing docker-reference: %s", payload)
	}
	// Must be valid JSON.
	var v map[string]any
	if err := json.Unmarshal(payload, &v); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}
}
