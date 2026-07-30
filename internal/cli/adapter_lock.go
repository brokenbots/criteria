package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/opencontainers/go-digest"
	"github.com/spf13/cobra"
	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/internal/adapter/manifest"
	"github.com/brokenbots/criteria/internal/adapter/oci"
	"github.com/brokenbots/criteria/internal/adapter/signing"
	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

func newAdapterLockCmd() *cobra.Command {
	var (
		upgrade         bool
		allowUnsigned   bool
		trustedKeyPaths []string
	)

	cmd := &cobra.Command{
		Use:   "lock [workflow-dir]",
		Short: "Update .criteria.lock.hcl to match workflow adapter references",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			workflowDir := "."
			if len(args) > 0 {
				workflowDir = args[0]
			}
			return runLock(cmd.Context(), workflowDir, upgrade, allowUnsigned, trustedKeyPaths, cmd.OutOrStdout(), nil)
		},
	}

	cmd.Flags().BoolVar(&upgrade, "upgrade", false, "Re-evaluate version constraints and accept digest drift under immutable version pins. Plain lock already re-fetches and re-verifies every adapter's pinned digest; use --upgrade only when an exact-version pin resolved to a new digest and you accept the supply-chain change")
	cmd.Flags().BoolVar(&allowUnsigned, "allow-unsigned", false, "Skip adapter signature verification (also via CRITERIA_ALLOW_UNSIGNED)")
	cmd.Flags().StringArrayVar(&trustedKeyPaths, "trusted-key", nil, "Path to a trusted PEM public key for key-mode verification (repeatable)")
	return cmd
}

type lockState struct {
	workflowDir string
	oldLF       *lockfile.Lockfile
	wfAdapters  map[string]*workflowAdapter
	aliases     map[string]string
	resolver    lockResolver
	policy      signing.Policy
}

// lockResolver abstracts the OCI layout + puller so the lock path can be
// tested without a real registry. The concrete implementation is ociLockResolver.
type lockResolver interface {
	HasBlob(digest.Digest) bool
	ListTags(context.Context, oci.Reference) ([]string, error)
	PullAndBuild(context.Context, oci.Reference, *signing.Policy) (digest.Digest, lockfile.LockedAdapter, error)
	Extract(digest.Digest, string) (string, error)
}

// ociLockResolver combines an OCI cache layout with a remote puller.
type ociLockResolver struct {
	layout *oci.Layout
	puller *oci.Puller
}

func (r *ociLockResolver) HasBlob(d digest.Digest) bool {
	if r == nil || r.layout == nil {
		return false
	}
	return r.layout.HasBlob(d)
}

func (r *ociLockResolver) ListTags(ctx context.Context, ref oci.Reference) ([]string, error) {
	if r == nil || r.puller == nil {
		return nil, fmt.Errorf("no puller available")
	}
	return r.puller.ListTags(ctx, ref)
}

func (r *ociLockResolver) PullAndBuild(ctx context.Context, ref oci.Reference, policy *signing.Policy) (digest.Digest, lockfile.LockedAdapter, error) {
	var zero lockfile.LockedAdapter
	if r == nil || r.puller == nil {
		return "", zero, fmt.Errorf("puller is nil")
	}
	if r.layout == nil {
		return "", zero, fmt.Errorf("layout is nil")
	}

	dg, err := r.puller.Pull(ctx, ref)
	if err != nil {
		return "", zero, fmt.Errorf("pull %s: %w", ref, err)
	}

	artFS, err := r.layout.Open(dg)
	if err != nil {
		return "", zero, fmt.Errorf("open artifact: %w", err)
	}

	m, err := manifest.ParseFromFS(artFS, "adapter.yaml")
	if err != nil {
		return "", zero, fmt.Errorf("read adapter.yaml: %w", err)
	}
	if err := m.Validate(); err != nil {
		return "", zero, fmt.Errorf("validate manifest: %w", err)
	}

	signer, err := signing.Verify(ctx, r.layout, dg, *policy)
	if err != nil {
		return "", zero, fmt.Errorf("signature verification: %w", err)
	}

	entry, err := lockfile.BuildEntry(ref, dg, m, signer, nil)
	if err != nil {
		return "", zero, fmt.Errorf("build lockfile entry: %w", err)
	}
	return dg, entry, nil
}

