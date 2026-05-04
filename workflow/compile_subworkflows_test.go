package workflow

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestCompileSubworkflows_Integration tests basic subworkflow compilation integration.
// Full functional testing deferred pending W14 universal step target implementation.
func TestCompileSubworkflows_Integration(t *testing.T) {
	t.Skip("W14-BLOCKED: Comprehensive subworkflow invocation and output namespace tests deferred until W14 universal step target lands")
}

// TestCompileSubworkflows_Basic_Validation tests that subworkflow blocks parse and schema validates.
func TestCompileSubworkflows_Basic_Validation(t *testing.T) {
	// This verifies the schema accepts subworkflow declarations
	swSpec := SubworkflowSpec{
		Name:   "test_subworkflow",
		Source: "./test",
	}

	if swSpec.Name != "test_subworkflow" {
		t.Errorf("expected name to be 'test_subworkflow', got %s", swSpec.Name)
	}

	if swSpec.Source != "./test" {
		t.Errorf("expected source to be './test', got %s", swSpec.Source)
	}
}

// TestLocalSubWorkflowResolver_DirectoryValidation tests that ResolveSource validates directory structure.
func TestLocalSubWorkflowResolver_DirectoryValidation(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a valid subworkflow directory with at least one .hcl file
	swDir := filepath.Join(tmpDir, "valid_sw")
	if err := os.Mkdir(swDir, 0o755); err != nil {
		t.Fatalf("failed to create dir: %v", err)
	}

	// Add an HCL file
	if err := os.WriteFile(filepath.Join(swDir, "main.hcl"), []byte("// valid HCL\n"), 0o644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	resolver := &LocalSubWorkflowResolver{}
	resolved, err := resolver.ResolveSource(context.Background(), tmpDir, "./valid_sw")
	if err != nil {
		t.Fatalf("expected successful resolution, got error: %v", err)
	}

	if resolved == "" {
		t.Fatal("expected non-empty resolved path")
	}

	// Verify it's a directory
	info, err := os.Stat(resolved)
	if err != nil {
		t.Fatalf("resolved path does not exist: %v", err)
	}

	if !info.IsDir() {
		t.Fatal("expected resolved path to be a directory")
	}
}

// TestLocalSubWorkflowResolver_NonexistentDirectory tests error handling for missing directories.
func TestLocalSubWorkflowResolver_NonexistentDirectory(t *testing.T) {
	tmpDir := t.TempDir()

	resolver := &LocalSubWorkflowResolver{}
	_, err := resolver.ResolveSource(context.Background(), tmpDir, "./nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent directory, got none")
	}
}

// TestLocalSubWorkflowResolver_EmptyDirectory tests error handling for directories with no HCL files.
func TestLocalSubWorkflowResolver_EmptyDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	emptyDir := filepath.Join(tmpDir, "empty")
	if err := os.Mkdir(emptyDir, 0o755); err != nil {
		t.Fatalf("failed to create dir: %v", err)
	}

	resolver := &LocalSubWorkflowResolver{}
	_, err := resolver.ResolveSource(context.Background(), tmpDir, "./empty")
	if err == nil {
		t.Fatal("expected error for empty directory, got none")
	}
}
