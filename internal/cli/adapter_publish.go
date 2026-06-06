package cli

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

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
		Use:   "publish <binary | bin-tree-dir>",
		Short: "Publish a built adapter binary (or a bin/<os>/<arch>/ multi-platform tree) to an OCI registry",
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

func runPublish(ctx context.Context, path string, f *publishFlags, out io.Writer) error {
	if out == nil {
		out = os.Stdout
	}
	if f.registry == "" {
		return fmt.Errorf("--registry is required (provide a fully-qualified reference or alias)")
	}
	if f.keyless && f.signKey != "" {
		return fmt.Errorf("--keyless and --sign-key are mutually exclusive")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}
	bins, manifestBin, err := resolvePlatformBinaries(path)
	if err != nil {
		return err
	}

	mfPath, cleanup, err := f.prepareManifest(ctx, manifestBin)
	if err != nil {
		return err
	}
	defer cleanup()

	ref, err := Resolve(ResolveContext{}, f.registry)
	if err != nil {
		return fmt.Errorf("resolve registry: %w", err)
	}

	signer, err := f.buildSigner(ctx)
	if err != nil {
		return err
	}

	// Push the artifact (and attach a cosign signature when signing was requested).
	dg, err := publish.PushMultiPlatformArtifact(ctx, ref, bins, mfPath, publish.Options{Signer: signer})
	if err != nil {
		return fmt.Errorf("publish artifact: %w", err)
	}

	signedNote := ""
	if signer != nil {
		signedNote = " (signed)"
	}
	fmt.Fprintf(out, "Published %s [%s] to %s (digest: %s)%s\n",
		filepath.Base(manifestBin), formatPlatforms(bins), ref, dg, signedNote)
	return nil
}

// prepareManifest emits the adapter manifest (adapter.yaml) from manifestBin —
// which must be runnable on the publish host; its content, including the
// platforms list, is declared in the adapter's Info() and is identical
// regardless of which platform's binary emits it — and records an optional
// already-pushed container image (D12) into it so the host can run the adapter
// under environment.runtime. The caller must invoke the returned cleanup.
func (f *publishFlags) prepareManifest(ctx context.Context, manifestBin string) (mfPath string, cleanup func(), err error) {
	mfPath, cleanup, err = emitManifestToTemp(ctx, manifestBin)
	if err != nil {
		return "", nil, err
	}
	if f.imageRef != "" {
		if err := publish.RecordContainerImage(ctx, mfPath, f.imageRef, publish.Options{}); err != nil {
			cleanup()
			return "", nil, err
		}
	}
	return mfPath, cleanup, nil
}

func formatPlatforms(bins []publish.PlatformBinary) string {
	plats := make([]string, len(bins))
	for i, b := range bins {
		plats[i] = b.OS + "/" + b.Arch
	}
	return strings.Join(plats, ", ")
}

// resolvePlatformBinaries maps the publish argument to the binaries to package
// and the binary used to emit the manifest. A directory is a multi-platform bin
// tree (bin/<os>/<arch>/<name>); a regular file is a single host-platform binary.
func resolvePlatformBinaries(path string) (bins []publish.PlatformBinary, manifestBin string, err error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, "", fmt.Errorf("stat path: %w", err)
	}
	if info.IsDir() {
		return collectPlatformBinaries(path)
	}
	return []publish.PlatformBinary{{OS: runtime.GOOS, Arch: runtime.GOARCH, Path: path}}, path, nil
}

// collectPlatformBinaries discovers adapter binaries laid out as
// <root>/bin/<os>/<arch>/<name> and returns them as PlatformBinary entries plus
// the binary matching the publish host (used to emit the manifest). It is an
// error if no host-matching binary is present, since --emit-manifest must run
// on the publish host.
func collectPlatformBinaries(root string) (bins []publish.PlatformBinary, hostBin string, err error) {
	binRoot := filepath.Join(root, "bin")
	walkErr := filepath.WalkDir(binRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// Only files at exactly bin/<os>/<arch>/<name> are platform binaries.
		rel, relErr := filepath.Rel(binRoot, p)
		if relErr != nil {
			return relErr
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) != 3 {
			return nil
		}
		goos, goarch := parts[0], parts[1]
		bins = append(bins, publish.PlatformBinary{OS: goos, Arch: goarch, Path: p})
		if goos == runtime.GOOS && goarch == runtime.GOARCH {
			hostBin = p
		}
		return nil
	})
	if walkErr != nil {
		return nil, "", fmt.Errorf("scan bin tree %s: %w", binRoot, walkErr)
	}
	if len(bins) == 0 {
		return nil, "", fmt.Errorf("no binaries found under %s (expected bin/<os>/<arch>/<name>)", binRoot)
	}
	if hostBin == "" {
		return nil, "", fmt.Errorf("bin tree has no binary for the publish host %s/%s; include it so --emit-manifest can run", runtime.GOOS, runtime.GOARCH)
	}
	return bins, hostBin, nil
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
