package cli

import (
	"context"
	"fmt"
	"os"
	"runtime"

	"github.com/opencontainers/go-digest"
	"github.com/spf13/cobra"

	"github.com/brokenbots/criteria/internal/adapter/manifest"
	"github.com/brokenbots/criteria/internal/adapter/oci"
	"github.com/brokenbots/criteria/internal/adapter/signing"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

func newAdapterPullCmd() *cobra.Command {
	var (
		allowUnsigned bool
		registryAlias string
	)

	cmd := &cobra.Command{
		Use:   "pull \u003cref\u003e",
		Short: "Pull an adapter artifact from an OCI registry into the local cache",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			return runPull(cmd.Context(), args[0], allowUnsigned, registryAlias)
		},
	}

	cmd.Flags().BoolVar(&allowUnsigned, "allow-unsigned", false, "Skip signature verification for this pull")
	cmd.Flags().StringVar(&registryAlias, "registry", "", "Registry alias to use for short-name resolution")
	return cmd
}

func runPull(ctx context.Context, rawRef string, allowUnsigned bool, registryAlias string) error {
	layout, err := openDefaultCache()
	if err != nil {
		return err
	}

	ref, err := resolveInput(rawRef, registryAlias)
	if err != nil {
		return err
	}

	policy, err := signing.PolicyFor(signing.PullContext{
		AllowUnsigned: allowUnsigned,
	})
	if err != nil {
		return fmt.Errorf("signing policy: %w", err)
	}

	puller := &oci.Puller{Layout: layout}
	dg, err := puller.Pull(ctx, ref)
	if err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}

	m, signer, err := validatePulledArtifact(ctx, layout, dg, &policy)
	if err != nil {
		return err
	}

	if err := assertHostPlatformSupported(ref, m); err != nil {
		return err
	}

	// Container-image fetch (if applicable and env is container-mode).
	// TODO: detect container-mode environment and pull image if needed.
	_ = m.ContainerImage

	// Update lockfile.  Since pull is not workflow-scoped, we cannot set
	// Type/Name here.  The lockfile entry is written only when the caller
	// (e.g. `adapter lock`) knows the workflow adapter mapping.
	_ = lockfile.BuildEntry

	printPullSummary(ref, dg, signer, m)
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

func printPullSummary(ref oci.Reference, dg digest.Digest, signer *signing.SignerIdentity, m *manifest.Manifest) {
	fmt.Fprintf(os.Stdout, "Pulled %s\n", ref)
	fmt.Fprintf(os.Stdout, "Digest:    %s\n", dg)
	if signer != nil {
		if signer.Keyless != nil {
			fmt.Fprintf(os.Stdout, "Signer:    %s (issuer: %s)\n", signer.Keyless.Subject, signer.Keyless.Issuer)
		} else if signer.Key != nil {
			fmt.Fprintf(os.Stdout, "Signer:    key %s/%s\n", signer.Key.Algorithm, signer.Key.Fingerprint)
		}
	} else {
		fmt.Fprintf(os.Stdout, "Signer:    (unsigned)\n")
	}
	fmt.Fprintf(os.Stdout, "Platforms: %v\n", m.Platforms)
}
