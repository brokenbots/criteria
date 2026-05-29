package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
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
