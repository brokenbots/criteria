//go:build integration

// Package cli_test contains integration tests that require a running Docker daemon.
// Run with: go test -tags integration ./internal/cli/ -run TestAdapterE2E
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// pushSyntheticArtifact pushes a minimal adapter OCI artifact to the
// registry at addr under the reference tag. The artifact supports the host
// platform and contains a fake binary layer and an adapter.yaml layer.
func pushSyntheticArtifact(t *testing.T, addr, tag string) digest.Digest {
	t.Helper()
	ctx := context.Background()
	store := memory.New()

	// Adapter manifest layer.
	adapterYAML := fmt.Sprintf(`name: test-adapter
protocol: v2
platforms:
  - os: %s
    arch: %s
`, runtime.GOOS, runtime.GOARCH)
	yamlDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageLayer,
		Digest:    digest.FromBytes([]byte(adapterYAML)),
		Size:      int64(len(adapterYAML)),
		Annotations: map[string]string{
			oci.AnnotationTitle: "adapter.yaml",
		},
	}
	require.NoError(t, store.Push(ctx, yamlDesc, bytes.NewReader([]byte(adapterYAML))))

	// Fake binary layer.
	fakeBin := []byte("#!/bin/sh\necho fake adapter\n")
	binDesc := ocispec.Descriptor{
		MediaType: "application/vnd.criteria.adapter.binary.v1",
		Digest:    digest.FromBytes(fakeBin),
		Size:      int64(len(fakeBin)),
		Annotations: map[string]string{
			oci.AnnotationTitle: fmt.Sprintf("bin/%s/%s/fake", runtime.GOOS, runtime.GOARCH),
		},
	}
	require.NoError(t, store.Push(ctx, binDesc, bytes.NewReader(fakeBin)))

	// Config blob.
	cfgData := []byte("{}")
	cfgDesc := ocispec.Descriptor{
		MediaType: "application/vnd.criteria.adapter.v1+json",
		Digest:    digest.FromBytes(cfgData),
		Size:      int64(len(cfgData)),
	}
	require.NoError(t, store.Push(ctx, cfgDesc, bytes.NewReader(cfgData)))

	// Image manifest.
	manifest := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    cfgDesc,
		Layers:    []ocispec.Descriptor{yamlDesc, binDesc},
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

	// Push to registry.
	repo, err := remote.NewRepository(fmt.Sprintf("%s/test/adapter", addr))
	require.NoError(t, err)
	repo.PlainHTTP = true

	_, err = oras.Copy(ctx, store, tag, repo, tag, oras.DefaultCopyOptions)
	require.NoError(t, err)

	return manifestDesc.Digest
}

func TestAdapterE2E_PullInfoWhere(t *testing.T) {
	addr, cleanup := startRegistry(t)
	defer cleanup()

	const tag = "v1.0.0"
	pushSyntheticArtifact(t, addr, tag)

	// Use a temporary state directory so the test is hermetic.
	stateDir := t.TempDir()
	t.Setenv("CRITERIA_STATE_DIR", stateDir)

	ref := fmt.Sprintf("%s/test/adapter:%s", addr, tag)

	// Step 1: Pull with --allow-unsigned.
	cmd := newAdapterPullCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--allow-unsigned", ref})
	require.NoError(t, cmd.Execute(), "pull command failed: %s", out.String())

	output := out.String()
	assert.Contains(t, output, "Pulled")
	assert.Contains(t, output, addr)

	// Step 2: Info should return the adapter name.
	out.Reset()
	cmd = newAdapterInfoCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{ref})
	require.NoError(t, cmd.Execute(), "info command failed: %s", out.String())

	output = out.String()
	assert.Contains(t, output, "test-adapter")

	// Step 3: Where should return a path containing the binary.
	out.Reset()
	cmd = newAdapterWhereCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{ref})
	require.NoError(t, cmd.Execute(), "where command failed: %s", out.String())

	binPath := strings.TrimSpace(out.String())
	assert.NotEmpty(t, binPath)
	_, err := os.Stat(binPath)
	require.NoError(t, err, "where output path should exist: %s", binPath)

	// Step 4: List --installed should show the pulled artifact.
	out.Reset()
	cmd = newAdapterListCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--installed"})
	require.NoError(t, cmd.Execute(), "list command failed: %s", out.String())

	assert.Contains(t, out.String(), "test-adapter")
}
