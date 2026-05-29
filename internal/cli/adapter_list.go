package cli

import (
	"fmt"
	"os"
	"sort"

	"github.com/spf13/cobra"

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
			return runList(installed, referenced)
		},
	}

	cmd.Flags().BoolVar(&installed, "installed", false, "List adapters present in the local OCI cache")
	cmd.Flags().BoolVar(&referenced, "referenced", false, "List adapters referenced by the current workflow's lockfile")
	return cmd
}

func runList(installed, referenced bool) error {
	if installed {
		if err := listInstalled(); err != nil {
			return err
		}
	}
	if referenced {
		if err := listReferenced(); err != nil {
			return err
		}
	}
	return nil
}

func listInstalled() error {
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
		fmt.Fprintln(os.Stdout, "(no cached adapters)")
		return nil
	}

	for _, m := range ix.Manifests {
		dg := m.Digest.String()
		ref := "(unknown)"
		if m.Annotations != nil {
			if v, ok := m.Annotations[oci.AnnotationProtocolVersion]; ok {
				ref = "protocol=" + v
			}
		}
		fmt.Fprintf(os.Stdout, "%s  %s\n", dg, ref)
	}
	return nil
}

func listReferenced() error {
	lf, err := lockfile.ReadFromDir(".")
	if err != nil {
		return fmt.Errorf("read lockfile: %w", err)
	}
	if lf == nil {
		fmt.Fprintln(os.Stdout, "(no lockfile in current directory)")
		return nil
	}

	sorted := make([]lockfile.LockedAdapter, len(lf.Adapters))
	copy(sorted, lf.Adapters)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Type+"."+sorted[i].Name < sorted[j].Type+"."+sorted[j].Name
	})

	for i := range sorted {
		fmt.Fprintf(os.Stdout, "%s.%s  %s  %s\n", sorted[i].Type, sorted[i].Name, sorted[i].Reference, sorted[i].ResolvedDigest)
	}
	return nil
}
