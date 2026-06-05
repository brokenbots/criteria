package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/brokenbots/criteria/internal/adapter/publish"
)

func newAdapterPublishCmd() *cobra.Command {
	var (
		registry  string
		withImage bool
		signKey   string
	)

	cmd := &cobra.Command{
		Use:   "publish <path>",
		Short: "Publish a built adapter binary to an OCI registry (developer convenience)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			return runPublish(cmd.Context(), args[0], registry, signKey, withImage, cmd.OutOrStdout())
		},
	}

	cmd.Flags().StringVar(&registry, "registry", "", "Registry alias or full reference to push to")
	cmd.Flags().BoolVar(&withImage, "with-image", false, "Also build and push a runnable container image")
	cmd.Flags().StringVar(&signKey, "sign-key", "", "Path to a PEM Ed25519 private key; attaches a cosign signature (keyless OIDC signing is CI-only)")
	return cmd
}

func runPublish(ctx context.Context, binPath, registry, signKey string, withImage bool, out io.Writer) error {
	if out == nil {
		out = os.Stdout
	}
	binPath, err := filepath.Abs(binPath)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}

	// Verify binary exists.
	if _, err := os.Stat(binPath); err != nil {
		return fmt.Errorf("stat binary: %w", err)
	}

	// Run binary with --emit-manifest to extract adapter.yaml.
	outBytes, err := exec.CommandContext(ctx, binPath, "--emit-manifest").Output()
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
	if err := os.WriteFile(mfPath, outBytes, 0o644); err != nil {
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

	opts := publish.Options{}
	if signKey != "" {
		signer, err := publish.LoadKeySignerPEM(signKey)
		if err != nil {
			return err
		}
		opts.Signer = signer
	}

	// Push the artifact (and attach a cosign signature when --sign-key is set).
	dg, err := publish.PushArtifact(ctx, ref, binPath, mfPath, opts)
	if err != nil {
		return fmt.Errorf("publish artifact: %w", err)
	}

	signedNote := ""
	if signKey != "" {
		signedNote = " (signed)"
	}
	fmt.Fprintf(out, "Published %s to %s (digest: %s)%s\n", filepath.Base(binPath), ref, dg, signedNote)
	return nil
}
