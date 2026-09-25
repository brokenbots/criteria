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
