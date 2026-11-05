package lockfile

import (
	"fmt"

	"github.com/opencontainers/go-digest"

	"github.com/brokenbots/criteria/internal/adapter/manifest"
	"github.com/brokenbots/criteria/internal/adapter/oci"
	"github.com/brokenbots/criteria/internal/adapter/signing"
)

// BuildEntry assembles a LockedAdapter from a successful pull.
//
// Fields populated from the inputs:
//   - Reference from ref.String()
//   - ResolvedDigest from dg.String()
//   - SourceURL, SDKProtocolVersion, Platforms, and ContainerImage from m
//   - Signature from signer (may be nil if unsigned and policy allows it)
//   - Remote from remote (may be nil if not bound to a remote environment)
//
// The lockfile is the trust anchor for every later pull/apply: a keyless
// signature lacking its OIDC issuer or subject is rejected here so an
// un-pinnable signer never becomes "whatever the policy default allows".
//
// The caller (e.g. WS08) must set Type and Name on the returned value,
// since those are workflow-scoped identifiers not present in the pull inputs.
func BuildEntry(ref oci.Reference, dg digest.Digest, m *manifest.Manifest, signer *signing.SignerIdentity, remote *RemoteFields) (LockedAdapter, error) {
	if m == nil {
		return LockedAdapter{}, fmt.Errorf("manifest is required")
	}
	if dg == "" {
		return LockedAdapter{}, fmt.Errorf("resolved digest is required")
	}
	if signer != nil && signer.Keyless != nil {
		k := signer.Keyless
		switch {
		case k.Issuer == "" && k.Subject == "":
			return LockedAdapter{}, fmt.Errorf("keyless signature is missing both its OIDC issuer and workflow subject; the lockfile pin must record both for the pull to be lockable")
		case k.Issuer == "":
			return LockedAdapter{}, fmt.Errorf("keyless signature is missing its OIDC issuer; the lockfile pin must record the issuer for the pull to be lockable")
		case k.Subject == "":
			return LockedAdapter{}, fmt.Errorf("keyless signature is missing its workflow subject; the lockfile pin must record the subject for the pull to be lockable")
		}
	}

	platforms := make([]string, 0, len(m.Platforms))
	for _, p := range m.Platforms {
		platforms = append(platforms, p.OS+"/"+p.Arch)
	}

	return LockedAdapter{
		Reference:          ref.String(),
		Version:            ref.Tag,
		ResolvedDigest:     dg.String(),
		SourceURL:          m.SourceURL,
		SDKProtocolVersion: m.SDKProtocolVersion,
		Platforms:          platforms,
		Signature:          lockedSignatureFromSigner(signer),
		ContainerImage:     lockedContainerImageFromManifest(m.ContainerImage),
		Remote:             lockedRemoteFromFields(remote),
	}, nil
}

func lockedSignatureFromSigner(s *signing.SignerIdentity) *LockedSignature {
	if s == nil {
		return nil
	}
	out := &LockedSignature{}
	if s.Keyless != nil {
		out.Keyless = &LockedKeyless{
			Issuer:  s.Keyless.Issuer,
			Subject: s.Keyless.Subject,
		}
	}
	if s.Key != nil {
		out.Key = &LockedKey{
			Algorithm:   s.Key.Algorithm,
			Fingerprint: s.Key.Fingerprint,
		}
	}
	return out
}

func lockedContainerImageFromManifest(ci *manifest.ContainerImageRef) *LockedContainerImage {
	if ci == nil {
		return nil
	}
	return &LockedContainerImage{
		Ref:    ci.Ref,
		Digest: ci.Digest,
	}
}

func lockedRemoteFromFields(r *RemoteFields) *LockedRemote {
	if r == nil {
		return nil
	}
	return &LockedRemote{
		ListenAddress:         r.ListenAddress,
		ServerCertFingerprint: r.ServerCertFingerprint,
	}
}
