package workflow_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/workflow"
)

// testdataDir holds the path to workflow/testdata/eval_functions.
var testdataDir = filepath.Join("testdata", "eval_functions")

// evalExpr is a test helper that compiles a single HCL expression string and
// evaluates it against the given FunctionOptions with empty vars.
func evalExpr(t *testing.T, expr string, opts workflow.FunctionOptions) (cty.Value, hcl.Diagnostics) {
	t.Helper()
	parsed, diags := hclsyntax.ParseExpression([]byte(expr), "test.hcl", hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		t.Fatalf("parse expression %q: %s", expr, diags.Error())
	}
	ctx := workflow.BuildEvalContextWithOpts(nil, opts)
	return parsed.Value(ctx)
}

// opts returns a FunctionOptions with the given workflowDir and default
// MaxBytes / no allowed paths.
func opts(workflowDir string) workflow.FunctionOptions {
	return workflow.FunctionOptions{
		WorkflowDir:  workflowDir,
		MaxBytes:     1 * 1024 * 1024,
		AllowedPaths: nil,
	}
}

// Test 1: file() reads an existing UTF-8 file correctly.
func TestFileFunction_ReadFile(t *testing.T) {
	val, diags := evalExpr(t, `file("hello.txt")`, opts(testdataDir))
	if diags.HasErrors() {
		t.Fatalf("unexpected error: %s", diags.Error())
	}
	if got := val.AsString(); got != "hello\n" {
		t.Errorf("file() = %q; want %q", got, "hello\n")
	}
}

// Test 2: file() errors when path escapes workflow directory.
func TestFileFunction_PathEscape(t *testing.T) {
	_, diags := evalExpr(t, `file("../../etc/passwd")`, opts(testdataDir))
	if !diags.HasErrors() {
		t.Fatal("expected error for path escape; got none")
	}
	if !strings.Contains(diags.Error(), "escapes workflow directory") {
		t.Errorf("error message %q should mention 'escapes workflow directory'", diags.Error())
	}
}

// Test 3: file() errors on missing file.
func TestFileFunction_MissingFile(t *testing.T) {
	_, diags := evalExpr(t, `file("does_not_exist.txt")`, opts(testdataDir))
	if !diags.HasErrors() {
		t.Fatal("expected error for missing file; got none")
	}
	if !strings.Contains(diags.Error(), "no such file") {
		t.Errorf("error message %q should mention 'no such file'", diags.Error())
	}
}

// Test 4: file() errors on invalid UTF-8 content.
func TestFileFunction_InvalidUTF8(t *testing.T) {
	_, diags := evalExpr(t, `file("invalid_utf8.bin")`, opts(testdataDir))
	if !diags.HasErrors() {
		t.Fatal("expected error for invalid UTF-8; got none")
	}
	if !strings.Contains(diags.Error(), "invalid UTF-8") {
		t.Errorf("error message %q should mention 'invalid UTF-8'", diags.Error())
	}
}

// Test 5: file() errors when the file exceeds MaxBytes.
func TestFileFunction_TooBig(t *testing.T) {
	dir := t.TempDir()
	bigPath := filepath.Join(dir, "big.txt")
	// Write 2 MiB of 'a' bytes.
	data := make([]byte, 2*1024*1024)
	for i := range data {
		data[i] = 'a'
	}
	if err := os.WriteFile(bigPath, data, 0o644); err != nil {
		t.Fatalf("write big.txt: %v", err)
	}
	// MaxBytes is 1 MiB; the 2 MiB file should be rejected.
	_, diags := evalExpr(t, `file("big.txt")`, opts(dir))
	if !diags.HasErrors() {
		t.Fatal("expected error for oversized file; got none")
	}
	if !strings.Contains(diags.Error(), "max is") {
		t.Errorf("error message %q should mention 'max is'", diags.Error())
	}
}

// Test 6: file() errors when workflow directory is empty.
func TestFileFunction_NoWorkflowDir(t *testing.T) {
	_, diags := evalExpr(t, `file("hello.txt")`, opts(""))
	if !diags.HasErrors() {
		t.Fatal("expected error when WorkflowDir is empty; got none")
	}
	if !strings.Contains(diags.Error(), "workflow directory not configured") {
		t.Errorf("error message %q should mention 'workflow directory not configured'", diags.Error())
	}
}

// Test 7: fileexists() returns true for an existing file.
func TestFileExistsFunction_Exists(t *testing.T) {
	val, diags := evalExpr(t, `fileexists("hello.txt")`, opts(testdataDir))
	if diags.HasErrors() {
		t.Fatalf("unexpected error: %s", diags.Error())
	}
	if !val.True() {
		t.Error("fileexists() = false; want true")
	}
}

