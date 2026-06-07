package oci_test

import (
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/brokenbots/criteria/internal/adapter/oci"
)

// buildFixtureArtifact creates a synthetic OCI artifact in the layout:
//
//	adapter.yaml  → "name: test-adapter\nprotocol: v2\n"
//	bin/linux-amd64  → binary placeholder
//
// It returns the manifest digest.
func buildFixtureArtifact(t *testing.T, l *oci.Layout) digest.Digest {
	t.Helper()

	adapterYAML := []byte("name: test-adapter\nprotocol: v2\n")
	yamlDigest := digest.FromBytes(adapterYAML)
	require.NoError(t, l.WriteBlob(bytes.NewReader(adapterYAML), yamlDigest))

	binData := []byte("\x7fELF placeholder binary")
	binDigest := digest.FromBytes(binData)
	require.NoError(t, l.WriteBlob(bytes.NewReader(binData), binDigest))

	manifest := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config: ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageConfig,
			Digest:    digest.FromBytes([]byte("{}")),
			Size:      2,
		},
		Layers: []ocispec.Descriptor{
			{
				MediaType: ocispec.MediaTypeImageLayer,
				Digest:    yamlDigest,
				Size:      int64(len(adapterYAML)),
				Annotations: map[string]string{
					oci.AnnotationTitle: "adapter.yaml",
				},
			},
			{
				MediaType: ocispec.MediaTypeImageLayer,
				Digest:    binDigest,
				Size:      int64(len(binData)),
				Annotations: map[string]string{
					oci.AnnotationTitle: "bin/linux-amd64",
				},
			},
		},
	}

	// Also write the empty config blob.
	cfgData := []byte("{}")
	cfgDigest := digest.FromBytes(cfgData)
	require.NoError(t, l.WriteBlob(bytes.NewReader(cfgData), cfgDigest))
	manifest.Config.Digest = cfgDigest

	manifestJSON, err := json.Marshal(manifest)
	require.NoError(t, err)
	manifestDigest := digest.FromBytes(manifestJSON)
	require.NoError(t, l.WriteBlob(bytes.NewReader(manifestJSON), manifestDigest))

	return manifestDigest
}

func TestLayoutOpen_ReadAdapterYAML(t *testing.T) {
	root := t.TempDir()
	l, err := oci.Open(root)
	require.NoError(t, err)

	manifestDigest := buildFixtureArtifact(t, l)

	fsys, err := l.Open(manifestDigest)
	require.NoError(t, err)

	f, err := fsys.Open("adapter.yaml")
	require.NoError(t, err)
	defer f.Close()

	got := make([]byte, 256)
	n, _ := f.Read(got)
	assert.Contains(t, string(got[:n]), "test-adapter")
}

func TestLayoutOpen_ReadBinaryBlob(t *testing.T) {
	root := t.TempDir()
	l, err := oci.Open(root)
	require.NoError(t, err)

	manifestDigest := buildFixtureArtifact(t, l)

	fsys, err := l.Open(manifestDigest)
	require.NoError(t, err)

	f, err := fsys.Open("bin/linux-amd64")
	require.NoError(t, err)
	defer f.Close()

	got := make([]byte, 256)
	n, _ := f.Read(got)
	assert.Contains(t, string(got[:n]), "ELF")
}

func TestLayoutOpen_MissingFile(t *testing.T) {
	root := t.TempDir()
	l, err := oci.Open(root)
	require.NoError(t, err)

	manifestDigest := buildFixtureArtifact(t, l)

	fsys, err := l.Open(manifestDigest)
	require.NoError(t, err)

	_, err = fsys.Open("does-not-exist.txt")
	require.Error(t, err)
}

func TestLayoutOpen_ManifestNotInLayout(t *testing.T) {
	root := t.TempDir()
	l, err := oci.Open(root)
	require.NoError(t, err)

	unknown := digest.FromBytes([]byte("not in layout"))
	_, err = l.Open(unknown)
	require.Error(t, err)
}

