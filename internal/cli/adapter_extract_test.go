package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/brokenbots/criteria/internal/adapter/oci"
	"github.com/brokenbots/criteria/internal/adapter/signing"
	"github.com/brokenbots/criteria/internal/adapterhost"
)

// fakeArtifactFixture builds a minimal, valid OCI adapter artifact in layout.
// It returns the manifest digest and the digest of the binary layer.
func fakeArtifactFixture(t *testing.T, layout *oci.Layout, name string, binary []byte) (manifestDigest, binaryLayerDigest digest.Digest) {
	t.Helper()

	adapterYAML := fmt.Sprintf(`schema_version: 1
name: %s
version: 0.5.1
source_url: https://example.com/%s
sdk_protocol_version: 2
platforms:
  - os: %s
    arch: %s
`, name, name, runtime.GOOS, runtime.GOARCH)

	yamlDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageLayer,
		Digest:    digest.FromString(adapterYAML),
		Size:      int64(len(adapterYAML)),
		Annotations: map[string]string{
			oci.AnnotationTitle: "adapter.yaml",
		},
	}
	require.NoError(t, layout.WriteBlob(bytes.NewReader([]byte(adapterYAML)), yamlDesc.Digest))

	binName := adapterhost.AdapterBinaryName(name)
	binTitle := path.Join("bin", runtime.GOOS, runtime.GOARCH, binName)
	binDesc := ocispec.Descriptor{
		MediaType: "application/vnd.criteria.adapter.binary.v1",
		Digest:    digest.FromBytes(binary),
		Size:      int64(len(binary)),
		Annotations: map[string]string{
			oci.AnnotationTitle: binTitle,
		},
	}
	require.NoError(t, layout.WriteBlob(bytes.NewReader(binary), binDesc.Digest))

	cfgData := []byte("{}")
	cfgDesc := ocispec.Descriptor{
		MediaType: "application/vnd.criteria.adapter.v1+json",
		Digest:    digest.FromBytes(cfgData),
		Size:      int64(len(cfgData)),
	}
	require.NoError(t, layout.WriteBlob(bytes.NewReader(cfgData), cfgDesc.Digest))

	manifest := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    cfgDesc,
		Layers:    []ocispec.Descriptor{yamlDesc, binDesc},
	}
	manifestJSON, err := json.Marshal(manifest)
	require.NoError(t, err)
	manifestDigest = digest.FromBytes(manifestJSON)
	require.NoError(t, layout.WriteBlob(bytes.NewReader(manifestJSON), manifestDigest))

	return manifestDigest, binDesc.Digest
}

func TestExtractOCIAdapterBinary_ExtractsToResolvablePath(t *testing.T) {
	stateDir := t.TempDir()
	adaptersDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)
	t.Setenv("CRITERIA_ADAPTERS", adaptersDir)

	layout, err := oci.Open(filepath.Join(stateDir, "cache", "oci"))
	require.NoError(t, err)

	const name = "testadapter"
	binContent := []byte("#!/bin/sh\necho ok\n")
	manifestDigest, binDigest := fakeArtifactFixture(t, layout, name, binContent)

	extractPath, err := extractOCIAdapterBinary(layout, manifestDigest, name)
	require.NoError(t, err)

	// The destination must be resolvable by DiscoverBinaryAt.
	resolved, err := adapterhost.DiscoverBinaryAt(name, adapterhost.EncodeDigest(manifestDigest))
	require.NoError(t, err)
	assert.Equal(t, extractPath, resolved)

	got, err := os.ReadFile(extractPath)
	require.NoError(t, err)
	assert.Equal(t, binContent, got)
	assert.Equal(t, binDigest.String(), digest.FromBytes(got).String())
}

func TestExtractOCIAdapterBinary_Idempotent(t *testing.T) {
	stateDir := t.TempDir()
	adaptersDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)
	t.Setenv("CRITERIA_ADAPTERS", adaptersDir)

	layout, err := oci.Open(filepath.Join(stateDir, "cache", "oci"))
	require.NoError(t, err)

	const name = "testadapter"
	binContent := []byte("#!/bin/sh\necho ok\n")
	manifestDigest, _ := fakeArtifactFixture(t, layout, name, binContent)

	first, err := extractOCIAdapterBinary(layout, manifestDigest, name)
	require.NoError(t, err)

	info, err := os.Stat(first)
	require.NoError(t, err)
	mtime := info.ModTime()

	second, err := extractOCIAdapterBinary(layout, manifestDigest, name)
	require.NoError(t, err)
	assert.Equal(t, first, second)

	info2, err := os.Stat(second)
	require.NoError(t, err)
	assert.Equal(t, mtime, info2.ModTime(), "re-extraction must not rewrite the file")
}

func TestExtractOCIAdapterBinary_RejectsTamperedBlob(t *testing.T) {
	stateDir := t.TempDir()
	adaptersDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)
	t.Setenv("CRITERIA_ADAPTERS", adaptersDir)

	layout, err := oci.Open(filepath.Join(stateDir, "cache", "oci"))
	require.NoError(t, err)

	const name = "testadapter"
	binContent := []byte("#!/bin/sh\necho ok\n")
	manifestDigest, _ := fakeArtifactFixture(t, layout, name, binContent)

	// Corrupt the cached binary blob so it no longer matches its layer digest.
	// The manifest still claims the old digest, so extraction must fail.
	layerDigest, err := artifactBinaryLayerDigest(layout, manifestDigest)
	require.NoError(t, err)
	blobPath := layout.BlobPath(layerDigest)
	require.NoError(t, os.WriteFile(blobPath, []byte("tampered"), 0o644))

	_, err = extractOCIAdapterBinary(layout, manifestDigest, name)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "digest mismatch")
}

func TestRunPullWithPuller_ExtractsToResolvablePath(t *testing.T) {
	stateDir := t.TempDir()
	adaptersDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)
	t.Setenv("CRITERIA_ADAPTERS", adaptersDir)

	cacheRoot := filepath.Join(stateDir, "cache", "oci")
	layout, err := oci.Open(cacheRoot)
	require.NoError(t, err)

	const name = "pulltest"
	binContent := []byte("#!/bin/sh\necho pulled\n")
	manifestDigest, _ := fakeArtifactFixture(t, layout, name, binContent)

	fp := &fakePuller{layout: layout, manifests: map[string]digest.Digest{
		"ghcr.io/example/pulltest:v1.0.0": manifestDigest,
	}}

	ref, err := oci.Parse("ghcr.io/example/pulltest:v1.0.0")
	require.NoError(t, err)

	var out bytes.Buffer
	err = runPullWithPuller(context.Background(), &out, ref, layout, &signing.Policy{Mode: signing.ModeOff}, fp)
	require.NoError(t, err)

	assert.Contains(t, out.String(), "Extracted:")
	assert.Contains(t, out.String(), adapterhost.EncodeDigest(manifestDigest))

	resolved, err := adapterhost.DiscoverBinaryAt(name, adapterhost.EncodeDigest(manifestDigest))
	require.NoError(t, err)
	assert.FileExists(t, resolved)
}

// fakePuller is a test double that returns a pre-staged manifest digest for a
// known reference, bypassing the network.
type fakePuller struct {
	layout    *oci.Layout
	manifests map[string]digest.Digest
}

func (f *fakePuller) Pull(ctx context.Context, ref oci.Reference) (digest.Digest, error) {
	dg, ok := f.manifests[ref.String()]
	if !ok {
		return "", fmt.Errorf("fakePuller: reference %s not staged", ref)
	}
	return dg, nil
}
