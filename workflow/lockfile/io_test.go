package lockfile_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/brokenbots/criteria/workflow/lockfile"
)

func TestRead_FullFixture(t *testing.T) {
	lf, err := lockfile.Read("testdata/full.lock.hcl")
	require.NoError(t, err)
	require.NotNil(t, lf)

	assert.Equal(t, 1, lf.SchemaVersion)
	require.Len(t, lf.Adapters, 2)

	// First adapter: claude.default (sorted alphabetically before copilot.default)
	a := lf.Adapters[0]
	assert.Equal(t, "claude", a.Type)
	assert.Equal(t, "default", a.Name)
	assert.Equal(t, "ghcr.io/criteria-adapters/claude:1.2.3", a.Reference)
	assert.Equal(t, "sha256:abc123def456", a.ResolvedDigest)
	assert.Equal(t, "https://github.com/criteria-adapters/claude", a.SourceURL)
	assert.Equal(t, 2, a.SDKProtocolVersion)
	assert.Equal(t, []string{"linux/amd64", "linux/arm64", "darwin/arm64"}, a.Platforms)

	require.NotNil(t, a.Signature)
	require.NotNil(t, a.Signature.Keyless)
	assert.Equal(t, "https://token.actions.githubusercontent.com", a.Signature.Keyless.Issuer)
	assert.Equal(t, "https://github.com/criteria-adapters/claude/.github/workflows/publish.yml@refs/tags/v1.2.3", a.Signature.Keyless.Subject)
	assert.Nil(t, a.Signature.Key)

	require.NotNil(t, a.ContainerImage)
	assert.Equal(t, "ghcr.io/criteria-adapters/claude:1.2.3-image", a.ContainerImage.Ref)
	assert.Equal(t, "sha256:def456abc789", a.ContainerImage.Digest)

	assert.Nil(t, a.Remote)
	assert.Empty(t, a.CompatibleEnvironmentsOverride)
	assert.Empty(t, a.OverriddenBy)

	// Second adapter: copilot.default
	a = lf.Adapters[1]
	assert.Equal(t, "copilot", a.Type)
	assert.Equal(t, "default", a.Name)
	assert.Equal(t, "ghcr.io/criteria-adapters/copilot:0.5.0", a.Reference)
	assert.Equal(t, "sha256:789012", a.ResolvedDigest)
	assert.Equal(t, 2, a.SDKProtocolVersion)
	assert.Equal(t, []string{"linux/amd64"}, a.Platforms)

	require.NotNil(t, a.Signature)
	assert.Nil(t, a.Signature.Keyless)
	require.NotNil(t, a.Signature.Key)
	assert.Equal(t, "ed25519", a.Signature.Key.Algorithm)
	assert.Equal(t, "sha256:pubkeyfp", a.Signature.Key.Fingerprint)

	require.NotNil(t, a.Remote)
	assert.Equal(t, "0.0.0.0:7778", a.Remote.ListenAddress)
	assert.Equal(t, "sha256:certfp", a.Remote.ServerCertFingerprint)

	assert.Equal(t, []string{"shell"}, a.CompatibleEnvironmentsOverride)
	assert.Equal(t, "workflow.hcl:42", a.OverriddenBy)
}

func TestRead_MinimalFixture(t *testing.T) {
	lf, err := lockfile.Read("testdata/minimal.lock.hcl")
	require.NoError(t, err)
	require.Len(t, lf.Adapters, 1)

	a := lf.Adapters[0]
	assert.Equal(t, "noop", a.Type)
	assert.Equal(t, "default", a.Name)
	assert.Equal(t, "sha256:000000", a.ResolvedDigest)
	assert.Nil(t, a.Signature)
	assert.Nil(t, a.ContainerImage)
	assert.Nil(t, a.Remote)
	assert.Empty(t, a.Platforms)
	assert.Empty(t, a.CompatibleEnvironmentsOverride)
}

func TestRead_RemoteFixture(t *testing.T) {
	lf, err := lockfile.Read("testdata/remote.lock.hcl")
	require.NoError(t, err)
	require.Len(t, lf.Adapters, 1)

	a := lf.Adapters[0]
	assert.Equal(t, "copilot", a.Type)
	require.NotNil(t, a.Remote)
	assert.Equal(t, "0.0.0.0:7778", a.Remote.ListenAddress)
	assert.Equal(t, "sha256:certfp", a.Remote.ServerCertFingerprint)
	assert.Nil(t, a.Signature)
	assert.Nil(t, a.ContainerImage)
}

func TestRead_ContainerImageFixture(t *testing.T) {
	lf, err := lockfile.Read("testdata/container_image.lock.hcl")
	require.NoError(t, err)
	require.Len(t, lf.Adapters, 1)

	a := lf.Adapters[0]
	require.NotNil(t, a.ContainerImage)
	assert.Equal(t, "ghcr.io/criteria-adapters/claude:1.2.3-image", a.ContainerImage.Ref)
	assert.Equal(t, "sha256:def456abc789", a.ContainerImage.Digest)
	assert.Nil(t, a.Signature)
	assert.Nil(t, a.Remote)
}