func (r *ociLockResolver) Extract(d digest.Digest, adapterType string) (string, error) {
	if r == nil || r.layout == nil {
		return "", fmt.Errorf("layout is nil")
	}
	return extractOCIAdapterBinary(r.layout, d, adapterType)
}

func prepareLockState(workflowDir string, upgrade, allowUnsigned bool, trustedKeyPaths []string, resolver lockResolver) (*lockState, error) {
	workflowDir, err := filepath.Abs(workflowDir)
	if err != nil {
		return nil, fmt.Errorf("resolve workflow dir: %w", err)
	}

	spec, diags := workflow.ParseFileOrDir(workflowDir)
	if diags.HasErrors() {
		return nil, fmt.Errorf("parse workflow: %w", newDiagsError(diags))
	}

	oldLF, err := lockfile.ReadFromDir(workflowDir)
	if err != nil {
		return nil, fmt.Errorf("read lockfile: %w", err)
	}

	wfAdapters := collectWorkflowAdapters(spec)

	aliases, err := collectWorkflowAliases(workflowDir, spec)
	if err != nil {
		return nil, err
	}

	workflowVerification := ""
	if spec.Header != nil {
		workflowVerification = spec.Header.Verification
	}
	trustedKeys, err := loadTrustedKeys(workflowDir, trustedKeyPaths)
	if err != nil {
		return nil, fmt.Errorf("load trusted keys: %w", err)
	}
	layout, policy, err := openCacheAndPolicy(allowUnsigned, workflowVerification, trustedKeys)
	if err != nil {
		return nil, err
	}

	if resolver == nil {
		resolver = newOCILockResolver(layout, upgrade, wfAdapters)
	}

	return &lockState{
		workflowDir: workflowDir,
		oldLF:       oldLF,
		wfAdapters:  wfAdapters,
		aliases:     aliases,
		resolver:    resolver,
		policy:      policy,
	}, nil
}

// newOCILockResolver builds the production OCI resolver. A puller is created
// whenever any OCI-sourced adapter exists, because a declared version bump on
// an already-locked adapter still needs re-resolution. Constructing the puller
// performs no network I/O, so this is safe and cheap.
func newOCILockResolver(layout *oci.Layout, upgrade bool, wfAdapters map[string]*workflowAdapter) lockResolver {
	var puller *oci.Puller
	if upgrade || hasOCIAdapters(wfAdapters) {
		puller = &oci.Puller{Layout: layout}
	}
	return &ociLockResolver{layout: layout, puller: puller}
}

func openCacheAndPolicy(allowUnsigned bool, workflowVerification string, trustedKeys []signing.KeyIdentity) (*oci.Layout, signing.Policy, error) {
	var policy signing.Policy
	cacheRoot, err := defaultCacheRoot()
	if err != nil {
		return nil, policy, err
	}
	layout, err := oci.Open(cacheRoot)
	if err != nil {
		return nil, policy, fmt.Errorf("open OCI cache: %w", err)
	}
	policy, err = resolveSigningPolicy(allowUnsigned, workflowVerification, trustedKeys)
	if err != nil {
		return nil, policy, fmt.Errorf("signing policy: %w", err)
	}
	return layout, policy, nil
}

