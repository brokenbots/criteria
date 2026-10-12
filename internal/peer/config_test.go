package peer

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func getenvFrom(m map[string]string) func(string) string {
	return func(name string) string {
		return m[name]
	}
}

func TestLoadConfig_Defaults(t *testing.T) {
	cfg, err := LoadConfig(getenvFrom(map[string]string{}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.ChildKeepAlive {
		t.Error("ChildKeepAlive default = false, want true")
	}
	if cfg.JournalLimit != DefaultJournalLimit {
		t.Errorf("JournalLimit default = %d, want %d", cfg.JournalLimit, DefaultJournalLimit)
	}
	if cfg.BackoffMin != DefaultBackoffMin {
		t.Errorf("BackoffMin default = %s, want %s", cfg.BackoffMin, DefaultBackoffMin)
	}
	if cfg.BackoffMax != DefaultBackoffMax {
		t.Errorf("BackoffMax default = %s, want %s", cfg.BackoffMax, DefaultBackoffMax)
	}
}

func TestLoadConfig_ReadsEnv(t *testing.T) {
	env := map[string]string{
		EnvRemoteHost:      "127.0.0.1:7999",
		EnvRemoteToken:     "tok",
		EnvRemoteScope:     "scope/inst",
		EnvRemoteDigest:    "sha256:abc",
		EnvRemoteTLSCert:   "/tls/cert.pem",
		EnvRemoteTLSKey:    "/tls/key.pem",
		EnvRemoteCA:        "/tls/ca.pem",
		EnvAdapterName:     "noopx",
		EnvAdapterVersion:  "1.2.3",
		EnvAdapterBinary:   "/bin/criteria-adapter-noopx",
		EnvAdapterManifest: "/etc/adapter.yaml",
		EnvLogLevel:        "debug",
		EnvChildKeepAlive:  "false",
		EnvJournalLimit:    "128",
		EnvBackoffMin:      "2s",
		EnvBackoffMax:      "45s",
	}
	cfg, err := LoadConfig(getenvFrom(env))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Host != "127.0.0.1:7999" || cfg.Token != "tok" || cfg.Scope != "scope/inst" {
		t.Errorf("connection fields not read: %+v", cfg)
	}
	if cfg.Digest != "sha256:abc" || cfg.TLSCertPath != "/tls/cert.pem" ||
		cfg.TLSKeyPath != "/tls/key.pem" || cfg.TLSCAPath != "/tls/ca.pem" {
		t.Errorf("tls/digest fields not read: %+v", cfg)
	}
	if cfg.AdapterName != "noopx" || cfg.AdapterVersion != "1.2.3" ||
		cfg.AdapterBinary != "/bin/criteria-adapter-noopx" || cfg.AdapterManifest != "/etc/adapter.yaml" {
		t.Errorf("adapter fields not read: %+v", cfg)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", cfg.LogLevel)
	}
	if cfg.ChildKeepAlive {
		t.Error("ChildKeepAlive = true, want false")
	}
	if cfg.JournalLimit != 128 || cfg.BackoffMin.String() != "2s" || cfg.BackoffMax.String() != "45s" {
		t.Errorf("peer knobs not read: %+v", cfg)
	}
}

func TestLoadConfig_InvalidValues(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"bad keepalive", map[string]string{EnvChildKeepAlive: "maybe"}, "CRITERIA_PEER_CHILD_KEEPALIVE"},
		{"non-numeric journal limit", map[string]string{EnvJournalLimit: "lots"}, "CRITERIA_PEER_JOURNAL_LIMIT"},
		{"zero journal limit", map[string]string{EnvJournalLimit: "0"}, "CRITERIA_PEER_JOURNAL_LIMIT"},
		{"negative journal limit", map[string]string{EnvJournalLimit: "-4"}, "CRITERIA_PEER_JOURNAL_LIMIT"},
		{"bad backoff min", map[string]string{EnvBackoffMin: "fast"}, "CRITERIA_PEER_BACKOFF_MIN"},
		{"bad backoff max", map[string]string{EnvBackoffMax: "0s"}, "CRITERIA_PEER_BACKOFF_MAX"},
		{"max below min", map[string]string{EnvBackoffMin: "5s", EnvBackoffMax: "1s"}, "must be >="},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfig(getenvFrom(tc.env))
			if err == nil {
				t.Fatal("want error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestResolve_RequiresHost(t *testing.T) {
	cfg, err := LoadConfig(getenvFrom(map[string]string{}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	err = cfg.Resolve()
	if err == nil || !strings.Contains(err.Error(), "CRITERIA_REMOTE_HOST is required") {
		t.Fatalf("want CRITERIA_REMOTE_HOST error, got %v", err)
	}
}

func TestResolve_TLSPartialIsErrorAndCompleteLoads(t *testing.T) {
	dir := t.TempDir()
	cfg, err := LoadConfig(getenvFrom(map[string]string{
		EnvRemoteHost:    "127.0.0.1:7999",
		EnvRemoteTLSCert: filepath.Join(dir, "cert.pem"),
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	err = cfg.Resolve()
	if err == nil || !strings.Contains(err.Error(), "mTLS requires") {
		t.Fatalf("want mTLS all-or-nothing error, got %v", err)
	}

	// No TLS paths configured: Resolve leaves TLS nil (plain connection).
	// Pin PATH to a dir holding a placeholder binary so the resolution
	// chain never depends on the host's PATH contents.
	binDir := t.TempDir()
	writeRunnable(t, filepath.Join(binDir, "criteria-adapter-plain"))
	t.Setenv("PATH", binDir)
	plain, err := LoadConfig(getenvFrom(map[string]string{EnvRemoteHost: "127.0.0.1:7999"}))
	if err != nil {
		t.Fatalf("load plain: %v", err)
	}
	if err := plain.Resolve(); err != nil {
		t.Fatalf("resolve plain: %v", err)
	}
	if plain.TLS != nil {
		t.Errorf("TLS = %v, want nil for plain connection", plain.TLS)
	}
}

// writeRunnable writes an executable placeholder file. The peer resolution
// path only stats executables; spawning happens later through the loader.
func writeRunnable(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// resolveFixture builds a Config from env map plus per-case overrides and
// runs Resolve, controlling PATH via t.Setenv.
func resolveFixture(t *testing.T, env map[string]string, path string) (Config, error) {
	t.Helper()
	t.Setenv("PATH", path)
	cfg, err := LoadConfig(getenvFrom(env))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	err = cfg.Resolve()
	return cfg, err
}

// TestResolve_BinaryPrecedenceMatrix covers the runner-parity resolution
// chain: manifest → CRITERIA_ADAPTER_BINARY → PATH criteria-adapter-<name>
// → conventional install path → first PATH entry.
func TestResolve_BinaryPrecedenceMatrix(t *testing.T) {
	dir := t.TempDir()

	writeRunnable(t, filepath.Join(dir, "criteria-adapter-zed"))
	writeRunnable(t, filepath.Join(dir, "criteria-adapter-alpha"))
	writeRunnable(t, filepath.Join(dir, "criteria-adapter-remote-runner")) // excluded from first-match

	manifestPath := filepath.Join(dir, "adapter.yaml")
	manifestYAML := "schema_version: 1\nname: manim\nversion: 0.2.0\nsource_url: https://example.invalid/adapter\nsdk_protocol_version: 2\n"
	if err := os.WriteFile(manifestPath, []byte(manifestYAML), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	cases := []struct {
		name        string
		env         map[string]string
		wantBinary  string
		wantName    string
		wantVersion string
		wantErr     string
	}{
		{
			name:        "env binary with separator wins",
			env:         map[string]string{EnvRemoteHost: "h:1", EnvAdapterBinary: filepath.Join(dir, "criteria-adapter-zed")},
			wantBinary:  filepath.Join(dir, "criteria-adapter-zed"),
			wantName:    "zed",
			wantVersion: DefaultVersion,
		},
		{
			name:        "env binary bare name resolved on PATH",
			env:         map[string]string{EnvRemoteHost: "h:1", EnvAdapterBinary: "criteria-adapter-zed"},
			wantBinary:  filepath.Join(dir, "criteria-adapter-zed"),
			wantName:    "zed",
			wantVersion: DefaultVersion,
		},
		{
			name:        "env version preserved",
			env:         map[string]string{EnvRemoteHost: "h:1", EnvAdapterBinary: filepath.Join(dir, "criteria-adapter-zed"), EnvAdapterVersion: "9.9.9"},
			wantBinary:  filepath.Join(dir, "criteria-adapter-zed"),
			wantName:    "zed",
			wantVersion: "9.9.9",
		},
		{
			name:        "named PATH lookup beats conventional path",
			env:         map[string]string{EnvRemoteHost: "h:1", EnvAdapterName: "zed"},
			wantBinary:  filepath.Join(dir, "criteria-adapter-zed"),
			wantName:    "zed",
			wantVersion: DefaultVersion,
		},
		{
			name:        "first PATH match when name unknown",
			env:         map[string]string{EnvRemoteHost: "h:1"},
			wantBinary:  filepath.Join(dir, "criteria-adapter-alpha"),
			wantName:    "alpha",
			wantVersion: DefaultVersion,
		},
		{
			name:        "manifest fills name and version",
			env:         map[string]string{EnvRemoteHost: "h:1", EnvAdapterManifest: manifestPath},
			wantBinary:  "/usr/local/bin/criteria-adapter-manim",
			wantName:    "manim",
			wantVersion: "0.2.0",
		},
		{
			name:    "missing bare binary on PATH",
			env:     map[string]string{EnvRemoteHost: "h:1", EnvAdapterBinary: "criteria-adapter-missing"},
			wantErr: "not found on PATH",
		},
		{
			name:    "no binary anywhere (unknown name, empty PATH)",
			env:     map[string]string{EnvRemoteHost: "h:1"},
			wantErr: "could not locate an adapter binary",
		},
		{
			name:    "manifest unreadable",
			env:     map[string]string{EnvRemoteHost: "h:1", EnvAdapterManifest: filepath.Join(dir, "absent.yaml")},
			wantErr: "read manifest",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Keep PATH deterministic: an empty PATH dir for the negative
			// cases (LookPath of "criteria-adapter-missing"/"nomatch" must
			// fail) and a dir holding exactly the fixture binaries otherwise.
			emptyDir := t.TempDir()
			lookupPath := dir
			if tc.wantErr != "" {
				lookupPath = emptyDir
			}
			cfg, err := resolveFixture(t, tc.env, lookupPath)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want error containing %q, got %v (cfg=%+v)", tc.wantErr, err, cfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if cfg.Binary() != tc.wantBinary {
				t.Errorf("binary = %q, want %q", cfg.Binary(), tc.wantBinary)
			}
			if cfg.AdapterName != tc.wantName {
				t.Errorf("name = %q, want %q", cfg.AdapterName, tc.wantName)
			}
			if cfg.AdapterVersion != tc.wantVersion {
				t.Errorf("version = %q, want %q", cfg.AdapterVersion, tc.wantVersion)
			}
		})
	}
}

func TestResolve_DigestPrefersPinnedBinary(t *testing.T) {
	dir := t.TempDir()
	digestHex := strings.Repeat("ab", 32)
	enc := "sha256-" + digestHex
	pinnedDir := filepath.Join(dir, "pinned", enc)
	if err := os.MkdirAll(pinnedDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	pinned := filepath.Join(pinnedDir, "criteria-adapter-pinme")
	writeRunnable(t, pinned)

	// CRITERIA_ADAPTERS points the adapterhost discovery roots at the pinned
	// layout so no real CRITERIA_HOME cache is consulted.
	t.Setenv("CRITERIA_ADAPTERS", filepath.Join(dir, "pinned"))
	t.Setenv("PATH", dir) // no flat criteria-adapter-* binaries on PATH

	cfg, err := LoadConfig(getenvFrom(map[string]string{
		EnvRemoteHost:   "h:1",
		EnvAdapterName:  "pinme",
		EnvRemoteDigest: "sha256:" + digestHex,
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := cfg.Resolve(); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cfg.Binary() != pinned {
		t.Errorf("binary = %q, want digest-pinned %q", cfg.Binary(), pinned)
	}

	// Unparseable digest fails closed.
	bad, err := LoadConfig(getenvFrom(map[string]string{
		EnvRemoteHost:   "h:1",
		EnvAdapterName:  "pinme",
		EnvRemoteDigest: "not-a-digest",
	}))
	if err != nil {
		t.Fatalf("load bad digest: %v", err)
	}
	err = bad.Resolve()
	if err == nil || !strings.Contains(err.Error(), "invalid CRITERIA_REMOTE_DIGEST") {
		t.Fatalf("want invalid digest error, got %v", err)
	}

	// Digest set but artifact not cached locally: the chain-resolved binary
	// is kept (no network pull).
	missing, err := LoadConfig(getenvFrom(map[string]string{
		EnvRemoteHost:   "h:1",
		EnvAdapterName:  "pinme",
		EnvRemoteDigest: "sha256:" + strings.Repeat("cd", 32),
	}))
	if err != nil {
		t.Fatalf("load missing digest: %v", err)
	}
	if err := missing.Resolve(); err != nil {
		t.Fatalf("resolve missing digest: %v", err)
	}
	if want := "/usr/local/bin/criteria-adapter-pinme"; missing.Binary() != want {
		t.Errorf("binary = %q, want conventional %q kept", missing.Binary(), want)
	}
}

// TestResolve_DigestDefinitionIdentity is the KB-246 regression for the Gate 3
// smoke signature `resolve CRITERIA_REMOTE_DIGEST: invalid adapter name
// "noop.demo"`: definition-keyed identities (KB-242) must resolve digest pins
// by TYPE from the local OCI cache. A cache hit swaps the pinned binary in,
// a cache miss keeps the chain-resolved binary, and neither errors — both
// shapes boot instead of dying before spawning an adapter child.
func TestResolve_DigestDefinitionIdentity(t *testing.T) {
	dir := t.TempDir()
	digestHex := strings.Repeat("ab", 32)
	enc := "sha256-" + digestHex

	// Cache hit: the local OCI cache holds the type-keyed binary
	// criteria-adapter-noop under the pinned digest directory; the
	// definition identity "noop.demo" must resolve through it.
	cacheDir := filepath.Join(dir, "cache")
	pinnedDir := filepath.Join(cacheDir, enc)
	if err := os.MkdirAll(pinnedDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	pinned := filepath.Join(pinnedDir, "criteria-adapter-noop")
	writeRunnable(t, pinned)
	t.Setenv("CRITERIA_ADAPTERS", cacheDir)
	t.Setenv("PATH", dir)

	hit, err := LoadConfig(getenvFrom(map[string]string{
		EnvRemoteHost:   "h:1",
		EnvAdapterName:  "noop.demo",
		EnvRemoteDigest: "sha256:" + digestHex,
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := hit.Resolve(); err != nil {
		t.Fatalf("resolve definition-identity digest pin: %v", err)
	}
	if hit.Binary() != pinned {
		t.Errorf("cache-hit binary = %q, want digest-pinned %q", hit.Binary(), pinned)
	}

	// Manifest-child shape: the same definition identity as a hosted child
	// of a multi-adapter peer.
	childCfg := Config{
		Host:     "h:1",
		Adapters: []AdapterSpec{{Name: "noop.demo", Digest: "sha256:" + digestHex}},
	}
	if err := childCfg.Resolve(); err != nil {
		t.Fatalf("resolve definition-identity child digest pin: %v", err)
	}
	if childCfg.Adapters[0].Binary != pinned {
		t.Errorf("cache-hit child binary = %q, want digest-pinned %q", childCfg.Adapters[0].Binary, pinned)
	}

	// Cache miss with an explicit chain-resolved binary (the CI smoke shape):
	// Resolve succeeds and keeps the declared binary.
	explicit := filepath.Join(dir, "criteria-adapter-noop")
	writeRunnable(t, explicit)
	miss, err := LoadConfig(getenvFrom(map[string]string{
		EnvRemoteHost:    "h:1",
		EnvAdapterName:   "noop.demo",
		EnvAdapterBinary: explicit,
		EnvRemoteDigest:  "sha256:" + strings.Repeat("cd", 32),
	}))
	if err != nil {
		t.Fatalf("load miss: %v", err)
	}
	if err := miss.Resolve(); err != nil {
		t.Fatalf("resolve cache-miss digest pin: %v", err)
	}
	if miss.Binary() != explicit {
		t.Errorf("cache-miss binary = %q, want explicit %q kept", miss.Binary(), explicit)
	}
}

// TestResolve_DefinitionIdentityBinaryLocation is the KB-246 follow-up
// regression for the single-kind definition-keyed peer boot failure: the
// peer's own binary-location paths (locateSpecBinary for manifest children,
// resolveDefaults for the legacy single-adapter shape) must derive the
// binary NAME from the adapter TYPE half — binaries install by type
// (criteria-adapter-shell) — while the hosted child stays keyed by the
// definition identity ("shell.develop", KB-242). Before the fix the full
// definition identity produced "/usr/local/bin/criteria-adapter-shell.develop"
// and the pod failed boot closed with "missing or not a regular file" even
// though the type-keyed binary was installed.
func TestResolve_DefinitionIdentityBinaryLocation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CRITERIA_ADAPTERS", filepath.Join(dir, "adapters"))
	t.Setenv("CRITERIA_HOME", filepath.Join(dir, "home"))

	// A type-keyed binary staged on PATH, with nothing for the full
	// definition identity: the LookPath arm must find it BY TYPE.
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	typBin := filepath.Join(binDir, "criteria-adapter-shell")
	writeRunnable(t, typBin)
	t.Setenv("PATH", binDir)

	// Manifest-mode child: one definition-keyed declaration with no
	// per-definition _BINARY override (the exposed single-kind fleet shape).
	child, err := LoadConfig(getenvFrom(map[string]string{
		EnvRemoteHost: "h:1",
		EnvAdapters:   "shell.develop",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := child.Resolve(); err != nil {
		t.Fatalf("resolve definition-keyed child without override: %v", err)
	}
	if child.Adapters[0].Binary != typBin {
		t.Errorf("child binary = %q, want type-keyed PATH hit %q", child.Adapters[0].Binary, typBin)
	}
	if child.Adapters[0].Name != "shell.develop" {
		t.Errorf("child name = %q, want the definition identity kept as the child key", child.Adapters[0].Name)
	}

	// Digest-pinned child whose pinned artifact is absent from the local
	// cache: the chain-resolved type-keyed binary is kept and the digest
	// stays recorded for host-side identity (no boot error).
	pinned := Config{
		Host: "h:1",
		Adapters: []AdapterSpec{
			{Name: "shell.develop", Digest: "sha256:" + strings.Repeat("cd", 32)},
		},
	}
	if err := pinned.Resolve(); err != nil {
		t.Fatalf("resolve digest-pinned child without cached artifact: %v", err)
	}
	if pinned.Adapters[0].Binary != typBin {
		t.Errorf("digest-miss child binary = %q, want chain-resolved %q kept", pinned.Adapters[0].Binary, typBin)
	}

	// Legacy single-adapter shape: resolveDefaults' LookPath arm resolves
	// the same type-keyed binary for the definition-keyed name.
	legacy, err := LoadConfig(getenvFrom(map[string]string{
		EnvRemoteHost:  "h:1",
		EnvAdapterName: "shell.develop",
	}))
	if err != nil {
		t.Fatalf("load legacy: %v", err)
	}
	if err := legacy.Resolve(); err != nil {
		t.Fatalf("resolve legacy definition identity: %v", err)
	}
	if legacy.Binary() != typBin {
		t.Errorf("legacy binary = %q, want type-keyed PATH hit %q", legacy.Binary(), typBin)
	}

	// With nothing on PATH, the conventional install derivation must land
	// on the TYPE-keyed path (the legacy shape does not stat the binary).
	empty := filepath.Join(dir, "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Setenv("PATH", empty)
	conv, err := LoadConfig(getenvFrom(map[string]string{
		EnvRemoteHost:  "h:1",
		EnvAdapterName: "shell.develop",
	}))
	if err != nil {
		t.Fatalf("load conventional: %v", err)
	}
	if err := conv.Resolve(); err != nil {
		t.Fatalf("resolve conventional: %v", err)
	}
	if want := "/usr/local/bin/criteria-adapter-shell"; conv.Binary() != want {
		t.Errorf("conventional binary = %q, want %q", conv.Binary(), want)
	}

	// Manifest-mode fail-closed shape (the boot failure the fleet hit): with
	// no type-keyed binary anywhere the child still fails closed, but the
	// error must name the TYPE-keyed conventional path — never the
	// definition-keyed one.
	missing, err := LoadConfig(getenvFrom(map[string]string{
		EnvRemoteHost: "h:1",
		EnvAdapters:   "shell.develop",
	}))
	if err != nil {
		t.Fatalf("load missing: %v", err)
	}
	err = missing.Resolve()
	if err == nil {
		t.Fatal("resolve missing binary: want fail-closed error, got nil")
	}
	// The error shape is only assertable on machines without a type-keyed
	// shell adapter installed at the conventional location; skip the shape
	// checks elsewhere rather than flaking.
	if _, statErr := os.Stat("/usr/local/bin/criteria-adapter-shell"); os.IsNotExist(statErr) {
		if !strings.Contains(err.Error(), `/usr/local/bin/criteria-adapter-shell"`) {
			t.Errorf("err = %v, want the type-keyed conventional path named", err)
		}
		if strings.Contains(err.Error(), "criteria-adapter-shell.develop") {
			t.Errorf("err = %v, want no definition-keyed binary path derived", err)
		}
	}

	// defaultBinaryPath applies the split for every conventional-path
	// derivation: definition identity → type-keyed path; bare name unchanged.
	if got := defaultBinaryPath("shell.develop"); got != "/usr/local/bin/criteria-adapter-shell" {
		t.Errorf("defaultBinaryPath(definition identity) = %q, want type-keyed path", got)
	}
	if got := defaultBinaryPath("shell"); got != "/usr/local/bin/criteria-adapter-shell" {
		t.Errorf("defaultBinaryPath(bare type) = %q, want unchanged conventional path", got)
	}
	if got := defaultBinaryPath(""); got != "" {
		t.Errorf("defaultBinaryPath(empty) = %q, want empty", got)
	}
}

// TestResolve_ManifestChildren pins the multi-adapter manifest Resolve
// contract (KB-213): the legacy single-adapter fields do not participate —
// every declared child resolves with the per-adapter precedence on its own,
// a per-child digest prefers its pinned binary, and a missing child binary
// fails the whole Resolve naming the adapter.
func TestResolve_ManifestChildren(t *testing.T) {
	dir := t.TempDir()
	noopBin := filepath.Join(dir, "criteria-adapter-noop")
	stateBin := filepath.Join(dir, "criteria-adapter-stateful")
	writeRunnable(t, noopBin)
	writeRunnable(t, stateBin)

	cfg := Config{
		Host: "h:1",
		Adapters: []AdapterSpec{
			{Name: "noop"},
			{Name: "stateful", Version: "1.2.3"},
		},
	}
	t.Setenv("PATH", dir)
	if err := cfg.Resolve(); err != nil {
		t.Fatalf("resolve manifest children: %v", err)
	}
	if cfg.Adapters[0].Binary != noopBin || cfg.Adapters[0].Version != DefaultVersion {
		t.Errorf("noop child resolved = %+v, want PATH-located binary and default version", cfg.Adapters[0])
	}
	if cfg.Adapters[1].Binary != stateBin || cfg.Adapters[1].Version != "1.2.3" {
		t.Errorf("stateful child resolved = %+v, want kept binary and version", cfg.Adapters[1])
	}
	// The legacy single-adapter identity stays untouched: the manifest set
	// is the declaration of record for the hosted children.
	if cfg.AdapterName != "" || cfg.Binary() != "" || cfg.Digest != "" {
		t.Errorf("legacy identity fields filled in manifest mode: %q/%q/%q", cfg.AdapterName, cfg.Binary(), cfg.Digest)
	}
	if !cfg.ManifestMode() || !cfg.MultiChild() {
		t.Errorf("mode flags: ManifestMode=%v MultiChild=%v", cfg.ManifestMode(), cfg.MultiChild())
	}

	// Per-child digest pinning prefers the digest-addressed binary from the
	// discovery roots (runner parity with the single-adapter shape).
	digestHex := strings.Repeat("ef", 32)
	enc := "sha256-" + digestHex
	pinnedDir := filepath.Join(dir, "pinned", enc)
	if err := os.MkdirAll(pinnedDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	pinnedState := filepath.Join(pinnedDir, "criteria-adapter-stateful")
	writeRunnable(t, pinnedState)
	t.Setenv("CRITERIA_ADAPTERS", filepath.Join(dir, "pinned"))

	pinned := Config{
		Host: "h:1",
		Adapters: []AdapterSpec{
			{Name: "stateful", Digest: "sha256:" + digestHex},
		},
	}
	if err := pinned.Resolve(); err != nil {
		t.Fatalf("resolve pinned child: %v", err)
	}
	if pinned.Adapters[0].Binary != pinnedState {
		t.Errorf("pinned child binary = %q, want %q", pinned.Adapters[0].Binary, pinnedState)
	}

	if got := pinned.ChildNames(); len(got) != 1 || got[0] != "stateful" {
		t.Errorf("ChildNames = %v, want [stateful]", got)
	}

	// A child whose binary cannot be located anywhere fails the whole
	// Resolve and names the adapter — a multi-child peer never half-boots
	// on an undeclared child.
	missing := Config{
		Host: "h:1",
		Adapters: []AdapterSpec{
			{Name: "noop"},
			{Name: "ghost"},
		},
	}
	err := missing.Resolve()
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("err = %v, want the missing ghost child's resolve failure", err)
	}
}

// TestResolve_ManifestChildrenExplicitBinaries pins the CI-fixture shape:
// every manifest spec carries its own explicit binary path, so Resolve needs
// no PATH lookup and no conventional install path — a manifest-only peer
// declaration resolves even when no criteria-adapter-* binary exists on
// PATH (the deployment shape of Dockerfile.peer).
func TestResolve_ManifestChildrenExplicitBinaries(t *testing.T) {
	dir := t.TempDir()
	noopBin := filepath.Join(dir, "criteria-adapter-noop")
	stateBin := filepath.Join(dir, "criteria-adapter-stateful")
	writeRunnable(t, noopBin)
	writeRunnable(t, stateBin)

	cfg := Config{
		Host: "h:1",
		Adapters: []AdapterSpec{
			{Name: "noop", Binary: noopBin},
			{Name: "stateful", Binary: stateBin},
		},
	}
	t.Setenv("PATH", "/nonexistent-empty-dir")
	if err := cfg.Resolve(); err != nil {
		t.Fatalf("resolve explicit-binary manifest without any PATH binary: %v", err)
	}
	for i, want := range []string{noopBin, stateBin} {
		if cfg.Adapters[i].Binary != want {
			t.Errorf("child %d binary = %q, want explicit %q", i, cfg.Adapters[i].Binary, want)
		}
	}
}

func TestResolve_KeepsNilTLSByDefault(t *testing.T) {
	cfg, err := resolveFixture(t, map[string]string{
		EnvRemoteHost:     "h:1",
		EnvAdapterBinary:  filepath.Join(t.TempDir(), "criteria-adapter-zed"),
		EnvChildKeepAlive: "false",
	}, "/nonexistent-path-dir")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cfg.TLS != nil {
		t.Errorf("TLS = %v, want nil", cfg.TLS)
	}
	if cfg.ChildKeepAlive {
		t.Error("ChildKeepAlive = true, want false")
	}
}

// Compile-time guard: the config must keep its tls.Config plumbing.
var _ = (*tls.Config)(nil)
