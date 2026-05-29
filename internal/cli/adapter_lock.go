package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

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
	var upgrade bool

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
			return runLock(cmd.Context(), workflowDir, upgrade)
		},
	}

	cmd.Flags().BoolVar(&upgrade, "upgrade", false, "Re-resolve all adapters and update to latest digest")
	return cmd
}

func runLock(ctx context.Context, workflowDir string, upgrade bool) error {
	workflowDir, err := filepath.Abs(workflowDir)
	if err != nil {
		return fmt.Errorf("resolve workflow dir: %w", err)
	}

	spec, diags := workflow.ParseFileOrDir(workflowDir)
	if diags.HasErrors() {
		return fmt.Errorf("parse workflow: %w", newDiagsError(diags))
	}

	// Read existing lockfile.
	oldLF, err := lockfile.ReadFromDir(workflowDir)
	if err != nil {
		return fmt.Errorf("read lockfile: %w", err)
	}

	// Collect workflow adapters and their OCI references from raw HCL.
	wfAdapters, err := collectWorkflowAdapters(workflowDir, spec)
	if err != nil {
		return err
	}

	// Build alias map from workflow HCL registry blocks.
	aliases, err := collectWorkflowAliases(workflowDir, spec)
	if err != nil {
		return err
	}

	newLF := &lockfile.Lockfile{SchemaVersion: 1}
	if oldLF != nil {
		newLF.SchemaVersion = oldLF.SchemaVersion
	}

	cacheRoot, err := defaultCacheRoot()
	if err != nil {
		return err
	}
	layout, err := oci.Open(cacheRoot)
	if err != nil {
		return fmt.Errorf("open OCI cache: %w", err)
	}

	policy, err := signing.PolicyFor(signing.PullContext{})
	if err != nil {
		return fmt.Errorf("signing policy: %w", err)
	}

	var puller *oci.Puller
	if upgrade || len(missingRefs(oldLF, wfAdapters)) > 0 {
		puller = &oci.Puller{Layout: layout}
	}

	// Process each workflow adapter.
	for key, wa := range wfAdapters {
		if wa.Reference == "" {
			// No OCI reference configured in workflow HCL.
			if oldLF != nil {
				if oldA := findLocked(oldLF, wa.Type, wa.Name); oldA != nil {
					// Keep existing entry even without a reference.
					newLF.Adapters = append(newLF.Adapters, *oldA)
					continue
				}
			}
			return fmt.Errorf("adapter %q has no OCI reference in workflow HCL and no existing lockfile entry; add a `reference = \"...\"` attribute or run `criteria adapter pull \u003cref\u003e` manually", key)
		}

		ref, err := resolveWithAliases(aliases, wa.Reference)
		if err != nil {
			return fmt.Errorf("adapter %q: %w", key, err)
		}

		var entry lockfile.LockedAdapter
		var foundOld bool
		if oldLF != nil {
			if oldA := findLocked(oldLF, wa.Type, wa.Name); oldA != nil {
				foundOld = true
				if !upgrade {
					// Re-use pinned digest if binary is cached.
					if layout.HasBlob(digest.Digest(oldA.ResolvedDigest)) {
						entry = *oldA
						entry.Reference = ref.String()
					} else {
						// Binary missing — need to pull.
						dg, pulledEntry, err := pullAndBuild(ctx, puller, layout, ref, policy)
						if err != nil {
							return fmt.Errorf("adapter %q: %w", key, err)
						}
						entry = pulledEntry
						entry.Reference = ref.String()
						entry.ResolvedDigest = dg.String()
					}
					entry.Type = wa.Type
					entry.Name = wa.Name
					newLF.Adapters = append(newLF.Adapters, entry)
					if !foundOld {
						fmt.Fprintf(os.Stderr, "locked %s -> %s\n", key, entry.ResolvedDigest)
					}
					continue
				}
			}
		}

		// Upgrade or new entry: pull and build.
		dg, pulledEntry, err := pullAndBuild(ctx, puller, layout, ref, policy)
		if err != nil {
			return fmt.Errorf("adapter %q: %w", key, err)
		}
		entry = pulledEntry
		entry.Reference = ref.String()
		entry.ResolvedDigest = dg.String()
		entry.Type = wa.Type
		entry.Name = wa.Name
		newLF.Adapters = append(newLF.Adapters, entry)

		if !foundOld {
			fmt.Fprintf(os.Stderr, "locked %s -> %s\n", key, entry.ResolvedDigest)
		}
	}

	// Detect stale entries (in lockfile but not in workflow).
	changes := lockfile.Diff(oldLF, newLF)
	for _, c := range changes {
		switch c.Kind {
		case lockfile.Added:
			fmt.Fprintf(os.Stderr, "+ %s\n", c.Adapter)
		case lockfile.Removed:
			fmt.Fprintf(os.Stderr, "- %s (stale)\n", c.Adapter)
		case lockfile.DigestChanged:
			fmt.Fprintf(os.Stderr, "~ %s digest %s -> %s\n", c.Adapter, c.Before, c.After)
		case lockfile.SignerChanged:
			fmt.Fprintf(os.Stderr, "~ %s signer changed\n", c.Adapter)
		case lockfile.PlatformsChanged:
			fmt.Fprintf(os.Stderr, "~ %s platforms changed\n", c.Adapter)
		case lockfile.ContainerImageChanged:
			fmt.Fprintf(os.Stderr, "~ %s container image changed\n", c.Adapter)
		case lockfile.RemoteChanged:
			fmt.Fprintf(os.Stderr, "~ %s remote changed\n", c.Adapter)
		case lockfile.OverrideChanged:
			fmt.Fprintf(os.Stderr, "~ %s override changed\n", c.Adapter)
		}
	}

	lockPath := filepath.Join(workflowDir, ".criteria.lock.hcl")
	if err := lockfile.Write(lockPath, newLF); err != nil {
		return fmt.Errorf("write lockfile: %w", err)
	}
	return nil
}

