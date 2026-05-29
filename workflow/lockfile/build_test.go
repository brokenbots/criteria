package lockfile_test

import (
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/brokenbots/criteria/workflow/lockfile"
)

func TestBuildEntry_Full(t *testing.T) {
	in := &lockfile.BuildInput{
		Type:               "claude",
		Name:               "default",
		Reference:          "ghcr.io/criteria-adapters/claude:1.2.3",
		ResolvedDigest:     digest.Digest("sha256:abc123"),
		SourceURL:          "https://github.com/criteria-adapters/claude",
		SDKProtocolVersion: 2,
		Platforms:          []string{"linux/amd64", "linux/arm64"},
		ContainerImage: &lockfile.LockedContainerImage{
			Ref:    "ghcr.io/criteria-adapters/claude:1.2.3-image",
			Digest: "sha256:img123",
		},
		Signer: &lockfile.LockedSignature{
			Keyless: &lockfile.LockedKeyless{
				Issuer:  "https://token.actions.githubusercontent.com",
				Subject: "subj",
			},
		},
		Remote: &lockfile.LockedRemote{
			ListenAddress:         "0.0.0.0:7778",
			ServerCertFingerprint: "sha256:cert123",
		},
		CompatibleEnvironmentsOverride: []string{"shell"},
		OverriddenBy:                   "workflow.hcl:42",
	}

	a, err := lockfile.BuildEntry(in)
	require.NoError(t, err)

	assert.Equal(t, "claude", a.Type)
	assert.Equal(t, "default", a.Name)
	assert.Equal(t, "ghcr.io/criteria-adapters/claude:1.2.3", a.Reference)
	assert.Equal(t, "sha256:abc123", a.ResolvedDigest)
	assert.Equal(t, "https://github.com/criteria-adapters/claude", a.SourceURL)
	assert.Equal(t, 2, a.SDKProtocolVersion)
	assert.Equal(t, []string{"linux/amd64", "linux/arm64"}, a.Platforms)

	require.NotNil(t, a.ContainerImage)
	assert.Equal(t, "sha256:img123", a.ContainerImage.Digest)

	require.NotNil(t, a.Signature)
	require.NotNil(t, a.Signature.Keyless)
	assert.Equal(t, "subj", a.Signature.Keyless.Subject)

	require.NotNil(t, a.Remote)
	assert.Equal(t, "sha256:cert123", a.Remote.ServerCertFingerprint)

	assert.Equal(t, []string{"shell"}, a.CompatibleEnvironmentsOverride)
	assert.Equal(t, "workflow.hcl:42", a.OverriddenBy)
}

func TestBuildEntry_KeySigner(t *testing.T) {
	in := &lockfile.BuildInput{
		Type:           "copilot",
		Name:           "default",
		Reference:      "r",
		ResolvedDigest: digest.Digest("sha256:d"),
		Signer: &lockfile.LockedSignature{
			Key: &lockfile.LockedKey{
				Algorithm:   "ed25519",
				Fingerprint: "sha256:fp",
			},
		},
	}

	a, err := lockfile.BuildEntry(in)
	require.NoError(t, err)
	require.NotNil(t, a.Signature)
	require.NotNil(t, a.Signature.Key)
	assert.Equal(t, "ed25519", a.Signature.Key.Algorithm)
	assert.Equal(t, "sha256:fp", a.Signature.Key.Fingerprint)
	assert.Nil(t, a.Signature.Keyless)
}

func TestBuildEntry_NilOptionalFields(t *testing.T) {
	in := &lockfile.BuildInput{
		Type:           "noop",
		Name:           "default",
		Reference:      "r",
		ResolvedDigest: digest.Digest("sha256:d"),
	}

	a, err := lockfile.BuildEntry(in)
	require.NoError(t, err)
	assert.Nil(t, a.Signature)
	assert.Nil(t, a.ContainerImage)
	assert.Nil(t, a.Remote)
	assert.Empty(t, a.Platforms)
	assert.Empty(t, a.CompatibleEnvironmentsOverride)
	assert.Empty(t, a.OverriddenBy)
}

func TestBuildEntry_MissingType(t *testing.T) {
	in := &lockfile.BuildInput{
		Name:           "default",
		Reference:      "r",
		ResolvedDigest: digest.Digest("sha256:d"),
	}
	_, err := lockfile.BuildEntry(in)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "type is required")
}

func TestBuildEntry_MissingName(t *testing.T) {
	in := &lockfile.BuildInput{
		Type:           "noop",
		Reference:      "r",
		ResolvedDigest: digest.Digest("sha256:d"),
	}
	_, err := lockfile.BuildEntry(in)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "name is required")
}

func TestBuildEntry_MissingReference(t *testing.T) {
	in := &lockfile.BuildInput{
		Type:           "noop",
		Name:           "default",
		ResolvedDigest: digest.Digest("sha256:d"),
	}
	_, err := lockfile.BuildEntry(in)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reference is required")
}

func TestBuildEntry_MissingDigest(t *testing.T) {
	in := &lockfile.BuildInput{
		Type:      "noop",
		Name:      "default",
		Reference: "r",
	}
	_, err := lockfile.BuildEntry(in)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resolved_digest is required")
}

func TestBuildEntry_SliceIsolation(t *testing.T) {
	in := &lockfile.BuildInput{
		Type:                           "noop",
		Name:                           "default",
		Reference:                      "r",
		ResolvedDigest:                 digest.Digest("sha256:d"),
		Platforms:                      []string{"linux/amd64"},
		CompatibleEnvironmentsOverride: []string{"shell"},
	}

	a, err := lockfile.BuildEntry(in)
	require.NoError(t, err)

	// Mutate the input slices after the call.
	in.Platforms[0] = "tampered"
	in.CompatibleEnvironmentsOverride[0] = "tampered"

	assert.Equal(t, []string{"linux/amd64"}, a.Platforms)
	assert.Equal(t, []string{"shell"}, a.CompatibleEnvironmentsOverride)
}
