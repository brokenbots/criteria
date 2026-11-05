package dirs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHome_Default(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("CRITERIA_HOME", "")
	t.Setenv("CRITERIA_STATE_DIR", "")
	t.Setenv("XDG_STATE_HOME", "")

	got, err := Home()
	if err != nil {
		t.Fatalf("Home() error = %v", err)
	}
	want := filepath.Join(tmp, defaultStateRel)
	if got != want {
		t.Fatalf("Home() = %q, want %q", got, want)
	}
}

func TestHome_XDG_STATE_HOME(t *testing.T) {
	tmp := t.TempDir()
	xdg := filepath.Join(tmp, "xdg-state")
	t.Setenv("HOME", tmp)
	t.Setenv("CRITERIA_HOME", "")
	t.Setenv("CRITERIA_STATE_DIR", "")
	t.Setenv("XDG_STATE_HOME", xdg)

	got, err := Home()
	if err != nil {
		t.Fatalf("Home() error = %v", err)
	}
	want := filepath.Join(xdg, xdgAppDir)
	if got != want {
		t.Fatalf("Home() = %q, want %q", got, want)
	}
}

func TestHome_XDG_STATE_HOME_NonAbsoluteIgnored(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("CRITERIA_HOME", "")
	t.Setenv("CRITERIA_STATE_DIR", "")
	t.Setenv("XDG_STATE_HOME", "relative/state")

	got, err := Home()
	if err != nil {
		t.Fatalf("Home() error = %v", err)
	}
	want := filepath.Join(tmp, defaultStateRel)
	if got != want {
		t.Fatalf("Home() = %q, want %q (non-absolute XDG values must be ignored)", got, want)
	}
}

func TestHome_CRITERIA_HOME(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("CRITERIA_HOME", "/explicit/home")
	t.Setenv("CRITERIA_STATE_DIR", "")
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "xdg-state"))

	got, err := Home()
	if err != nil {
		t.Fatalf("Home() error = %v", err)
	}
	if got != "/explicit/home" {
		t.Fatalf("Home() = %q, want /explicit/home", got)
	}
}

func TestHome_CRITERIA_STATE_DIR(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("CRITERIA_HOME", "")
	t.Setenv("CRITERIA_STATE_DIR", "/legacy/state")

	got, err := Home()
	if err != nil {
		t.Fatalf("Home() error = %v", err)
	}
	if got != "/legacy/state" {
		t.Fatalf("Home() = %q, want /legacy/state", got)
	}
}

func TestHome_CRITERIA_HOME_WinsOverStateDir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("CRITERIA_HOME", "/new/home")
	t.Setenv("CRITERIA_STATE_DIR", "/legacy/state")

	got, err := Home()
	if err != nil {
		t.Fatalf("Home() error = %v", err)
	}
	if got != "/new/home" {
		t.Fatalf("Home() = %q, want /new/home", got)
	}
}

