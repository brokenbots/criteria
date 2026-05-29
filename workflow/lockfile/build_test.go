package lockfile_test

import (
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/brokenbots/criteria/internal/adapter/manifest"
	"github.com/brokenbots/criteria/internal/adapter/oci"
	"github.com/brokenbots/criteria/internal/adapter/signing"

	"github.com/brokenbots/criteria/workflow/lockfile"
)

func TestBuildEntry_Full(t *testing.T) {
	ref := oci.Reference{Registry: "ghcr.io", Repo: "criteria-adapters/claude", Tag: "1.2.3"}
	dg := digest.Digest("sha256:abc123")
	m := &manifest.Manifest{
		SourceURL:          "https://github.com/criteria-adapters/claude",
		SDKProtocolVersion: 2,
		Platforms: []manifest.Platform{
			{OS: "linux", Arch: "amd64"},
			{OS: "linux", Arch: "arm64"},
		},
		ContainerImage: &manifest.ContainerImageRef{
			Ref:    "ghcr.io/criteria-adapters/claude:1.2.3-image",
			Digest: "sha256:img123",
		},
	}
	signer := &signing.SignerIdentity{
		Keyless: &signing.KeylessIdentity{
			Issuer:  "https://token.actions.githubusercontent.com",
			Subject: "subj",
		},
	}
	remote := &lockfile.RemoteFields{
		ListenAddress:         "0.0.0.0:7778",
		ServerCertFingerprint: "sha256:cert123",
	}

	a, err := lockfile.BuildEntry(ref, dg, m, signer, remote)
	require.NoError(t, err)

	// Type and Name are workflow-scoped; caller must set them.
	assert.Empty(t, a.Type)
	assert.Empty(t, a.Name)

	assert.Equal(t, "ghcr.io/criteria-adapters/claude:1.2.3", a.Reference)
	assert.Equal(t, "sha256:abc123", a.ResolvedDigest)
	assert.Equal(t, "https://github.com/criteria-adapters/claude", a.SourceURL)
	assert.Equal(t, 2, a.SDKProtocolVersion)
	assert.Equal(t, []string{"linux/amd64", "linux/arm64"}, a.Platforms)

	require.NotNil(t, a.ContainerImage)
	assert.Equal(t, "ghcr.io/criteria-adapters/claude:1.2.3-image", a.ContainerImage.Ref)
	assert.Equal(t, "sha256:img123", a.ContainerImage.Digest)

	require.NotNil(t, a.Signature)
	require.NotNil(t, a.Signature.Keyless)
	assert.Equal(t, "https://token.actions.githubusercontent.com", a.Signature.Keyless.Issuer)
	assert.Equal(t, "subj", a.Signature.Keyless.Subject)
	assert.Nil(t, a.Signature.Key)

	require.NotNil(t, a.Remote)
	assert.Equal(t, "0.0.0.0:7778", a.Remote.ListenAddress)
	assert.Equal(t, "sha256:cert123", a.Remote.ServerCertFingerprint)
}

func TestBuildEntry_KeySigner(t *testing.T) {
	ref := oci.Reference{Registry: "ghcr.io", Repo: "criteria-adapters/copilot", Tag: "0.5.0"}
	dg := digest.Digest("sha256:d")
	m := &manifest.Manifest{SourceURL: "https://github.com/criteria-adapters/copilot", SDKProtocolVersion: 2}
	signer := &signing.SignerIdentity{
		Key: &signing.KeyIdentity{
			Algorithm:   "ed25519",
			Fingerprint: "sha256:fp",
		},
	}

	a, err := lockfile.BuildEntry(ref, dg, m, signer, nil)
	require.NoError(t, err)
	require.NotNil(t, a.Signature)
	require.NotNil(t, a.Signature.Key)
	assert.Equal(t, "ed25519", a.Signature.Key.Algorithm)
	assert.Equal(t, "sha256:fp", a.Signature.Key.Fingerprint)
	assert.Nil(t, a.Signature.Keyless)
}

func TestBuildEntry_NilOptionalFields(t *testing.T) {
	ref := oci.Reference{Registry: "ghcr.io", Repo: "criteria-adapters/noop", Tag: "latest"}
	dg := digest.Digest("sha256:d")
	m := &manifest.Manifest{SourceURL: "https://github.com/criteria-adapters/noop", SDKProtocolVersion: 2}

	a, err := lockfile.BuildEntry(ref, dg, m, nil, nil)
	require.NoError(t, err)
	assert.Nil(t, a.Signature)
	assert.Nil(t, a.ContainerImage)
	assert.Nil(t, a.Remote)
	assert.Empty(t, a.Platforms)
	assert.Empty(t, a.CompatibleEnvironmentsOverride)
	assert.Empty(t, a.OverriddenBy)
}

func TestBuildEntry_NilManifest(t *testing.T) {
	ref := oci.Reference{Registry: "ghcr.io", Repo: "criteria-adapters/noop", Tag: "latest"}
	dg := digest.Digest("sha256:d")
	_, err := lockfile.BuildEntry(ref, dg, nil, nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "manifest is required")
}

func TestBuildEntry_EmptyDigest(t *testing.T) {
	ref := oci.Reference{Registry: "ghcr.io", Repo: "criteria-adapters/noop", Tag: "latest"}
	m := &manifest.Manifest{SourceURL: "https://github.com/criteria-adapters/noop", SDKProtocolVersion: 2}
	_, err := lockfile.BuildEntry(ref, digest.Digest(""), m, nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resolved digest is required")
}

func TestBuildEntry_SliceIsolation(t *testing.T) {
	ref := oci.Reference{Registry: "ghcr.io", Repo: "criteria-adapters/noop", Tag: "latest"}
	dg := digest.Digest("sha256:d")
	m := &manifest.Manifest{
		SourceURL:          "https://github.com/criteria-adapters/noop",
		SDKProtocolVersion: 2,
		Platforms: []manifest.Platform{
			{OS: "linux", Arch: "amd64"},
		},
	}

	a, err := lockfile.BuildEntry(ref, dg, m, nil, nil)
	require.NoError(t, err)

	// Mutate the manifest platforms after the call.
	m.Platforms[0].OS = "tampered"

	assert.Equal(t, []string{"linux/amd64"}, a.Platforms)
}
