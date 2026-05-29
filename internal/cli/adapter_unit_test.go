package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/brokenbots/criteria/internal/adapter/oci"
)

func TestParseHumanDuration(t *testing.T) {
	tests := []struct {
		input    string
		want     time.Duration
		wantErr  bool
	}{
		{"30d", 30 * 24 * time.Hour, false},
		{"1d", 1 * 24 * time.Hour, false},
		{"1d12h", 1*24*time.Hour + 12*time.Hour, false},
		{"2d30m", 2*24*time.Hour + 30*time.Minute, false},
		{"24h", 24 * time.Hour, false},
		{"30m", 30 * time.Minute, false},
		{"", 0, true},
		{"abc", 0, true},
		{"d", 0, true},
		{"1.5d", time.Duration(1.5*24) * time.Hour, false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseHumanDuration(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolveRefOrName_DirectDigest(t *testing.T) {
	root := t.TempDir()
	layout, err := oci.Open(root)
	require.NoError(t, err)

	// Create a fake blob so the digest exists.
	data := []byte("fake adapter blob")
	dg := digest.FromBytes(data)
	blobPath := filepath.Join(root, "blobs", "sha256", dg.Encoded())
	require.NoError(t, os.MkdirAll(filepath.Dir(blobPath), 0o755))
	require.NoError(t, os.WriteFile(blobPath, data, 0o644))

	got, err := resolveRefOrName(layout, dg.String())
	require.NoError(t, err)
	assert.Equal(t, dg, got)
}

func TestResolveRefOrName_ByAdapterName(t *testing.T) {
	root := t.TempDir()
	layout, err := oci.Open(root)
	require.NoError(t, err)

	// Build a minimal OCI artifact with an adapter.yaml layer.
	adapterYAML := []byte("name: my-test-adapter\nprotocol: v2\n")
	manifestJSON := []byte(fmt.Sprintf(`{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.manifest.v1+json",
  "config": {
    "mediaType": "application/vnd.criteria.adapter.v1+json",
    "digest": "%s",
    "size": 2
  },
  "layers": [
    {
      "mediaType": "application/vnd.oci.image.layer.v1.tar",
      "digest": "%s",
      "size": %d,
      "annotations": {
        "org.opencontainers.image.title": "adapter.yaml"
      }
    }
  ]
}`, digest.FromBytes([]byte("{}")), digest.FromBytes(adapterYAML), len(adapterYAML)))

	// Write blobs.
	cfgBlobPath := filepath.Join(root, "blobs", "sha256", digest.FromBytes([]byte("{}")).Encoded())
	yamlBlobPath := filepath.Join(root, "blobs", "sha256", digest.FromBytes(adapterYAML).Encoded())
	manifestBlobPath := filepath.Join(root, "blobs", "sha256", digest.FromBytes(manifestJSON).Encoded())
	require.NoError(t, os.MkdirAll(filepath.Dir(cfgBlobPath), 0o755))
	require.NoError(t, os.WriteFile(cfgBlobPath, []byte("{}"), 0o644))
	require.NoError(t, os.WriteFile(yamlBlobPath, adapterYAML, 0o644))
	require.NoError(t, os.WriteFile(manifestBlobPath, manifestJSON, 0o644))

	// Write index.
	ix := &ocispec.Index{Manifests: []ocispec.Descriptor{{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Digest:    digest.FromBytes(manifestJSON),
		Size:      int64(len(manifestJSON)),
	}}}
	require.NoError(t, layout.WriteIndex(ix))

	got, err := resolveRefOrName(layout, "my-test-adapter")
	require.NoError(t, err)
	assert.Equal(t, digest.FromBytes(manifestJSON), got)
}

func TestResolveRefOrName_NotFound(t *testing.T) {
	root := t.TempDir()
	layout, err := oci.Open(root)
	require.NoError(t, err)

	_, err = resolveRefOrName(layout, "nonexistent")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestResolveRefOrName_Ambiguous(t *testing.T) {
	root := t.TempDir()
	layout, err := oci.Open(root)
	require.NoError(t, err)

	// Build two artifacts with the same adapter name.
	for i := 0; i < 2; i++ {
		adapterYAML := []byte("name: ambiguous-adapter\nprotocol: v2\n")
		manifestJSON := []byte(fmt.Sprintf(`{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.manifest.v1+json",
  "config": {
    "mediaType": "application/vnd.criteria.adapter.v1+json",
    "digest": "%s",
    "size": 2
  },
  "layers": [
    {
      "mediaType": "application/vnd.oci.image.layer.v1.tar",
      "digest": "%s",
      "size": %d,
      "annotations": {
        "org.opencontainers.image.title": "adapter.yaml"
      }
    }
  ]
}`, digest.FromBytes([]byte("{}")), digest.FromBytes(adapterYAML), len(adapterYAML)))

		cfgBlobPath := filepath.Join(root, "blobs", "sha256", digest.FromBytes([]byte("{}")).Encoded())
		yamlBlobPath := filepath.Join(root, "blobs", "sha256", digest.FromBytes(adapterYAML).Encoded())
		manifestBlobPath := filepath.Join(root, "blobs", "sha256", digest.FromBytes(manifestJSON).Encoded())
		require.NoError(t, os.MkdirAll(filepath.Dir(cfgBlobPath), 0o755))
		require.NoError(t, os.WriteFile(cfgBlobPath, []byte("{}"), 0o644))
		require.NoError(t, os.WriteFile(yamlBlobPath, adapterYAML, 0o644))
		require.NoError(t, os.WriteFile(manifestBlobPath, manifestJSON, 0o644))

		ix, _ := layout.Index()
		ix.Manifests = append(ix.Manifests, ocispec.Descriptor{
			MediaType: "application/vnd.oci.image.manifest.v1+json",
			Digest:    digest.FromBytes(manifestJSON),
			Size:      int64(len(manifestJSON)),
		})
		require.NoError(t, layout.WriteIndex(ix))
	}

	_, err = resolveRefOrName(layout, "ambiguous-adapter")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "ambiguous")
}
