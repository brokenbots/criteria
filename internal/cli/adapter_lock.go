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
		upgrade       bool
		allowUnsigned bool
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
			return runLock(cmd.Context(), workflowDir, upgrade, allowUnsigned, cmd.OutOrStdout())
		},
	}

	cmd.Flags().BoolVar(&upgrade, "upgrade", false, "Re-resolve all adapters and update to latest digest")
	cmd.Flags().BoolVar(&allowUnsigned, "allow-unsigned", false, "Skip adapter signature verification (also via CRITERIA_ALLOW_UNSIGNED)")
	return cmd
}

type lockState struct {
	workflowDir string
	oldLF       *lockfile.Lockfile
	wfAdapters  map[string]*workflowAdapter
	aliases     map[string]string
	layout      *oci.Layout
	puller      *oci.Puller
	policy      signing.Policy
}

func prepareLockState(workflowDir string, upgrade, allowUnsigned bool) (*lockState, error) {
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
	layout, policy, err := openCacheAndPolicy(allowUnsigned, workflowVerification)
	if err != nil {
		return nil, err
	}

	var puller *oci.Puller
	if upgrade || len(missingRefs(oldLF, wfAdapters)) > 0 {
		puller = &oci.Puller{Layout: layout}
	}

	return &lockState{
		workflowDir: workflowDir,
		oldLF:       oldLF,
		wfAdapters:  wfAdapters,
		aliases:     aliases,
		layout:      layout,
		puller:      puller,
		policy:      policy,
	}, nil
}

func openCacheAndPolicy(allowUnsigned bool, workflowVerification string) (*oci.Layout, signing.Policy, error) {
	var policy signing.Policy
	cacheRoot, err := defaultCacheRoot()
	if err != nil {
		return nil, policy, err
	}
	layout, err := oci.Open(cacheRoot)
	if err != nil {
		return nil, policy, fmt.Errorf("open OCI cache: %w", err)
	}
	policy, err = resolveSigningPolicy(allowUnsigned, workflowVerification)
	if err != nil {
		return nil, policy, fmt.Errorf("signing policy: %w", err)
	}
	return layout, policy, nil
}

func runLock(ctx context.Context, workflowDir string, upgrade, allowUnsigned bool, out io.Writer) error {
	if out == nil {
		out = os.Stderr
	}
	state, err := prepareLockState(workflowDir, upgrade, allowUnsigned)
	if err != nil {
		return err
	}

	newLF := &lockfile.Lockfile{SchemaVersion: 1}
	if state.oldLF != nil {
		newLF.SchemaVersion = state.oldLF.SchemaVersion
	}

	for key, wa := range state.wfAdapters {
		entry, err := resolveOneAdapter(ctx, wa, state.oldLF, state.layout, state.puller, state.aliases, upgrade, &state.policy, out)
		if err != nil {
			return fmt.Errorf("adapter %q: %w", key, err)
		}
		newLF.Adapters = append(newLF.Adapters, entry)
	}

	printLockDiff(state.oldLF, newLF, out)

	lockPath := filepath.Join(state.workflowDir, ".criteria.lock.hcl")
	if err := lockfile.Write(lockPath, newLF); err != nil {
		return fmt.Errorf("write lockfile: %w", err)
	}
	return nil
}

// resolveOneAdapter returns the lockfile entry for a single workflow adapter.
func resolveOneAdapter(ctx context.Context, wa *workflowAdapter, oldLF *lockfile.Lockfile, layout *oci.Layout, puller *oci.Puller, aliases map[string]string, upgrade bool, policy *signing.Policy, out io.Writer) (lockfile.LockedAdapter, error) {
	var entry lockfile.LockedAdapter

	if wa.Source == "" {
		if oldLF != nil {
			if oldA := findLocked(oldLF, wa.Type, wa.Name); oldA != nil {
				return *oldA, nil
			}
		}
		return entry, fmt.Errorf("adapter has no `source` and no existing lockfile entry; add `source = \"...\"` + `version = \"...\"` to the adapter block, or run `criteria adapter pull <ref>` manually")
	}

	// Reuse an existing pin unless --upgrade re-resolves the constraint.
	if reused, ok, err := tryReusePin(ctx, wa, oldLF, layout, puller, upgrade, policy); ok || err != nil {
		return reused, err
	}

	ref, err := resolveSourceVersion(ctx, ResolveContext{WorkflowAliases: aliases}, puller, wa.Source, wa.Version)
	if err != nil {
		return entry, fmt.Errorf("resolve %s@%s: %w", wa.Source, versionOrLatest(wa.Version), err)
	}

	dg, pulledEntry, err := pullAndBuild(ctx, puller, layout, ref, policy)
	if err != nil {
		return entry, err
	}
	entry = pulledEntry
	entry.Reference = ref.String()
	entry.ResolvedDigest = dg.String()
	entry.Version = ref.Tag
	entry.Type = wa.Type
	entry.Name = wa.Name

	fmt.Fprintf(out, "locked %s.%s -> %s (%s)\n", wa.Type, wa.Name, entry.ResolvedDigest, ref.Tag)
	return entry, nil
}

