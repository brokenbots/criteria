package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"

	"github.com/opencontainers/go-digest"
	"github.com/spf13/cobra"

	"github.com/brokenbots/criteria/internal/adapter/environment/container"
	"github.com/brokenbots/criteria/internal/adapter/manifest"
	"github.com/brokenbots/criteria/internal/adapter/oci"
	"github.com/brokenbots/criteria/internal/adapter/signing"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

func newAdapterPullCmd() *cobra.Command {
	var (
		allowUnsigned   bool
		registryAlias   string
		trustedKeyPaths []string
	)

	cmd := &cobra.Command{
		Use:   "pull \u003cref\u003e",
		Short: "Pull an adapter artifact from an OCI registry into the local cache",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			return runPull(cmd.Context(), cmd.OutOrStdout(), args[0], allowUnsigned, registryAlias, trustedKeyPaths)
		},
	}

	cmd.Flags().BoolVar(&allowUnsigned, "allow-unsigned", false, "Skip signature verification for this pull")
	cmd.Flags().StringVar(&registryAlias, "registry", "", "Registry alias to use for short-name resolution")
	cmd.Flags().StringArrayVar(&trustedKeyPaths, "trusted-key", nil, "Path to a trusted PEM public key for key-mode verification (repeatable)")
	return cmd
}

func runPull(ctx context.Context, out io.Writer, rawRef string, allowUnsigned bool, registryAlias string, trustedKeyPaths []string) error {
	layout, err := openDefaultCache()
	if err != nil {
		return err
	}

	ref, err := resolveInput(rawRef, registryAlias)
	if err != nil {
		return err
	}

	// pull is not workflow-scoped, so there is no workflow `verification` attr;
	// the shared resolver still honors --allow-unsigned and CRITERIA_ALLOW_UNSIGNED.
	// Trusted keys come from the global trust config plus any --trusted-key flags.
	trustedKeys, err := loadTrustedKeys("", trustedKeyPaths)
	if err != nil {
		return fmt.Errorf("load trusted keys: %w", err)
	}
	policy, err := resolveSigningPolicy(allowUnsigned, "", trustedKeys)
	if err != nil {
		return fmt.Errorf("signing policy: %w", err)
	}

	puller := &oci.Puller{Layout: layout}
	return runPullWithPuller(ctx, out, ref, layout, &policy, puller)
}

// puller is the minimal surface runPull needs from an OCI puller. It exists
// so tests can inject a fake registry/artifact without a live Docker daemon.
type puller interface {
	Pull(context.Context, oci.Reference) (digest.Digest, error)
}

func runPullWithPuller(ctx context.Context, out io.Writer, ref oci.Reference, layout *oci.Layout, policy *signing.Policy, p puller) error {
	dg, err := p.Pull(ctx, ref)
	if err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}

	m, signer, err := validatePulledArtifact(ctx, layout, dg, policy)
	if err != nil {
		return err
	}

	if err := assertHostPlatformSupported(ref, m); err != nil {
		return err
	}

	// Container-image fetch (if applicable).
	if m.ContainerImage != nil {
		if err := pullContainerImage(ctx, out, *m.ContainerImage); err != nil {
			return err
		}
	}

	// Extract the platform binary to the digest-addressed install path so the
	// adapter is resolvable for compile-time schema verification without
	// requiring a prior run.
	extractPath, err := extractOCIAdapterBinary(layout, dg, m.Name)
	if err != nil {
		return fmt.Errorf("extract adapter binary: %w", err)
	}

	// Update lockfile.  Since pull is not workflow-scoped, we cannot set
	// Type/Name here.  The lockfile entry is written only when the caller
	// (e.g. `adapter lock`) knows the workflow adapter mapping.
	_ = lockfile.BuildEntry

	printPullSummary(out, ref, dg, signer, m, extractPath)
	return nil
}

func openDefaultCache() (*oci.Layout, error) {
	cacheRoot, err := defaultCacheRoot()
	if err != nil {
		return nil, err
	}
	return oci.Open(cacheRoot)
}

func resolveInput(rawRef, registryAlias string) (oci.Reference, error) {
	rctx := ResolveContext{}
	if registryAlias != "" {
		rctx.WorkflowAliases = map[string]string{"default": registryAlias}
	}
	return Resolve(rctx, rawRef)
}

func validatePulledArtifact(ctx context.Context, layout *oci.Layout, dg digest.Digest, policy *signing.Policy) (*manifest.Manifest, *signing.SignerIdentity, error) {
	artFS, err := layout.Open(dg)
	if err != nil {
		return nil, nil, fmt.Errorf("open pulled artifact: %w", err)
	}

	m, err := manifest.ParseFromFS(artFS, "adapter.yaml")
	if err != nil {
		return nil, nil, fmt.Errorf("read adapter.yaml: %w", err)
	}

	if err := m.Validate(); err != nil {
		return nil, nil, fmt.Errorf("validate manifest: %w", err)
	}

	signer, err := signing.Verify(ctx, layout, dg, *policy)
	if err != nil {
		return nil, nil, fmt.Errorf("signature verification: %w", err)
	}
	return m, signer, nil
}

func assertHostPlatformSupported(ref oci.Reference, m *manifest.Manifest) error {
	hostPlatform := runtime.GOOS + "/" + runtime.GOARCH
	for _, p := range m.Platforms {
		if p.OS+"/"+p.Arch == hostPlatform {
			return nil
		}
	}
	return fmt.Errorf("adapter %s does not support host platform %s; contact the publisher to add support", ref, hostPlatform)
}

func printPullSummary(out io.Writer, ref oci.Reference, dg digest.Digest, signer *signing.SignerIdentity, m *manifest.Manifest, extractPath string) {
	fmt.Fprintf(out, "Pulled %s\n", ref)
	fmt.Fprintf(out, "Digest:    %s\n", dg)
	if signer != nil {
		if signer.Keyless != nil {
			fmt.Fprintf(out, "Signer:    %s (issuer: %s)\n", signer.Keyless.Subject, signer.Keyless.Issuer)
		} else if signer.Key != nil {
			fmt.Fprintf(out, "Signer:    key %s/%s\n", signer.Key.Algorithm, signer.Key.Fingerprint)
		}
	} else {
		fmt.Fprintf(out, "Signer:    (unsigned)\n")
	}
	fmt.Fprintf(out, "Platforms: %v\n", m.Platforms)
	fmt.Fprintf(out, "Extracted: %s\n", extractPath)
}

// pullContainerImage tries docker pull, then podman pull, returning the first
// success or a combined error if neither runtime is available.
func pullContainerImage(ctx context.Context, out io.Writer, ref manifest.ContainerImageRef) error {
	for _, runtime := range []string{"docker", "podman"} {
		err := container.PullContainerImage(ctx, ref, runtime)
		if err == nil {
			fmt.Fprintf(out, "Container image pulled (%s): %s\n", runtime, ref.Ref)
			return nil
		}
		if isExecNotFound(err) {
			continue // try next runtime
		}
		return err // real pull failure
	}
	return fmt.Errorf("no container runtime (docker or podman) found to pull image %s", ref.Ref)
}

func isExecNotFound(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, exec.ErrNotFound)
}
