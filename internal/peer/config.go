// Package peer implements the engine-side peer supervisor (ADR-0007 Stage A).
//
// The peer is the engine-side counterpart of the phone-home runner: it
// resolves and launches an adapter child as a real go-plugin subprocess,
// keeps the child alive across host disconnects (child keepalive), and
// records process-lifecycle facts into a bounded supervision journal that a
// later stage streams to the host via PeerService.
//
// The peer is an engine-side supervisor: it may import internal/adapterhost
// and internal/adapter, and sdk/pb/criteria/v1 for the journal event
// contract. It must not re-implement adapter hosting logic.
package peer

import (
	"crypto/tls"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/brokenbots/criteria/internal/adapter/manifest"
	"github.com/brokenbots/criteria/internal/adapterhost"
)

// Environment variables consumed by the peer. The CRITERIA_REMOTE_* block is
// shared with the phone-home runner so k8s manifests keep working; the
// CRITERIA_PEER_* block is peer-specific.
const (
	EnvRemoteHost      = "CRITERIA_REMOTE_HOST"
	EnvRemoteToken     = "CRITERIA_REMOTE_TOKEN"
	EnvRemoteScope     = "CRITERIA_REMOTE_SCOPE"
	EnvRemoteDigest    = "CRITERIA_REMOTE_DIGEST"
	EnvRemoteTLSCert   = "CRITERIA_REMOTE_TLS_CERT"
	EnvRemoteTLSKey    = "CRITERIA_REMOTE_TLS_KEY"
	EnvRemoteCA        = "CRITERIA_REMOTE_CA"
	EnvAdapterName     = "CRITERIA_ADAPTER_NAME"
	EnvAdapterVersion  = "CRITERIA_ADAPTER_VERSION"
	EnvAdapterBinary   = "CRITERIA_ADAPTER_BINARY"
	EnvAdapterManifest = "CRITERIA_ADAPTER_MANIFEST"
	EnvLogLevel        = "CRITERIA_LOG_LEVEL"
	EnvChildKeepAlive  = "CRITERIA_PEER_CHILD_KEEPALIVE"
	EnvJournalLimit    = "CRITERIA_PEER_JOURNAL_LIMIT"
	EnvBackoffMin      = "CRITERIA_PEER_BACKOFF_MIN"
	EnvBackoffMax      = "CRITERIA_PEER_BACKOFF_MAX"
)

const (
	// DefaultJournalLimit is the default bounded journal capacity
	// (ADR-0007: 4096 events, env-tunable via CRITERIA_PEER_JOURNAL_LIMIT).
	DefaultJournalLimit = 4096
	// DefaultBackoffMin is the default floor for host reconnect backoff.
	DefaultBackoffMin = time.Second
	// DefaultBackoffMax is the default ceiling for host reconnect backoff.
	DefaultBackoffMax = 30 * time.Second
	// DefaultVersion is used when neither the manifest nor the environment
	// declares an adapter version (runner parity).
	DefaultVersion = "0.0.0"
	// adapterBinaryPrefix is the conventional adapter binary basename prefix.
	adapterBinaryPrefix = "criteria-adapter-"
)

// Config holds the peer's environment-driven connection, identity, and
// supervision settings (env-first so manifest parity with the current runner
// is preserved).
type Config struct {
	Host     string // EnvRemoteHost: required; host:port or unix socket path
	Token    string // EnvRemoteToken
	Scope    string // EnvRemoteScope
	Digest   string // EnvRemoteDigest
	LogLevel string // EnvLogLevel

	TLSCertPath string // EnvRemoteTLSCert
	TLSKeyPath  string // EnvRemoteTLSKey
	TLSCAPath   string // EnvRemoteCA

	AdapterName     string // EnvAdapterName
	AdapterVersion  string // EnvAdapterVersion
	AdapterBinary   string // EnvAdapterBinary
	AdapterManifest string // EnvAdapterManifest

	// ChildKeepAlive: when true (the default) the child adapter survives a
	// host disconnect and is killed only by peer shutdown or a Control RPC.
	ChildKeepAlive bool
	// JournalLimit caps the bounded supervision journal.
	JournalLimit int
	// BackoffMin/BackoffMax bound the host reconnect backoff (Stage B wires
	// reconnects; parsed and validated here so peers fail fast on bad
	// configuration).
	BackoffMin time.Duration
	BackoffMax time.Duration

	// TLS is loaded from the three CRITERIA_REMOTE_TLS_* paths by Resolve via
	// adapterhost.LoadClientTLS; nil when no TLS material is configured.
	TLS *tls.Config
}

