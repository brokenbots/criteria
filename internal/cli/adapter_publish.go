package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"
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

	// TODO: construct OCI artifact and push via oras-go/v2.
	_ = out
	_ = registry
	_ = withImage

	return fmt.Errorf("publish is not yet fully implemented (waiting on internal/adapter/publish/ shared library)")
}
