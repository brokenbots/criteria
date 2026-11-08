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
	"github.com/brokenbots/criteria/internal/envreg"
	"github.com/brokenbots/criteria/internal/tunables"
)

// Environment variables consumed by the peer. Names register in the central
// CRITERIA_* registry (internal/envreg; docs/env-vars.md) and are aliased
// here so the peer's parsing and tests keep their package-local names. The
// CRITERIA_REMOTE_* block is shared with the retired phone-home runner's
// k8s manifests; the CRITERIA_* adapter block is peer-specific.
var (
	EnvRemoteHost      = envreg.CRITERIARemoteHost.Env()
	EnvRemoteToken     = envreg.CRITERIARemoteToken.Env()
	EnvRemoteScope     = envreg.CRITERIARemoteScope.Env()
	EnvRemoteDigest    = envreg.CRITERIARemoteDigest.Env()
	EnvRemoteTLSCert   = envreg.CRITERIARemoteTLSCert.Env()
	EnvRemoteTLSKey    = envreg.CRITERIARemoteTLSKey.Env()
	EnvRemoteCA        = envreg.CRITERIARemoteCA.Env()
	EnvAdapterName     = envreg.CRITERIAAdapterName.Env()
	EnvAdapterVersion  = envreg.CRITERIAAdapterVersion.Env()
	EnvAdapterBinary   = envreg.CRITERIAAdapterBinary.Env()
	EnvAdapterManifest = envreg.CRITERIAAdapterManifest.Env()
	// The peer's pod-level adapter manifest deliberately lives under the
	// CRITERIA_REMOTE_* family, NOT CRITERIA_ADAPTERS: that pre-existing name
	// is the adapter install DIRECTORY consumed by adapterhost discovery
	// (internal/adapter/dirs, adapterhost), and reusing it for the multi-
	// adapter list would collapse two different variables into one value.
	// Under the remote prefix the manifest is also scrubbed from child
	// environments automatically (see resolve.go ChildEnv).
	EnvAdapters        = envreg.CRITERIARemoteAdapters.Env()
	EnvAdaptersDir     = envreg.CRITERIARemoteAdaptersDir.Env()
	EnvRemoteScopesDir = envreg.CRITERIARemoteScopesDir.Env()
	EnvLogLevel        = envreg.CRITERIALogLevel.Env()
	EnvChildKeepAlive  = envreg.CRITERIAPeerChildKeepalive.Env()
	// The timing/budget override names are owned by the tunables
	// registry; they are aliased here so the peer's strict parsing and tests
	// keep their package-local names.
	EnvJournalLimit = tunables.EnvPeerJournalLimit
	EnvBackoffMin   = tunables.EnvPeerBackoffMin
	EnvBackoffMax   = tunables.EnvPeerBackoffMax
)

// Defaults for the peer's parsed tunables come from the shared tunables
// registry; the single-source constants are aliased here so the
// peer's own naming and tests keep working. ADR-0007: 4096-event journal,
// 1s–30s reconnect backoff.
const (
	// DefaultJournalLimit is the default bounded journal capacity
	// (ADR-0007: 4096 events, env-tunable via CRITERIA_PEER_JOURNAL_LIMIT).
	DefaultJournalLimit = tunables.DefaultPeerJournalLimit
	// DefaultBackoffMin is the default floor for host reconnect backoff.
	DefaultBackoffMin = tunables.DefaultPeerBackoffMin
	// DefaultBackoffMax is the default ceiling for host reconnect backoff.
	DefaultBackoffMax = tunables.DefaultPeerBackoffMax
	// DefaultVersion is used when neither the manifest nor the environment
	// declares an adapter version (runner parity).
	DefaultVersion = "0.0.0"
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

	// Adapters is the multi-adapter manifest: one spec per adapter
	// child the peer hosts, built from CRITERIA_REMOTE_ADAPTERS /
	// CRITERIA_REMOTE_ADAPTERS_DIR with per-adapter overrides. Empty means
	// the legacy single-adapter shape above.
	Adapters []AdapterSpec
	// ScopesDir is the runner's remote-tokens root (EnvRemoteScopesDir);
	// when set, the peer dials one conn per (scope, adapter) token file it
	// finds there — a scope-SET conn set instead of the single run-wide
	// conn. Empty keeps the legacy single-conn dial.
	ScopesDir string

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
	if err := cfg.parseTunables(getenv); err != nil {
		return Config{}, err
	}
	cfg.ScopesDir = strings.TrimSpace(getenv(EnvRemoteScopesDir))
	// The multi-adapter manifest only replaces the legacy single-adapter
	// shape when it actually declares entries; a malformed declaration is an
	// error, never a silent fall-back.
	adapters, err := ParseAdaptersConfig(getenv)
	if err != nil {
		return Config{}, err
	}
	if len(adapters) > 0 {
		cfg.Adapters = adapters
	}
	return cfg, nil
}

// ManifestMode reports whether the adapter set was declared through the
// multi-adapter manifest (CRITERIA_REMOTE_ADAPTERS / CRITERIA_REMOTE_ADAPTERS_DIR)
// rather than the legacy single-adapter variables.
func (c *Config) ManifestMode() bool { return len(c.Adapters) > 0 }

// MultiChild reports whether the peer hosts more than one adapter child.
func (c *Config) MultiChild() bool { return len(c.Adapters) > 1 }

// ChildKeepAlive parses a CRITERIA_PEER_CHILD_KEEPALIVE value: surrounding
// whitespace is trimmed, unset or empty keeps the default true, and a
// non-boolean value is an error naming the variable. The single parser
// behind both the peer Config tunables path and the engine's container-lane
// consumer, so both accept exactly the same value shape.
func ChildKeepAlive(raw string) (bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return true, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean, got %q", EnvChildKeepAlive, raw)
	}
	return v, nil
}

