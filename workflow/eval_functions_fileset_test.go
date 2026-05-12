package workflow

// Tests for filesetFunction and resolveConfinedDir.
// This is an internal test (package workflow) so it can invoke the unexported
// filesetFunction and resolveConfinedDir directly, matching the pattern used
// by eval_functions_templatefile_test.go.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
)

// fsOpts returns a FunctionOptions pointing at workflowDir with no MaxBytes
// constraint (fileset doesn't read file content) and no extra allowed paths.
func fsOpts(workflowDir string) FunctionOptions {
	return FunctionOptions{
		WorkflowDir:  workflowDir,
		MaxBytes:     defaultMaxBytes,
		AllowedPaths: nil,
	}
}

// callFileset invokes filesetFunction and returns the []string result or error.
func callFileset(opts FunctionOptions, path, pattern string) ([]string, error) {
	val, err := filesetFunction(opts).Call([]cty.Value{
		cty.StringVal(path),
		cty.StringVal(pattern),
	})
	if err != nil {
		return nil, err
	}
	if val.Type() == cty.List(cty.String) && val.LengthInt() == 0 {
		return nil, nil
	}
	var out []string
	it := val.ElementIterator()
	for it.Next() {
		_, v := it.Element()
		out = append(out, v.AsString())
	}
	return out, nil
}

// writeFile writes content to name inside dir.
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// mkdir creates a subdirectory inside dir.
func mkdir(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", name, err)
	}
}

// Test 1: fileset returns sorted relative paths for files matching the glob.
func TestFileset_HappyPath_GlobMatchesFiles(t *testing.T) {
	dir := t.TempDir()
	mkdir(t, dir, "prompts")
	writeFile(t, filepath.Join(dir, "prompts"), "a.md", "# a")
	writeFile(t, filepath.Join(dir, "prompts"), "b.md", "# b")
	writeFile(t, filepath.Join(dir, "prompts"), "c.txt", "text")

	got, err := callFileset(fsOpts(dir), "prompts", "*.md")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"prompts/a.md", "prompts/b.md"}
	if len(got) != len(want) {
		t.Fatalf("got %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] got %q; want %q", i, got[i], want[i])
		}
	}
}

// Test 2: fileset returns empty list (no error) when no files match.
func TestFileset_NoMatches_ReturnsEmptyList(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.txt", "text")

	got, err := callFileset(fsOpts(dir), ".", "*.md")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v; want empty list", got)
	}
}

// Test 3: fileset(".", "*.md") lists files at the top of WorkflowDir.
func TestFileset_DotPath_ListsTopLevel(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.md", "# a")

	got, err := callFileset(fsOpts(dir), ".", "*.md")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"a.md"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Errorf("got %v; want %v", got, want)
	}
}

// Test 4: fileset does not recurse into subdirectories.
func TestFileset_NestedDirNotRecursed(t *testing.T) {
	dir := t.TempDir()
	mkdir(t, dir, "prompts")
	mkdir(t, dir, filepath.Join("prompts", "sub"))
	writeFile(t, filepath.Join(dir, "prompts"), "a.md", "# a")
	writeFile(t, filepath.Join(dir, "prompts", "sub"), "b.md", "# b")

	got, err := callFileset(fsOpts(dir), "prompts", "*.md")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"prompts/a.md"}
	if len(got) != len(want) {
		t.Fatalf("got %v; want %v (recursion must not happen)", got, want)
	}
	if got[0] != want[0] {
		t.Errorf("got %q; want %q", got[0], want[0])
	}
}

// Test 5: fileset excludes subdirectories from the result when pattern is "*".
func TestFileset_DirectoriesExcluded(t *testing.T) {
	dir := t.TempDir()
	mkdir(t, dir, "prompts")
	mkdir(t, dir, filepath.Join("prompts", "sub"))
	writeFile(t, filepath.Join(dir, "prompts"), "a.md", "# a")

	got, err := callFileset(fsOpts(dir), "prompts", "*")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, p := range got {
		if strings.HasSuffix(p, string(filepath.Separator)+"sub") {
			t.Errorf("directory %q must not appear in fileset output", p)
		}
	}
	want := []string{"prompts/a.md"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Errorf("got %v; want %v", got, want)
	}
}

// Test 6: fileset excludes symlinks to files (v1 behavior: !entry.Type().IsRegular()).
// Symlinks are excluded because DirEntry.Type().IsRegular() is false for symlinks.
// This is documented v1 behavior — users who need symlink following should open a
// follow-up workstream.
func TestFileset_SymlinkToFile_Excluded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require elevated privileges on Windows")
	}
	dir := t.TempDir()
	writeFile(t, dir, "a.md", "# a")
	if err := os.Symlink(filepath.Join(dir, "a.md"), filepath.Join(dir, "link-a.md")); err != nil {
		t.Skipf("symlink creation failed (may need privilege): %v", err)
	}

	got, err := callFileset(fsOpts(dir), ".", "*.md")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Only the regular file should appear; the symlink is excluded.
	want := []string{"a.md"}
	if len(got) != len(want) {
		t.Fatalf("got %v; want %v (symlink must be excluded)", got, want)
	}
	if got[0] != want[0] {
		t.Errorf("got %q; want %q", got[0], want[0])
	}
}

