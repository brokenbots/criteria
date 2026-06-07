package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/cobra"

	"github.com/brokenbots/criteria/internal/adapter/oci"
)

func newAdapterRemoveCmd() *cobra.Command {
	var prune bool

	cmd := &cobra.Command{
		Use:   "remove <ref-or-name>",
		Short: "Remove an adapter from the local OCI cache index",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			return runRemove(args[0], prune, cmd.OutOrStdout())
		},
	}

	cmd.Flags().BoolVar(&prune, "prune", false, "Run GC after removal to reclaim blob space")
	return cmd
}

func runRemove(refOrName string, doPrune bool, out io.Writer) error {
	if out == nil {
		out = os.Stderr
	}
	cacheRoot, err := defaultCacheRoot()
	if err != nil {
		return err
	}
	layout, err := oci.Open(cacheRoot)
	if err != nil {
		return fmt.Errorf("open cache: %w", err)
	}

	dg, err := resolveRefOrName(layout, refOrName)
	if err != nil {
		return err
	}

	release, err := layout.Lock()
	if err != nil {
		return fmt.Errorf("acquire lock: %w", err)
	}

	ix, err := layout.Index()
	if err != nil {
		release()
		return fmt.Errorf("read index: %w", err)
	}

	keep, found := filterIndexRemove(ix.Manifests, dg)
	if !found {
		release()
		return fmt.Errorf("adapter %s not found in cache index", dg)
	}

	ix.Manifests = keep
	if err := layout.WriteIndex(ix); err != nil {
		release()
		return fmt.Errorf("write index: %w", err)
	}
	release()

	fmt.Fprintf(out, "removed %s from cache index\n", dg)

	if doPrune {
		return pruneAfterRemove(layout, out)
	}
	return nil
}

func filterIndexRemove(manifests []ocispec.Descriptor, dg digest.Digest) ([]ocispec.Descriptor, bool) {
	keep := make([]ocispec.Descriptor, 0, len(manifests))
	found := false
	for i := range manifests {
		if manifests[i].Digest == dg {
			found = true
			continue
		}
		keep = append(keep, manifests[i])
	}
	return keep, found
}

func pruneAfterRemove(layout *oci.Layout, out io.Writer) error {
	result, err := layout.GC(oci.GCOptions{})
	if err != nil {
		return fmt.Errorf("gc: %w", err)
	}
	fmt.Fprintf(out, "pruned %d blobs, freed %d bytes\n", result.RemovedBlobs, result.FreedBytes)
	return nil
}