// tryReusePin reuses an existing lockfile entry without re-evaluating the
// version constraint. Returns ok=true when the caller should return the entry.
// When the pinned blob is missing from the cache it is re-pulled from the
// entry's pinned reference (still no constraint re-evaluation).
func tryReusePin(ctx context.Context, wa *workflowAdapter, oldLF *lockfile.Lockfile, layout *oci.Layout, puller *oci.Puller, upgrade bool, policy *signing.Policy) (lockfile.LockedAdapter, bool, error) {
	var entry lockfile.LockedAdapter
	if oldLF == nil || upgrade {
		return entry, false, nil
	}
	oldA := findLocked(oldLF, wa.Type, wa.Name)
	if oldA == nil {
		return entry, false, nil
	}
	if layout.HasBlob(digest.Digest(oldA.ResolvedDigest)) {
		return *oldA, true, nil
	}
	// Pinned blob evicted from cache: re-pull the exact pinned reference.
	ref, err := oci.Parse(oldA.Reference)
	if err != nil {
		return entry, false, fmt.Errorf("parse pinned reference %q: %w", oldA.Reference, err)
	}
	dg, pulledEntry, err := pullAndBuild(ctx, puller, layout, ref, policy)
	if err != nil {
		return entry, false, err
	}
	entry = pulledEntry
	entry.Reference = ref.String()
	entry.ResolvedDigest = dg.String()
	entry.Version = oldA.Version
	entry.Type = wa.Type
	entry.Name = wa.Name
	return entry, true, nil
}

// resolveSourceVersion resolves an adapter's location + version constraint into
// a fully-qualified, tagged oci.Reference. Exact versions skip registry tag
// listing; constraints (^, ~, x, latest) list tags and pick the highest match.
func resolveSourceVersion(ctx context.Context, rctx ResolveContext, puller *oci.Puller, source, version string) (oci.Reference, error) {
	base, err := ResolveSource(rctx, source)
	if err != nil {
		return oci.Reference{}, err
	}
	v := versionOrLatest(version)
	if oci.IsExactVersion(v) {
		base.Tag = strings.TrimSpace(v)
		return base, nil
	}
	if puller == nil {
		return oci.Reference{}, fmt.Errorf("version constraint %q requires registry access", v)
	}
	tags, err := puller.ListTags(ctx, base)
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

func printLockDiff(oldLF, newLF *lockfile.Lockfile, out io.Writer) {
	if out == nil {
		out = os.Stderr
	}
	changes := lockfile.Diff(oldLF, newLF)
	for i := range changes {
		c := &changes[i]
		switch c.Kind {
		case lockfile.Added:
			fmt.Fprintf(out, "+ %s\n", c.Adapter)
		case lockfile.Removed:
			fmt.Fprintf(out, "- %s (stale)\n", c.Adapter)
		case lockfile.DigestChanged:
			fmt.Fprintf(out, "~ %s digest %s -> %s\n", c.Adapter, c.Before, c.After)
		case lockfile.SignerChanged:
			fmt.Fprintf(out, "~ %s signer changed\n", c.Adapter)
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
	for i := range lf.Adapters {
		if lf.Adapters[i].Type == typ && lf.Adapters[i].Name == name {
			return &lf.Adapters[i]
		}
	}
	return nil
}

func pullAndBuild(ctx context.Context, puller *oci.Puller, layout *oci.Layout, ref oci.Reference, policy *signing.Policy) (digest.Digest, lockfile.LockedAdapter, error) {
	var zero lockfile.LockedAdapter
	if puller == nil {
		return "", zero, fmt.Errorf("puller is nil")
	}
	dg, err := puller.Pull(ctx, ref)
	if err != nil {
		return "", zero, fmt.Errorf("pull %s: %w", ref, err)
	}

	artFS, err := layout.Open(dg)
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

	signer, err := signing.Verify(ctx, layout, dg, *policy)
	if err != nil {
		return "", zero, fmt.Errorf("signature verification: %w", err)
	}

	entry, err := lockfile.BuildEntry(ref, dg, m, signer, nil)
	if err != nil {
		return "", zero, fmt.Errorf("build lockfile entry: %w", err)
	}
	return dg, entry, nil
}
