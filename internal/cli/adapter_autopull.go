package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/opencontainers/go-digest"

	"github.com/brokenbots/criteria/internal/adapter/environment/container"
	"github.com/brokenbots/criteria/internal/adapter/manifest"
	"github.com/brokenbots/criteria/internal/adapter/oci"
	"github.com/brokenbots/criteria/internal/adapter/signing"
	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

// autoPullCompileAdapters is called by the compile and apply paths when the
// workflow contains adapter blocks with a `source` attribute.  It validates the
// lockfile, pulls any missing cached binaries, and extracts the platform binary
// to the digest-addressed install dir so adapterhost can resolve them.
//
// allowUnsigned forces the unsigned-override (WS46); the workflow-level
// `verification` attribute (off|warn|strict) is read from the spec header. The
// resolved policy governs signature verification of every pulled artifact.
func autoPullCompileAdapters(ctx context.Context, workflowDir string, spec *workflow.Spec, allowUnsigned bool) error {
	// Read lockfile.
	lf, err := lockfile.ReadFromDir(workflowDir)
	if err != nil {
		return fmt.Errorf("read lockfile: %w", err)
	}
	if lf == nil {
		return fmt.Errorf("workflow uses OCI adapter references but %q is missing; run `criteria adapter lock`", filepath.Join(workflowDir, ".criteria.lock.hcl"))
	}

	// Build set of OCI-referenced adapters.
	ociAdapters := collectWorkflowAdapters(spec)

	// Validate lockfile covers all workflow adapters.
	missing := []string{}
	for key, wa := range ociAdapters {
		if wa.Source == "" {
			continue // not OCI-based
		}
		if findLocked(lf, wa.Type, wa.Name) == nil {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("lockfile missing entries for adapters: %v; run `criteria adapter lock`", missing)
	}

	cacheRoot, err := defaultCacheRoot()
	if err != nil {
		return err
	}
	layout, err := oci.Open(cacheRoot)
	if err != nil {
		return fmt.Errorf("open OCI cache: %w", err)
	}

	workflowVerification := ""
	if spec.Header != nil {
		workflowVerification = spec.Header.Verification
	}
	trustedKeys, err := loadTrustedKeys(workflowDir, nil)
	if err != nil {
		return fmt.Errorf("load trusted keys: %w", err)
	}
	policy, err := resolveSigningPolicy(allowUnsigned, workflowVerification, trustedKeys)
	if err != nil {
		return fmt.Errorf("signing policy: %w", err)
	}
	puller := &oci.Puller{Layout: layout}

	for key, wa := range ociAdapters {
		if wa.Source == "" {
			continue
		}
		if err := ensureAdapterCached(ctx, key, wa, lf, layout, puller, &policy); err != nil {
			return err
		}
	}

	return nil
}

func ensureAdapterCached(ctx context.Context, key string, wa *workflowAdapter, lf *lockfile.Lockfile, layout *oci.Layout, puller *oci.Puller, policy *signing.Policy) error {
	entry := findLocked(lf, wa.Type, wa.Name)
	if entry == nil {
		return nil // validated above
	}

	dg := digest.Digest(entry.ResolvedDigest)
	if dg == "" {
		return fmt.Errorf("adapter %q lockfile entry has empty digest", key)
	}

	// If binary already cached, re-verify against the pinned signer before use so
	// the trust anchor is enforced on every run (not just the first pull), then
	// ensure it's in the plugin directory.
	if layout.HasBlob(dg) {
		if err := verifyAgainstPin(ctx, key, layout, dg, entry, policy); err != nil {
			return err
		}
		if err := extractOCIAdapterBinary(layout, dg, wa.Type); err != nil {
			return fmt.Errorf("adapter %q extract binary: %w", key, err)
		}
		return nil
	}

	// Binary missing from cache — pull silently.
	ref, err := oci.Parse(entry.Reference)
	if err != nil {
		return fmt.Errorf("adapter %q parse reference %q: %w", key, entry.Reference, err)
	}

	pulledDg, err := puller.Pull(ctx, ref)
	if err != nil {
		return fmt.Errorf("adapter %q pull %s: %w", key, ref, err)
	}

	artFS, err := layout.Open(pulledDg)
	if err != nil {
		return fmt.Errorf("adapter %q open artifact: %w", key, err)
	}
	m, err := manifest.ParseFromFS(artFS, "adapter.yaml")
	if err != nil {
		return fmt.Errorf("adapter %q read manifest: %w", key, err)
	}
	if err := m.Validate(); err != nil {
		return fmt.Errorf("adapter %q validate manifest: %w", key, err)
	}
	if err := verifyAgainstPin(ctx, key, layout, pulledDg, entry, policy); err != nil {
		return err
	}

	if err := extractOCIAdapterBinary(layout, pulledDg, wa.Type); err != nil {
		return fmt.Errorf("adapter %q extract binary: %w", key, err)
	}

	return maybePullContainerImage(ctx, key, m)
}

// verifyAgainstPin verifies the cached artifact under a policy tightened to the
// lockfile-pinned signer (the trust anchor), then confirms the verified signer
// matches the pin. A nil signer (ModeOff, or a ModeWarn failure) skips the pin
// check; see policyForPin and assertSignerMatchesPin.
func verifyAgainstPin(ctx context.Context, key string, layout *oci.Layout, dg digest.Digest, entry *lockfile.LockedAdapter, policy *signing.Policy) error {
	effective := policyForPin(*policy, entry.Signature)
	signer, err := signing.Verify(ctx, layout, dg, effective)
	if err != nil {
		return fmt.Errorf("adapter %q signature verification: %w", key, err)
	}
	return assertSignerMatchesPin(key, signer, entry.Signature)
}

func maybePullContainerImage(ctx context.Context, key string, m *manifest.Manifest) error {
	if m.ContainerImage == nil {
		return nil
	}
	for _, runtime := range []string{"docker", "podman"} {
		err := container.PullContainerImage(ctx, *m.ContainerImage, runtime)
		if err == nil {
			return nil
		}
		if isExecNotFound(err) {
			continue
		}
		return fmt.Errorf("adapter %q container image pull %s: %w", key, m.ContainerImage.Ref, err)
	}
	return fmt.Errorf("adapter %q container image pull %s: no container runtime found", key, m.ContainerImage.Ref)
}

// hasOCIReferences reports whether any adapter block declares a `source`
// (OCI location).  If none do, the workflow does not require a lockfile for
// compilation.
func hasOCIReferences(spec *workflow.Spec) bool {
	for _, wa := range collectWorkflowAdapters(spec) {
		if wa.Source != "" {
			return true
		}
	}
	return false
}

// extractOCIAdapterBinary reads the platform-specific binary from the OCI
// artifact and copies it into a digest-addressed directory under the adapter
// install root (~/.criteria/adapters/<digest>/) so that multiple versions of
// the same adapter type can coexist and adapterhost.DiscoverBinaryAt can
// resolve the exact pinned binary.
func extractOCIAdapterBinary(layout *oci.Layout, dg digest.Digest, adapterType string) error {
	artFS, err := layout.Open(dg)
	if err != nil {
		return err
	}

	platformPath := fmt.Sprintf("bin/%s/%s/criteria-adapter-%s", runtime.GOOS, runtime.GOARCH, adapterType)
	f, err := artFS.Open(platformPath)
	if err != nil {
		return fmt.Errorf("open %s in artifact: %w", platformPath, err)
	}
	defer f.Close()

	dest, err := adapterhost.AdapterInstallPath(adapterType, adapterhost.EncodeDigest(dg))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}

	tmp := dest + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, f); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

// collectWorkflowAdapters reads the adapter declarations from the parsed spec.
// The `source`/`version` attributes decode directly onto AdapterDeclSpec, so no
// raw-HCL re-scan is needed. The returned map key is "type.name".
func collectWorkflowAdapters(spec *workflow.Spec) map[string]*workflowAdapter {
	out := make(map[string]*workflowAdapter, len(spec.Adapters))
	for i := range spec.Adapters {
		a := &spec.Adapters[i]
		key := a.Type + "." + a.Name
		out[key] = &workflowAdapter{Type: a.Type, Name: a.Name, Source: a.Source, Version: a.Version}
	}
	return out
}

// listHCLFiles returns all .hcl files in a directory (not recursive).
func listHCLFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !hasSuffix(e.Name(), ".hcl") {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	return out, nil
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}
