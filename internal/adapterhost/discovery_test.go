package adapterhost

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDiscoverBinaryRejectsInvalidAdapterNames(t *testing.T) {
	invalid := []string{"../noop", "nested/noop", "dot.name", "Noop", "name with space"}
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

func TestDiscoverBinaryPrefersEnvOverHome(t *testing.T) {
	envDir := filepath.Join(t.TempDir(), "env")
	homeDir := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(envDir, 0o755); err != nil {
		t.Fatalf("mkdir env: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(homeDir, ".criteria", "plugins"), 0o755); err != nil {
		t.Fatalf("mkdir home plugins: %v", err)
	}
	envPath := filepath.Join(envDir, "criteria-adapter-noop")
	homePath := filepath.Join(homeDir, ".criteria", "plugins", "criteria-adapter-noop")
	writeExecutable(t, envPath)
	writeExecutable(t, homePath)

	t.Setenv("CRITERIA_PLUGINS", envDir)
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
	if err := os.MkdirAll(filepath.Join(homeDir, ".criteria", "plugins"), 0o755); err != nil {
		t.Fatalf("mkdir home plugins: %v", err)
	}
	homePath := filepath.Join(homeDir, ".criteria", "plugins", "criteria-adapter-noop")
	writeExecutable(t, homePath)

	t.Setenv("CRITERIA_PLUGINS", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("HOME", homeDir)

	got, err := DiscoverBinary("noop")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if got != homePath {
		t.Fatalf("path=%q want %q", got, homePath)
	}
}

func TestDiscoverBinaryNotFoundIncludesSearchedPaths(t *testing.T) {
	envDir := filepath.Join(t.TempDir(), "env")
	homeDir := filepath.Join(t.TempDir(), "home")
	t.Setenv("CRITERIA_PLUGINS", envDir)
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
		filepath.Join(homeDir, ".criteria", "plugins", "criteria-adapter-copilot"),
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
