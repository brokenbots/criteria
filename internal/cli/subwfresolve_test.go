package cli_test

// subwfresolve_test.go — integration tests for LocalSubWorkflowResolver, exercised
// from the CLI package boundary to ensure the resolver is accessible and correct
// when wired through the CLI compile path.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brokenbots/criteria/workflow"
)

// TestLocalResolver_LocalRelative verifies that a relative source path resolves
// against the caller directory to an absolute path.
func TestLocalResolver_LocalRelative(t *testing.T) {
	tmpDir := t.TempDir()
	swDir := filepath.Join(tmpDir, "inner")
	if err := os.Mkdir(swDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(swDir, "main.hcl"), []byte("// content"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	r := &workflow.LocalSubWorkflowResolver{}
	resolved, err := r.ResolveSource(context.Background(), tmpDir, "./inner")
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if !filepath.IsAbs(resolved) {
		t.Errorf("expected absolute path, got: %q", resolved)
	}
	expected, _ := filepath.Abs(swDir)
	if resolved != expected {
		t.Errorf("expected %q, got %q", expected, resolved)
	}
}

// TestLocalResolver_LocalAbsolute verifies that an absolute source path is used directly.
func TestLocalResolver_LocalAbsolute(t *testing.T) {
	tmpDir := t.TempDir()
	swDir := filepath.Join(tmpDir, "inner")
	if err := os.Mkdir(swDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(swDir, "main.hcl"), []byte("// content"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	r := &workflow.LocalSubWorkflowResolver{}
	// callerDir is irrelevant when source is absolute.
	resolved, err := r.ResolveSource(context.Background(), "/irrelevant", swDir)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	expected, _ := filepath.Abs(swDir)
	if resolved != expected {
		t.Errorf("expected %q, got %q", expected, resolved)
	}
}

// TestLocalResolver_RemoteScheme_Error verifies that remote-scheme sources produce
// a descriptive error pointing to Phase 4.
func TestLocalResolver_RemoteScheme_Error(t *testing.T) {
	remotes := []string{
		"https://github.com/org/repo",
		"git://example.com/repo",
		"s3://bucket/path",
	}
	r := &workflow.LocalSubWorkflowResolver{}
	for _, remote := range remotes {
		_, err := r.ResolveSource(context.Background(), "/caller", remote)
		if err == nil {
			t.Errorf("expected error for %q, got none", remote)
			continue
		}
		if !strings.Contains(err.Error(), "Phase 4") && !strings.Contains(err.Error(), "not supported") {
			t.Errorf("error for %q should reference Phase 4 or 'not supported', got: %v", remote, err)
		}
	}
}

// TestLocalResolver_AllowedRootsRestriction verifies that AllowedRoots restricts
// resolution to paths under allowed roots only.
func TestLocalResolver_AllowedRootsRestriction(t *testing.T) {
	allowed := t.TempDir()
	notAllowed := t.TempDir()

	// Create a valid subworkflow directory under `allowed`.
	swDir := filepath.Join(allowed, "inner")
	if err := os.Mkdir(swDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(swDir, "main.hcl"), []byte("// content"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Create a valid subworkflow directory under `notAllowed`.
	forbiddenDir := filepath.Join(notAllowed, "inner")
	if err := os.Mkdir(forbiddenDir, 0o755); err != nil {
		t.Fatalf("mkdir forbidden: %v", err)
	}
	if err := os.WriteFile(filepath.Join(forbiddenDir, "main.hcl"), []byte("// content"), 0o644); err != nil {
		t.Fatalf("write forbidden: %v", err)
	}

	r := &workflow.LocalSubWorkflowResolver{AllowedRoots: []string{allowed}}

	// Resolving a path under `allowed` should succeed.
	if _, err := r.ResolveSource(context.Background(), allowed, "./inner"); err != nil {
		t.Errorf("expected success for path under allowed root, got: %v", err)
	}

	// Resolving a path under `notAllowed` should fail.
	_, err := r.ResolveSource(context.Background(), notAllowed, "./inner")
	if err == nil {
		t.Fatal("expected error for path outside allowed root, got none")
	}
	if !strings.Contains(err.Error(), "allowed root") {
		t.Errorf("error should mention 'allowed root', got: %v", err)
	}
}

// TestLocalResolver_NotADirectory_Error verifies that pointing source at a file
// (not a directory) produces an error.
func TestLocalResolver_NotADirectory_Error(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "workflow.hcl")
	if err := os.WriteFile(filePath, []byte("// content"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	r := &workflow.LocalSubWorkflowResolver{}
	_, err := r.ResolveSource(context.Background(), tmpDir, "./workflow.hcl")
	if err == nil {
		t.Fatal("expected error: source is a file not a directory, got none")
	}
	if !strings.Contains(err.Error(), "directory") {
		t.Errorf("error should mention 'directory', got: %v", err)
	}
}
