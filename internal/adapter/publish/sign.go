package publish

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	ctypes "github.com/sigstore/cosign/v2/pkg/types"
	"github.com/sigstore/sigstore-go/pkg/sign"
	"oras.land/oras-go/v2/content"

	"github.com/brokenbots/criteria/internal/adapter/oci"
)

// DefaultFulcioURL is the public-good Sigstore Fulcio CA used for keyless
// signing when no override is supplied.
const DefaultFulcioURL = "https://fulcio.sigstore.dev"

// cosignSignatureAnnotation is the manifest-layer annotation cosign uses to
// carry the base64-encoded signature. It is the key the verifier reads
// (see internal/adapter/signing.recordFromManifest).
const cosignSignatureAnnotation = "dev.cosignproject.cosign/signature"

// Signer produces a cosign signature over an artifact's simple-signing payload.
//
// Key mode (KeySigner) returns empty cert/chain. Keyless mode (KeylessSigner,
// Sigstore OIDC — CI only) returns the Fulcio leaf certificate PEM, which
// signArtifact attaches as the dev.sigstore.cosign/certificate annotation the
// verifier reads for keyless identities (signing.verifyKeylessLegacy).
type Signer interface {
	Sign(payload []byte) (sig []byte, certPEM, chainPEM string, err error)
}

// KeySigner signs with an in-process Ed25519 private key — the cosign
// explicit-key (`--key`) signing mode. The matching public key (PKIX DER) is
// recorded in the verifier's trust policy as a KeyIdentity.
type KeySigner struct {
	Priv ed25519.PrivateKey
}

// Sign implements Signer.
func (s KeySigner) Sign(payload []byte) (sig []byte, certPEM, chainPEM string, err error) {
	if len(s.Priv) == 0 {
		return nil, "", "", fmt.Errorf("publish: KeySigner has no private key")
	}
	return ed25519.Sign(s.Priv, payload), "", "", nil
}

// LoadKeySignerPEM loads a PKCS#8 PEM-encoded Ed25519 private key and returns a
// KeySigner. Used by `criteria adapter publish --sign-key`. Keyless (Sigstore
// OIDC) signing is a separate, CI-only signer.
func LoadKeySignerPEM(path string) (Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("publish: read signing key: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("publish: signing key %q is not PEM-encoded", path)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("publish: parse signing key: %w", err)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("publish: signing key must be Ed25519 (got %T)", key)
	}
	return KeySigner{Priv: priv}, nil
}

// KeylessSigner signs with an ephemeral key whose public key is bound to an
// OIDC identity by a short-lived Fulcio certificate — the cosign keyless
// (`--identity-token`) signing mode. It is CI-only: obtaining a Fulcio
// certificate requires an OIDC identity token from a trusted issuer (GitHub
// Actions, GitLab CI, Google, …). The matching public key never leaves the
// process; only the Fulcio leaf certificate is published, and the verifier
// chains it to the Sigstore roots (signing.verifyKeylessLegacy).
type KeylessSigner struct {
	keypair *sign.EphemeralKeypair
	certPEM string
}

// KeylessOptions configures a KeylessSigner.
type KeylessOptions struct {
	// IDToken is the OIDC identity token presented to Fulcio. Required.
	IDToken string
	// FulcioURL overrides the Fulcio CA endpoint. Defaults to DefaultFulcioURL.
	FulcioURL string
	// Transport is an optional HTTP transport for the Fulcio request, used by
	// tests to route to a mock CA. nil uses the default transport.
	Transport http.RoundTripper
}

