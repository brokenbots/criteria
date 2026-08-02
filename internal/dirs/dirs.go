// Package dirs provides the single source of truth for paths that criteria
// derives from its root directory.
//
// The canonical root environment variable is CRITERIA_HOME, defaulting to
// ~/.local/criteria on Unix. The legacy CRITERIA_STATE_DIR variable is still
// honored when CRITERIA_HOME is not set, and an existing ~/.criteria directory
// is detected and kept in place so older installs continue to work after an
// upgrade.
package dirs

import (
	"os"
	"path/filepath"
	"strings"
)

const (
	legacyRootName = ".criteria"
	defaultRoot    = ".local/criteria"
	adaptersSubdir = "adapters"
)

// Home returns the criteria root directory.
//
// Resolution order:
//  1. $CRITERIA_HOME
//  2. $CRITERIA_STATE_DIR (deprecated alias)
//  3. An existing directory at ~/.criteria (legacy in-place use)
//  4. ~/.local/criteria
func Home() (string, error) {
	if v := strings.TrimSpace(os.Getenv("CRITERIA_HOME")); v != "" {
		return v, nil
	}

	if v := strings.TrimSpace(os.Getenv("CRITERIA_STATE_DIR")); v != "" {
		return v, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	legacy := filepath.Join(home, legacyRootName)
	if info, err := os.Stat(legacy); err == nil && info.IsDir() {
		return legacy, nil
	}

	return filepath.Join(home, defaultRoot), nil
}

// AdaptersDir returns the directory used for filesystem-installed adapter
// binaries. If CRITERIA_ADAPTERS is set it wins; otherwise the result is
// Home()/adapters.
func AdaptersDir() (string, error) {
	if v := strings.TrimSpace(os.Getenv("CRITERIA_ADAPTERS")); v != "" {
		return v, nil
	}
	return DefaultAdaptersDir()
}

// DefaultAdaptersDir returns Home()/adapters, ignoring any CRITERIA_ADAPTERS
// override. It is useful when callers need both the override directory and
// the default install directory (e.g. discovery search roots).
func DefaultAdaptersDir() (string, error) {
	root, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, adaptersSubdir), nil
}

// CacheOCI returns the directory used for the OCI adapter cache.
func CacheOCI() (string, error) {
	root, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "cache", "oci"), nil
}

// CacheWorkflows returns the directory used for fetched subworkflow caches.
func CacheWorkflows() (string, error) {
	root, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "cache", "workflows"), nil
}

// CacheSigstore returns the directory used for sigstore verification caches.
func CacheSigstore() (string, error) {
	root, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "cache", "sigstore"), nil
}

// ConfigPath returns the path to the global criteria configuration file.
func ConfigPath() (string, error) {
	root, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "config.hcl"), nil
}

// TrustConfigPath returns the path to the global trust configuration file.
func TrustConfigPath() (string, error) {
	root, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "trust.hcl"), nil
}
