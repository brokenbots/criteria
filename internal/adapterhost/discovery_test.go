package adapterhost

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// fixtureDefinitionName is a definition identity (KB-242) used across the
// KB-246 grammar regression tests: type "noop", definition "demo".
const fixtureDefinitionName = "noop.demo"

func TestDiscoverBinaryRejectsInvalidAdapterNames(t *testing.T) {
	invalid := []string{
		"../noop", "nested/noop", "Noop", "name with space",
		// Definition identities are legal now, but only with exactly one dot
		// and both halves valid.
		".demo", "noop.", "noop..demo", "noop.demo.extra", "..",
		"Noop.demo", "noop.Demo",
	}
	for _, name := range invalid {
		name := name
		t.Run(name, func(t *testing.T) {
			_, err := DiscoverBinary(name)
			if !errors.Is(err, ErrInvalidAdapterName) {
				t.Fatalf("err=%v want ErrInvalidAdapterName", err)
			}
		})
	}
}

// TestIsValidAdapterNameDefinitionGrammar pins the KB-246 grammar: the
// definition form "<type>.<name>" (KB-242's child key) is valid alongside the
// legacy bare names, exactly one dot, both halves non-empty and valid.
func TestIsValidAdapterNameDefinitionGrammar(t *testing.T) {
	for _, tc := range []struct {
		name  string
		valid bool
	}{
		{"noop", true},
		{"noop-1_2", true},
		{"noop.demo", true},
		{"noop.default", true},
		{"fail.breaker", true},
		{"mcp.default", true},
		{"", false},
		{".", false},
		{".demo", false},      // empty type half
		{"noop.", false},      // empty name half
		{"noop..demo", false}, // empty middle
		{"a.b.c", false},      // more than one dot
		{"Noop.demo", false},
		{"noop.Demo", false},
		{"noop/demo", false},
	} {
		if got := IsValidAdapterName(tc.name); got != tc.valid {
			t.Errorf("IsValidAdapterName(%q) = %v, want %v", tc.name, got, tc.valid)
		}
	}
}