// LoadConfig reads the peer configuration from the provided environment
// lookup (env-first). Values that need parsing (booleans, integers,
// durations) are validated here; anything unparseable is an error, not a
// silent default.
func LoadConfig(getenv func(string) string) (Config, error) {
	cfg := Config{
		Host:            getenv(EnvRemoteHost),
		Token:           getenv(EnvRemoteToken),
		Scope:           getenv(EnvRemoteScope),
		Digest:          strings.TrimSpace(getenv(EnvRemoteDigest)),
		LogLevel:        getenv(EnvLogLevel),
		TLSCertPath:     getenv(EnvRemoteTLSCert),
		TLSKeyPath:      getenv(EnvRemoteTLSKey),
		TLSCAPath:       getenv(EnvRemoteCA),
		AdapterName:     getenv(EnvAdapterName),
		AdapterVersion:  getenv(EnvAdapterVersion),
		AdapterBinary:   getenv(EnvAdapterBinary),
		AdapterManifest: getenv(EnvAdapterManifest),
	}

	keepAlive := strings.TrimSpace(getenv(EnvChildKeepAlive))
	if keepAlive == "" {
		cfg.ChildKeepAlive = true
	} else {
		v, err := strconv.ParseBool(keepAlive)
		if err != nil {
			return Config{}, fmt.Errorf("%s must be a boolean, got %q", EnvChildKeepAlive, keepAlive)
		}
		cfg.ChildKeepAlive = v
	}

	cfg.JournalLimit = DefaultJournalLimit
	if raw := strings.TrimSpace(getenv(EnvJournalLimit)); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil {
			return Config{}, fmt.Errorf("%s must be a positive integer, got %q", EnvJournalLimit, raw)
		}
		if v <= 0 {
			return Config{}, fmt.Errorf("%s must be a positive integer, got %d", EnvJournalLimit, v)
		}
		cfg.JournalLimit = v
	}

	cfg.BackoffMin = DefaultBackoffMin
	if raw := strings.TrimSpace(getenv(EnvBackoffMin)); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			return Config{}, fmt.Errorf("%s must be a positive duration, got %q", EnvBackoffMin, raw)
		}
		cfg.BackoffMin = d
	}
	cfg.BackoffMax = DefaultBackoffMax
	if raw := strings.TrimSpace(getenv(EnvBackoffMax)); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			return Config{}, fmt.Errorf("%s must be a positive duration, got %q", EnvBackoffMax, raw)
		}
		cfg.BackoffMax = d
	}
	if cfg.BackoffMax < cfg.BackoffMin {
		return Config{}, fmt.Errorf("%s (%s) must be >= %s (%s)", EnvBackoffMax, cfg.BackoffMax, EnvBackoffMin, cfg.BackoffMin)
	}
	return cfg, nil
}

// LoadConfigFromEnv reads the peer configuration from the process
// environment.
func LoadConfigFromEnv() (Config, error) {
	return LoadConfig(os.Getenv)
}

// Resolve validates the configuration and fills identity and binary defaults.
// Precedence is ported from the phone-home runner
// (cmd/criteria-adapter-remote-runner/runner.go resolve()): manifest →
// CRITERIA_ADAPTER_BINARY → conventional install path → PATH lookup of
// criteria-adapter-<name> → first PATH entry.
func (c *Config) Resolve() error {
	if c.Host == "" {
		return errors.New("CRITERIA_REMOTE_HOST is required")
	}

	tlsCfg, err := adapterhost.LoadClientTLS(c.TLSCertPath, c.TLSKeyPath, c.TLSCAPath)
	if err != nil {
		return err
	}
	c.TLS = tlsCfg

	if err := c.resolveFromManifest(); err != nil {
		return err
	}
	c.resolveDefaults()

	if c.Binary() == "" {
		return errors.New("could not locate an adapter binary; set CRITERIA_ADAPTER_BINARY")
	}
	if err := c.resolveBinaryPath(); err != nil {
		return err
	}
	return c.resolveDigestBinary()
}