// parseTunables reads and validates the parsed peer tunables: child
// keepalive, journal limit, and backoff bounds.
func (c *Config) parseTunables(getenv func(string) string) error {
	keepAlive, err := ChildKeepAlive(getenv(EnvChildKeepAlive))
	if err != nil {
		return err
	}
	c.ChildKeepAlive = keepAlive

	c.JournalLimit = DefaultJournalLimit
	if raw := strings.TrimSpace(getenv(EnvJournalLimit)); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil {
			return fmt.Errorf("%s must be a positive integer, got %q", EnvJournalLimit, raw)
		}
		if v <= 0 {
			return fmt.Errorf("%s must be a positive integer, got %d", EnvJournalLimit, v)
		}
		c.JournalLimit = v
	}

	c.BackoffMin, err = parseBackoff(getenv, EnvBackoffMin, DefaultBackoffMin)
	if err != nil {
		return err
	}
	c.BackoffMax, err = parseBackoff(getenv, EnvBackoffMax, DefaultBackoffMax)
	if err != nil {
		return err
	}
	if c.BackoffMax < c.BackoffMin {
		return fmt.Errorf("%s (%s) must be >= %s (%s)", EnvBackoffMax, c.BackoffMax, EnvBackoffMin, c.BackoffMin)
	}
	return nil
}

// parseBackoff parses one CRITERIA_PEER_BACKOFF_* duration, falling back to
// def when unset.
func parseBackoff(getenv func(string) string, name string, def time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(getenv(name))
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration, got %q", name, raw)
	}
	return d, nil
}

// LoadConfigFromEnv reads the peer configuration from the process
// environment.
func LoadConfigFromEnv() (Config, error) {
	return LoadConfig(envreg.Lookup)
}

// Resolve validates the configuration and fills identity and binary defaults.
// Precedence follows the retired phone-home runner's resolve
// (cmd/criteria-adapter-remote-runner, removed in PR #551): manifest →
// CRITERIA_ADAPTER_BINARY → conventional install path → PATH lookup of
// criteria-adapter-<name> → first PATH entry. Multi-adapter manifest mode
// resolves every declared child with the same precedence per adapter
// (ResolveAdapterSpec); the legacy single-adapter fields do not participate
// there — the manifest is the declaration of record for the hosted child set.
func (c *Config) Resolve() error {
	if c.Host == "" {
		return errors.New("CRITERIA_REMOTE_HOST is required")
	}

	tlsCfg, err := adapterhost.LoadClientTLS(c.TLSCertPath, c.TLSKeyPath, c.TLSCAPath)
	if err != nil {
		return err
	}
	c.TLS = tlsCfg

	if c.ManifestMode() {
		return c.resolveManifestChildren()
	}

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

// resolveManifestChildren resolves every manifest child with the per-adapter
// precedence: manifest defaults, binary location, PATH validation,
// and per-child digest pinning. A child that fails to resolve fails the whole
// Resolve — a multi-child peer never half-boots on an undeclared child.
func (c *Config) resolveManifestChildren() error {
	for i := range c.Adapters {
		if err := ResolveAdapterSpec(&c.Adapters[i]); err != nil {
			return err
		}
	}
	return nil
}

// ChildNames returns the names of the declared manifest children, in
// declaration order. Empty in legacy single-adapter mode, where identity is
// carried by AdapterName.
func (c *Config) ChildNames() []string {
	names := make([]string, 0, len(c.Adapters))
	for _, spec := range c.Adapters {
		names = append(names, spec.Name)
	}
	return names
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
		// Binaries install and live on PATH by TYPE (mirroring the
		// adapterhost DiscoverBinary split): a definition-keyed identity
		// ("shell.develop") looks up its binary by the type half
		// while the child stays keyed by the definition identity.
		if p, err := exec.LookPath(adapterhost.AdapterBinaryPrefix + adapterhost.DefinitionType(c.AdapterName)); err == nil {
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
		var notFound *adapterhost.ErrAdapterNotFound
		if errors.As(err, &notFound) {
			// No pinned artifact in the local cache: keep the chain-resolved
			// binary; the digest is still recorded on spawn events.
			return nil
		}
		return fmt.Errorf("resolve %s: %w", EnvRemoteDigest, err)
	}
	c.AdapterBinary = pinned
	return nil
}

// defaultBinaryPath returns the conventional install path for a named adapter
// (runner parity: k8s manifests install adapters at /usr/local/bin). Binaries
// install by TYPE, so a definition identity ("shell.develop") derives
// its conventional path from the type half while the child stays keyed by the
// definition identity (mirroring the adapterhost DiscoverBinary split
// and peer locateSpecBinary).
func defaultBinaryPath(name string) string {
	if name == "" {
		return ""
	}
	return "/usr/local/bin/" + adapterhost.AdapterBinaryPrefix + adapterhost.DefinitionType(name)
}

// nameFromBinary derives an adapter name from a binary path, stripping the
// conventional prefix when present (runner parity).
func nameFromBinary(binary string) string {
	base := filepath.Base(binary)
	if strings.HasPrefix(base, adapterhost.AdapterBinaryPrefix) {
		return strings.TrimPrefix(base, adapterhost.AdapterBinaryPrefix)
	}
	return base
}

// firstAdapterBinary scans PATH in order and returns the first conventional
// adapter binary that is not the retired criteria-adapter-remote-runner
// binary. The exclusion is deliberate: leftover runner binaries from a
// pre-#551 install must not be picked up as adapters on upgrade.
func firstAdapterBinary() string {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), adapterhost.AdapterBinaryPrefix) && !strings.Contains(e.Name(), "remote-runner") {
				return filepath.Join(dir, e.Name())
			}
		}
	}
	return ""
}