// Test 7: fileset returns paths in lexicographic order regardless of OS order.
func TestFileset_SortedOutput(t *testing.T) {
	dir := t.TempDir()
	// Write in non-lexicographic order to verify sort.
	for _, name := range []string{"c.md", "a.md", "b.md"} {
		writeFile(t, dir, name, "")
	}

	got, err := callFileset(fsOpts(dir), ".", "*.md")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"a.md", "b.md", "c.md"}
	if len(got) != len(want) {
		t.Fatalf("got %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] got %q; want %q", i, got[i], want[i])
		}
	}
}

// Test 8: fileset matches with '?' single-character wildcard.
func TestFileset_QuestionMarkPattern(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a1.txt", "a2.txt", "ab.txt"} {
		writeFile(t, dir, name, "")
	}

	got, err := callFileset(fsOpts(dir), ".", "a?.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// All three match: '?' matches any single non-slash char.
	want := []string{"a1.txt", "a2.txt", "ab.txt"}
	if len(got) != len(want) {
		t.Fatalf("got %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] got %q; want %q", i, got[i], want[i])
		}
	}
}

// Test 9: fileset matches with '[0-9]' character class.
func TestFileset_CharClassPattern(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a1.md", "a2.md", "aB.md"} {
		writeFile(t, dir, name, "")
	}

	got, err := callFileset(fsOpts(dir), ".", "a[0-9].md")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"a1.md", "a2.md"}
	if len(got) != len(want) {
		t.Fatalf("got %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] got %q; want %q", i, got[i], want[i])
		}
	}
}

// Test 10: invalid pattern returns an error containing "fileset()", "invalid pattern", and the pattern.
func TestFileset_InvalidPattern_ReturnsError(t *testing.T) {
	dir := t.TempDir()

	_, err := callFileset(fsOpts(dir), ".", "[")
	if err == nil {
		t.Fatal("expected error for invalid pattern; got none")
	}
	msg := err.Error()
	for _, want := range []string{"fileset()", "invalid pattern", "["} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q should contain %q", msg, want)
		}
	}
}

// Test 11: non-existent path returns an error containing "fileset()" and "does not exist".
func TestFileset_PathDoesNotExist_ReturnsError(t *testing.T) {
	dir := t.TempDir()

	_, err := callFileset(fsOpts(dir), "nonexistent", "*")
	if err == nil {
		t.Fatal("expected error for nonexistent path; got none")
	}
	msg := err.Error()
	for _, want := range []string{"fileset()", "does not exist"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q should contain %q", msg, want)
		}
	}
}

// Test 12: path pointing to a regular file returns an error containing "fileset()" and "is not a directory".
func TestFileset_PathIsFile_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.md", "# a")

	_, err := callFileset(fsOpts(dir), "a.md", "*")
	if err == nil {
		t.Fatal("expected error when path is a file; got none")
	}
	msg := err.Error()
	for _, want := range []string{"fileset()", "is not a directory"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q should contain %q", msg, want)
		}
	}
}

// Test 13: path escaping WorkflowDir returns a confinement error.
func TestFileset_PathEscape_ReturnsError(t *testing.T) {
	dir := t.TempDir()

	_, err := callFileset(fsOpts(dir), "../escape", "*")
	if err == nil {
		t.Fatal("expected confinement error; got none")
	}
	if !strings.Contains(err.Error(), "escapes workflow directory") {
		t.Errorf("error %q should mention 'escapes workflow directory'", err.Error())
	}
}

// Test 14: absolute path is rejected with a descriptive error.
func TestFileset_AbsolutePath_Rejected(t *testing.T) {
	dir := t.TempDir()

	_, err := callFileset(fsOpts(dir), "/etc", "*")
	if err == nil {
		t.Fatal("expected error for absolute path; got none")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("error %q should mention 'absolute'", err.Error())
	}
}

// Test 15: AllowedPaths allows access to a directory outside WorkflowDir.
func TestFileset_AllowedPathsHonored(t *testing.T) {
	parent := t.TempDir()
	wfDir := filepath.Join(parent, "wf")
	allowedDir := filepath.Join(parent, "allowed")
	if err := os.MkdirAll(wfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(allowedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, allowedDir, "x.md", "# x")
	writeFile(t, allowedDir, "y.md", "# y")

	opts := FunctionOptions{
		WorkflowDir:  wfDir,
		MaxBytes:     defaultMaxBytes,
		AllowedPaths: []string{allowedDir},
	}

	// "../allowed" relative to wfDir resolves to allowedDir.
	got, err := callFileset(opts, "../allowed", "*.md")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"../allowed/x.md", "../allowed/y.md"}
	if len(got) != len(want) {
		t.Fatalf("got %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] got %q; want %q", i, got[i], want[i])
		}
	}
}

// Test 16: empty directory returns empty list with no error.
func TestFileset_EmptyDirectory_ReturnsEmptyList(t *testing.T) {
	dir := t.TempDir()
	mkdir(t, dir, "empty")

	got, err := callFileset(fsOpts(dir), "empty", "*")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v; want empty list", got)
	}
}

