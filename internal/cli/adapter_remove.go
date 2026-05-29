package cli

import (
	"fmt"
	"os"

	"github.com/opencontainers/go-digest"
	"github.com/spf13/cobra"

	"github.com/brokenbots/criteria/internal/adapter/oci"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func newAdapterRemoveCmd() *cobra.Command {
	var prune bool

	cmd := &cobra.Command{
		Use:   "remove <ref-or-name>",
		Short: "Remove an adapter from the local OCI cache index",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			return runRemove(args[0], prune)
		},
	}

	cmd.Flags().BoolVar(&prune, "prune", false, "Run GC after removal to reclaim blob space")
	return cmd
}

func runRemove(refOrName string, doPrune bool) error {
	cacheRoot, err := defaultCacheRoot()
	if err != nil {
		return err
	}
	layout, err := oci.Open(cacheRoot)
	if err != nil {
		return fmt.Errorf("open cache: %w", err)
	}

	dg, err := digest.Parse(refOrName)
	if err != nil {
		return fmt.Errorf("remove requires a digest reference: %w", err)
	}

	release, err := layout.Lock()
	if err != nil {
		return fmt.Errorf("acquire lock: %w", err)
	}
	defer release()

	ix, err := layout.Index()
	if err != nil {
		return fmt.Errorf("read index: %w", err)
	}

	found := false
	var keep []ocispec.Descriptor
	for _, m := range ix.Manifests {
		if m.Digest == dg {
			found = true
			continue
		}
		keep = append(keep, m)
	}
	if !found {
		return fmt.Errorf("adapter %s not found in cache index", dg)
	}

	ix.Manifests = keep
	if err := layout.WriteIndex(ix); err != nil {
		return fmt.Errorf("write index: %w", err)
	}

	fmt.Fprintf(os.Stderr, "removed %s from cache index\n", dg)

	if doPrune {
		result, err := layout.GC(oci.GCOptions{})
		if err != nil {
			return fmt.Errorf("gc: %w", err)
		}
		fmt.Fprintf(os.Stderr, "pruned %d blobs, freed %d bytes\n", result.RemovedBlobs, result.FreedBytes)
	}
	return nil
}
