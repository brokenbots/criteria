package cli

import (
	"fmt"
	"io"
	"sort"

	"github.com/spf13/cobra"

	"github.com/brokenbots/criteria/internal/adapter/manifest"
	"github.com/brokenbots/criteria/internal/adapter/oci"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

func newAdapterListCmd() *cobra.Command {
	var (
		installed  bool
		referenced bool
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List cached or workflow-referenced adapters",
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			if !installed && !referenced {
				installed = true
			}
			return runList(cmd.OutOrStdout(), installed, referenced)
		},
	}

	cmd.Flags().BoolVar(&installed, "installed", false, "List adapters present in the local OCI cache")
	cmd.Flags().BoolVar(&referenced, "referenced", false, "List adapters referenced by the current workflow's lockfile")
	return cmd
}

func runList(out io.Writer, installed, referenced bool) error {
	if installed {
		if err := listInstalled(out); err != nil {
			return err
		}
	}
	if referenced {
		if err := listReferenced(out); err != nil {
			return err
		}
	}
	return nil
}

func listInstalled(out io.Writer) error {
	cacheRoot, err := defaultCacheRoot()
	if err != nil {
		return err
	}
	layout, err := oci.Open(cacheRoot)
	if err != nil {
		return fmt.Errorf("open cache: %w", err)
	}

	ix, err := layout.Index()
	if err != nil {
		return fmt.Errorf("read index: %w", err)
	}

	if len(ix.Manifests) == 0 {
		fmt.Fprintln(out, "(no cached adapters)")
		return nil
	}

	for _, m := range ix.Manifests {
		dg := m.Digest.String()
		name := "(unknown)"
		artFS, err := layout.Open(m.Digest)
		if err == nil {
			if mf, err := manifest.ParseFromFS(artFS, "adapter.yaml"); err == nil {
				name = mf.Name
			}
		}
		fmt.Fprintf(out, "%s  %s\n", dg, name)
	}
	return nil
}

func listReferenced(out io.Writer) error {
	lf, err := lockfile.ReadFromDir(".")
	if err != nil {
		return fmt.Errorf("read lockfile: %w", err)
	}
	if lf == nil {
		fmt.Fprintln(out, "(no lockfile in current directory)")
		return nil
	}

	sorted := make([]lockfile.LockedAdapter, len(lf.Adapters))
	copy(sorted, lf.Adapters)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Type+"."+sorted[i].Name < sorted[j].Type+"."+sorted[j].Name
	})

	for i := range sorted {
		fmt.Fprintf(out, "%s.%s  %s  %s\n", sorted[i].Type, sorted[i].Name, sorted[i].Reference, sorted[i].ResolvedDigest)
	}
	return nil
}