// TestHome_NoLegacyCriteriaAdoption pins the fail-closed default: a
// pre-existing (or attacker-created) ~/.criteria directory is never adopted
// as the criteria root, so it can never silently become the trust-anchor
// location.
func TestHome_NoLegacyCriteriaAdoption(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("CRITERIA_HOME", "")
	t.Setenv("CRITERIA_STATE_DIR", "")
	t.Setenv("XDG_STATE_HOME", "")

	legacy := filepath.Join(tmp, ".criteria")
	if err := os.MkdirAll(filepath.Join(legacy, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir legacy dir: %v", err)
	}

	got, err := Home()
	if err != nil {
		t.Fatalf("Home() error = %v", err)
	}
	want := filepath.Join(tmp, defaultStateRel)
	if got != want {
		t.Fatalf("Home() = %q, want %q (legacy ~/.criteria must not be adopted)", got, want)
	}
}

func TestConfigPath_Default(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("CRITERIA_HOME", "")
	t.Setenv("CRITERIA_STATE_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")

	got, err := ConfigPath()
	if err != nil {
		t.Fatalf("ConfigPath() error = %v", err)
	}
	want := filepath.Join(tmp, defaultConfigRel, "config.hcl")
	if got != want {
		t.Fatalf("ConfigPath() = %q, want %q", got, want)
	}

	trust, err := TrustConfigPath()
	if err != nil {
		t.Fatalf("TrustConfigPath() error = %v", err)
	}
	wantTrust := filepath.Join(tmp, defaultConfigRel, "trust.hcl")
	if trust != wantTrust {
		t.Fatalf("TrustConfigPath() = %q, want %q", trust, wantTrust)
	}
}

func TestConfigPath_XDG_CONFIG_HOME(t *testing.T) {
	tmp := t.TempDir()
	xdg := filepath.Join(tmp, "xdg-config")
	t.Setenv("HOME", tmp)
	t.Setenv("CRITERIA_HOME", "")
	t.Setenv("CRITERIA_STATE_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", xdg)

	got, err := ConfigPath()
	if err != nil {
		t.Fatalf("ConfigPath() error = %v", err)
	}
	want := filepath.Join(xdg, xdgAppDir, "config.hcl")
	if got != want {
		t.Fatalf("ConfigPath() = %q, want %q", got, want)
	}
}

func TestConfigPath_AppRootWinsOverXDG(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("CRITERIA_HOME", "/explicit/home")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "xdg-config"))

	got, err := ConfigPath()
	if err != nil {
		t.Fatalf("ConfigPath() error = %v", err)
	}
	if got != "/explicit/home/config.hcl" {
		t.Fatalf("ConfigPath() = %q, want /explicit/home/config.hcl", got)
	}
}

func TestConfigPath_CRITERIA_STATE_DIRAlias(t *testing.T) {
	t.Setenv("CRITERIA_HOME", "")
	t.Setenv("CRITERIA_STATE_DIR", "/legacy/state")

	got, err := ConfigPath()
	if err != nil {
		t.Fatalf("ConfigPath() error = %v", err)
	}
	if got != "/legacy/state/config.hcl" {
		t.Fatalf("ConfigPath() = %q, want /legacy/state/config.hcl", got)
	}
}

func TestConfigPath_XDG_CONFIG_HOME_NonAbsoluteIgnored(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("CRITERIA_HOME", "")
	t.Setenv("CRITERIA_STATE_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "relative/config")

	got, err := ConfigPath()
	if err != nil {
		t.Fatalf("ConfigPath() error = %v", err)
	}
	want := filepath.Join(tmp, defaultConfigRel, "config.hcl")
	if got != want {
		t.Fatalf("ConfigPath() = %q, want %q", got, want)
	}
}

func TestSecretsDir_Default(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("CRITERIA_HOME", "")
	t.Setenv("CRITERIA_STATE_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")

	got, err := SecretsDir()
	if err != nil {
		t.Fatalf("SecretsDir() error = %v", err)
	}
	want := filepath.Join(tmp, defaultConfigRel, secretsSubdir)
	if got != want {
		t.Fatalf("SecretsDir() = %q, want %q", got, want)
	}
}

func TestSecretsDir_XDG_CONFIG_HOME(t *testing.T) {
	tmp := t.TempDir()
	xdg := filepath.Join(tmp, "xdg-config")
	t.Setenv("HOME", tmp)
	t.Setenv("CRITERIA_HOME", "")
	t.Setenv("CRITERIA_STATE_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", xdg)

	got, err := SecretsDir()
	if err != nil {
		t.Fatalf("SecretsDir() error = %v", err)
	}
	want := filepath.Join(xdg, xdgAppDir, secretsSubdir)
	if got != want {
		t.Fatalf("SecretsDir() = %q, want %q", got, want)
	}
}

func TestSecretsDir_NonAbsoluteXDGIgnored(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("CRITERIA_HOME", "")
	t.Setenv("CRITERIA_STATE_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "relative/config")

	got, err := SecretsDir()
	if err != nil {
		t.Fatalf("SecretsDir() error = %v", err)
	}
	want := filepath.Join(tmp, defaultConfigRel, secretsSubdir)
	if got != want {
		t.Fatalf("SecretsDir() = %q, want %q (non-absolute XDG values must be ignored)", got, want)
	}
}

func TestSecretsDir_AppRootKeepsSingleTree(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("CRITERIA_HOME", "/explicit/home")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "xdg-config"))

	got, err := SecretsDir()
	if err != nil {
		t.Fatalf("SecretsDir() error = %v", err)
	}
	want := "/explicit/home/" + secretsSubdir
	if got != want {
		t.Fatalf("SecretsDir() = %q, want %q", got, want)
	}
}

