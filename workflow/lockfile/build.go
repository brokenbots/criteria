package lockfile

import (
	"fmt"

	"github.com/opencontainers/go-digest"
)

// BuildInput carries the data required to assemble a LockedAdapter.
// Callers (e.g. WS08 CLI) populate this from the results of a pull,
// manifest parse, and optional signer verification.
type BuildInput struct {
	Type                           string
	Name                           string
	Reference                      string
	ResolvedDigest                 digest.Digest
	SourceURL                      string
	SDKProtocolVersion             int
	Platforms                      []string
	ContainerImage                 *LockedContainerImage
	Signer                         *LockedSignature
	Remote                         *LockedRemote
	CompatibleEnvironmentsOverride []string
	OverriddenBy                   string
}

// BuildEntry assembles a LockedAdapter from a successful pull.
func BuildEntry(in *BuildInput) (LockedAdapter, error) {
	if in == nil {
		return LockedAdapter{}, fmt.Errorf("build input is nil")
	}
	if in.Type == "" {
		return LockedAdapter{}, fmt.Errorf("adapter type is required")
	}
	if in.Name == "" {
		return LockedAdapter{}, fmt.Errorf("adapter name is required")
	}
	if in.Reference == "" {
		return LockedAdapter{}, fmt.Errorf("reference is required")
	}
	if in.ResolvedDigest == "" {
		return LockedAdapter{}, fmt.Errorf("resolved_digest is required")
	}

	return LockedAdapter{
		Type:                           in.Type,
		Name:                           in.Name,
		Reference:                      in.Reference,
		ResolvedDigest:                 in.ResolvedDigest.String(),
		SourceURL:                      in.SourceURL,
		SDKProtocolVersion:             in.SDKProtocolVersion,
		Platforms:                      copyStringSlice(in.Platforms),
		Signature:                      cloneSignature(in.Signer),
		ContainerImage:                 cloneContainerImage(in.ContainerImage),
		Remote:                         cloneRemote(in.Remote),
		CompatibleEnvironmentsOverride: copyStringSlice(in.CompatibleEnvironmentsOverride),
		OverriddenBy:                   in.OverriddenBy,
	}, nil
}

func copyStringSlice(s []string) []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}

func cloneSignature(s *LockedSignature) *LockedSignature {
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

func cloneContainerImage(c *LockedContainerImage) *LockedContainerImage {
	if c == nil {
		return nil
	}
	return &LockedContainerImage{
		Ref:    c.Ref,
		Digest: c.Digest,
	}
}

func cloneRemote(r *LockedRemote) *LockedRemote {
	if r == nil {
		return nil
	}
	return &LockedRemote{
		ListenAddress:         r.ListenAddress,
		ServerCertFingerprint: r.ServerCertFingerprint,
	}
}
