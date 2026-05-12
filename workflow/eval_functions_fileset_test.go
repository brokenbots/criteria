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
// file(each.value) in the input block. This test exercises the full evaluation
// stack: fileset() list production, WithEachBinding wiring each.value to each
// path, and file(each.value) loading actual file content per iteration.
//
// A regression in any of:
//   - fileset() sorted list production
//   - each.value binding via WithEachBinding
//   - file(each.value) content resolution
//
// causes this test to fail. It would NOT pass if the engine delivered wrong
// paths, silently skipped file loading, or bound the wrong value per iteration.
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
	hclPath := filepath.Join(dir, "main.hcl")
	if err := os.WriteFile(hclPath, []byte(hclContent), 0o644); err != nil {
		t.Fatalf("write main.hcl: %v", err)
	}

	spec, diags := Parse(hclPath, []byte(hclContent))
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

	// Evaluate for_each to get the fileset() paths.
	fnOpts := DefaultFunctionOptions(dir)
	evalCtx := BuildEvalContextWithOpts(nil, fnOpts)
	listVal, diags := node.ForEach.Value(evalCtx)
	if diags.HasErrors() {
		t.Fatalf("evaluate ForEach: %s", diags.Error())
	}
	if !listVal.Type().IsListType() {
		t.Fatalf("ForEach value type = %s; want list", listVal.Type().FriendlyName())
	}

	var iterPaths []string
	it := listVal.ElementIterator()
	for it.Next() {
		_, v := it.Element()
		iterPaths = append(iterPaths, v.AsString())
	}
	wantPaths := []string{"prompts/alpha.md", "prompts/beta.md"}
	if len(iterPaths) != len(wantPaths) {
		t.Fatalf("ForEach paths = %v; want %v", iterPaths, wantPaths)
	}
	for i := range wantPaths {
		if iterPaths[i] != wantPaths[i] {
			t.Errorf("[%d] path = %q; want %q", i, iterPaths[i], wantPaths[i])
		}
	}

	// For each path, bind each.value and evaluate file(each.value).
	// This proves that per-iteration input resolution delivers actual file
	// contents — the same evaluation the engine performs before dispatching
	// to the adapter.
	wantContents := map[string]string{
		"prompts/alpha.md": "# alpha prompt",
		"prompts/beta.md":  "# beta prompt",
	}
	total := len(iterPaths)
	for i, iterPath := range iterPaths {
		binding := &EachBinding{
			Value: cty.StringVal(iterPath),
			Index: i,
			Total: total,
			First: i == 0,
			Last:  i == total-1,
		}
		vars := WithEachBinding(nil, binding)
		resolved, err := ResolveInputExprsWithOpts(node.InputExprs, vars, fnOpts)
		if err != nil {
			t.Fatalf("iter %d (%s): ResolveInputExprs: %v", i, iterPath, err)
		}
		got, hasPrompt := resolved["prompt"]
		if !hasPrompt {
			t.Fatalf("iter %d (%s): input 'prompt' missing from resolved map", i, iterPath)
		}
		if want := wantContents[iterPath]; got != want {
			t.Errorf("iter %d (%s): prompt = %q; want %q", i, iterPath, got, want)
		}
	}
}

// Tests for resolveConfinedDir branches not covered by the fileset surface tests.

// TestResolveConfinedDir_SymlinkEscapesAfterResolution verifies the
// post-EvalSymlinks confinement check: a symlink inside WorkflowDir that
// resolves to a path outside WorkflowDir (and not in AllowedPaths) must be
// rejected.
func TestResolveConfinedDir_SymlinkEscapesAfterResolution(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevated privileges on Windows")
	}
	parent := t.TempDir()
	wfDir := filepath.Join(parent, "wf")
	outsideDir := filepath.Join(parent, "outside")
	if err := os.MkdirAll(wfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Symlink inside WorkflowDir that points to outside — passes pre-check but
	// must fail the post-EvalSymlinks confinement check.
	if err := os.Symlink(outsideDir, filepath.Join(wfDir, "escaped")); err != nil {
		t.Fatal(err)
	}

	_, err := resolveConfinedDir("escaped", wfDir, nil)
	if err == nil {
		t.Fatal("expected confinement error for symlink escaping WorkflowDir; got none")
	}
	if !strings.Contains(err.Error(), "escapes") {
		t.Errorf("error %q should mention 'escapes'", err.Error())
	}
}

// TestResolveConfinedDir_PermissionDeniedInEvalSymlinks verifies the
// os.IsPermission branch in EvalSymlinks error handling: a directory whose
// parent has mode 0o000 cannot be traversed, so EvalSymlinks returns a
// permission error.
func TestResolveConfinedDir_PermissionDeniedInEvalSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission model differs on Windows")
	}
	dir := t.TempDir()
	restricted := filepath.Join(dir, "restricted")
	inner := filepath.Join(restricted, "inner")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(restricted, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(restricted, 0o755) })

	_, err := resolveConfinedDir("restricted/inner", dir, nil)
	if err == nil {
		t.Fatal("expected permission error from EvalSymlinks; got none")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "permission") {
		t.Errorf("error %q should mention 'permission'", err.Error())
	}
}

// TestResolveConfinedDir_NonDirComponentInPath verifies the generic EvalSymlinks
// error branch: when an intermediate path component is a regular file (not a
// directory), EvalSymlinks returns ENOTDIR — neither IsNotExist nor IsPermission —
// which falls to the generic error return.
func TestResolveConfinedDir_NonDirComponentInPath(t *testing.T) {
	dir := t.TempDir()
	// "blob" is a regular file, so "blob/subdir" has a non-directory component.
	writeFile(t, dir, "blob", "data")

	_, err := resolveConfinedDir("blob/subdir", dir, nil)
	if err == nil {
		t.Fatal("expected error for non-dir component in path; got none")
	}
	if !strings.Contains(err.Error(), "fileset()") {
		t.Errorf("error %q should mention 'fileset()'", err.Error())
	}
}