func TestCacheOCI_Default(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("CRITERIA_HOME", "")
	t.Setenv("CRITERIA_STATE_DIR", "")
	t.Setenv("XDG_CACHE_HOME", "")

	got, err := CacheOCI()
	if err != nil {
		t.Fatalf("CacheOCI() error = %v", err)
	}
	want := filepath.Join(tmp, defaultCacheRel, "cache", "oci")
	if got != want {
		t.Fatalf("CacheOCI() = %q, want %q", got, want)
	}
}

func TestCacheOCI_XDG_CACHE_HOME(t *testing.T) {
	tmp := t.TempDir()
	xdg := filepath.Join(tmp, "xdg-cache")
	t.Setenv("HOME", tmp)
	t.Setenv("CRITERIA_HOME", "")
	t.Setenv("CRITERIA_STATE_DIR", "")
	t.Setenv("XDG_CACHE_HOME", xdg)

	got, err := CacheOCI()
	if err != nil {
		t.Fatalf("CacheOCI() error = %v", err)
	}
	want := filepath.Join(xdg, xdgAppDir, "cache", "oci")
	if got != want {
		t.Fatalf("CacheOCI() = %q, want %q", got, want)
	}
}

func TestCache_AppRootKeepsSingleTree(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("CRITERIA_HOME", "/explicit/home")
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmp, "xdg-cache"))

	resolvers := []struct {
		name string
		fn   func() (string, error)
	}{
		{"CacheOCI", CacheOCI},
		{"CacheWorkflows", CacheWorkflows},
		{"CacheSigstore", CacheSigstore},
	}
	for _, r := range resolvers {
		got, err := r.fn()
		if err != nil {
			t.Fatalf("%s() error = %v", r.name, err)
		}
		if !strings.HasPrefix(got, "/explicit/home"+string(filepath.Separator)) {
			t.Fatalf("%s() = %q, want it rooted at /explicit/home", r.name, got)
		}
	}
}

func TestAdaptersDir_CRITERIA_ADAPTERS(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("CRITERIA_ADAPTERS", "/explicit/adapters")

	got, err := AdaptersDir()
	if err != nil {
		t.Fatalf("AdaptersDir() error = %v", err)
	}
	if got != "/explicit/adapters" {
		t.Fatalf("AdaptersDir() = %q, want /explicit/adapters", got)
	}
}

func TestAdaptersDir_Default_AppRoot(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("CRITERIA_ADAPTERS", "")
	t.Setenv("CRITERIA_HOME", "")
	t.Setenv("CRITERIA_STATE_DIR", "/legacy/state")

	got, err := AdaptersDir()
	if err != nil {
		t.Fatalf("AdaptersDir() error = %v", err)
	}
	want := "/legacy/state/" + adaptersSubdir
	if got != want {
		t.Fatalf("AdaptersDir() = %q, want %q", got, want)
	}
}

// TestAdaptersDir_Default_XDGMode pins the XDG-mode adapter default to the
// well-known install.sh location so adapters installed without
// CRITERIA_ADAPTERS stay discoverable after the XDG split.
func TestAdaptersDir_Default_XDGMode(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("CRITERIA_ADAPTERS", "")
	t.Setenv("CRITERIA_HOME", "")
	t.Setenv("CRITERIA_STATE_DIR", "")

	got, err := AdaptersDir()
	if err != nil {
		t.Fatalf("AdaptersDir() error = %v", err)
	}
	want := filepath.Join(tmp, DefaultAdaptersRootName)
	if got != want {
		t.Fatalf("AdaptersDir() = %q, want %q", got, want)
	}
}
