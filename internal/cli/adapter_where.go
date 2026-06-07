package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
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
			return runWhere(cmd.OutOrStdout(), args[0])
		},
	}
	return cmd
}

func runWhere(out io.Writer, refOrName string) error {
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

	_, err = layout.Open(dg)
	if err != nil {
		return fmt.Errorf("open artifact: %w", err)
	}

	platformBin := fmt.Sprintf("bin/%s/%s", runtime.GOOS, runtime.GOARCH)
	expectedPrefix := platformBin + "/"

	// Read the manifest blob directly to find the binary layer digest.
	manifestData, err := os.ReadFile(layout.BlobPath(dg))
	if err != nil {
		return fmt.Errorf("read manifest blob: %w", err)
	}
	var manifestDesc ocispec.Manifest
	if err := json.Unmarshal(manifestData, &manifestDesc); err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}

	for _, layer := range manifestDesc.Layers {
		title, ok := layer.Annotations[oci.AnnotationTitle]
		if !ok {
			continue
		}
		if strings.HasPrefix(title, expectedPrefix) {
			fmt.Fprintln(out, layout.BlobPath(layer.Digest))
			return nil
		}
	}

	return fmt.Errorf("host platform binary not found in artifact for %s/%s", runtime.GOOS, runtime.GOARCH)
}
