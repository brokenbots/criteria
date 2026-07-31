package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

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
func autoPullCompileAdapters(ctx context.Context, workflowDir string, spec *workflow.Spec, pinSet *lockfile.Lockfile, allowUnsigned bool) error {
	// pinSet is the merged, tree-wide lockfile resolved once at startup. A nil
	// set means the whole workflow tree has no OCI adapter references.
	lf := pinSet
	if lf == nil {
		lf = &lockfile.Lockfile{}
	}

	// Build set of OCI-referenced adapters and validate the merged pin set
	// covers them.
	ociAdapters := collectWorkflowAdapters(spec)
	if err := assertLockfileCoversAdapters(lf, workflowDir, ociAdapters); err != nil {
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

	return pullAdaptersForDirectory(ctx, workflowDir, spec, ociAdapters, lf, pinSet, allowUnsigned)
}

// pullAdaptersForDirectory opens the OCI cache, resolves the signing policy,
// and pulls/extracts every OCI-referenced adapter declared in this directory.
func pullAdaptersForDirectory(ctx context.Context, workflowDir string, spec *workflow.Spec, ociAdapters map[string]*workflowAdapter, lf, pinSet *lockfile.Lockfile, allowUnsigned bool) error {
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

	// Also pull adapters declared in subworkflows. They are covered by the same
	// merged pin set, so verification and extraction use one consistent source.
	if err := pullSubworkflowAdapters(ctx, workflowDir, spec, layout, puller, &policy, pinSet); err != nil {
		return err
	}

	return nil
}

// assertLockfileCoversAdapters errors when any OCI-referenced workflow adapter
// has no entry in the lockfile, or when an entry lacks a required provenance
// field (reference, resolved_digest, source_url).
func assertLockfileCoversAdapters(lf *lockfile.Lockfile, workflowDir string, ociAdapters map[string]*workflowAdapter) error {
	var ociKeys []string
	for key, wa := range ociAdapters {
		if wa.Source != "" {
			ociKeys = append(ociKeys, key)
		}
	}
	if len(ociKeys) == 0 {
		return nil
	}
	if lf == nil {
		return fmt.Errorf("workflow uses OCI adapter references but %q is missing; run `criteria adapter lock`", filepath.Join(workflowDir, workflow.LockfileName))
	}

	var missing []string
	var incomplete []string
	for key, wa := range ociAdapters {
		if wa.Source == "" {
			continue // not OCI-based
		}
		entry := findLocked(lf, wa.Type, wa.Name)
		if entry == nil {
			missing = append(missing, key)
			continue
		}
		if entry.Reference == "" || entry.ResolvedDigest == "" || entry.SourceURL == "" {
			incomplete = append(incomplete, key)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("lockfile missing entries for adapters: %v; run `criteria adapter lock`", missing)
	}
	if len(incomplete) > 0 {
		return fmt.Errorf("lockfile entries for adapters %v are incomplete (each entry must have reference, resolved_digest, and source_url); run `criteria adapter lock`", incomplete)
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

func ensureAdapterCached(ctx context.Context, key string, wa *workflowAdapter, lf *lockfile.Lockfile, layout *oci.Layout, puller puller, policy *signing.Policy) error {
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
		return useCachedAdapter(ctx, key, wa, layout, dg, entry, policy)
	}

	return pullAndInstallAdapter(ctx, key, wa, dg, entry, layout, puller, policy)
}

// useCachedAdapter verifies, annotates and extracts a cached adapter blob.
func useCachedAdapter(ctx context.Context, key string, wa *workflowAdapter, layout *oci.Layout, dg digest.Digest, entry *lockfile.LockedAdapter, policy *signing.Policy) error {
	if err := verifyAgainstPin(ctx, key, layout, dg, entry, policy); err != nil {
		return err
	}
	if err := annotateProvenance(layout, dg, entry); err != nil {
		return fmt.Errorf("adapter %q %w", key, err)
	}
	if _, err := extractOCIAdapterBinary(layout, dg, wa.Type); err != nil {
		return fmt.Errorf("adapter %q extract binary: %w", key, err)
	}
	return nil
}

// pullAndInstallAdapter pulls an adapter by its pinned digest, verifies it,
// records provenance, extracts the platform binary, and pulls any container
// image declared by the artifact manifest.
func pullAndInstallAdapter(ctx context.Context, key string, wa *workflowAdapter, dg digest.Digest, entry *lockfile.LockedAdapter, layout *oci.Layout, puller puller, policy *signing.Policy) error {
	// Binary missing from cache — pull by the pinned digest, never by tag.
	ref, err := oci.Parse(entry.Reference)
	if err != nil {
		return fmt.Errorf("adapter %q parse reference %q: %w", key, entry.Reference, err)
	}
	ref.Tag = ""
	ref.Digest = dg

	pulledDg, err := puller.Pull(ctx, ref)
	if err != nil {
		return fmt.Errorf("adapter %q pull %s: %w", key, ref, err)
	}
	if pulledDg != dg {
		return fmt.Errorf("adapter %q digest mismatch: lockfile pins %s but registry returned %s", key, dg, pulledDg)
	}

	if err := annotateProvenance(layout, pulledDg, entry); err != nil {
		return fmt.Errorf("adapter %q %w", key, err)
	}

	m, err := readAdapterManifest(layout, pulledDg, key)
	if err != nil {
		return err
	}
	if err := verifyAgainstPin(ctx, key, layout, pulledDg, entry, policy); err != nil {
		return err
	}
	if _, err := extractOCIAdapterBinary(layout, pulledDg, wa.Type); err != nil {
		return fmt.Errorf("adapter %q extract binary: %w", key, err)
	}
	return maybePullContainerImage(ctx, key, m)
}

// readAdapterManifest opens the artifact and parses its adapter.yaml manifest.
func readAdapterManifest(layout *oci.Layout, dg digest.Digest, key string) (*manifest.Manifest, error) {
	artFS, err := layout.Open(dg)
	if err != nil {
		return nil, fmt.Errorf("adapter %q open artifact: %w", key, err)
	}
	m, err := manifest.ParseFromFS(artFS, "adapter.yaml")
	if err != nil {
		return nil, fmt.Errorf("adapter %q read manifest: %w", key, err)
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("adapter %q validate manifest: %w", key, err)
	}
	return m, nil
}

// verifyAgainstPin verifies the cached artifact under a policy tightened to the
// lockfile-pinned signer (the trust anchor), then confirms the verified signer
// matches the pin. A nil signer (ModeOff, or a ModeWarn failure) skips the pin
// check; see policyForPin and assertSignerMatchesPin.
// annotateProvenance records the original reference and source URL on a cached
// manifest so `criteria adapter list` can attribute the entry. It is a no-op
// when the entry carries no provenance.
func annotateProvenance(layout *oci.Layout, dg digest.Digest, entry *lockfile.LockedAdapter) error {
	if entry.Reference == "" && entry.SourceURL == "" {
		return nil
	}
	extra := make(map[string]string)
	if entry.Reference != "" {
		extra[oci.AnnotationReference] = entry.Reference
	}
	if entry.SourceURL != "" {
		extra[oci.AnnotationSourceURL] = entry.SourceURL
	}
	if err := layout.Annotate(dg, extra); err != nil {
		return fmt.Errorf("annotate cache entry: %w", err)
	}
	return nil
}

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
//
// The copied binary is verified against the digest of its OCI layer, and the
// write is idempotent: if the destination already exists with the expected
// digest it is left untouched.
func extractOCIAdapterBinary(layout *oci.Layout, dg digest.Digest, adapterType string) (string, error) {
	expectedBinDigest, err := artifactBinaryLayerDigest(layout, dg)
	if err != nil {
		return "", err
	}

	artFS, err := layout.Open(dg)
	if err != nil {
		return "", err
	}

	platformPath, err := artifactBinaryPath(artFS)
	if err != nil {
		return "", err
	}
	f, err := artFS.Open(platformPath)
	if err != nil {
		return "", fmt.Errorf("open %s in artifact: %w", platformPath, err)
	}
	defer f.Close()

	dest, err := adapterhost.AdapterInstallPath(adapterType, adapterhost.EncodeDigest(dg))
	if err != nil {
		return "", err
	}

	// Idempotency: if the destination already exists and matches the expected
	// layer digest, leave it in place. Still ensure the executable bit is set,
	// because a previous extraction may have lost it (e.g. chmod, umask, or a
	// copy from a non-exec source) and adapterhost.DiscoverBinaryAt relies on it.
	if existingErr := verifyFileDigest(dest, expectedBinDigest); existingErr == nil {
		if err := ensureExecutable(dest); err == nil {
			return dest, nil
		}
		// The file can disappear between verify and chmod when another goroutine
		// is replacing the same digest; fall through to rewrite rather than fail.
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	}

	return writeVerifiedBinary(f, dest, expectedBinDigest, adapterType, dg)
}

// ensureExecutable sets 0755 permissions on path, returning a wrapped error
// that explains the intent. It is safe to call on all platforms; on Windows it
// is a no-op semantically but never fails.
func ensureExecutable(path string) error {
	if err := os.Chmod(path, 0o755); err != nil {
		return fmt.Errorf("ensure extracted binary is executable: %w", err)
	}
	return nil
}

// writeVerifiedBinary copies src into dest (with a digest verifier) and makes
// it executable. The write is atomic: a sibling temp file is renamed into
// place after verification. If verification fails the temp file is removed.
func writeVerifiedBinary(src io.Reader, dest string, expected digest.Digest, adapterType string, dg digest.Digest) (string, error) {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}

	// Use a per-call unique temp path so concurrent extractions of the same
	// digest do not race on a shared sibling file.
	f, err := os.CreateTemp(filepath.Dir(dest), filepath.Base(dest)+".tmp.*")
	if err != nil {
		return "", fmt.Errorf("create temporary extraction file: %w", err)
	}
	tmp := f.Name()
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("close temporary extraction file handle: %w", err)
	}

	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("open temporary extraction file: %w", err)
	}
	verifier := expected.Verifier()
	w := io.MultiWriter(out, verifier)
	if _, err := io.Copy(w, src); err != nil {
		out.Close()
		os.Remove(tmp)
		return "", fmt.Errorf("copy binary: %w", err)
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("close temporary extraction file: %w", err)
	}
	if !verifier.Verified() {
		os.Remove(tmp)
		return "", fmt.Errorf("extracted binary digest mismatch for %q under %s: expected %s", adapterType, dg, expected)
	}
	// Remove any existing destination so the rename is atomic and also succeeds
	// on Windows, where os.Rename cannot overwrite an existing file.
	if err := os.Remove(dest); err != nil && !errors.Is(err, fs.ErrNotExist) {
		os.Remove(tmp)
		return "", fmt.Errorf("remove stale extracted binary: %w", err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("rename extracted binary into place: %w", err)
	}
	if err := ensureExecutable(dest); err != nil {
		// Another concurrent extraction may have replaced dest between our rename
		// and chmod; the final file is still verified by the winning writer.
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	}
	return dest, nil
}

// artifactBinaryLayerDigest returns the digest of the host platform binary
// layer inside the artifact identified by manifest digest d.
func artifactBinaryLayerDigest(layout *oci.Layout, d digest.Digest) (digest.Digest, error) {
	var zero digest.Digest
	data, err := os.ReadFile(layout.BlobPath(d))
	if err != nil {
		return zero, fmt.Errorf("read manifest blob %s: %w", d, err)
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return zero, fmt.Errorf("parse manifest %s: %w", d, err)
	}

	platformDir := path.Join("bin", runtime.GOOS, runtime.GOARCH)
	expectedPrefix := platformDir + "/"
	for _, layer := range m.Layers {
		title, ok := layer.Annotations[oci.AnnotationTitle]
		if !ok || title == "" {
			continue
		}
		title = strings.TrimPrefix(title, "/")
		if strings.HasPrefix(title, expectedPrefix) {
			return layer.Digest, nil
		}
	}
	return zero, fmt.Errorf("artifact %s has no binary for %s/%s", d, runtime.GOOS, runtime.GOARCH)
}

// verifyFileDigest reports whether the file at path matches the expected
// digest. Any error (missing file, read failure, digest mismatch) is returned
// so the caller can fall through to a fresh extraction.
func verifyFileDigest(path string, expected digest.Digest) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	verifier := expected.Verifier()
	if _, err := io.Copy(verifier, f); err != nil {
		return err
	}
	if !verifier.Verified() {
		return fmt.Errorf("digest mismatch: expected %s", expected)
	}
	return nil
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
// and ensures their OCI adapter binaries are extracted. The same merged pinSet
// covers the whole tree, so every adapter is verified and extracted against the
// same lockfile the engine will use at run time. Remote workflow sources are
// fetched by the parent's pinned resolved_ref so the pulled binaries match the
// locked tree.
func pullSubworkflowAdapters(ctx context.Context, workflowDir string, spec *workflow.Spec, layout *oci.Layout, puller puller, policy *signing.Policy, pinSet *lockfile.Lockfile) error {
	fetcher := newWorkflowFetcherFunc()
	for _, swSpec := range spec.Subworkflows {
		subDir, _, err := resolveSubworkflowForLock(ctx, workflowDir, swSpec.Source, fetcher)
		if err != nil {
			return fmt.Errorf("subworkflow %q in %q: %w", swSpec.Name, workflowDir, err)
		}
		// Use the merged tree-wide pin set instead of re-reading this directory's
		// lockfile, guaranteeing auto-pull and the engine agree.
		lf := pinSet
		if lf == nil {
			lf = &lockfile.Lockfile{}
		}
		adapters := collectWorkflowAdaptersFromDir(subDir)
		for key, wa := range adapters {
			if wa.Source == "" {
				continue
			}
			if err := ensureAdapterCached(ctx, key, wa, lf, layout, puller, policy); err != nil {
				return fmt.Errorf("subworkflow %q adapter %s: %w", swSpec.Name, key, err)
			}
		}
		// Recurse: this subworkflow may itself declare subworkflows.
		subSpec, diags := workflow.ParseDir(subDir)
		if diags.HasErrors() || subSpec == nil {
			continue
		}
		if err := pullSubworkflowAdapters(ctx, subDir, subSpec, layout, puller, policy, pinSet); err != nil {
			return fmt.Errorf("subworkflow %q: %w", swSpec.Name, err)
		}
	}
	return nil
}

// collectWorkflowAdaptersFromDir parses a workflow directory and returns its
// declared adapter map keyed by "type.name".
func collectWorkflowAdaptersFromDir(dir string) map[string]*workflowAdapter {
	spec, diags := workflow.ParseDir(dir)
	if diags.HasErrors() || spec == nil {
		return nil
	}
	return collectWorkflowAdapters(spec)
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
