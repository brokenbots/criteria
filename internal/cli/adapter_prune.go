package cli

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
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
			return runPrune(olderThan, maxSize, cmd.OutOrStdout())
		},
	}

	cmd.Flags().StringVar(&olderThan, "older-than", "", "Remove blobs older than this duration (e.g. 30d)")
	cmd.Flags().Int64Var(&maxSize, "max-size", 0, "Target maximum total cache size in bytes")
	return cmd
}

func runPrune(olderThan string, maxSize int64, out io.Writer) error {
	if out == nil {
		out = os.Stderr
	}
	var opts oci.GCOptions
	if olderThan != "" {
		d, err := parseHumanDuration(olderThan)
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

	fmt.Fprintf(out, "pruned %d blobs, freed %d bytes\n", result.RemovedBlobs, result.FreedBytes)
	if len(result.Errors) > 0 {
		for _, e := range result.Errors {
			fmt.Fprintf(out, "warning: %v\n", e)
		}
	}
	return nil
}

// parseHumanDuration extends Go's time.ParseDuration with support for days
// (e.g. "30d", "1d12h").
func parseHumanDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	// Simple replacement: convert whole days to hours before delegating.
	var days float64
	rest := s
	for {
		i := strings.Index(rest, "d")
		if i == -1 {
			break
		}
		if i == 0 {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		// Parse the numeric prefix.
		j := i - 1
		for j >= 0 && (rest[j] >= '0' && rest[j] <= '9' || rest[j] == '.') {
			j--
		}
		if j == i-1 {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		n, err := strconv.ParseFloat(rest[j+1:i], 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q: %w", s, err)
		}
		days += n
		rest = rest[:j+1] + rest[i+1:]
	}
	if rest == "" {
		return time.Duration(days*24) * time.Hour, nil
	}
	d, err := time.ParseDuration(rest)
	if err != nil {
		return 0, err
	}
	return d + time.Duration(days*24)*time.Hour, nil
}
