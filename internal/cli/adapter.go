package cli

import (
	"fmt"

	"github.com/opencontainers/go-digest"
	"github.com/spf13/cobra"

	"github.com/brokenbots/criteria/internal/adapter/manifest"
	"github.com/brokenbots/criteria/internal/adapter/oci"
	"github.com/brokenbots/criteria/internal/dirs"
)

// NewAdapterCmd returns the `criteria adapter` parent command.
func NewAdapterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "adapter",
		Short: "Manage adapter plugins (pull, lock, publish, list, info, remove, prune, dev)",
	}

	cmd.AddCommand(newAdapterPullCmd())
	cmd.AddCommand(newAdapterLockCmd())
	cmd.AddCommand(newAdapterPublishCmd())
	cmd.AddCommand(newAdapterListCmd())
	cmd.AddCommand(newAdapterInfoCmd())
	cmd.AddCommand(newAdapterWhereCmd())
	cmd.AddCommand(newAdapterRemoveCmd())
	cmd.AddCommand(newAdapterPruneCmd())
	cmd.AddCommand(newAdapterDevCmd())

	return cmd
}

// defaultCacheRoot returns the default OCI cache root.
func defaultCacheRoot() (string, error) {
	return dirs.CacheOCI()
}

// defaultGlobalConfigPath returns the default global config file path.
func defaultGlobalConfigPath() (string, error) {
	return dirs.ConfigPath()
}

// defaultTrustConfigPath returns the default global trust-config file path.
func defaultTrustConfigPath() (string, error) {
	return dirs.TrustConfigPath()
}

// resolveRefOrName attempts to turn a user-supplied string into a digest.
// If the string is already a digest it is returned as-is.  Otherwise the
// cached adapters are searched by reading adapter.yaml and matching on
// manifest.Name or a "type.name" pattern.
func resolveRefOrName(layout *oci.Layout, refOrName string) (digest.Digest, error) {
	// Direct digest.
	if dg, err := digest.Parse(refOrName); err == nil {
		if layout.HasBlob(dg) {
			return dg, nil
		}
		return "", fmt.Errorf("digest %s not found in cache", dg)
	}

	ix, err := layout.Index()
	if err != nil {
		return "", fmt.Errorf("read index: %w", err)
	}

	var candidates []digest.Digest
	for _, m := range ix.Manifests {
		dg := m.Digest
		artFS, err := layout.Open(dg)
		if err != nil {
			continue
		}
		mf, err := manifest.ParseFromFS(artFS, "adapter.yaml")
		if err != nil {
			continue
		}
		if mf.Name == refOrName || refOrName == fmt.Sprintf("%s/%s", dg.Algorithm(), dg.Encoded()) {
			candidates = append(candidates, dg)
		}
	}

	if len(candidates) == 1 {
		return candidates[0], nil
	}
	if len(candidates) > 1 {
		return "", fmt.Errorf("ambiguous name %q matches multiple cached adapters", refOrName)
	}

	return "", fmt.Errorf("adapter %q not found in cache", refOrName)
}