func TestLayoutOpen_DirectoryListing(t *testing.T) {
	root := t.TempDir()
	l, err := oci.Open(root)
	require.NoError(t, err)

	manifestDigest := buildFixtureArtifact(t, l)

	fsys, err := l.Open(manifestDigest)
	require.NoError(t, err)

	// Open the root "." directory and list it.
	dir, err := fsys.Open(".")
	require.NoError(t, err)
	defer dir.Close()

	rd, ok := dir.(fs.ReadDirFile)
	require.True(t, ok, "root dir must implement fs.ReadDirFile")

	entries, err := rd.ReadDir(-1)
	require.NoError(t, err)
	assert.NotEmpty(t, entries)

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.Contains(t, names, "adapter.yaml")
	assert.Contains(t, names, "bin")
}

func TestLayoutOpen_InvalidPath(t *testing.T) {
	root := t.TempDir()
	l, err := oci.Open(root)
	require.NoError(t, err)

	manifestDigest := buildFixtureArtifact(t, l)
	fsys, err := l.Open(manifestDigest)
	require.NoError(t, err)

	// Invalid paths (absolute, etc.) must return an error.
	_, err = fsys.Open("../escape")
	assert.Error(t, err)
}

func TestLayoutOpen_LayerWithoutTitleSkipped(t *testing.T) {
	root := t.TempDir()
	l, err := oci.Open(root)
	require.NoError(t, err)

	cfgData := []byte("{}")
	cfgDigest := digest.FromBytes(cfgData)
	require.NoError(t, l.WriteBlob(bytes.NewReader(cfgData), cfgDigest))

	// Layer has no title annotation.
	untitled := []byte("untitled blob data")
	untitledDigest := digest.FromBytes(untitled)
	require.NoError(t, l.WriteBlob(bytes.NewReader(untitled), untitledDigest))

	manifest := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    ocispec.Descriptor{MediaType: ocispec.MediaTypeImageConfig, Digest: cfgDigest, Size: 2},
		Layers: []ocispec.Descriptor{
			{
				MediaType: ocispec.MediaTypeImageLayer,
				Digest:    untitledDigest,
				Size:      int64(len(untitled)),
				// No AnnotationTitle.
			},
		},
	}
	manifestJSON, err := json.Marshal(manifest)
	require.NoError(t, err)
	manifestDigest := digest.FromBytes(manifestJSON)
	require.NoError(t, l.WriteBlob(bytes.NewReader(manifestJSON), manifestDigest))

	fsys, err := l.Open(manifestDigest)
	require.NoError(t, err)

	// The directory should be empty (nothing with a title).
	dir, err := fsys.Open(".")
	require.NoError(t, err)
	defer dir.Close()

	rd := dir.(fs.ReadDirFile)
	entries, err := rd.ReadDir(-1)
	require.NoError(t, err)
	assert.Empty(t, entries, "no entries expected when no title annotations present")

	// Reading the blob path directly by its digest also works.
	_, err = os.Stat(l.BlobPath(untitledDigest))
	require.NoError(t, err)
}

func TestLayoutOpen_ReadDirEOFContract(t *testing.T) {
	root := t.TempDir()
	l, err := oci.Open(root)
	require.NoError(t, err)

	manifestDigest := buildFixtureArtifact(t, l)

	fsys, err := l.Open(manifestDigest)
	require.NoError(t, err)

	dir, err := fsys.Open(".")
	require.NoError(t, err)
	defer dir.Close()

	rd, ok := dir.(fs.ReadDirFile)
	require.True(t, ok, "root dir must implement fs.ReadDirFile")

	// Read one entry at a time until we exhaust all entries.
	var all []fs.DirEntry
	for {
		entries, err := rd.ReadDir(1)
		if err != nil {
			// io.EOF signals the end of directory per fs.ReadDirFile contract.
			require.ErrorIs(t, err, io.EOF, "ReadDir(1) past end must return io.EOF, not any other error")
			break
		}
		all = append(all, entries...)
	}
	assert.NotEmpty(t, all, "must have read at least one entry before EOF")

	// Calling ReadDir(0) on an exhausted directory must return (nil, nil).
	entries, err := rd.ReadDir(0)
	assert.NoError(t, err)
	assert.Nil(t, entries)
}
