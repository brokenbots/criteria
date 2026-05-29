// Package publish constructs and pushes adapter OCI artifacts to a remote
// registry. It is consumed by the `criteria adapter publish` CLI verb and by
// CI actions (WS28).
package publish

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/brokenbots/criteria/internal/adapter/oci"
)

const (
	mediaTypeAdapterConfig = "application/vnd.criteria.adapter.v1+json"
	mediaTypeAdapterBinary = "application/vnd.criteria.adapter.binary.v1"
)

// Options controls how an artifact is packaged and pushed.
type Options struct {
	// PlainHTTP allows pushing to HTTP (non-TLS) registries (e.g. local test
	// registries).
	PlainHTTP bool

	// Auth resolves registry credentials. If nil, anonymous access is used.
	Auth oci.AuthProvider
}

// PushArtifact packages a single-platform adapter binary and its manifest
// into an OCI artifact and pushes it to the registry identified by ref.
//
// binPath is the local adapter binary.
// manifestPath is the adapter.yaml emitted by `--emit-manifest`.
//
// Returns the digest of the published manifest.
func PushArtifact(ctx context.Context, ref oci.Reference, binPath, manifestPath string, opts Options) (digest.Digest, error) {
	if !ref.FullyQualified() {
		return "", fmt.Errorf("publish: reference must be fully-qualified (got %q)", ref)
	}

	// Read the binary and manifest bytes.
	binData, err := os.ReadFile(binPath)
	if err != nil {
		return "", fmt.Errorf("publish: read binary: %w", err)
	}
	mfData, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", fmt.Errorf("publish: read manifest: %w", err)
	}

	store := memory.New()

	// Build layer descriptors.
	binTitle := filepath.ToSlash(filepath.Join("bin", runtimeGOOS, runtimeGOARCH, filepath.Base(binPath)))
	binDesc := ocispec.Descriptor{
		MediaType: mediaTypeAdapterBinary,
		Digest:    digest.FromBytes(binData),
		Size:      int64(len(binData)),
		Annotations: map[string]string{
			oci.AnnotationTitle: binTitle,
		},
	}
	if err := store.Push(ctx, binDesc, bytes.NewReader(binData)); err != nil {
		return "", fmt.Errorf("publish: push binary layer: %w", err)
	}

	mfDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageLayer,
		Digest:    digest.FromBytes(mfData),
		Size:      int64(len(mfData)),
		Annotations: map[string]string{
			oci.AnnotationTitle: "adapter.yaml",
		},
	}
	if err := store.Push(ctx, mfDesc, bytes.NewReader(mfData)); err != nil {
		return "", fmt.Errorf("publish: push manifest layer: %w", err)
	}

	// Config blob (per D10).
	cfgData := []byte("{}")
	cfgDesc := ocispec.Descriptor{
		MediaType: mediaTypeAdapterConfig,
		Digest:    digest.FromBytes(cfgData),
		Size:      int64(len(cfgData)),
	}
	if err := store.Push(ctx, cfgDesc, bytes.NewReader(cfgData)); err != nil {
		return "", fmt.Errorf("publish: push config: %w", err)
	}

	// Assemble the image manifest.
	manifest := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    cfgDesc,
		Layers:    []ocispec.Descriptor{mfDesc, binDesc},
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return "", fmt.Errorf("publish: marshal manifest: %w", err)
	}
	manifestDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    digest.FromBytes(manifestJSON),
		Size:      int64(len(manifestJSON)),
	}
	if err := store.Push(ctx, manifestDesc, bytes.NewReader(manifestJSON)); err != nil {
		return "", fmt.Errorf("publish: push manifest: %w", err)
	}
	if err := store.Tag(ctx, manifestDesc, ref.Tag); err != nil {
		return "", fmt.Errorf("publish: tag manifest: %w", err)
	}

	// Push to remote registry.
	repo, err := newRepository(ref, opts)
	if err != nil {
		return "", err
	}

	pushedDesc, err := oras.Copy(ctx, store, ref.Tag, repo, ref.Tag, oras.DefaultCopyOptions)
	if err != nil {
		return "", fmt.Errorf("publish: push to registry: %w", err)
	}
	return pushedDesc.Digest, nil
}

// runtimeGOOS and runtimeGOARCH are overridden by tests.
var (
	runtimeGOOS   = os.Getenv("GOOS")
	runtimeGOARCH = os.Getenv("GOARCH")
)

func init() {
	if runtimeGOOS == "" {
		runtimeGOOS = "linux"
	}
	if runtimeGOARCH == "" {
		runtimeGOARCH = "amd64"
	}
}

func newRepository(ref oci.Reference, opts Options) (*remote.Repository, error) {
	repoRef := ref.Registry
	if ref.Repo != "" {
		repoRef += "/" + ref.Repo
	}
	repo, err := remote.NewRepository(repoRef)
	if err != nil {
		return nil, fmt.Errorf("publish: build remote repository for %q: %w", repoRef, err)
	}
	repo.PlainHTTP = opts.PlainHTTP

	ap := opts.Auth
	if ap == nil {
		ap = oci.DefaultAuthProvider()
	}
	repo.Client = &auth.Client{
		Client: httpDefaultClient(),
		Credential: func(ctx context.Context, hostport string) (auth.Credential, error) {
			return ap.Credential(ctx, hostport)
		},
	}
	return repo, nil
}

var httpDefaultClient = func() *http.Client { return http.DefaultClient }

// SetHTTPClient overrides the default HTTP client used for registry pushes.
// It exists so tests can inject a transport that routes to a local test
// registry.
func SetHTTPClient(c *http.Client) {
	httpDefaultClient = func() *http.Client { return c }
}