func TestWrite_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".criteria.lock.hcl")

	original := &lockfile.Lockfile{
		SchemaVersion: 1,
		Adapters: []lockfile.LockedAdapter{
			{
				Type:               "claude",
				Name:               "default",
				Reference:          "ghcr.io/criteria-adapters/claude:1.2.3",
				ResolvedDigest:     "sha256:abc123",
				SourceURL:          "https://github.com/criteria-adapters/claude",
				SDKProtocolVersion: 2,
				Platforms:          []string{"linux/amd64", "linux/arm64"},
				Signature: &lockfile.LockedSignature{
					Keyless: &lockfile.LockedKeyless{
						Issuer:  "https://token.actions.githubusercontent.com",
						Subject: "subj",
					},
				},
				ContainerImage: &lockfile.LockedContainerImage{
					Ref:    "ghcr.io/criteria-adapters/claude:1.2.3-image",
					Digest: "sha256:img123",
				},
				Remote: &lockfile.LockedRemote{
					ListenAddress:         "0.0.0.0:7778",
					ServerCertFingerprint: "sha256:cert123",
				},
				CompatibleEnvironmentsOverride: []string{"shell"},
				OverriddenBy:                   "workflow.hcl:7",
			},
			{
				Type:               "noop",
				Name:               "default",
				Reference:          "ghcr.io/criteria-adapters/noop:latest",
				ResolvedDigest:     "sha256:000000",
				SourceURL:          "https://github.com/criteria-adapters/noop",
				SDKProtocolVersion: 2,
			},
		},
	}

	require.NoError(t, lockfile.Write(path, original))

	reloaded, err := lockfile.Read(path)
	require.NoError(t, err)

	assert.Equal(t, original.SchemaVersion, reloaded.SchemaVersion)
	require.Len(t, reloaded.Adapters, 2)

	// Verify sorting: noop.default should come before claude.default alphabetically.
	assert.Equal(t, "claude", reloaded.Adapters[0].Type)
	assert.Equal(t, "noop", reloaded.Adapters[1].Type)

	// Deep-equal the first adapter (claude).
	a := reloaded.Adapters[0]
	assert.Equal(t, "claude", a.Type)
	assert.Equal(t, "default", a.Name)
	assert.Equal(t, "sha256:abc123", a.ResolvedDigest)
	assert.Equal(t, []string{"linux/amd64", "linux/arm64"}, a.Platforms)
	require.NotNil(t, a.Signature)
	require.NotNil(t, a.Signature.Keyless)
	assert.Equal(t, "subj", a.Signature.Keyless.Subject)
	require.NotNil(t, a.ContainerImage)
	assert.Equal(t, "sha256:img123", a.ContainerImage.Digest)
	require.NotNil(t, a.Remote)
	assert.Equal(t, "sha256:cert123", a.Remote.ServerCertFingerprint)
	assert.Equal(t, []string{"shell"}, a.CompatibleEnvironmentsOverride)
	assert.Equal(t, "workflow.hcl:7", a.OverriddenBy)

	// Deep-equal the second adapter (noop).
	a = reloaded.Adapters[1]
	assert.Equal(t, "noop", a.Type)
	assert.Nil(t, a.Signature)
	assert.Nil(t, a.ContainerImage)
	assert.Nil(t, a.Remote)
}

func TestWrite_ByteStability(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lock.hcl")

	lf := &lockfile.Lockfile{
		SchemaVersion: 1,
		Adapters: []lockfile.LockedAdapter{
			{
				Type:               "z",
				Name:               "last",
				Reference:          "r",
				ResolvedDigest:     "sha256:d1",
				SourceURL:          "https://example.com",
				SDKProtocolVersion: 2,
				Platforms:          []string{"linux/amd64"},
			},
			{
				Type:               "a",
				Name:               "first",
				Reference:          "r",
				ResolvedDigest:     "sha256:d2",
				SourceURL:          "https://example.com",
				SDKProtocolVersion: 2,
			},
		},
	}

	require.NoError(t, lockfile.Write(path, lf))
	first, err := os.ReadFile(path)
	require.NoError(t, err)

	require.NoError(t, lockfile.Write(path, lf))
	second, err := os.ReadFile(path)
	require.NoError(t, err)

	assert.True(t, bytes.Equal(first, second), "two Write calls with identical inputs must produce identical bytes")
}

func TestReadFromDir_Found(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".criteria.lock.hcl")
	require.NoError(t, os.WriteFile(path, []byte(`schema_version = 1

adapter "x" "y" {
  reference            = "r"
  resolved_digest      = "sha256:d"
  source_url           = "https://example.com"
  sdk_protocol_version = 2
}
`), 0o644))

	lf, err := lockfile.ReadFromDir(dir)
	require.NoError(t, err)
	require.NotNil(t, lf)
	assert.Equal(t, 1, lf.SchemaVersion)
}

func TestReadFromDir_NotFound(t *testing.T) {
	dir := t.TempDir()
	lf, err := lockfile.ReadFromDir(dir)
	require.NoError(t, err)
	assert.Nil(t, lf)
}

func TestRead_MissingFile(t *testing.T) {
	_, err := lockfile.Read("testdata/does_not_exist.lock.hcl")
	require.Error(t, err)
}

func TestRead_InvalidHCL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.lock.hcl")
	require.NoError(t, os.WriteFile(path, []byte(`not valid hcl {`), 0o644))

	_, err := lockfile.Read(path)
	require.Error(t, err)
}
