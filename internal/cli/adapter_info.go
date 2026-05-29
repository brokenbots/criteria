package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/opencontainers/go-digest"
	"github.com/spf13/cobra"

	"github.com/brokenbots/criteria/internal/adapter/manifest"
	"github.com/brokenbots/criteria/internal/adapter/oci"
)

func newAdapterInfoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "info <ref-or-name>",
		Short: "Print cached manifest and signer info for an adapter",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			return runInfo(args[0])
		},
	}
	return cmd
}

func runInfo(refOrName string) error {
	cacheRoot, err := defaultCacheRoot()
	if err != nil {
		return err
	}
	layout, err := oci.Open(cacheRoot)
	if err != nil {
		return fmt.Errorf("open cache: %w", err)
	}

	// Try to resolve as a digest first.
	dg, err := digest.Parse(refOrName)
	if err != nil {
		// If not a digest, search index by annotation or treat as unknown.
		ix, idxErr := layout.Index()
		if idxErr != nil {
			return fmt.Errorf("read index: %w", idxErr)
		}
		found := false
		for _, m := range ix.Manifests {
			if m.Annotations != nil && m.Annotations["org.opencontainers.image.title"] == refOrName {
				dg = m.Digest
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("adapter %q not found in cache", refOrName)
		}
	}

	artFS, err := layout.Open(dg)
	if err != nil {
		return fmt.Errorf("open artifact: %w", err)
	}

	m, err := manifest.ParseFromFS(artFS, "adapter.yaml")
	if err != nil {
		return fmt.Errorf("read adapter.yaml: %w", err)
	}

	fmt.Fprintf(os.Stdout, "Name:         %s\n", m.Name)
	fmt.Fprintf(os.Stdout, "Version:      %s\n", m.Version)
	fmt.Fprintf(os.Stdout, "Description:  %s\n", m.Description)
	fmt.Fprintf(os.Stdout, "Source:       %s\n", m.SourceURL)
	fmt.Fprintf(os.Stdout, "Protocol:     %d\n", m.SDKProtocolVersion)
	fmt.Fprintf(os.Stdout, "Platforms:    %s\n", strings.Join(platformStrings(m.Platforms), ", "))
	if m.ContainerImage != nil {
		fmt.Fprintf(os.Stdout, "Container:    %s (%s)\n", m.ContainerImage.Ref, m.ContainerImage.Digest)
	}
	return nil
}

func platformStrings(ps []manifest.Platform) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.OS + "/" + p.Arch
	}
	return out
}
