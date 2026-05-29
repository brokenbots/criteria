package cli

import (
	"fmt"
	"os"
	"runtime"

	"github.com/opencontainers/go-digest"
	"github.com/spf13/cobra"

	"github.com/brokenbots/criteria/internal/adapter/oci"
)

func newAdapterWhereCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "where <ref-or-name>",
		Short: "Print the on-disk binary path for the host platform",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			return runWhere(args[0])
		},
	}
	return cmd
}

func runWhere(refOrName string) error {
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
		return fmt.Errorf("where requires a digest reference: %w", err)
	}

	artFS, err := layout.Open(dg)
	if err != nil {
		return fmt.Errorf("open artifact: %w", err)
	}

	platformBin := fmt.Sprintf("bin/%s/%s", runtime.GOOS, runtime.GOARCH)
	f, err := artFS.Open(platformBin)
	if err != nil {
		return fmt.Errorf("host platform binary not found in artifact: %w", err)
	}
	_ = f.Close()

	// Print the actual blob path.
	fmt.Fprintln(os.Stdout, layout.BlobPath(dg))
	return nil
}
