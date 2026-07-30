package cli

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

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
		return fmt.Errorf("workflow uses OCI adapter references but %q is missing; run `criteria adapter lock`", filepath.Join(workflowDir, workflow.LockfileName))
	}

	// Build set of OCI-referenced adapters and validate the lockfile covers them.
	ociAdapters := collectWorkflowAdapters(spec)
	if err := assertLockfileCoversAdapters(lf, ociAdapters); err != nil {
		return err
	}

	// Fail closed when the lockfile no longer matches the workflow's declared
	// adapter constraints. A stale pin is not safe to run silently, even if the
	// blob is still cached and the signer matches. The recommended recovery is
	// `criteria adapter lock`, which re-resolves against the declared constraints.
	aliases, err := collectWorkflowAliases(workflowDir, spec)
	if err != nil {
		return fmt.Errorf("collect aliases: %w", err)
	}
	if err := assertLockfileMatchesDeclarations(lf, ociAdapters, aliases); err != nil {
		return err
	}

	cacheRoot, err := defaultCacheRoot()
	if err != nil {
		return err
	}
	layout, err := oci.Open(cacheRoot)
	if err != nil {
		return fmt.Errorf("open OCI cache: %w", err)
	}

	policy, err := autoPullPolicy(workflowDir, spec, allowUnsigned)
	if err != nil {
		return err
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

	// Also pull adapters declared in subworkflows, which have their own lockfiles.
	if err := pullSubworkflowAdapters(ctx, workflowDir, spec, layout, puller, &policy); err != nil {
		return err
	}

	return nil
}

// assertLockfileCoversAdapters errors when any OCI-referenced workflow adapter
// has no entry in the lockfile.
func assertLockfileCoversAdapters(lf *lockfile.Lockfile, ociAdapters map[string]*workflowAdapter) error {
	var missing []string
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
	return nil
}

// assertLockfileMatchesDeclarations errors when a lockfile entry's source or
// version no longer matches the workflow's declared constraint. This guards the
// run path so a stale pin cannot be used silently after the workflow author has
// changed the declaration.
func assertLockfileMatchesDeclarations(lf *lockfile.Lockfile, ociAdapters map[string]*workflowAdapter, aliases map[string]string) error {
	var mismatches []string
	for key, wa := range ociAdapters {
		if wa.Source == "" {
			continue // not OCI-based
		}
		entry := findLocked(lf, wa.Type, wa.Name)
		if entry == nil {
			continue // covered by assertLockfileCoversAdapters
		}
		if reason := declMatchesPin(wa, entry, aliases); reason != "" {
			mismatches = append(mismatches, fmt.Sprintf("%s: %s", key, reason))
		}
	}
	if len(mismatches) > 0 {
		return fmt.Errorf("lockfile does not match workflow adapter declarations:\n  - %s\nrun `criteria adapter lock` to update it", strings.Join(mismatches, "\n  - "))
	}
	return nil
}

