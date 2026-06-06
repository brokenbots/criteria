package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/sigstore/cosign/v2/pkg/providers"
	_ "github.com/sigstore/cosign/v2/pkg/providers/github" // register GitHub Actions ambient OIDC provider

	"github.com/brokenbots/criteria/internal/adapter/manifest"
	"github.com/brokenbots/criteria/internal/adapter/publish"
)

// publishFlags holds the resolved `criteria adapter publish` flag values.
type publishFlags struct {
	registry  string
	imageRef  string
	signKey   string
	keyless   bool
	idToken   string
	fulcioURL string
}

func newAdapterPublishCmd() *cobra.Command {
	var f publishFlags

	cmd := &cobra.Command{
		Use:   "publish <path>",
		Short: "Publish a built adapter binary to an OCI registry (developer convenience)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			return runPublish(cmd.Context(), args[0], &f, cmd.OutOrStdout())
		},
	}

	cmd.Flags().StringVar(&f.registry, "registry", "", "Registry alias or full reference to push to")
	cmd.Flags().StringVar(&f.imageRef, "image", "", "Reference of an already-pushed runnable container image to record in the manifest (e.g. ghcr.io/org/name:1.2.3-image); the host runs it under environment.runtime")
	cmd.Flags().StringVar(&f.signKey, "sign-key", "", "Path to a PEM Ed25519 private key; attaches an explicit-key cosign signature")
	cmd.Flags().BoolVar(&f.keyless, "keyless", false, "Sign keyless via Sigstore Fulcio using an ambient OIDC identity (CI); mutually exclusive with --sign-key")
	cmd.Flags().StringVar(&f.idToken, "identity-token", "", "OIDC identity token for --keyless (defaults to SIGSTORE_ID_TOKEN or the ambient CI provider)")
	cmd.Flags().StringVar(&f.fulcioURL, "fulcio-url", publish.DefaultFulcioURL, "Fulcio CA URL for --keyless signing")
	return cmd
}

func runPublish(ctx context.Context, binPath string, f *publishFlags, out io.Writer) error {
	if out == nil {
		out = os.Stdout
	}
	if f.registry == "" {
		return fmt.Errorf("--registry is required (provide a fully-qualified reference or alias)")
	}
	if f.keyless && f.signKey != "" {
		return fmt.Errorf("--keyless and --sign-key are mutually exclusive")
	}
	binPath, err := filepath.Abs(binPath)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}
	if _, err := os.Stat(binPath); err != nil {
		return fmt.Errorf("stat binary: %w", err)
	}

	mfPath, cleanup, err := emitManifestToTemp(ctx, binPath)
	if err != nil {
		return err
	}
	defer cleanup()

	// Record an already-pushed runnable container image (D12) into the manifest
	// before it is embedded in the artifact, so the host can run the adapter
	// under environment.runtime. publish does not build the image — the
	// adapter's CI does, exactly as it builds the binary.
	if f.imageRef != "" {
		if err := publish.RecordContainerImage(ctx, mfPath, f.imageRef, publish.Options{}); err != nil {
			return err
		}
	}

	ref, err := Resolve(ResolveContext{}, f.registry)
	if err != nil {
		return fmt.Errorf("resolve registry: %w", err)
	}

	signer, err := f.buildSigner(ctx)
	if err != nil {
		return err
	}

	// Push the artifact (and attach a cosign signature when signing was requested).
	dg, err := publish.PushArtifact(ctx, ref, binPath, mfPath, publish.Options{Signer: signer})
	if err != nil {
		return fmt.Errorf("publish artifact: %w", err)
	}

	signedNote := ""
	if signer != nil {
		signedNote = " (signed)"
	}
	fmt.Fprintf(out, "Published %s to %s (digest: %s)%s\n", filepath.Base(binPath), ref, dg, signedNote)
	return nil
}

// buildSigner resolves the requested signing mode into a publish.Signer, or nil
// when no signing was requested. --sign-key and --keyless are mutually
// exclusive (validated in runPublish).
func (f *publishFlags) buildSigner(ctx context.Context) (publish.Signer, error) {
	switch {
	case f.signKey != "":
		return publish.LoadKeySignerPEM(f.signKey)
	case f.keyless:
		token, err := resolveIdentityToken(ctx, f.idToken)
		if err != nil {
			return nil, err
		}
		return publish.NewKeylessSigner(ctx, publish.KeylessOptions{IDToken: token, FulcioURL: f.fulcioURL})
	default:
		return nil, nil
	}
}

// resolveIdentityToken finds the OIDC identity token used for keyless signing.
// Precedence: explicit --identity-token, then SIGSTORE_ID_TOKEN, then an
// ambient CI provider (GitHub Actions, GitLab CI, Google, …). Returns a clear
// error when none is available, since keyless signing cannot proceed without
// a verifiable identity.
func resolveIdentityToken(ctx context.Context, explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if t := os.Getenv("SIGSTORE_ID_TOKEN"); t != "" {
		return t, nil
	}
	if providers.Enabled(ctx) {
		token, err := providers.Provide(ctx, "sigstore")
		if err != nil {
			return "", fmt.Errorf("obtain ambient OIDC token: %w", err)
		}
		return token, nil
	}
	return "", fmt.Errorf("--keyless requires an OIDC identity token: pass --identity-token, set SIGSTORE_ID_TOKEN, or run in a supported CI (GitHub Actions, GitLab CI, …)")
}

// emitManifestToTemp runs the adapter binary with --emit-manifest and writes the
// resulting adapter.yaml to a temp file. The returned cleanup removes it.
func emitManifestToTemp(ctx context.Context, binPath string) (mfPath string, cleanup func(), err error) {
	outBytes, err := exec.CommandContext(ctx, binPath, "--emit-manifest").Output()
	if err != nil {
		return "", nil, fmt.Errorf("--emit-manifest failed: %w", err)
	}
	tmpDir, err := os.MkdirTemp("", "criteria-publish-")
	if err != nil {
		return "", nil, fmt.Errorf("create temp dir: %w", err)
	}
	mfPath = filepath.Join(tmpDir, "adapter.yaml")
	if err := os.WriteFile(mfPath, outBytes, 0o644); err != nil {
		os.RemoveAll(tmpDir)
		return "", nil, fmt.Errorf("write manifest: %w", err)
	}
	// Validate before publishing so we fail fast with a clear error rather than
	// pushing an artifact the host would later reject at pull time.
	m, err := manifest.ParseFile(mfPath)
	if err != nil {
		os.RemoveAll(tmpDir)
		return "", nil, fmt.Errorf("parse emitted manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		os.RemoveAll(tmpDir)
		return "", nil, fmt.Errorf("emitted manifest is invalid: %w", err)
	}
	return mfPath, func() { os.RemoveAll(tmpDir) }, nil
}