// Test 8: fileexists() returns false for a missing file.
func TestFileExistsFunction_Missing(t *testing.T) {
	val, diags := evalExpr(t, `fileexists("no_such_file.txt")`, opts(testdataDir))
	if diags.HasErrors() {
		t.Fatalf("unexpected error: %s", diags.Error())
	}
	if val.True() {
		t.Error("fileexists() = true; want false for missing file")
	}
}

// Test 9: fileexists() returns false for a directory path.
func TestFileExistsFunction_Directory(t *testing.T) {
	val, diags := evalExpr(t, `fileexists("subdir")`, opts(testdataDir))
	if diags.HasErrors() {
		t.Fatalf("unexpected error: %s", diags.Error())
	}
	if val.True() {
		t.Error("fileexists() = true; want false for directory")
	}
}

// Test 10: trimfrontmatter() strips a valid YAML frontmatter block.
func TestTrimFrontmatterFunction_Strips(t *testing.T) {
	val, diags := evalExpr(t, `trimfrontmatter("---\nfoo: 1\n---\nbody\n")`, opts(""))
	if diags.HasErrors() {
		t.Fatalf("unexpected error: %s", diags.Error())
	}
	if got := val.AsString(); got != "body\n" {
		t.Errorf("trimfrontmatter() = %q; want %q", got, "body\n")
	}
}

// Test 11: trimfrontmatter() returns the input unchanged when no frontmatter.
func TestTrimFrontmatterFunction_NoFrontmatter(t *testing.T) {
	input := "just some text\n"
	val, diags := evalExpr(t, `trimfrontmatter("just some text\n")`, opts(""))
	if diags.HasErrors() {
		t.Fatalf("unexpected error: %s", diags.Error())
	}
	if got := val.AsString(); got != input {
		t.Errorf("trimfrontmatter() = %q; want %q", got, input)
	}
}

// Test 12: file() + trimfrontmatter() composition reads file then strips frontmatter.
func TestFileAndTrimFrontmatterComposition(t *testing.T) {
	dir := t.TempDir()
	content := "---\ntitle: test\n---\nhello from body\n"
	if err := os.WriteFile(filepath.Join(dir, "prompt.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write prompt.md: %v", err)
	}
	val, diags := evalExpr(t, `trimfrontmatter(file("prompt.md"))`, opts(dir))
	if diags.HasErrors() {
		t.Fatalf("unexpected error: %s", diags.Error())
	}
	if got := val.AsString(); got != "hello from body\n" {
		t.Errorf("composition = %q; want %q", got, "hello from body\n")
	}
}

// Test 13: file() is accessible from the AllowedPaths when outside WorkflowDir.
func TestFileFunction_AllowedPath(t *testing.T) {
	// Create a parent dir with two children: workflowDir and sharedDir.
	parent := t.TempDir()
	workflowDir := filepath.Join(parent, "workflow")
	sharedDir := filepath.Join(parent, "shared")
	if err := os.MkdirAll(workflowDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sharedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sharedDir, "extra.txt"), []byte("allowed\n"), 0o644); err != nil {
		t.Fatalf("write extra.txt: %v", err)
	}
	o := workflow.FunctionOptions{
		WorkflowDir:  workflowDir,
		MaxBytes:     1 * 1024 * 1024,
		AllowedPaths: []string{sharedDir},
	}
	// Use a relative path that traverses up into the shared dir.
	val, diags := evalExpr(t, `file("../shared/extra.txt")`, o)
	if diags.HasErrors() {
		t.Fatalf("unexpected error: %s", diags.Error())
	}
	if got := val.AsString(); got != "allowed\n" {
		t.Errorf("file() via AllowedPaths = %q; want %q", got, "allowed\n")
	}
}

// Test 14 (R1): file() size cap is raised when CRITERIA_FILE_FUNC_MAX_BYTES is set.
func TestFileFunction_MaxBytesEnvOverride(t *testing.T) {
	dir := t.TempDir()
	bigPath := filepath.Join(dir, "big.txt")
	// Write 2 MiB of 'a' bytes.
	data := make([]byte, 2*1024*1024)
	for i := range data {
		data[i] = 'a'
	}
	if err := os.WriteFile(bigPath, data, 0o644); err != nil {
		t.Fatalf("write big.txt: %v", err)
	}

	// Without env override, 2 MiB is rejected.
	_, diags := evalExpr(t, `file("big.txt")`, workflow.DefaultFunctionOptions(dir))
	if !diags.HasErrors() {
		t.Fatal("expected size cap error without env override; got none")
	}

	// With CRITERIA_FILE_FUNC_MAX_BYTES=4194304 (4 MiB), 2 MiB is accepted.
	t.Setenv("CRITERIA_FILE_FUNC_MAX_BYTES", "4194304")
	val, diags := evalExpr(t, `file("big.txt")`, workflow.DefaultFunctionOptions(dir))
	if diags.HasErrors() {
		t.Fatalf("unexpected error with raised cap: %s", diags.Error())
	}
	if got := val.AsString(); len(got) != 2*1024*1024 {
		t.Errorf("file() with raised cap: got %d bytes; want %d", len(got), 2*1024*1024)
	}
}

