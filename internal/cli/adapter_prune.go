package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/brokenbots/criteria/internal/adapter/oci"
)

func newAdapterPruneCmd() *cobra.Command {
	var (
		olderThan string
		maxSize   int64
	)

	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Remove unused or old adapter blobs from the local cache",
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			return runPrune(olderThan, maxSize)
		},
	}

	cmd.Flags().StringVar(&olderThan, "older-than", "", "Remove blobs older than this duration (e.g. 30d)")
	cmd.Flags().Int64Var(&maxSize, "max-size", 0, "Target maximum total cache size in bytes")
	return cmd
}

func runPrune(olderThan string, maxSize int64) error {
	var opts oci.GCOptions
	if olderThan != "" {
		d, err := time.ParseDuration(olderThan)
		if err != nil {
			return fmt.Errorf("parse --older-than: %w", err)
		}
		opts.OlderThan = d
	}
	if maxSize > 0 {
		opts.MaxSize = maxSize
	}

	cacheRoot, err := defaultCacheRoot()
	if err != nil {
		return err
	}
	layout, err := oci.Open(cacheRoot)
	if err != nil {
		return fmt.Errorf("open cache: %w", err)
	}

	result, err := layout.GC(opts)
	if err != nil {
		return fmt.Errorf("gc: %w", err)
	}

	fmt.Fprintf(os.Stderr, "pruned %d blobs, freed %d bytes\n", result.RemovedBlobs, result.FreedBytes)
	if len(result.Errors) > 0 {
		for _, e := range result.Errors {
			fmt.Fprintf(os.Stderr, "warning: %v\n", e)
		}
	}
	return nil
}
