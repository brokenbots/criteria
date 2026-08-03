package publish

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	ctypes "github.com/sigstore/cosign/v3/pkg/types"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/sign"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"

	"github.com/brokenbots/criteria/internal/adapter/oci"
)

// DefaultFulcioURL is the public-good Sigstore Fulcio CA used for keyless
// signing when no override is supplied.
const DefaultFulcioURL = "https://fulcio.sigstore.dev"

// cosignSignatureAnnotation is the manifest-layer annotation cosign uses to
// carry the base64-encoded signature. It is the key the verifier reads
// (see internal/adapter/signing.recordFromManifest).
const cosignSignatureAnnotation = "dev.cosignproject.cosign/signature"

// cosignSignatureArtifactType is the OCI manifest ArtifactType cosign filters on
// when discovering signatures via OCI 1.1 referrers. It matches the value
// github.com/sigstore/cosign/v3/pkg/oci/remote tests expect for a signature
// referrer (wantArtifactType in write_test.go). Cosign does not export the
// constant, so we keep a local mirror with its source noted.
const cosignSignatureArtifactType = "application/vnd.dev.cosign.artifact.sig.v1+json"

// SignResult is the output of a Signer: the raw signature over the
// simple-signing payload plus, for keyless mode, the Fulcio leaf certificate and
// a Sigstore bundle (cert + transparency-log inclusion proof + signed entry
// timestamp). signArtifact attaches each non-empty field as the corresponding
// cosign annotation the verifier reads.
type SignResult struct {
	Signature []byte // raw signature over the payload (dev.cosignproject.cosign/signature)
	CertPEM   string // Fulcio leaf certificate PEM, keyless only (dev.sigstore.cosign/certificate)
	ChainPEM  string // certificate chain PEM, optional (dev.sigstore.cosign/chain)
	Bundle    []byte // Sigstore protobundle JSON, keyless only (dev.sigstore.cosign/bundle)
}

// Signer produces a cosign signature over an artifact's simple-signing payload.
//
// Key mode (KeySigner) returns only a signature. Keyless mode (KeylessSigner,
// Sigstore OIDC — CI only) returns the Fulcio leaf certificate PEM and a
// Sigstore bundle carrying the Rekor transparency-log proof so the signature
// remains verifiable after the ~10-minute Fulcio certificate expires
// (signing.verifyKeylessBundle).
type Signer interface {
	Sign(payload []byte) (SignResult, error)
}

// KeySigner signs with an in-process Ed25519 private key — the cosign
// explicit-key (`--key`) signing mode. The matching public key (PKIX DER) is
// recorded in the verifier's trust policy as a KeyIdentity.
type KeySigner struct {
	Priv ed25519.PrivateKey
}

