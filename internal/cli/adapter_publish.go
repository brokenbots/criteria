package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/brokenbots/criteria/internal/adapter/oci"
	"github.com/brokenbots/criteria/internal/adapter/publish"
)

func newAdapterPublishCmd() *cobra.Command {
	var (
		registry  string
		withImage bool
	)

	cmd := &cobra.Command{
		Use:   "publish <path>",
		Short: "Publish a built adapter binary to an OCI registry (developer convenience)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			return runPublish(cmd.Context(), args[0], registry, withImage)
		},
	}

	cmd.Flags().StringVar(&registry, "registry", "", "Registry alias or full reference to push to")
	cmd.Flags().BoolVar(&withImage, "with-image", false, "Also build and push a runnable container image")
	return cmd
}

func runPublish(ctx context.Context, binPath, registry string, withImage bool) error {
	binPath, err := filepath.Abs(binPath)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}

	// Verify binary exists.
	if _, err := os.Stat(binPath); err != nil {
		return fmt.Errorf("stat binary: %w", err)
	}

	// Run binary with --emit-manifest to extract adapter.yaml.
	out, err := exec.CommandContext(ctx, binPath, "--emit-manifest").Output()
	if err != nil {
		return fmt.Errorf("--emit-manifest failed: %w", err)
	}

	// Write manifest to a temporary file for the publish package.
	tmpDir, err := os.MkdirTemp("", "criteria-publish-")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	mfPath := filepath.Join(tmpDir, "adapter.yaml")
	if err := os.WriteFile(mfPath, out, 0o644); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}

	// Resolve registry reference.
	if registry == "" {
		return fmt.Errorf("--registry is required (provide a fully-qualified reference or alias)")
	}

	ref, err := Resolve(ResolveContext{}, registry)
	if err != nil {
		return fmt.Errorf("resolve registry: %w", err)
	}

	if withImage {
		return fmt.Errorf("--with-image is not yet implemented")
	}

	// Push the artifact.
	var _ oci.Reference = ref // ensure import is used
	dg, err := publish.PushArtifact(ctx, ref, binPath, mfPath, publish.Options{})
	if err != nil {
		return fmt.Errorf("publish artifact: %w", err)
	}

	fmt.Printf("Published %s to %s (digest: %s)\n", filepath.Base(binPath), ref, dg)
	return nil
}
