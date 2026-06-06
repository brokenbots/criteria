package publish

import (
	"context"
	"fmt"
	"os"

	"github.com/opencontainers/go-digest"
	"gopkg.in/yaml.v3"

	"github.com/brokenbots/criteria/internal/adapter/manifest"
	"github.com/brokenbots/criteria/internal/adapter/oci"
)

// RecordContainerImage resolves imageRef in its registry to a digest and writes
// the container_image block (D12b) into the adapter manifest at manifestPath.
//
// publish does not build the image. The adapter's own CI builds and pushes it
// (its Dockerfile, its toolchain) exactly as it builds and pushes the binary;
// this records the already-published image so the host can run the adapter
// under environment.runtime = "docker"|"podman" (D12c). The image is pinned by
// digest, and because the recorded manifest is embedded in the signed OCI
// artifact, the image digest is transitively protected by the artifact
// signature — no separate image signature is required for the trust chain.
func RecordContainerImage(ctx context.Context, manifestPath, imageRef string, opts Options) error {
	ref, err := oci.Parse(imageRef)
	if err != nil {
		return fmt.Errorf("publish: parse image reference %q: %w", imageRef, err)
	}
	if !ref.FullyQualified() {
		return fmt.Errorf("publish: image reference must be fully-qualified (got %q)", imageRef)
	}

	dg, err := resolveImageDigest(ctx, ref, opts)
	if err != nil {
		return err
	}

	m, err := manifest.ParseFile(manifestPath)
	if err != nil {
		return fmt.Errorf("publish: read manifest: %w", err)
	}
	m.ContainerImage = &manifest.ContainerImageRef{Ref: imageRef, Digest: dg.String()}
	if err := m.Validate(); err != nil {
		return fmt.Errorf("publish: manifest invalid after recording image: %w", err)
	}

	data, err := yaml.Marshal(m)
	if err != nil {
		return fmt.Errorf("publish: marshal manifest: %w", err)
	}
	if err := os.WriteFile(manifestPath, data, 0o600); err != nil {
		return fmt.Errorf("publish: write manifest: %w", err)
	}
	return nil
}

// resolveImageDigest queries the registry for imageRef's canonical digest
// without fetching any blobs. It fails closed if the image is not present, so
// --image can never record a reference the host would later fail to pull.
func resolveImageDigest(ctx context.Context, ref oci.Reference, opts Options) (digest.Digest, error) {
	repo, err := newRepository(ref, opts)
	if err != nil {
		return "", err
	}
	tag := ref.Tag
	if ref.Digest != "" {
		tag = ref.Digest.String()
	}
	if tag == "" {
		return "", fmt.Errorf("publish: image reference needs a tag or digest (got %q)", ref)
	}
	desc, err := repo.Resolve(ctx, tag)
	if err != nil {
		return "", fmt.Errorf("publish: resolve image %s: %w", ref, err)
	}
	return desc.Digest, nil
}