// Sign implements Signer.
func (s KeySigner) Sign(payload []byte) (SignResult, error) {
	if len(s.Priv) == 0 {
		return SignResult{}, fmt.Errorf("publish: KeySigner has no private key")
	}
	return SignResult{Signature: ed25519.Sign(s.Priv, payload)}, nil
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

// DefaultRekorURL is the public-good Sigstore Rekor transparency log used to
// record keyless signatures when no override is supplied.
const DefaultRekorURL = "https://rekor.sigstore.dev"

// KeylessSigner signs with an ephemeral key whose public key is bound to an
// OIDC identity by a short-lived Fulcio certificate — the cosign keyless
// (`--identity-token`) signing mode. It is CI-only: obtaining a Fulcio
// certificate requires an OIDC identity token from a trusted issuer (GitHub
// Actions, GitLab CI, Google, …).
//
// Sign assembles a Sigstore bundle (Fulcio leaf certificate + Rekor inclusion
// proof + signed entry timestamp). The Rekor entry is what keeps the signature
// verifiable after the ~10-minute Fulcio certificate expires: the verifier
// checks the certificate at the log timestamp, not at verification time
// (signing.verifyKeylessBundle).
type KeylessSigner struct {
	keypair *sign.EphemeralKeypair
	fulcio  *sign.Fulcio
	rekor   *sign.Rekor // nil disables transparency-log inclusion (offline/tests)
	idToken string
}

// KeylessOptions configures a KeylessSigner.
type KeylessOptions struct {
	// IDToken is the OIDC identity token presented to Fulcio. Required.
	IDToken string
	// FulcioURL overrides the Fulcio CA endpoint. Defaults to DefaultFulcioURL.
	FulcioURL string
	// RekorURL overrides the Rekor transparency-log endpoint. Empty disables
	// transparency-log inclusion (the resulting bundle cannot be verified after
	// certificate expiry — used only by offline tests); production callers set
	// this to DefaultRekorURL.
	RekorURL string
	// Transport is an optional HTTP transport for the Fulcio request, used by
	// tests to route to a mock CA. nil uses the default transport.
	Transport http.RoundTripper
}

// NewKeylessSigner generates an ephemeral keypair and prepares the Fulcio (and,
// when RekorURL is set, Rekor) clients. The network calls happen in Sign, which
// has the payload to sign and submit.
func NewKeylessSigner(_ context.Context, opts KeylessOptions) (*KeylessSigner, error) {
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

	s := &KeylessSigner{
		keypair: keypair,
		fulcio:  sign.NewFulcio(&sign.FulcioOptions{BaseURL: fulcioURL, Transport: opts.Transport}),
		idToken: opts.IDToken,
	}
	if opts.RekorURL != "" {
		s.rekor = sign.NewRekor(&sign.RekorOptions{BaseURL: opts.RekorURL})
	}
	return s, nil
}

// Sign implements Signer. It signs the payload with the ephemeral key (ECDSA
// P-256 over SHA-256), obtains a Fulcio certificate, optionally records the
// signature in Rekor, and assembles a Sigstore bundle. The leaf certificate and
// bundle let the verifier establish the keyless identity and verify it against
// the transparency-log timestamp.
func (s *KeylessSigner) Sign(payload []byte) (SignResult, error) {
	if s.keypair == nil || s.fulcio == nil {
		return SignResult{}, fmt.Errorf("publish: KeylessSigner is not initialised")
	}

	bopts := sign.BundleOptions{
		CertificateProvider:        s.fulcio,
		CertificateProviderOptions: &sign.CertificateProviderOptions{IDToken: s.idToken},
		Context:                    context.Background(),
	}
	if s.rekor != nil {
		bopts.TransparencyLogs = []sign.Transparency{s.rekor}
	}

	pb, err := sign.Bundle(&sign.PlainData{Data: payload}, s.keypair, bopts)
	if err != nil {
		return SignResult{}, fmt.Errorf("publish: keyless bundle: %w", err)
	}

	b, err := bundle.NewBundle(pb)
	if err != nil {
		return SignResult{}, fmt.Errorf("publish: assemble bundle: %w", err)
	}
	bundleJSON, err := b.MarshalJSON()
	if err != nil {
		return SignResult{}, fmt.Errorf("publish: marshal bundle: %w", err)
	}

	res := SignResult{
		Signature: pb.GetMessageSignature().GetSignature(),
		Bundle:    bundleJSON,
	}
	if cert := pb.GetVerificationMaterial().GetCertificate(); cert != nil {
		res.CertPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.GetRawBytes()}))
	}
	return res, nil
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
func buildSignatureManifest(artifactDesc *ocispec.Descriptor, payload []byte, res *SignResult) (manifestJSON, payloadOut []byte) {
	payloadDesc := ocispec.Descriptor{
		MediaType: ctypes.SimpleSigningMediaType,
		Digest:    digest.FromBytes(payload),
		Size:      int64(len(payload)),
		Annotations: map[string]string{
			cosignSignatureAnnotation: base64.StdEncoding.EncodeToString(res.Signature),
		},
	}
	if res.CertPEM != "" {
		payloadDesc.Annotations["dev.sigstore.cosign/certificate"] = res.CertPEM
	}
	if res.ChainPEM != "" {
		payloadDesc.Annotations["dev.sigstore.cosign/chain"] = res.ChainPEM
	}
	if len(res.Bundle) > 0 {
		payloadDesc.Annotations["dev.sigstore.cosign/bundle"] = string(res.Bundle)
	}

	sigManifest := ocispec.Manifest{
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: cosignSignatureArtifactType,
		// An OCI image manifest requires a valid config descriptor. Without one
		// the zero value marshals as {"mediaType":"","digest":"","size":0},
		// which strict registries (notably GHCR) reject with a 500 on push. Use
		// the standard OCI 1.1 empty-JSON config (the same shape cosign/oras
		// produce for referrer artifacts); the empty blob is pushed in
		// signArtifact.
		Config: ocispec.DescriptorEmptyJSON,
		Subject: &ocispec.Descriptor{
			MediaType: artifactDesc.MediaType,
			Digest:    artifactDesc.Digest,
			Size:      artifactDesc.Size,
		},
		Layers: []ocispec.Descriptor{payloadDesc},
	}
	// SchemaVersion 2 is mandatory; set via the promoted field to avoid importing
	// specs-go just for the struct literal.
	sigManifest.SchemaVersion = 2
	manifestJSON, _ = json.Marshal(sigManifest)
	return manifestJSON, payload
}