func runLock(ctx context.Context, workflowDir string, upgrade, allowUnsigned bool, trustedKeyPaths []string, out io.Writer, resolver lockResolver) error {
	if out == nil {
		out = os.Stderr
	}
	state, err := prepareLockState(workflowDir, upgrade, allowUnsigned, trustedKeyPaths, resolver)
	if err != nil {
		return err
	}

	newLF := &lockfile.Lockfile{SchemaVersion: 1}
	if state.oldLF != nil {
		newLF.SchemaVersion = state.oldLF.SchemaVersion
	}

	for key, wa := range state.wfAdapters {
		entry, err := resolveOneAdapter(ctx, wa, state.oldLF, state.resolver, state.aliases, upgrade, &state.policy, out)
		if err != nil {
			return fmt.Errorf("adapter %q: %w", key, err)
		}
		newLF.Adapters = append(newLF.Adapters, entry)
	}

	printLockDiff(state.oldLF, newLF, out, len(state.wfAdapters))

	lockPath := filepath.Join(state.workflowDir, workflow.LockfileName)
	if err := lockfile.Write(lockPath, newLF); err != nil {
		return fmt.Errorf("write lockfile: %w", err)
	}
	return nil
}

// resolveOneAdapter returns the lockfile entry for a single workflow adapter.
// It always re-resolves OCI-sourced adapters so that every run of `lock`
// re-verifies the pinned digest. Mutable tags and semver constraints are
// expected to drift; an exact-version pin whose digest moves is treated as a
// supply-chain red flag unless --upgrade is supplied.
func resolveOneAdapter(ctx context.Context, wa *workflowAdapter, oldLF *lockfile.Lockfile, resolver lockResolver, aliases map[string]string, upgrade bool, policy *signing.Policy, out io.Writer) (lockfile.LockedAdapter, error) {
	var entry lockfile.LockedAdapter

	if wa.Source == "" {
		if oldLF != nil {
			if oldA := findLocked(oldLF, wa.Type, wa.Name); oldA != nil {
				return *oldA, nil
			}
		}
		return entry, fmt.Errorf("adapter has no `source` and no existing lockfile entry; add `source = \"...\"` + `version = \"...\"` to the adapter block, or run `criteria adapter pull <ref>` manually")
	}

	ref, err := resolveSourceVersion(ctx, ResolveContext{WorkflowAliases: aliases}, resolver, wa.Source, wa.Version)
	if err != nil {
		return entry, fmt.Errorf("resolve %s@%s: %w", wa.Source, versionOrLatest(wa.Version), err)
	}

	dg, pulledEntry, err := resolver.PullAndBuild(ctx, ref, policy)
	if err != nil {
		return entry, err
	}

	entry = pulledEntry
	entry.Reference = ref.String()
	entry.ResolvedDigest = dg.String()
	entry.Version = ref.Tag
	entry.Type = wa.Type
	entry.Name = wa.Name

	// Re-verify the exact pin: a digest change under an immutable version is a
	// supply-chain event and must not be applied silently.
	oldA := findLocked(oldLF, wa.Type, wa.Name)
	if oldA != nil && oldA.ResolvedDigest != dg.String() {
		if isImmutableExactPin(wa.Version) && oldA.Version == ref.Tag {
			if !upgrade {
				return entry, fmt.Errorf("immutable pin digest drift detected for %s.%s: %s -> %s (version %q was re-pushed); run with --upgrade only if you accept this drift", wa.Type, wa.Name, oldA.ResolvedDigest, dg, ref.Tag)
			}
			fmt.Fprintf(out, "! %s.%s immutable pin digest drift: %s -> %s (accepted by --upgrade)\n", wa.Type, wa.Name, oldA.ResolvedDigest, dg)
		}
	}

	// Extract the platform binary to the digest-addressed install path so that
	// compile-time schema verification can resolve it without a prior run.
	if _, err := resolver.Extract(dg, wa.Type); err != nil {
		return entry, fmt.Errorf("extract binary for %s.%s: %w", wa.Type, wa.Name, err)
	}

	fmt.Fprintf(out, "locked %s.%s -> %s (%s)\n", wa.Type, wa.Name, entry.ResolvedDigest, ref.Tag)
	return entry, nil
}