// workflowAdapter holds the parsed adapter declaration plus an optional OCI
// reference extracted from raw HCL.
type workflowAdapter struct {
	Type      string
	Name      string
	Reference string // from raw HCL attribute, may be empty
}

// collectWorkflowAliases extracts registry alias blocks from workflow HCL.
func collectWorkflowAliases(workflowDir string, spec *workflow.Spec) (map[string]string, error) {
	aliases := make(map[string]string)

	hclFiles, err := listHCLFiles(workflowDir)
	if err != nil {
		return nil, err
	}

	for _, path := range hclFiles {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %q: %w", path, err)
		}
		file, diags := hclparse.NewParser().ParseHCL(data, path)
		if diags.HasErrors() {
			continue
		}
		body, ok := file.Body.(*hclsyntax.Body)
		if !ok {
			continue
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
	}

	return aliases, nil
}

func missingRefs(lf *lockfile.Lockfile, adapters map[string]*workflowAdapter) []string {
	if lf == nil {
		var out []string
		for k := range adapters {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	set := make(map[string]struct{}, len(lf.Adapters))
	for _, a := range lf.Adapters {
		set[a.Type+"."+a.Name] = struct{}{}
	}
	var out []string
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

func pullAndBuild(ctx context.Context, puller *oci.Puller, layout *oci.Layout, ref oci.Reference, policy signing.Policy) (digest.Digest, lockfile.LockedAdapter, error) {
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

	signer, err := signing.Verify(ctx, layout, dg, policy)
	if err != nil {
		return "", zero, fmt.Errorf("signature verification: %w", err)
	}

	entry, err := lockfile.BuildEntry(ref, dg, m, signer, nil)
	if err != nil {
		return "", zero, fmt.Errorf("build lockfile entry: %w", err)
	}
	return dg, entry, nil
}

func resolveWithAliases(aliases map[string]string, raw string) (oci.Reference, error) {
	ctx := ResolveContext{WorkflowAliases: aliases}
	return Resolve(ctx, raw)
}
