package dirs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHome_Default(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("CRITERIA_HOME", "")
	t.Setenv("CRITERIA_STATE_DIR", "")

	got, err := Home()
	if err != nil {
		t.Fatalf("Home() error = %v", err)
	}
	want := filepath.Join(tmp, defaultRoot)
	if got != want {
		t.Fatalf("Home() = %q, want %q", got, want)
	}
}

func TestHome_CRITERIA_HOME(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("CRITERIA_HOME", "/explicit/home")
	t.Setenv("CRITERIA_STATE_DIR", "")

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

func TestHome_LegacyCriteriaSurvives(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("CRITERIA_HOME", "")
	t.Setenv("CRITERIA_STATE_DIR", "")

	legacy := filepath.Join(tmp, legacyRootName)
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatalf("mkdir legacy: %v", err)
	}

	got, err := Home()
	if err != nil {
		t.Fatalf("Home() error = %v", err)
	}
	if got != legacy {
		t.Fatalf("Home() = %q, want %q", got, legacy)
	}
}

func TestHome_CRITERIA_HOMESkipsLegacy(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("CRITERIA_HOME", filepath.Join(tmp, "explicit"))
	if err := os.MkdirAll(filepath.Join(tmp, legacyRootName), 0o755); err != nil {
		t.Fatalf("mkdir legacy: %v", err)
	}

	got, err := Home()
	if err != nil {
		t.Fatalf("Home() error = %v", err)
	}
	want := filepath.Join(tmp, "explicit")
	if got != want {
		t.Fatalf("Home() = %q, want %q", got, want)
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

func TestAdaptersDir_Default(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("CRITERIA_ADAPTERS", "")
	t.Setenv("CRITERIA_HOME", "")

	got, err := AdaptersDir()
	if err != nil {
		t.Fatalf("AdaptersDir() error = %v", err)
	}
	want := filepath.Join(tmp, defaultRoot, adaptersSubdir)
	if got != want {
		t.Fatalf("AdaptersDir() = %q, want %q", got, want)
	}
}
