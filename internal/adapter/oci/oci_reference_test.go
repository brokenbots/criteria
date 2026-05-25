package oci_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/brokenbots/criteria/internal/adapter/oci"
)

func TestParse_TaggedReference(t *testing.T) {
	ref, err := oci.Parse("ghcr.io/brokenbots/claude:1.2.3")
	require.NoError(t, err)
	assert.Equal(t, "ghcr.io", ref.Registry)
	assert.Equal(t, "brokenbots/claude", ref.Repo)
	assert.Equal(t, "1.2.3", ref.Tag)
	assert.Empty(t, ref.Digest)
}

func TestParse_DigestReference(t *testing.T) {
	d := "sha256:a665a45920422f9d417e4867efdc4fb8a04a1f3fff1fa07e998e86f7f7a27ae3"
	ref, err := oci.Parse("ghcr.io/brokenbots/claude@" + d)
	require.NoError(t, err)
	assert.Equal(t, "ghcr.io", ref.Registry)
	assert.Equal(t, "brokenbots/claude", ref.Repo)
	assert.Empty(t, ref.Tag)
	assert.Equal(t, d, ref.Digest.String())
}

func TestParse_RepoOnly(t *testing.T) {
	ref, err := oci.Parse("ghcr.io/org/name")
	require.NoError(t, err)
	assert.Equal(t, "ghcr.io", ref.Registry)
	assert.Equal(t, "org/name", ref.Repo)
	assert.Empty(t, ref.Tag)
	assert.Empty(t, ref.Digest)
}

func TestParse_RegistryWithPort(t *testing.T) {
	ref, err := oci.Parse("localhost:5000/myrepo:v1")
	require.NoError(t, err)
	assert.Equal(t, "localhost:5000", ref.Registry)
	assert.Equal(t, "myrepo", ref.Repo)
	assert.Equal(t, "v1", ref.Tag)
}

func TestParse_Empty(t *testing.T) {
	_, err := oci.Parse("")
	require.Error(t, err)
}

func TestParse_InvalidDigest(t *testing.T) {
	_, err := oci.Parse("ghcr.io/org/name@notadigest")
	require.Error(t, err)
}

func TestParse_EmptyTag(t *testing.T) {
	_, err := oci.Parse("ghcr.io/org/name:")
	require.Error(t, err)
}

func TestParse_MissingRegistry(t *testing.T) {
	// A bare name with no slashes at all — treated as registry only,
	// but no repo/tag/digest → error.
	_, err := oci.Parse("noregistry")
	require.Error(t, err)
}

func TestReference_String_Tagged(t *testing.T) {
	ref, err := oci.Parse("ghcr.io/brokenbots/claude:1.2.3")
	require.NoError(t, err)
	assert.Equal(t, "ghcr.io/brokenbots/claude:1.2.3", ref.String())
}

func TestReference_String_Digest(t *testing.T) {
	d := "sha256:a665a45920422f9d417e4867efdc4fb8a04a1f3fff1fa07e998e86f7f7a27ae3"
	ref, err := oci.Parse("ghcr.io/brokenbots/claude@" + d)
	require.NoError(t, err)
	assert.Equal(t, "ghcr.io/brokenbots/claude@"+d, ref.String())
}

func TestReference_FullyQualified(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"ghcr.io/brokenbots/claude:1.2.3", true},
		{"ghcr.io/brokenbots/claude@sha256:a665a45920422f9d417e4867efdc4fb8a04a1f3fff1fa07e998e86f7f7a27ae3", true},
		{"ghcr.io/brokenbots/claude", false}, // no tag or digest
	}
	for _, tt := range tests {
		ref, err := oci.Parse(tt.input)
		require.NoError(t, err, tt.input)
		assert.Equal(t, tt.want, ref.FullyQualified(), tt.input)
	}
}

func TestParse_DeepRepo(t *testing.T) {
	// org/sub/name:tag
	ref, err := oci.Parse("registry.example.com/org/sub/name:tag")
	require.NoError(t, err)
	assert.Equal(t, "registry.example.com", ref.Registry)
	assert.Equal(t, "org/sub/name", ref.Repo)
	assert.Equal(t, "tag", ref.Tag)
}
