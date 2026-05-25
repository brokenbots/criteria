//go:build integration

// Package oci_test contains integration tests that require a running Docker daemon.
// Run with: go test -tags integration ./internal/adapter/oci/ -run TestPull
package oci_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/registry/remote"

	"github.com/brokenbots/criteria/internal/adapter/oci"
)

// startRegistry starts a distribution registry:2.8 container and returns its
// address ("host:port") and a cleanup function.
func startRegistry(t *testing.T) (string, func()) {
	t.Helper()
	ctx := context.Background()

	req := testcontainers.ContainerRequest{
		Image:        "registry:2.8",
		ExposedPorts: []string{"5000/tcp"},
		WaitingFor:   wait.ForListeningPort("5000/tcp").WithStartupTimeout(30 * time.Second),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err)

	host, err := c.Host(ctx)
	require.NoError(t, err)
	port, err := c.MappedPort(ctx, "5000")
	require.NoError(t, err)

	addr := fmt.Sprintf("%s:%s", host, port.Port())
	return addr, func() { _ = c.Terminate(ctx) }
}

// pushSyntheticArtifact pushes a minimal OCI artifact to the registry at addr
// under the reference tag, and returns the manifest digest.
func pushSyntheticArtifact(t *testing.T, addr, tag string) (digest.Digest, error) {
	t.Helper()

	adapterYAML := []byte("name: test-adapter\nprotocol: v2\n")
	store := memory.New()
	ctx := context.Background()

	yamlDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageLayer,
		Digest:    digest.FromBytes(adapterYAML),
		Size:      int64(len(adapterYAML)),
		Annotations: map[string]string{
			oci.AnnotationTitle: "adapter.yaml",
		},
	}
	require.NoError(t, store.Push(ctx, yamlDesc, bytes.NewReader(adapterYAML)))

	cfgData := []byte("{}")
	cfgDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageConfig,
		Digest:    digest.FromBytes(cfgData),
		Size:      int64(len(cfgData)),
	}
	require.NoError(t, store.Push(ctx, cfgDesc, bytes.NewReader(cfgData)))

	manifest := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    cfgDesc,
		Layers:    []ocispec.Descriptor{yamlDesc},
	}
	manifestJSON, err := json.Marshal(manifest)
	require.NoError(t, err)

	manifestDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    digest.FromBytes(manifestJSON),
		Size:      int64(len(manifestJSON)),
	}
	require.NoError(t, store.Push(ctx, manifestDesc, bytes.NewReader(manifestJSON)))
	require.NoError(t, store.Tag(ctx, manifestDesc, tag))

	repo, err := remote.NewRepository(fmt.Sprintf("%s/test/adapter", addr))
	require.NoError(t, err)
	repo.PlainHTTP = true

	_, err = oras.Copy(ctx, store, tag, repo, tag, oras.DefaultCopyOptions)
	require.NoError(t, err)

	return manifestDesc.Digest, nil
}

func TestPull_FetchesArtifact(t *testing.T) {
	addr, cleanup := startRegistry(t)
	defer cleanup()

	const tag = "v1.0.0"
	manifestDigest, err := pushSyntheticArtifact(t, addr, tag)
	require.NoError(t, err)

	root := t.TempDir()
	l, err := oci.Open(root)
	require.NoError(t, err)

	puller := &oci.Puller{
		Layout:    l,
		PlainHTTP: true,
	}

	ref, err := oci.Parse(fmt.Sprintf("%s/test/adapter:%s", addr, tag))
	require.NoError(t, err)

	ctx := context.Background()
	gotDigest, err := puller.Pull(ctx, ref)
	require.NoError(t, err)
	assert.Equal(t, manifestDigest, gotDigest)

	// Verify the manifest blob is present in the layout.
	assert.True(t, l.HasBlob(gotDigest))

	// Verify the adapter.yaml is accessible via Open.
	fsys, err := l.Open(gotDigest)
	require.NoError(t, err)
	f, err := fsys.Open("adapter.yaml")
	require.NoError(t, err)
	defer f.Close()

	buf := make([]byte, 256)
	n, _ := f.Read(buf)
	assert.Contains(t, string(buf[:n]), "test-adapter")

	// Protocol version annotations must be written to index.json after Pull.
	ix, err := l.Index()
	require.NoError(t, err)
	var found bool
	for _, m := range ix.Manifests {
		if m.Digest == gotDigest {
			found = true
			assert.Equal(t, "2", m.Annotations[oci.AnnotationProtocolVersion],
				"index.json descriptor must carry protocol version annotation after Pull")
			assert.Equal(t, "1", m.Annotations[oci.AnnotationSchemaVersion],
				"index.json descriptor must carry schema version annotation after Pull")
		}
	}
	require.True(t, found, "pulled manifest must appear in index.json")

	// ArtifactProtocolVersion must decode the annotation.
	ver := l.ArtifactProtocolVersion(gotDigest)
	assert.Equal(t, uint32(2), ver)
}

func TestResolve_ReturnsDigest(t *testing.T) {
	addr, cleanup := startRegistry(t)
	defer cleanup()

	const tag = "v2.0.0"
	manifestDigest, err := pushSyntheticArtifact(t, addr, tag)
	require.NoError(t, err)

	root := t.TempDir()
	l, err := oci.Open(root)
	require.NoError(t, err)

	puller := &oci.Puller{
		Layout:    l,
		PlainHTTP: true,
	}

	ref, err := oci.Parse(fmt.Sprintf("%s/test/adapter:%s", addr, tag))
	require.NoError(t, err)

	ctx := context.Background()
	gotDigest, err := puller.Resolve(ctx, ref)
	require.NoError(t, err)
	assert.Equal(t, manifestDigest, gotDigest)

	// Resolve must NOT pull blobs.
	assert.False(t, l.HasBlob(gotDigest), "Resolve must not fetch blobs into the layout")
}

func TestPull_IdempotentOnRepeat(t *testing.T) {
	addr, cleanup := startRegistry(t)
	defer cleanup()

	const tag = "v3.0.0"
	_, err := pushSyntheticArtifact(t, addr, tag)
	require.NoError(t, err)

	root := t.TempDir()
	l, err := oci.Open(root)
	require.NoError(t, err)

	puller := &oci.Puller{Layout: l, PlainHTTP: true}
	ref, err := oci.Parse(fmt.Sprintf("%s/test/adapter:%s", addr, tag))
	require.NoError(t, err)

	ctx := context.Background()
	d1, err := puller.Pull(ctx, ref)
	require.NoError(t, err)

	d2, err := puller.Pull(ctx, ref)
	require.NoError(t, err)
	assert.Equal(t, d1, d2)
}