// isImmutableExactPin reports whether the declared version is a single,
// fully-specified version (e.g. "0.5.3"). Mutable tags and semver constraints
// return false.
func isImmutableExactPin(version string) bool {
	return oci.IsExactVersion(versionOrLatest(version))
}

// declMatchesPin returns an empty string when the workflow declaration matches
// the lockfile entry's source and version. Otherwise it returns a short
// human-readable reason describing the mismatch.
func declMatchesPin(wa *workflowAdapter, oldA *lockfile.LockedAdapter, aliases map[string]string) string {
	resolvedSource, err := ResolveSource(ResolveContext{WorkflowAliases: aliases}, wa.Source)
	if err != nil {
		// Treat unresolvable sources as a mismatch so we re-resolve rather than
		// silently reuse a potentially stale pin.
		return "source could not be resolved"
	}

	oldRef, err := oci.Parse(oldA.Reference)
	if err != nil {
		return "existing reference is unparsable"
	}
	if oldRef.Registry != resolvedSource.Registry || oldRef.Repo != resolvedSource.Repo {
		return fmt.Sprintf("source changed (%s/%s -> %s/%s)", oldRef.Registry, oldRef.Repo, resolvedSource.Registry, resolvedSource.Repo)
	}

	v := versionOrLatest(wa.Version)
	if oldA.Version == "" {
		return "existing pin has no resolved version"
	}
	if oci.IsExactVersion(v) {
		if strings.TrimSpace(oldA.Version) != strings.TrimSpace(v) {
			return fmt.Sprintf("version changed (%s -> %s)", oldA.Version, v)
		}
		return ""
	}
	// Constraint: the pinned concrete version must satisfy the declared
	// constraint. A single-tag list is enough to test membership.
	if _, err := oci.SelectVersion(v, []string{oldA.Version}); err != nil {
		return fmt.Sprintf("pinned version %s no longer satisfies constraint %q", oldA.Version, v)
	}
	return ""
}

// resolveSourceVersion resolves an adapter's location + version constraint into
// a fully-qualified, tagged oci.Reference. Exact versions skip registry tag
// listing; constraints (^, ~, x, latest) list tags and pick the highest match.
func resolveSourceVersion(ctx context.Context, rctx ResolveContext, resolver lockResolver, source, version string) (oci.Reference, error) {
	base, err := ResolveSource(rctx, source)
	if err != nil {
		return oci.Reference{}, err
	}
	v := versionOrLatest(version)
	if oci.IsExactVersion(v) {
		base.Tag = strings.TrimSpace(v)
		return base, nil
	}
	if resolver == nil {
		return oci.Reference{}, fmt.Errorf("version constraint %q requires registry access", v)
	}
	tags, err := resolver.ListTags(ctx, base)
	if err != nil {
		return oci.Reference{}, err
	}
	chosen, err := oci.SelectVersion(v, tags)
	if err != nil {
		return oci.Reference{}, err
	}
	base.Tag = chosen
	return base, nil
}

func versionOrLatest(v string) string {
	if strings.TrimSpace(v) == "" {
		return "latest"
	}
	return v
}