// tagPusher is a store that can both push manifests and tag them by reference.
// remote.Repository and oras memory/oci stores implement this.
type tagPusher interface {
	content.Pusher
	Tag(ctx context.Context, desc ocispec.Descriptor, reference string) error
}

// signArtifact signs artifactDesc with signer and pushes the cosign signature
// manifest (a referrer) plus its payload blob into pusher. For a remote
// repository the registry records the referrer against the artifact; for an
// in-memory/on-disk store the manifest is staged for later copy. Returns the
// signature manifest descriptor.
//
// To stay discoverable by default (non-experimental) cosign, the signature
// manifest is also tagged as sha256-<algorithm>-<hex>.sig, the legacy tag
// cosign's non-OCI-1.1 path resolves.
func signArtifact(ctx context.Context, pusher content.Pusher, ref oci.Reference, artifactDesc *ocispec.Descriptor, signer Signer) (ocispec.Descriptor, error) {
	payload := simpleSigningPayload(ref, artifactDesc.Digest)
	res, err := signer.Sign(payload)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("publish: sign: %w", err)
	}

	manifestJSON, payloadBytes := buildSignatureManifest(artifactDesc, payload, &res)

	payloadDesc := ocispec.Descriptor{
		MediaType: ctypes.SimpleSigningMediaType,
		Digest:    digest.FromBytes(payloadBytes),
		Size:      int64(len(payloadBytes)),
	}
	if err := pusher.Push(ctx, payloadDesc, bytes.NewReader(payloadBytes)); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("publish: push signature payload: %w", err)
	}

	// Push the empty-JSON config blob the signature manifest references. It is
	// content-addressed by digest, so it may already exist in the repo (the
	// artifact's own config is also "{}"); treat already-exists as success.
	emptyCfg := ocispec.DescriptorEmptyJSON
	if err := pusher.Push(ctx, emptyCfg, bytes.NewReader(emptyCfg.Data)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		return ocispec.Descriptor{}, fmt.Errorf("publish: push signature config: %w", err)
	}

	sigDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    digest.FromBytes(manifestJSON),
		Size:      int64(len(manifestJSON)),
	}
	if err := pusher.Push(ctx, sigDesc, bytes.NewReader(manifestJSON)); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("publish: push signature manifest: %w", err)
	}

	// Also publish the legacy cosign signature tag so default (non-OCI-1.1)
	// cosign verification can find the signature. The tag is content-addressed
	// from the artifact digest and does not conflict with the OCI 1.1 referrers
	// fallback index tag (sha256-<algorithm>-<hex>).
	if tp, ok := pusher.(tagPusher); ok {
		sigTag := legacyCosignSignatureTag(artifactDesc.Digest)
		if err := tp.Tag(ctx, sigDesc, sigTag); err != nil {
			return ocispec.Descriptor{}, fmt.Errorf("publish: tag signature manifest %q: %w", sigTag, err)
		}
	}

	return sigDesc, nil
}

// legacyCosignSignatureTag returns the cosign legacy signature tag for a digest:
// "sha256-<algorithm>-<hex>.sig". This matches cosign's SignatureTag naming
// (github.com/sigstore/cosign/v3/pkg/oci/remote.normalize).
func legacyCosignSignatureTag(d digest.Digest) string {
	return strings.Replace(d.String(), ":", "-", 1) + ".sig"
}
