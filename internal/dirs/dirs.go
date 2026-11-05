// Package dirs provides the single source of truth for paths that criteria
// derives from its directories.
//
// Resolution precedence:
//
//  1. $CRITERIA_HOME — the app override. When set, the whole tree (config,
//     trust, state, cache, adapters) stays rooted in that single directory,
//     exactly as before the XDG split.
//  2. $CRITERIA_STATE_DIR — deprecated alias, honored only when
//     $CRITERIA_HOME is unset (same single-tree behavior).
//  3. XDG base directories — when no app root is set, each category roots
//     independently, honoring $XDG_CONFIG_HOME / $XDG_STATE_HOME /
//     $XDG_CACHE_HOME and falling back to their specification defaults
//     under $HOME when unset (or non-absolute, which the XDG spec says to
//     ignore).
//
// There is no existence-based adoption of a legacy directory: a
// pre-existing ~/.criteria is never read, never written, and never promoted
// to a trust-anchor location. Trust configuration lives at the resolved
// config root (trust.hcl), and trust is only ever loaded from paths this
// package returned.
package dirs

import (
	"os"
	"path/filepath"
	"strings"
)

const (
	// DefaultAdaptersRootName is the well-known adapter install location
	// under $HOME. install.sh and `make install` place adapter binaries
	// here without exporting CRITERIA_ADAPTERS, so discovery defaults to
	// this path even when criteria resolves its config/state/cache roots
	// through the XDG base directories.
	DefaultAdaptersRootName = ".local/criteria/adapters"

	// xdgAppDir is the subdirectory appended to an XDG base directory to
	// scope it to criteria.
	xdgAppDir = "criteria"

	// XDG specification defaults, relative to $HOME.
	defaultConfigRel = ".config/criteria"
	defaultStateRel  = ".local/state/criteria"
	defaultCacheRel  = ".cache/criteria"

	adaptersSubdir = "adapters"

	configBase = "config.hcl"
	trustBase  = "trust.hcl"
)

// appRoot returns the single-tree app override directory and whether it was
// configured: $CRITERIA_HOME, or the deprecated $CRITERIA_STATE_DIR alias
// when $CRITERIA_HOME is unset.
func appRoot() (string, bool) {
	if v := strings.TrimSpace(os.Getenv("CRITERIA_HOME")); v != "" {
		return v, true
	}
	if v := strings.TrimSpace(os.Getenv("CRITERIA_STATE_DIR")); v != "" {
		return v, true
	}
	return "", false
}

// xdgRoot resolves an XDG base-directory variable to the criteria-scoped
// directory under it. The variable is honored only when set to an absolute
// path (the XDG spec directs programs to ignore non-absolute values);
// otherwise the $HOME-relative specification default is used.
func xdgRoot(envVar, homeRel string) (string, error) {
	if v := strings.TrimSpace(os.Getenv(envVar)); v != "" && filepath.IsAbs(v) {
		return filepath.Join(v, xdgAppDir), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, homeRel), nil
}

// Home returns the criteria state root: the directory holding run records,
// step checkpoints, audit logs, persisted approval/signal decisions, and
// remote-adapter token files.
//
// Resolution order:
//  1. $CRITERIA_HOME
//  2. $CRITERIA_STATE_DIR (deprecated alias)
//  3. $XDG_STATE_HOME/criteria (default ~/.local/state/criteria)
func Home() (string, error) {
	if root, ok := appRoot(); ok {
		return root, nil
	}
	return xdgRoot("XDG_STATE_HOME", defaultStateRel)
}

// AdaptersDir returns the directory used for filesystem-installed adapter
// binaries. If CRITERIA_ADAPTERS is set it wins; otherwise the result is
// appRoot()/adapters when an app root is set, or $HOME/.local/criteria/adapters
// (the install.sh target) when criteria runs at its XDG defaults.
func AdaptersDir() (string, error) {
	if v := strings.TrimSpace(os.Getenv("CRITERIA_ADAPTERS")); v != "" {
		return v, nil
	}
	return DefaultAdaptersDir()
}

// DefaultAdaptersDir returns AdaptersDir's result ignoring any
// CRITERIA_ADAPTERS override. It is useful when callers need both the
// override directory and the default install directory (e.g. discovery
// search roots).
func DefaultAdaptersDir() (string, error) {
	if root, ok := appRoot(); ok {
		return filepath.Join(root, adaptersSubdir), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, DefaultAdaptersRootName), nil
}

// CacheOCI returns the directory used for the OCI adapter cache:
// appRoot()/cache/oci when an app root is set, otherwise
// $XDG_CACHE_HOME/criteria/oci (default ~/.cache/criteria/oci).
func CacheOCI() (string, error) {
	root, err := cacheRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "cache", "oci"), nil
}

// CacheWorkflows returns the directory used for fetched subworkflow caches.
func CacheWorkflows() (string, error) {
	root, err := cacheRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "cache", "workflows"), nil
}

// CacheSigstore returns the directory used for sigstore verification caches.
func CacheSigstore() (string, error) {
	root, err := cacheRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "cache", "sigstore"), nil
}

// cacheRoot returns the cache base directory: the app root when set,
// otherwise the cache-scoped XDG directory.
func cacheRoot() (string, error) {
	if root, ok := appRoot(); ok {
		return root, nil
	}
	return xdgRoot("XDG_CACHE_HOME", defaultCacheRel)
}

// ConfigPath returns the path to the global criteria configuration file:
// appRoot()/config.hcl when an app root is set, otherwise
// $XDG_CONFIG_HOME/criteria/config.hcl (default ~/.config/criteria/config.hcl).
func ConfigPath() (string, error) {
	base, err := configRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, configBase), nil
}

// TrustConfigPath returns the path to the global trust configuration file
// (the trust anchor for adapter signature verification).
func TrustConfigPath() (string, error) {
	base, err := configRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, trustBase), nil
}

// configRoot returns the config base directory: the app root when set,
// otherwise the config-scoped XDG directory.
func configRoot() (string, error) {
	if root, ok := appRoot(); ok {
		return root, nil
	}
	return xdgRoot("XDG_CONFIG_HOME", defaultConfigRel)
}