// autoPullPolicy resolves the signing policy for the auto-pull path from the
// override flag, the workflow `verification` attribute, and the trusted keys.
func autoPullPolicy(workflowDir string, spec *workflow.Spec, allowUnsigned bool) (signing.Policy, error) {
	workflowVerification := ""
	if spec.Header != nil {
		workflowVerification = spec.Header.Verification
	}
	trustedKeys, err := loadTrustedKeys(workflowDir, nil)
	if err != nil {
		return signing.Policy{}, fmt.Errorf("load trusted keys: %w", err)
	}
	policy, err := resolveSigningPolicy(allowUnsigned, workflowVerification, trustedKeys)
	if err != nil {
		return signing.Policy{}, fmt.Errorf("signing policy: %w", err)
	}
	return policy, nil
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
	effective := policyForPin(policy, entry.Signature)
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
//
// adapterType is the workflow's `adapter "<type>" "<name>"` label. It names the
// binary at the *destination* (that is the identity adapterhost routes on), but
// it never selects the source path inside the artifact: the artifact says where
// its own binary lives. A workflow is free to label an adapter whatever it
// likes, so the two names need not agree.
func extractOCIAdapterBinary(layout *oci.Layout, dg digest.Digest, adapterType string) error {
	artFS, err := layout.Open(dg)
	if err != nil {
		return err
	}

	platformPath, err := artifactBinaryPath(artFS)
	if err != nil {
		return err
	}
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

// artifactBinaryPath locates this host's platform binary inside an adapter
// artifact. The name is supplied by the adapter: preferentially the manifest's
// own `name` field, otherwise the sole file published under bin/<os>/<arch>/.
//
// Manifest.Validate constrains name to ^[a-z][a-z0-9-]*$, so it cannot escape
// the artifact FS; the directory scan only accepts plain, non-directory names.
func artifactBinaryPath(artFS fs.FS) (string, error) {
	dir := path.Join("bin", runtime.GOOS, runtime.GOARCH)

	if m, err := manifest.ParseFromFS(artFS, "adapter.yaml"); err == nil && m.Validate() == nil {
		named := path.Join(dir, adapterhost.AdapterBinaryName(m.Name))
		if info, err := fs.Stat(artFS, named); err == nil && !info.IsDir() {
			return named, nil
		}
	}

	entries, err := fs.ReadDir(artFS, dir)
	if err != nil {
		return "", fmt.Errorf("artifact has no binary for %s/%s (published platforms: %s)",
			runtime.GOOS, runtime.GOARCH, strings.Join(artifactPlatforms(artFS), ", "))
	}

	found := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || strings.ContainsRune(e.Name(), '/') {
			continue
		}
		found = append(found, e.Name())
	}
	switch len(found) {
	case 1:
		return path.Join(dir, found[0]), nil
	case 0:
		return "", fmt.Errorf("artifact has no binary for %s/%s (published platforms: %s)",
			runtime.GOOS, runtime.GOARCH, strings.Join(artifactPlatforms(artFS), ", "))
	default:
		return "", fmt.Errorf("artifact publishes %d files under %s (%s); its adapter.yaml `name` must select one",
			len(found), dir, strings.Join(found, ", "))
	}
}

// artifactPlatforms lists the os/arch pairs the artifact ships binaries for, so
// a platform miss can say what it does have. Best effort: used only in errors.
func artifactPlatforms(artFS fs.FS) []string {
	oses, err := fs.ReadDir(artFS, "bin")
	if err != nil {
		return []string{"none"}
	}
	var out []string
	for _, osEntry := range oses {
		arches, err := fs.ReadDir(artFS, path.Join("bin", osEntry.Name()))
		if err != nil {
			continue
		}
		for _, arch := range arches {
			out = append(out, osEntry.Name()+"/"+arch.Name())
		}
	}
	if len(out) == 0 {
		return []string{"none"}
	}
	return out
}

// pullSubworkflowAdapters recursively processes subworkflows declared in spec
// and ensures their OCI adapter binaries are extracted. Each subworkflow has
// its own lockfile; this is necessary because autoPullCompileAdapters only
// sees the top-level spec's adapters.
func pullSubworkflowAdapters(ctx context.Context, workflowDir string, spec *workflow.Spec, layout *oci.Layout, puller *oci.Puller, policy *signing.Policy) error {
	for _, swSpec := range spec.Subworkflows {
		subDir := resolveSubworkflowSourceDir(workflowDir, swSpec.Source)
		subLF, err := lockfile.ReadFromDir(subDir)
		if err != nil || subLF == nil {
			continue // no lockfile means no OCI adapters in this subworkflow
		}
		for i := range subLF.Adapters {
			entry := &subLF.Adapters[i]
			wa := &workflowAdapter{Type: entry.Type, Name: entry.Name, Source: entry.Reference}
			key := entry.Type + "." + entry.Name
			if err := ensureAdapterCached(ctx, key, wa, subLF, layout, puller, policy); err != nil {
				return fmt.Errorf("subworkflow %q adapter %s: %w", swSpec.Name, key, err)
			}
		}
		// Recurse: this subworkflow may itself declare subworkflows.
		subSpec, diags := workflow.ParseDir(subDir)
		if diags.HasErrors() || subSpec == nil {
			continue
		}
		if err := pullSubworkflowAdapters(ctx, subDir, subSpec, layout, puller, policy); err != nil {
			return fmt.Errorf("subworkflow %q: %w", swSpec.Name, err)
		}
	}
	return nil
}

// resolveSubworkflowSourceDir resolves a subworkflow source path (relative or
// absolute) against the caller's workflow directory.
func resolveSubworkflowSourceDir(callerDir, source string) string {
	if filepath.IsAbs(source) {
		return source
	}
	return filepath.Join(callerDir, source)
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
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".hcl") {
			continue
		}
		// The lockfile ends in .hcl but is not a workflow source file.
		if e.Name() == workflow.LockfileName {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	return out, nil
}
