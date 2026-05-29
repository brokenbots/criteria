package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/opencontainers/go-digest"
	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/internal/adapter/manifest"
	"github.com/brokenbots/criteria/internal/adapter/oci"
	"github.com/brokenbots/criteria/internal/adapter/signing"
	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

// autoPullCompileAdapters is called by parseCompileForCli when the workflow
// contains adapter blocks with `reference` attributes.  It validates the
// lockfile, pulls any missing cached binaries, and extracts the platform binary
// to the plugin directory so adapterhost.Loader can resolve them.
func autoPullCompileAdapters(ctx context.Context, workflowDir string, spec *workflow.Spec) error {
	// Read lockfile.
	lf, err := lockfile.ReadFromDir(workflowDir)
	if err != nil {
		return fmt.Errorf("read lockfile: %w", err)
	}
	if lf == nil {
		return fmt.Errorf("workflow uses OCI adapter references but %q is missing; run `criteria adapter lock`", filepath.Join(workflowDir, ".criteria.lock.hcl"))
	}

	// Build set of OCI-referenced adapters.
	ociAdapters, err := collectWorkflowAdapters(workflowDir, spec)
	if err != nil {
		return err
	}

	// Validate lockfile covers all workflow adapters.
	missing := []string{}
	for key, wa := range ociAdapters {
		if wa.Reference == "" {
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

	policy, err := signing.PolicyFor(signing.PullContext{})
	if err != nil {
		return fmt.Errorf("signing policy: %w", err)
	}
	puller := &oci.Puller{Layout: layout}

	for key, wa := range ociAdapters {
		if wa.Reference == "" {
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

	// If binary already cached, ensure it's in the plugin directory.
	if layout.HasBlob(dg) {
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
	_, err = signing.Verify(ctx, layout, pulledDg, *policy)
	if err != nil {
		return fmt.Errorf("adapter %q signature verification: %w", key, err)
	}

	if err := extractOCIAdapterBinary(layout, pulledDg, wa.Type); err != nil {
		return fmt.Errorf("adapter %q extract binary: %w", key, err)
	}
	return nil
}

// hasOCIReferences scans the workflow HCL files for adapter blocks that
// carry a `reference` attribute.  If none are found the workflow does not
// require a lockfile for compilation.
func hasOCIReferences(workflowDir string, spec *workflow.Spec) (bool, error) {
	adapters, err := collectWorkflowAdapters(workflowDir, spec)
	if err != nil {
		return false, err
	}
	for _, wa := range adapters {
		if wa.Reference != "" {
			return true, nil
		}
	}
	return false, nil
}

// extractOCIAdapterBinary reads the platform-specific binary from the OCI
// artifact and copies it into ~/.criteria/plugins/ so adapterhost.Loader
// can discover it.
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

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	pluginDir := filepath.Join(home, ".criteria", "plugins")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		return err
	}

	dest := filepath.Join(pluginDir, "criteria-adapter-"+adapterType)
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

// collectWorkflowAdapters merges spec.Adapters with any `reference` attributes
// found in raw HCL blocks.  The returned map key is "type.name".
func collectWorkflowAdapters(workflowDir string, spec *workflow.Spec) (map[string]*workflowAdapter, error) {
	out := make(map[string]*workflowAdapter, len(spec.Adapters))
	for _, a := range spec.Adapters {
		key := a.Type + "." + a.Name
		out[key] = &workflowAdapter{Type: a.Type, Name: a.Name}
	}

	hclFiles, err := listHCLFiles(workflowDir)
	if err != nil {
		return nil, err
	}

	for _, path := range hclFiles {
		if err := scanFileForReferences(path, out); err != nil {
			continue
		}
	}

	return out, nil
}

func scanFileForReferences(path string, out map[string]*workflowAdapter) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
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
		if block.Type != "adapter" || len(block.Labels) != 2 {
			continue
		}
		key := block.Labels[0] + "." + block.Labels[1]
		wa, ok := out[key]
		if !ok {
			continue
		}
		attrs, _, _ := block.Body.PartialContent(&hcl.BodySchema{
			Attributes: []hcl.AttributeSchema{{Name: "reference"}},
		})
		if attr, ok := attrs.Attributes["reference"]; ok {
			val, valDiags := attr.Expr.Value(nil)
			if !valDiags.HasErrors() && val.Type() == cty.String && !val.IsNull() {
				wa.Reference = val.AsString()
			}
		}
	}
	return nil
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