// Test 15 (R2): trimfrontmatter() returns input unchanged when closing --- is beyond 64 KiB.
func TestTrimFrontmatterFunction_NoCloseWithin64KiB(t *testing.T) {
	// Build a string that starts with "---\n" but has 100 KiB of content
	// without a closing "\n---\n" within the first 64 KiB.
	body := strings.Repeat("x", 100*1024)
	input := "---\n" + body
	// Construct the expression inline would be huge; evaluate through Go directly.
	dir := t.TempDir()
	path := filepath.Join(dir, "nofm.txt")
	if err := os.WriteFile(path, []byte(input), 0o644); err != nil {
		t.Fatalf("write nofm.txt: %v", err)
	}
	val, diags := evalExpr(t, `trimfrontmatter(file("nofm.txt"))`,
		workflow.FunctionOptions{WorkflowDir: dir, MaxBytes: 200 * 1024})
	if diags.HasErrors() {
		t.Fatalf("unexpected error: %s", diags.Error())
	}
	if got := val.AsString(); got != input {
		t.Errorf("trimfrontmatter() should return input unchanged when no close within 64 KiB; got len=%d want len=%d", len(got), len(input))
	}
}

// Test 16 (R3): file() rejects a symlink that escapes the workflow directory.
func TestFileFunction_SymlinkEscape(t *testing.T) {
	parent := t.TempDir()
	workflowDir := filepath.Join(parent, "workflow")
	if err := os.MkdirAll(workflowDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Create a target file outside workflowDir.
	outsideFile := filepath.Join(parent, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("secret\n"), 0o644); err != nil {
		t.Fatalf("write secret.txt: %v", err)
	}
	// Create a symlink inside workflowDir pointing to the outside file.
	symlinkPath := filepath.Join(workflowDir, "link.txt")
	if err := os.Symlink(outsideFile, symlinkPath); err != nil {
		t.Skipf("os.Symlink not supported: %v", err)
	}
	_, diags := evalExpr(t, `file("link.txt")`, opts(workflowDir))
	if !diags.HasErrors() {
		t.Fatal("expected confinement error for symlink escape; got none")
	}
	if !strings.Contains(diags.Error(), "escapes workflow directory") {
		t.Errorf("error %q should mention 'escapes workflow directory'", diags.Error())
	}
}

// Test 17 (R4): CRITERIA_WORKFLOW_ALLOWED_PATHS env var is read by DefaultFunctionOptions.
func TestFileFunction_AllowedPathsEnvVar(t *testing.T) {
	parent := t.TempDir()
	workflowDir := filepath.Join(parent, "workflow")
	sharedDir := filepath.Join(parent, "shared")
	if err := os.MkdirAll(workflowDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sharedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sharedDir, "extra.txt"), []byte("allowed\n"), 0o644); err != nil {
		t.Fatalf("write extra.txt: %v", err)
	}
	t.Setenv("CRITERIA_WORKFLOW_ALLOWED_PATHS", sharedDir)
	val, diags := evalExpr(t, `file("../shared/extra.txt")`, workflow.DefaultFunctionOptions(workflowDir))
	if diags.HasErrors() {
		t.Fatalf("unexpected error: %s", diags.Error())
	}
	if got := val.AsString(); got != "allowed\n" {
		t.Errorf("file() via env AllowedPaths = %q; want %q", got, "allowed\n")
	}
}

// Test 18 (R7+R6): fileexists() confinement error says "fileexists()" not "file()".
func TestFileExistsFunction_PathEscape(t *testing.T) {
	_, diags := evalExpr(t, `fileexists("../../etc/passwd")`, opts(testdataDir))
	if !diags.HasErrors() {
		t.Fatal("expected confinement error; got none")
	}
	msg := diags.Error()
	if !strings.Contains(msg, "fileexists()") {
		t.Errorf("error %q should mention 'fileexists()'", msg)
	}
	if strings.Contains(msg, "file():") {
		t.Errorf("error %q should NOT say 'file():' for a fileexists() call", msg)
	}
	if !strings.Contains(msg, "escapes workflow directory") {
		t.Errorf("error %q should mention 'escapes workflow directory'", msg)
	}
}

// Test 19 (R8): file() rejects absolute paths with a clear error.
func TestFileFunction_AbsolutePath(t *testing.T) {
	_, diags := evalExpr(t, `file("/etc/passwd")`, opts(testdataDir))
	if !diags.HasErrors() {
		t.Fatal("expected error for absolute path; got none")
	}
	if !strings.Contains(diags.Error(), "absolute paths are not supported") {
		t.Errorf("error %q should mention 'absolute paths are not supported'", diags.Error())
	}
}
