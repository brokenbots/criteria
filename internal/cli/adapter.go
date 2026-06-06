package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/opencontainers/go-digest"
	"github.com/spf13/cobra"

	"github.com/brokenbots/criteria/internal/adapter/manifest"
	"github.com/brokenbots/criteria/internal/adapter/oci"
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

// defaultCacheRoot returns the default OCI cache root, honouring
// CRITERIA_STATE_DIR.
func defaultCacheRoot() (string, error) {
	base := os.Getenv("CRITERIA_STATE_DIR")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}
		base = filepath.Join(home, ".criteria")
	}
	return filepath.Join(base, "cache", "oci"), nil
}

// defaultGlobalConfigPath returns the default global config file path.
func defaultGlobalConfigPath() (string, error) {
	base := os.Getenv("CRITERIA_STATE_DIR")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}
		base = filepath.Join(home, ".criteria")
	}
	return filepath.Join(base, "config.hcl"), nil
}

// defaultTrustConfigPath returns the default global trust-config file path
// (~/.criteria/trust.hcl), honoring CRITERIA_STATE_DIR.
func defaultTrustConfigPath() (string, error) {
	base := os.Getenv("CRITERIA_STATE_DIR")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}
		base = filepath.Join(home, ".criteria")
	}
	return filepath.Join(base, "trust.hcl"), nil
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
