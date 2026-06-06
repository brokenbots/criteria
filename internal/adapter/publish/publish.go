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
	"runtime"

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

	// Signer, when non-nil, signs the pushed artifact and attaches a cosign
	// signature manifest as an OCI referrer. nil means publish unsigned.
	Signer Signer
}

// PlatformBinary is one platform's (cross-)compiled adapter binary, used to
// assemble a multi-platform artifact. OS/Arch are GOOS/GOARCH values (e.g.
// "linux"/"amd64", "darwin"/"arm64"); Path is the local binary on disk.
type PlatformBinary struct {
	OS   string
	Arch string
	Path string
}

// PushArtifact packages a single adapter binary (for the publish host's
// platform) and its manifest into an OCI artifact and pushes it to ref. It is a
// thin wrapper over PushMultiPlatformArtifact for the common single-platform
// case.
//
// binPath is the local adapter binary.
// manifestPath is the adapter.yaml emitted by `--emit-manifest`.
//
// Returns the digest of the published manifest.
func PushArtifact(ctx context.Context, ref oci.Reference, binPath, manifestPath string, opts Options) (digest.Digest, error) {
	return PushMultiPlatformArtifact(ctx, ref,
		[]PlatformBinary{{OS: runtimeGOOS, Arch: runtimeGOARCH, Path: binPath}},
		manifestPath, opts)
}

// PushMultiPlatformArtifact packages one or more per-platform adapter binaries
// and a single shared manifest into ONE OCI artifact and pushes it to ref. Each
// binary becomes its own layer titled bin/<os>/<arch>/<name>; the host selects
// the layer matching its platform at pull time. The manifest (adapter.yaml) is
// shared across platforms — its `platforms:` list must enumerate every platform
// supplied here (the adapter declares this in Info(); the publisher cross-builds
// the matching binaries).
//
// Returns the digest of the published manifest.
func PushMultiPlatformArtifact(ctx context.Context, ref oci.Reference, bins []PlatformBinary, manifestPath string, opts Options) (digest.Digest, error) {
	if !ref.FullyQualified() {
		return "", fmt.Errorf("publish: reference must be fully-qualified (got %q)", ref)
	}
	if len(bins) == 0 {
		return "", fmt.Errorf("publish: at least one platform binary is required")
	}

	mfData, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", fmt.Errorf("publish: read manifest: %w", err)
	}

	store := memory.New()
	if _, err = buildManifestInStore(ctx, store, ref, bins, mfData); err != nil {
		return "", err
	}

	repo, err := newRepository(ref, opts)
	if err != nil {
		return "", err
	}

	pushedDesc, err := oras.Copy(ctx, store, ref.Tag, repo, ref.Tag, oras.DefaultCopyOptions)
	if err != nil {
		return "", fmt.Errorf("publish: push to registry: %w", err)
	}

	if opts.Signer != nil {
		if _, err := signArtifact(ctx, repo, ref, &pushedDesc, opts.Signer); err != nil {
			return "", err
		}
	}

	return pushedDesc.Digest, nil
}

func buildManifestInStore(ctx context.Context, store *memory.Store, ref oci.Reference, bins []PlatformBinary, mfData []byte) (ocispec.Descriptor, error) {
	mfDesc, err := stageLayer(ctx, store, mfData, ocispec.MediaTypeImageLayer, "adapter.yaml")
	if err != nil {
		return ocispec.Descriptor{}, err
	}

	binLayers, err := stageBinaryLayers(ctx, store, bins)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	layers := append([]ocispec.Descriptor{mfDesc}, binLayers...)

	cfgDesc, err := stageConfig(ctx, store)
	if err != nil {
		return ocispec.Descriptor{}, err
	}

	manifest := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    cfgDesc,
		Layers:    layers,
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("publish: marshal manifest: %w", err)
	}
	manifestDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    digest.FromBytes(manifestJSON),
		Size:      int64(len(manifestJSON)),
	}
	if err := store.Push(ctx, manifestDesc, bytes.NewReader(manifestJSON)); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("publish: push manifest: %w", err)
	}
	if err := store.Tag(ctx, manifestDesc, ref.Tag); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("publish: tag manifest: %w", err)
	}
	return manifestDesc, nil
}

// stageBinaryLayers stages one adapter-binary layer per platform, titled
// bin/<os>/<arch>/<name>. All platforms must share the same binary basename (the
// adapter type's canonical name, e.g. criteria-adapter-copilot) so the host can
// resolve bin/<goos>/<goarch>/criteria-adapter-<type> uniformly.
func stageBinaryLayers(ctx context.Context, store *memory.Store, bins []PlatformBinary) ([]ocispec.Descriptor, error) {
	layers := make([]ocispec.Descriptor, 0, len(bins))
	seen := make(map[string]bool, len(bins))
	var binBase string
	for _, b := range bins {
		if b.OS == "" || b.Arch == "" {
			return nil, fmt.Errorf("publish: platform binary %q is missing os/arch", b.Path)
		}
		key := b.OS + "/" + b.Arch
		if seen[key] {
			return nil, fmt.Errorf("publish: duplicate platform %q", key)
		}
		seen[key] = true

		base := filepath.Base(b.Path)
		if binBase == "" {
			binBase = base
		} else if base != binBase {
			return nil, fmt.Errorf("publish: platform binaries must share one basename; got %q and %q", binBase, base)
		}

		binData, err := os.ReadFile(b.Path)
		if err != nil {
			return nil, fmt.Errorf("publish: read binary %s: %w", b.Path, err)
		}
		binTitle := filepath.ToSlash(filepath.Join("bin", b.OS, b.Arch, base))
		binDesc, err := stageLayer(ctx, store, binData, mediaTypeAdapterBinary, binTitle)
		if err != nil {
			return nil, err
		}
		layers = append(layers, binDesc)
	}
	return layers, nil
}

func stageLayer(ctx context.Context, store *memory.Store, data []byte, mediaType, title string) (ocispec.Descriptor, error) {
	desc := ocispec.Descriptor{
		MediaType: mediaType,
		Digest:    digest.FromBytes(data),
		Size:      int64(len(data)),
		Annotations: map[string]string{
			oci.AnnotationTitle: title,
		},
	}
	if err := store.Push(ctx, desc, bytes.NewReader(data)); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("publish: push layer %q: %w", title, err)
	}
	return desc, nil
}

func stageConfig(ctx context.Context, store *memory.Store) (ocispec.Descriptor, error) {
	data := []byte("{}")
	desc := ocispec.Descriptor{
		MediaType: mediaTypeAdapterConfig,
		Digest:    digest.FromBytes(data),
		Size:      int64(len(data)),
	}
	if err := store.Push(ctx, desc, bytes.NewReader(data)); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("publish: push config: %w", err)
	}
	return desc, nil
}

// runtimeGOOS and runtimeGOARCH are overridden by tests.
var (
	runtimeGOOS   = runtime.GOOS
	runtimeGOARCH = runtime.GOARCH
)

func newRepository(ref oci.Reference, opts Options) (*remote.Repository, error) {
	repoRef := ref.Registry
	if ref.Repo != "" {
		repoRef += "/" + ref.Repo
	}
	repo, err := remote.NewRepository(repoRef)
	if err != nil {
		return nil, fmt.Errorf("publish: build remote repository for %q: %w", repoRef, err)
	}
	repo.PlainHTTP = opts.PlainHTTP || oci.IsLocalhost(ref.Registry)

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
