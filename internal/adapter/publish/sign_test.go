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
	"io"
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
	res, err := KeySigner{Priv: priv}.Sign(payload)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if res.CertPEM != "" || res.ChainPEM != "" {
		t.Fatalf("key signer must not emit cert/chain, got cert=%q chain=%q", res.CertPEM, res.ChainPEM)
	}
	if len(res.Bundle) != 0 {
		t.Fatalf("key signer must not emit a bundle, got %d bytes", len(res.Bundle))
	}
	manifestJSON, payloadBytes := buildSignatureManifest(&artifactDesc, payload, &res)

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

// TestBuildSignatureManifest_HasEmptyConfigAndSchemaVersion guards the GHCR
// compatibility fix: the cosign signature manifest must carry schemaVersion 2
// and a valid (empty-JSON) config descriptor. Without these the zero-value
// config marshals as {"mediaType":"","digest":"","size":0}, which strict
// registries (GHCR) reject with a 500 on push.
func TestBuildSignatureManifest_HasEmptyConfigAndSchemaVersion(t *testing.T) {
	artifactDesc := &ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    digest.FromString("artifact"),
		Size:      123,
	}
	manifestJSON, _ := buildSignatureManifest(artifactDesc, []byte("payload"), &SignResult{Signature: []byte("sig")})

	var m ocispec.Manifest
	if err := json.Unmarshal(manifestJSON, &m); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	if m.SchemaVersion != 2 {
		t.Errorf("schemaVersion = %d, want 2", m.SchemaVersion)
	}
	if m.Config.MediaType != ocispec.MediaTypeEmptyJSON {
		t.Errorf("config mediaType = %q, want %q", m.Config.MediaType, ocispec.MediaTypeEmptyJSON)
	}
	if m.Config.Digest != ocispec.DescriptorEmptyJSON.Digest {
		t.Errorf("config digest = %q, want empty-JSON digest %q", m.Config.Digest, ocispec.DescriptorEmptyJSON.Digest)
	}
	if m.Subject == nil || m.Subject.Digest != artifactDesc.Digest {
		t.Errorf("subject must reference the artifact digest")
	}
	if m.ArtifactType != cosignSignatureArtifactType {
		t.Errorf("artifactType = %q, want %q", m.ArtifactType, cosignSignatureArtifactType)
	}
}

// TestLegacyCosignSignatureTag matches the cosign "sha256-<algorithm>-<hex>.sig"
// convention. This is the tag default cosign verification resolves when the
// registry does not support the OCI 1.1 referrers API.
func TestLegacyCosignSignatureTag(t *testing.T) {
	dg := digest.FromString("some-artifact")
	got := legacyCosignSignatureTag(dg)
	want := strings.Replace(dg.String(), ":", "-", 1) + ".sig"
	if got != want {
		t.Errorf("legacyCosignSignatureTag = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, "sha256-") || !strings.HasSuffix(got, ".sig") {
		t.Errorf("legacy tag %q does not look like a cosign signature tag", got)
	}
}

// recordingTagPusher is a content.Pusher that also records tags pushed to it.
type recordingTagPusher struct {
	pushed map[digest.Digest][]byte
	tagged map[string]digest.Digest
}

func newRecordingTagPusher() *recordingTagPusher {
	return &recordingTagPusher{pushed: map[digest.Digest][]byte{}, tagged: map[string]digest.Digest{}}
}

//nolint:gocritic // desc is passed by value to satisfy the content.Pusher interface signature.
func (p *recordingTagPusher) Push(ctx context.Context, desc ocispec.Descriptor, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	p.pushed[desc.Digest] = data
	return nil
}

//nolint:gocritic // desc is passed by value to satisfy the tagPusher interface signature.
func (p *recordingTagPusher) Tag(_ context.Context, desc ocispec.Descriptor, reference string) error {
	p.tagged[reference] = desc.Digest
	return nil
}

// TestSignArtifact_PublishesLegacySigTag verifies signArtifact tags the
// signature manifest with the cosign legacy .sig tag when the store supports
// tagging.
func TestSignArtifact_PublishesLegacySigTag(t *testing.T) {
	artifact := ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    ocispec.Descriptor{MediaType: mediaTypeAdapterConfig, Digest: digest.FromString("{}"), Size: 2},
	}
	artifactData, _ := json.Marshal(artifact)
	artifactDesc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(artifactData), Size: int64(len(artifactData))}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	ref := oci.Reference{Registry: "localhost:5001", Repo: "test/artifact", Tag: "v1"}
	p := newRecordingTagPusher()

	sigDesc, err := signArtifact(context.Background(), p, ref, &artifactDesc, KeySigner{Priv: priv})
	if err != nil {
		t.Fatalf("signArtifact: %v", err)
	}

	wantTag := legacyCosignSignatureTag(artifactDesc.Digest)
	if taggedDigest, ok := p.tagged[wantTag]; !ok {
		t.Errorf("signature manifest was not tagged with %q", wantTag)
	} else if taggedDigest != sigDesc.Digest {
		t.Errorf("tagged digest for %q = %q, want %q", wantTag, taggedDigest, sigDesc.Digest)
	}
}