// Binary returns the resolved adapter binary path.
func (c *Config) Binary() string {
	return c.AdapterBinary
}

// resolveFromManifest fills identity from the adapter manifest when the
// environment does not already declare it (runner parity).
func (c *Config) resolveFromManifest() error {
	if c.AdapterManifest == "" {
		return nil
	}
	m, err := manifest.ParseFile(c.AdapterManifest)
	if err != nil {
		return fmt.Errorf("read manifest %q: %w", c.AdapterManifest, err)
	}
	if c.AdapterName == "" {
		c.AdapterName = m.Name
	}
	if c.AdapterVersion == "" {
		c.AdapterVersion = m.Version
	}
	if c.AdapterBinary == "" {
		c.AdapterBinary = defaultBinaryPath(m.Name)
	}
	return nil
}

// resolveDefaults applies the identity/binary fallbacks shared with the
// runner: PATH lookup of the conventional per-name binary, then the
// conventional install path (runner parity: k8s manifests install adapters
// at /usr/local/bin), then the first criteria-adapter-* entry found on PATH.
// The first-match fallback only applies when the adapter name is unknown — a
// known adapter must never silently swap to an unrelated binary.
func (c *Config) resolveDefaults() {
	if c.AdapterBinary == "" && c.AdapterName != "" {
		if p, err := exec.LookPath(adapterBinaryPrefix + c.AdapterName); err == nil {
			c.AdapterBinary = p
		}
	}
	if c.AdapterBinary == "" && c.AdapterName != "" {
		c.AdapterBinary = defaultBinaryPath(c.AdapterName)
	}
	if c.AdapterBinary == "" {
		c.AdapterBinary = firstAdapterBinary()
	}
	if c.AdapterName == "" {
		c.AdapterName = nameFromBinary(c.AdapterBinary)
	}
	if c.AdapterVersion == "" {
		c.AdapterVersion = DefaultVersion
	}
}

// resolveBinaryPath resolves a bare (no separator) CRITERIA_ADAPTER_BINARY
// against PATH (runner parity). A resolved path or a conventional
// /usr/local/bin path is left untouched.
func (c *Config) resolveBinaryPath() error {
	binary := c.Binary()
	if binary == "" || strings.Contains(binary, string(filepath.Separator)) {
		return nil
	}
	p, err := exec.LookPath(binary)
	if err != nil {
		return fmt.Errorf("adapter binary %q not found on PATH: %w", binary, err)
	}
	c.AdapterBinary = p
	return nil
}

// resolveDigestBinary prefers the digest-addressed binary from the local OCI
// cache when CRITERIA_REMOTE_DIGEST is set. No network pull happens here:
// pulling the artifact is the operator's job, same as the runner today. When
// no pinned artifact exists locally the chain-resolved binary is kept and
// the digest is still recorded on spawn events for host-side identity.
func (c *Config) resolveDigestBinary() error {
	if c.Digest == "" || c.AdapterName == "" {
		return nil
	}
	d, err := digest.Parse(c.Digest)
	if err != nil {
		return fmt.Errorf("invalid %s: %w", EnvRemoteDigest, err)
	}
	pinned, err := adapterhost.DiscoverBinaryAt(c.AdapterName, adapterhost.EncodeDigest(d))
	if err != nil {
		return nil
	}
	c.AdapterBinary = pinned
	return nil
}

// defaultBinaryPath returns the conventional install path for a named adapter
// (runner parity: k8s manifests install adapters at /usr/local/bin).
func defaultBinaryPath(name string) string {
	if name == "" {
		return ""
	}
	return "/usr/local/bin/" + adapterBinaryPrefix + name
}

// nameFromBinary derives an adapter name from a binary path, stripping the
// conventional prefix when present (runner parity).
func nameFromBinary(binary string) string {
	base := filepath.Base(binary)
	if strings.HasPrefix(base, adapterBinaryPrefix) {
		return strings.TrimPrefix(base, adapterBinaryPrefix)
	}
	return base
}

// firstAdapterBinary scans PATH in order and returns the first conventional
// adapter binary that is not the remote runner itself (runner parity).
func firstAdapterBinary() string {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), adapterBinaryPrefix) && !strings.Contains(e.Name(), "remote-runner") {
				return filepath.Join(dir, e.Name())
			}
		}
	}
	return ""
}