// NewKeylessSigner generates an ephemeral keypair and exchanges opts.IDToken at
// Fulcio for a code-signing certificate over that key. The single network call
// (to Fulcio) happens here; Sign is purely local afterwards.
func NewKeylessSigner(ctx context.Context, opts KeylessOptions) (*KeylessSigner, error) {
	if opts.IDToken == "" {
		return nil, fmt.Errorf("publish: keyless signing requires an OIDC identity token")
	}
	fulcioURL := opts.FulcioURL
	if fulcioURL == "" {
		fulcioURL = DefaultFulcioURL
	}

	keypair, err := sign.NewEphemeralKeypair(nil) // ECDSA P-256 / SHA-256
	if err != nil {
		return nil, fmt.Errorf("publish: generate ephemeral key: %w", err)
	}

	fulcio := sign.NewFulcio(&sign.FulcioOptions{BaseURL: fulcioURL, Transport: opts.Transport})
	certDER, err := fulcio.GetCertificate(ctx, keypair, &sign.CertificateProviderOptions{IDToken: opts.IDToken})
	if err != nil {
		return nil, fmt.Errorf("publish: fulcio certificate request: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	return &KeylessSigner{keypair: keypair, certPEM: string(certPEM)}, nil
}

// Sign implements Signer. It signs the payload with the ephemeral key (ECDSA
// P-256 over SHA-256, the algorithm the Fulcio certificate certifies) and
// returns the leaf certificate so the verifier can establish the keyless
// identity. No chain PEM is emitted — the verifier supplies Fulcio intermediates
// from the Sigstore trusted root.
func (s *KeylessSigner) Sign(payload []byte) (sig []byte, certPEM, chainPEM string, err error) {
	if s.keypair == nil {
		return nil, "", "", fmt.Errorf("publish: KeylessSigner is not initialised")
	}
	sig, _, err = s.keypair.SignData(context.Background(), payload)
	if err != nil {
		return nil, "", "", fmt.Errorf("publish: keyless sign: %w", err)
	}
	return sig, s.certPEM, "", nil
}

// simpleSigningPayload builds the cosign "simple signing" payload that binds a
// signature to a specific artifact digest. This is the exact shape the verifier
// (and upstream cosign) expects; the signature is computed over these bytes.
func simpleSigningPayload(ref oci.Reference, artifactDigest digest.Digest) []byte {
	dockerRef := ref.Registry
	if ref.Repo != "" {
		dockerRef += "/" + ref.Repo
	}
	payload := struct {
		Critical struct {
			Identity struct {
				DockerReference string `json:"docker-reference"`
			} `json:"identity"`
			Image struct {
				DockerManifestDigest string `json:"docker-manifest-digest"`
			} `json:"image"`
			Type string `json:"type"`
		} `json:"critical"`
		Optional any `json:"optional"`
	}{}
	payload.Critical.Identity.DockerReference = dockerRef
	payload.Critical.Image.DockerManifestDigest = artifactDigest.String()
	payload.Critical.Type = "cosign container image signature"
	b, _ := json.Marshal(payload)
	return b
}

// buildSignatureManifest constructs the cosign signature manifest (an OCI
// referrer of artifactDesc) and its payload blob. Returned as in-memory bytes
// so callers can stage them in a store (remote push) or an on-disk layout
// (tests) identically.
func buildSignatureManifest(artifactDesc *ocispec.Descriptor, payload, sig []byte, certPEM, chainPEM string) (manifestJSON, payloadOut []byte) {
	payloadDesc := ocispec.Descriptor{
		MediaType: ctypes.SimpleSigningMediaType,
		Digest:    digest.FromBytes(payload),
		Size:      int64(len(payload)),
		Annotations: map[string]string{
			cosignSignatureAnnotation: base64.StdEncoding.EncodeToString(sig),
		},
	}
	if certPEM != "" {
		payloadDesc.Annotations["dev.sigstore.cosign/certificate"] = certPEM
	}
	if chainPEM != "" {
		payloadDesc.Annotations["dev.sigstore.cosign/chain"] = chainPEM
	}

	sigManifest := ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Subject: &ocispec.Descriptor{
			MediaType: artifactDesc.MediaType,
			Digest:    artifactDesc.Digest,
			Size:      artifactDesc.Size,
		},
		Layers: []ocispec.Descriptor{payloadDesc},
	}
	manifestJSON, _ = json.Marshal(sigManifest)
	return manifestJSON, payload
}

// signArtifact signs artifactDesc with signer and pushes the cosign signature
// manifest (a referrer) plus its payload blob into pusher. For a remote
// repository the registry records the referrer against the artifact; for an
// in-memory/on-disk store the manifest is staged for later copy. Returns the
// signature manifest descriptor.
func signArtifact(ctx context.Context, pusher content.Pusher, ref oci.Reference, artifactDesc *ocispec.Descriptor, signer Signer) (ocispec.Descriptor, error) {
	payload := simpleSigningPayload(ref, artifactDesc.Digest)
	sig, certPEM, chainPEM, err := signer.Sign(payload)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("publish: sign: %w", err)
	}

	manifestJSON, payloadBytes := buildSignatureManifest(artifactDesc, payload, sig, certPEM, chainPEM)

	payloadDesc := ocispec.Descriptor{
		MediaType: ctypes.SimpleSigningMediaType,
		Digest:    digest.FromBytes(payloadBytes),
		Size:      int64(len(payloadBytes)),
	}
	if err := pusher.Push(ctx, payloadDesc, bytes.NewReader(payloadBytes)); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("publish: push signature payload: %w", err)
	}

	sigDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    digest.FromBytes(manifestJSON),
		Size:      int64(len(manifestJSON)),
	}
	if err := pusher.Push(ctx, sigDesc, bytes.NewReader(manifestJSON)); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("publish: push signature manifest: %w", err)
	}
	return sigDesc, nil
}