// Test 17: pattern "*" matches all regular files in the directory.
func TestFileset_MatchesAllWithStar(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a", "b", "c"} {
		writeFile(t, dir, name, "")
	}

	got, err := callFileset(fsOpts(dir), ".", "*")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] got %q; want %q", i, got[i], want[i])
		}
	}
}

// Test 18: permission-denied directory returns an error containing "permission".
// Skipped on Windows where chmod(0) does not deny access to the process owner.
func TestFileset_PermissionDeniedOnDir_ReturnsError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission model differs on Windows")
	}
	dir := t.TempDir()
	mkdir(t, dir, "locked")
	locked := filepath.Join(dir, "locked")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	_, err := callFileset(fsOpts(dir), "locked", "*")
	if err == nil {
		t.Fatal("expected permission error; got none")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "permission") {
		t.Errorf("error %q should mention permission", err.Error())
	}
}

// Test 19: concurrent calls against the same directory produce no data races.
// Run with -race to verify.
func TestFileset_ConcurrentCalls_NoRace(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, name := range []string{"a.md", "b.md", "c.md"} {
		writeFile(t, dir, name, "")
	}
	opts := fsOpts(dir)

	const workers = 50
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			got, err := callFileset(opts, ".", "*.md")
			if err != nil {
				t.Errorf("unexpected error in concurrent call: %v", err)
			}
			if len(got) != 3 {
				t.Errorf("concurrent call: got %d results; want 3", len(got))
			}
		}()
	}
	wg.Wait()
}

// Test 20: E2E — compile a workflow that uses for_each = fileset(...) and
// file(each.value) in the input block. This is the load-bearing integration
// check verifying that fileset() is correctly registered and evaluated during
// the compile-time fold pass.
func TestFileset_PairsWithForEach_E2E(t *testing.T) {
	dir := t.TempDir()
	mkdir(t, dir, "prompts")
	writeFile(t, filepath.Join(dir, "prompts"), "alpha.md", "# alpha prompt")
	writeFile(t, filepath.Join(dir, "prompts"), "beta.md", "# beta prompt")

	hclContent := `
workflow "fileset_e2e" {
  version       = "1"
  initial_state = "process"
  target_state  = "done"
}

adapter "noop" "runner" {}

step "process" {
  target   = adapter.noop.runner
  for_each = fileset("prompts", "*.md")
  input {
    prompt = file(each.value)
  }
  outcome "all_succeeded" { next = "done" }
  outcome "any_failed"    { next = "failed" }
}

state "done" {
  terminal = true
  success  = true
}

state "failed" {
  terminal = true
  success  = false
}
`
	path := filepath.Join(dir, "main.hcl")
	if err := os.WriteFile(path, []byte(hclContent), 0o644); err != nil {
		t.Fatalf("write main.hcl: %v", err)
	}

	spec, diags := Parse(path, []byte(hclContent))
	if diags.HasErrors() {
		t.Fatalf("parse: %s", diags.Error())
	}

	graph, compileDiags := CompileWithOpts(spec, nil, CompileOpts{WorkflowDir: dir})
	if compileDiags.HasErrors() {
		t.Fatalf("compile: %s", compileDiags.Error())
	}

	node, ok := graph.Steps["process"]
	if !ok {
		t.Fatal("compiled graph missing step 'process'")
	}
	if node.ForEach == nil {
		t.Fatal("step 'process': ForEach expression must not be nil")
	}

	// Evaluate the for_each expression to confirm fileset() produced the
	// expected paths.
	opts := DefaultFunctionOptions(dir)
	ctx := BuildEvalContextWithOpts(nil, opts)
	val, diags := node.ForEach.Value(ctx)
	if diags.HasErrors() {
		t.Fatalf("evaluate ForEach: %s", diags.Error())
	}
	if !val.Type().IsListType() {
		t.Fatalf("ForEach value type = %s; want list", val.Type().FriendlyName())
	}

	var paths []string
	it := val.ElementIterator()
	for it.Next() {
		_, v := it.Element()
		paths = append(paths, v.AsString())
	}
	want := []string{"prompts/alpha.md", "prompts/beta.md"}
	if len(paths) != len(want) {
		t.Fatalf("ForEach paths = %v; want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Errorf("[%d] path = %q; want %q", i, paths[i], want[i])
		}
	}

	// Verify the input expressions reference each.value (runtime binding) so
	// fileset() paths drive the per-iteration file() calls.
	inputExpr, hasPrompt := node.InputExprs["prompt"]
	if !hasPrompt {
		t.Fatal("step 'process': input expr 'prompt' not found")
	}
	// Confirm the expression references "each" — this is a runtime binding
	// that the engine resolves per iteration with each matched path.
	vars := inputExpr.Variables()
	foundEach := false
	for _, v := range vars {
		if len(v) > 0 {
			if root, ok := v[0].(hcl.TraverseRoot); ok && root.Name == "each" {
				foundEach = true
				break
			}
		}
	}
	if !foundEach {
		t.Error("input 'prompt' expression should reference 'each' (runtime binding)")
	}
}