func printLockDiff(oldLF, newLF *lockfile.Lockfile, out io.Writer, adapterCount int) {
	if out == nil {
		out = os.Stderr
	}
	changes := lockfile.Diff(oldLF, newLF)
	if len(changes) == 0 {
		fmt.Fprintf(out, "lockfile up to date, %d adapter(s)\n", adapterCount)
		return
	}

	var signerChanges []lockfile.Change
	var otherChanges []lockfile.Change
	for i := range changes {
		c := &changes[i]
		if c.Kind == lockfile.SignerChanged {
			signerChanges = append(signerChanges, *c)
		} else {
			otherChanges = append(otherChanges, *c)
		}
	}

	for _, c := range signerChanges {
		fmt.Fprintf(out, "! %s signer changed\n", c.Adapter)
	}
	for i := range otherChanges {
		c := &otherChanges[i]
		switch c.Kind {
		case lockfile.Added:
			fmt.Fprintf(out, "+ %s\n", c.Adapter)
		case lockfile.Removed:
			fmt.Fprintf(out, "- %s (stale)\n", c.Adapter)
		case lockfile.DigestChanged:
			fmt.Fprintf(out, "~ %s digest %s -> %s\n", c.Adapter, c.Before, c.After)
		case lockfile.PlatformsChanged:
			fmt.Fprintf(out, "~ %s platforms changed\n", c.Adapter)
		case lockfile.ContainerImageChanged:
			fmt.Fprintf(out, "~ %s container image changed\n", c.Adapter)
		case lockfile.RemoteChanged:
			fmt.Fprintf(out, "~ %s remote changed\n", c.Adapter)
		case lockfile.OverrideChanged:
			fmt.Fprintf(out, "~ %s override changed\n", c.Adapter)
		}
	}
}

// workflowAdapter holds the parsed adapter declaration: its OCI location
// (Source) and version constraint, decoded directly from the `adapter` block.
type workflowAdapter struct {
	Type    string
	Name    string
	Source  string // OCI location (registry/repo or alias); empty means non-OCI
	Version string // semver constraint (exact, ^, ~, x, latest); empty == latest
}

// collectWorkflowAliases extracts registry alias blocks from workflow HCL.
func collectWorkflowAliases(workflowDir string, _ *workflow.Spec) (map[string]string, error) {
	aliases := make(map[string]string)

	hclFiles, err := listHCLFiles(workflowDir)
	if err != nil {
		return nil, err
	}

	for _, path := range hclFiles {
		if parseErr := parseAliasesFromFile(path, aliases); parseErr != nil {
			return nil, parseErr
		}
	}

	return aliases, nil
}

func parseAliasesFromFile(path string, aliases map[string]string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %q: %w", path, err)
	}
	file, diags := hclparse.NewParser().ParseHCL(data, path)
	if diags.HasErrors() {
		return nil
	}
	body, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		return nil
	}
	for _, block := range body.Blocks {
		if block.Type != "registry" || len(block.Labels) != 1 {
			continue
		}
		alias := block.Labels[0]
		attrs, _, _ := block.Body.PartialContent(&hcl.BodySchema{
			Attributes: []hcl.AttributeSchema{{Name: "source"}},
		})
		if attr, ok := attrs.Attributes["source"]; ok {
			val, valDiags := attr.Expr.Value(nil)
			if !valDiags.HasErrors() && val.Type() == cty.String && !val.IsNull() {
				aliases[alias] = val.AsString()
			}
		}
	}
	return nil
}

// hasOCIAdapters reports whether any adapter in the map references an OCI
// source. If so, the lock command needs a puller available for re-resolution.
func hasOCIAdapters(adapters map[string]*workflowAdapter) bool {
	for _, wa := range adapters {
		if wa.Source != "" {
			return true
		}
	}
	return false
}

func missingRefs(lf *lockfile.Lockfile, adapters map[string]*workflowAdapter) []string {
	if lf == nil {
		out := make([]string, 0, len(adapters))
		for k := range adapters {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	set := make(map[string]struct{}, len(lf.Adapters))
	for i := range lf.Adapters {
		set[lf.Adapters[i].Type+"."+lf.Adapters[i].Name] = struct{}{}
	}
	out := make([]string, 0, len(adapters))
	for k := range adapters {
		if _, ok := set[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func findLocked(lf *lockfile.Lockfile, typ, name string) *lockfile.LockedAdapter {
	if lf == nil {
		return nil
	}
	for i := range lf.Adapters {
		if lf.Adapters[i].Type == typ && lf.Adapters[i].Name == name {
			return &lf.Adapters[i]
		}
	}
	return nil
}
