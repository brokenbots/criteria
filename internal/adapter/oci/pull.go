package oci

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
)

// AuthProvider supplies registry credentials for a given registry host.
type AuthProvider interface {
	// Credential returns the credential for the given registry host (host:port).
	// A zero-value auth.Credential means anonymous access.
	Credential(ctx context.Context, hostport string) (auth.Credential, error)
}

// DefaultAuthProvider returns an AuthProvider that reads credentials from
// ~/.docker/config.json and well-known credential helpers (AWS ECR, GCR, etc.).
// Falls back to anonymous access when no credential is found.
func DefaultAuthProvider() AuthProvider {
	store, err := credentials.NewStoreFromDocker(credentials.StoreOptions{})
	if err != nil {
		// If the Docker config cannot be loaded (e.g. no ~/.docker directory),
		// fall back to anonymous access rather than failing at construction time.
		return &anonAuthProvider{}
	}
	return &dockerAuthProvider{store: store}
}

// dockerAuthProvider resolves credentials from the Docker credential store.
type dockerAuthProvider struct {
	store *credentials.DynamicStore
}

func (d *dockerAuthProvider) Credential(ctx context.Context, hostport string) (auth.Credential, error) {
	return d.store.Get(ctx, hostport)
}

// anonAuthProvider always returns empty (anonymous) credentials.
type anonAuthProvider struct{}

func (a *anonAuthProvider) Credential(_ context.Context, _ string) (auth.Credential, error) {
	return auth.Credential{}, nil
}

// Puller fetches OCI artifacts from a remote registry into a local Layout.
type Puller struct {
	Layout *Layout
	// Auth resolves registry credentials. If nil, DefaultAuthProvider is used.
	Auth AuthProvider
	// PlainHTTP allows unauthenticated HTTP (for local test registries).
	PlainHTTP bool
}

// Pull fetches the artifact identified by ref and writes all blobs into the
// Layout. It returns the resolved manifest digest. The caller can subsequently
// call Layout.Open(d) to read the adapter.yaml and binary blobs.
//
// If the artifact is already present in the layout (by digest), Pull is a
// no-op that returns the cached digest.
func (p *Puller) Pull(ctx context.Context, ref Reference) (digest.Digest, error) {
	if !ref.FullyQualified() {
		return "", fmt.Errorf("oci: pull requires a fully-qualified reference (got %q)", ref)
	}

	repo, err := p.newRepository(ref)
	if err != nil {
		return "", err
	}

	store, err := oci.New(p.Layout.Root)
	if err != nil {
		return "", fmt.Errorf("oci: open oras store: %w", err)
	}
	store.AutoSaveIndex = true

	tag := ref.Tag
	if ref.Digest != "" {
		tag = ref.Digest.String()
	}

	desc, err := oras.Copy(ctx, repo, tag, store, tag, oras.DefaultCopyOptions)
	if err != nil {
		return "", fmt.Errorf("oci: pull %s: %w", ref, err)
	}

	// Annotate the index descriptor with the protocol/schema version so
	// the host loader can discriminate cached artifacts without re-parsing
	// adapter.yaml.
	if err := p.annotateIndex(&desc); err != nil {
		return "", err
	}

	return desc.Digest, nil
}

// Resolve queries the registry for the canonical digest of ref without
// fetching any blobs. Used by the lockfile command (WS07) to compute lockfile
// entries cheaply.
func (p *Puller) Resolve(ctx context.Context, ref Reference) (digest.Digest, error) {
	if ref.Registry == "" {
		return "", fmt.Errorf("oci: resolve requires a registry in reference (got %q)", ref)
	}

	repo, err := p.newRepository(ref)
	if err != nil {
		return "", err
	}

	tag := ref.Tag
	if ref.Digest != "" {
		tag = ref.Digest.String()
	}
	if tag == "" {
		return "", fmt.Errorf("oci: resolve requires a tag or digest in reference (got %q)", ref)
	}

	desc, err := repo.Resolve(ctx, tag)
	if err != nil {
		return "", fmt.Errorf("oci: resolve %s: %w", ref, err)
	}
	return desc.Digest, nil
}

// ListTags returns every tag published for ref's registry+repo. It is used by
// the lockfile command to resolve a semver constraint (e.g. "^1.2") to a
// concrete tag before pinning the digest. Tag/digest components of ref are
// ignored; only Registry+Repo matter.
func (p *Puller) ListTags(ctx context.Context, ref Reference) ([]string, error) {
	if ref.Registry == "" || ref.Repo == "" {
		return nil, fmt.Errorf("oci: list tags requires a registry and repo (got %q)", ref)
	}
	repo, err := p.newRepository(ref)
	if err != nil {
		return nil, err
	}
	var tags []string
	if err := repo.Tags(ctx, "", func(page []string) error {
		tags = append(tags, page...)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("oci: list tags for %s: %w", ref, err)
	}
	return tags, nil
}

// newRepository builds the oras-go remote.Repository for ref.
func (p *Puller) newRepository(ref Reference) (*remote.Repository, error) {
	repoRef := ref.Registry
	if ref.Repo != "" {
		repoRef += "/" + ref.Repo
	}
	repo, err := remote.NewRepository(repoRef)
	if err != nil {
		return nil, fmt.Errorf("oci: build remote repository for %q: %w", repoRef, err)
	}
	repo.PlainHTTP = p.PlainHTTP || isLocalhost(ref.Registry)

	ap := p.Auth
	if ap == nil {
		ap = DefaultAuthProvider()
	}
	repo.Client = &auth.Client{
		Client: http.DefaultClient,
		Credential: func(ctx context.Context, hostport string) (auth.Credential, error) {
			return ap.Credential(ctx, hostport)
		},
	}
	return repo, nil
}

// annotateIndex updates the index.json entry for the given descriptor,
// adding protocol/schema version annotations. The index is locked during
// the update.
func (p *Puller) annotateIndex(desc *ocispec.Descriptor) error {
	release, err := p.Layout.Lock()
	if err != nil {
		return err
	}
	defer release()

	ix, err := p.Layout.Index()
	if err != nil {
		return err
	}

	for i, m := range ix.Manifests {
		if m.Digest == desc.Digest {
			if ix.Manifests[i].Annotations == nil {
				ix.Manifests[i].Annotations = make(map[string]string)
			}
			ix.Manifests[i].Annotations[AnnotationProtocolVersion] = "2"
			ix.Manifests[i].Annotations[AnnotationSchemaVersion] = "1"
			return p.Layout.WriteIndex(ix)
		}
	}
	// Descriptor not yet in index — unexpected after a successful oras.Copy.
	return fmt.Errorf("oci: annotateIndex: descriptor %s not found in index.json after pull", desc.Digest)
}

// isLocalhost reports whether host (which may include a :port suffix) refers
// to the local machine. When true, the puller defaults to PlainHTTP so that
// local test registries work without an explicit flag.
func isLocalhost(host string) bool {
	host = strings.ToLower(host)
	if idx := strings.LastIndex(host, ":"); idx != -1 {
		host = host[:idx]
	}
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