// TestDefinitionType pins the exported type-half split the peer's own
// binary-location paths share with adapterhost discovery (KB-246): bare
// names pass through, definition identities keep only the type half —
// binaries install and are listed by type. Callers legitimately pass only
// IsValidAdapterName-accepted names, so multi-dot names never reach here.
func TestDefinitionType(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"noop", "noop"},
		{"noop.demo", "noop"},
		{"copilot.branch_repair", "copilot"},
		{"claude_x.y_z", "claude_x"},
	} {
		if got := DefinitionType(tc.name); got != tc.want {
			t.Errorf("DefinitionType(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestDiscoverBinaryDefinitionIdentityResolvesByType is the KB-246 regression,
// flat-root half: a definition identity ("noop.demo") discovers the type-keyed
// binary "criteria-adapter-noop" instead of failing with ErrInvalidAdapterName.
func TestDiscoverBinaryDefinitionIdentityResolvesByType(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CRITERIA_HOME", "")
	t.Setenv("CRITERIA_ADAPTERS", root)
	typePath := filepath.Join(root, "criteria-adapter-noop")
	writeExecutable(t, typePath)

	got, err := DiscoverBinary(fixtureDefinitionName)
	if err != nil {
		t.Fatalf("discover definition identity: %v", err)
	}
	if got != typePath {
		t.Fatalf("path=%q want %q", got, typePath)
	}
}

// TestDiscoverBinaryNotFoundNamesDefinitionIdentity checks that a cache miss
// for a definition identity reports *ErrAdapterNotFound (which callers may
// treat as non-fatal) with the child key intact, not ErrInvalidAdapterName.
func TestDiscoverBinaryNotFoundNamesDefinitionIdentity(t *testing.T) {
	t.Setenv("CRITERIA_HOME", "")
	t.Setenv("CRITERIA_ADAPTERS", t.TempDir())

	_, err := DiscoverBinary(fixtureDefinitionName)
	var notFound *ErrAdapterNotFound
	if !errors.As(err, &notFound) {
		t.Fatalf("err=%T (%v), want *ErrAdapterNotFound", err, err)
	}
	if notFound.Name != fixtureDefinitionName {
		t.Fatalf("name=%q want %q", notFound.Name, fixtureDefinitionName)
	}
}

func TestDiscoverBinaryPrefersEnvOverHome(t *testing.T) {
	envDir := filepath.Join(t.TempDir(), "env")
	homeDir := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(envDir, 0o755); err != nil {
		t.Fatalf("mkdir env: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(homeDir, ".local", "criteria", "adapters"), 0o755); err != nil {
		t.Fatalf("mkdir home plugins: %v", err)
	}
	envPath := filepath.Join(envDir, "criteria-adapter-noop")
	homePath := filepath.Join(homeDir, ".local", "criteria", "adapters", "criteria-adapter-noop")
	writeExecutable(t, envPath)
	writeExecutable(t, homePath)

	t.Setenv("CRITERIA_HOME", "")
	t.Setenv("CRITERIA_ADAPTERS", envDir)
	t.Setenv("HOME", homeDir)

	got, err := DiscoverBinary("noop")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if got != envPath {
		t.Fatalf("path=%q want %q", got, envPath)
	}
}

func TestDiscoverBinaryFallsBackToHome(t *testing.T) {
	homeDir := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(filepath.Join(homeDir, ".local", "criteria", "adapters"), 0o755); err != nil {
		t.Fatalf("mkdir home plugins: %v", err)
	}
	homePath := filepath.Join(homeDir, ".local", "criteria", "adapters", "criteria-adapter-noop")
	writeExecutable(t, homePath)

	t.Setenv("CRITERIA_HOME", "")
	t.Setenv("CRITERIA_ADAPTERS", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("HOME", homeDir)

	got, err := DiscoverBinary("noop")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if got != homePath {
		t.Fatalf("path=%q want %q", got, homePath)
	}
}

func TestDiscoverBinaryLegacyCriteriaHomeSurvives(t *testing.T) {
	homeDir := filepath.Join(t.TempDir(), "home")
	legacyDir := filepath.Join(homeDir, ".criteria", "adapters")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatalf("mkdir legacy adapters: %v", err)
	}
	legacyPath := filepath.Join(legacyDir, "criteria-adapter-noop")
	writeExecutable(t, legacyPath)

	t.Setenv("CRITERIA_HOME", "")
	t.Setenv("CRITERIA_STATE_DIR", "")
	t.Setenv("CRITERIA_ADAPTERS", "")
	t.Setenv("HOME", homeDir)

	got, err := DiscoverBinary("noop")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if got != legacyPath {
		t.Fatalf("path=%q want %q", got, legacyPath)
	}
}

func TestDiscoverBinaryNotFoundIncludesSearchedPaths(t *testing.T) {
	envDir := filepath.Join(t.TempDir(), "env")
	homeDir := filepath.Join(t.TempDir(), "home")
	t.Setenv("CRITERIA_HOME", "")
	t.Setenv("CRITERIA_ADAPTERS", envDir)
	t.Setenv("HOME", homeDir)

	_, err := DiscoverBinary("copilot")
	if err == nil {
		t.Fatal("expected error")
	}

	var notFound *ErrAdapterNotFound
	if !errors.As(err, &notFound) {
		t.Fatalf("error type=%T; want *ErrAdapterNotFound", err)
	}
	want := []string{
		filepath.Join(envDir, "criteria-adapter-copilot"),
		filepath.Join(homeDir, ".local", "criteria", "adapters", "criteria-adapter-copilot"),
	}
	if !reflect.DeepEqual(notFound.Searched, want) {
		t.Fatalf("searched=%v want=%v", notFound.Searched, want)
	}
}

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write executable %s: %v", path, err)
	}
}
