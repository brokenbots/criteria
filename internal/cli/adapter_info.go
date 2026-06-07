package cli

import (
	"fmt"
	"io"
	"strings"

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
			return runInfo(cmd.OutOrStdout(), args[0])
		},
	}
	return cmd
}

func runInfo(out io.Writer, refOrName string) error {
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

	artFS, err := layout.Open(dg)
	if err != nil {
		return fmt.Errorf("open artifact: %w", err)
	}

	m, err := manifest.ParseFromFS(artFS, "adapter.yaml")
	if err != nil {
		return fmt.Errorf("read adapter.yaml: %w", err)
	}

	fmt.Fprintf(out, "Name:         %s\n", m.Name)
	fmt.Fprintf(out, "Version:      %s\n", m.Version)
	fmt.Fprintf(out, "Description:  %s\n", m.Description)
	fmt.Fprintf(out, "Source:       %s\n", m.SourceURL)
	fmt.Fprintf(out, "Protocol:     %d\n", m.SDKProtocolVersion)
	fmt.Fprintf(out, "Platforms:    %s\n", strings.Join(platformStrings(m.Platforms), ", "))
	if m.ContainerImage != nil {
		fmt.Fprintf(out, "Container:    %s (%s)\n", m.ContainerImage.Ref, m.ContainerImage.Digest)
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
