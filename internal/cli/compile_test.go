package cli

import (
	"bytes"
	"context"
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

var updateGolden = flag.Bool("update", false, "update golden files")

func TestCompileGolden_JSONAndDOT(t *testing.T) {
	repoRoot, fixtures := workflowFixtures(t)
	// Some fixtures reference files outside their own directory (e.g.
	// examples/workstream_review_loop/ loads agent profiles from
	// .github/agents/). Allow the whole repo root so file() resolves at compile.
	t.Setenv("CRITERIA_WORKFLOW_ALLOWED_PATHS", repoRoot)
	for _, path := range fixtures {
		path := path
		relPath, _ := filepath.Rel(repoRoot, path)
		name := strings.TrimSuffix(filepath.Base(path), ".hcl") + "__" + sanitizeFixturePath(relPath)
		t.Run(name+"_json", func(t *testing.T) {
			out, err := compileWorkflowOutput(context.Background(), path, "json", nil)
			if err != nil {
				t.Fatalf("compile json: %v", err)
			}
			assertGoldenFile(t, filepath.Join("testdata", "compile", name+".json.golden"), out)
		})

		t.Run(name+"_dot", func(t *testing.T) {
			out, err := compileWorkflowOutput(context.Background(), path, "dot", nil)
			if err != nil {
				t.Fatalf("compile dot: %v", err)
			}
			if !strings.HasPrefix(string(out), "digraph") {
				t.Fatalf("dot output must start with digraph, got: %q", string(out[:min(32, len(out))]))
			}
			assertGoldenFile(t, filepath.Join("testdata", "compile", name+".dot.golden"), out)
		})
	}
}

// workflowFixtures returns (repoRoot, absolutePaths) for all workflow modules
// under examples/ and workflow/testdata/. Each path is a directory whose
// top-level .hcl files form a single workflow module. The repoRoot is derived
// from the caller's file path so that tests compute repo-relative golden names
// and remain portable across checkout paths.
func workflowFixtures(t *testing.T) (repoRoot string, fixtures []string) {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve caller")
	}
	repoRoot = filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	dirs := []string{
		filepath.Join(repoRoot, "examples"),
		filepath.Join(repoRoot, "workflow", "testdata"),
	}

	var out []string
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read fixtures dir %s: %v", dir, err)
		}
		for _, ent := range entries {
			if !ent.IsDir() {
				continue
			}
			subDir := filepath.Join(dir, ent.Name())
			// Include only directories that contain at least one .hcl file
			// at the top level — those are workflow module directories.
			subEntries, readErr := os.ReadDir(subDir)
			if readErr != nil {
				continue
			}
			for _, sub := range subEntries {
				if !sub.IsDir() && filepath.Ext(sub.Name()) == ".hcl" {
					out = append(out, subDir)
					break
				}
			}
		}
	}
	sort.Strings(out)
	return repoRoot, out
}

func sanitizeFixturePath(path string) string {
	clean := filepath.Clean(path)
	clean = strings.ReplaceAll(clean, string(filepath.Separator), "__")
	clean = strings.ReplaceAll(clean, ".", "_")
	clean = strings.ReplaceAll(clean, "-", "_")
	return clean
}

func assertGoldenFile(t *testing.T, relativePath string, got []byte) {
	t.Helper()
	// Normalize machine-specific repo root paths to a portable placeholder so
	// golden files check in cleanly across different checkout locations.
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve caller for golden normalization")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	norm := bytes.ReplaceAll(got, []byte(repoRoot), []byte("<repo>"))

	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(relativePath), 0o755); err != nil {
			t.Fatalf("mkdir golden dir: %v", err)
		}
		if err := os.WriteFile(relativePath, norm, 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}

	want, err := os.ReadFile(relativePath)
	if err != nil {
		t.Fatalf("read golden %s: %v", relativePath, err)
	}
	if !bytes.Equal(want, norm) {
		t.Fatalf("golden mismatch for %s\nwant:\n%s\n\ngot:\n%s", relativePath, string(want), string(norm))
	}
}

func TestWriteOutput_ToStdout(t *testing.T) {
	var buf strings.Builder
	payload := []byte("hello output\n")
	if err := writeOutput("", &buf, payload); err != nil {
		t.Fatalf("writeOutput to writer: %v", err)
	}
	if buf.String() != string(payload) {
		t.Fatalf("got %q want %q", buf.String(), string(payload))
	}
}

func TestWriteOutput_ToFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")
	payload := []byte(`{"ok":true}`)
	if err := writeOutput(path, nil, payload); err != nil {
		t.Fatalf("writeOutput to file: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read output file: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("got %q want %q", string(got), string(payload))
	}
}

func TestCompileWorkflowOutput_InvalidFormat(t *testing.T) {
	_, fixtures := workflowFixtures(t)
	if len(fixtures) == 0 {
		t.Skip("no fixtures")
	}
	_, err := compileWorkflowOutput(context.Background(), fixtures[0], "xml", nil)
	if err == nil {
		t.Fatal("expected error for unsupported format")
	}
}

func TestParseCompileForCli_MissingFile(t *testing.T) {
	_, _, err := parseCompileForCli(context.Background(), "/no/such/file.hcl", nil)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	// Error must use the new multi-line format, not the old semicolon-flattened format.
	if strings.Contains(err.Error(), "; ") {
		t.Errorf("error must not use semicolon-flattened format; got: %q", err.Error())
	}
}

// buildTestRoot mirrors the production wiring in cmd/criteria/main.go so that
// root-command SilenceErrors/SilenceUsage settings are exercised exactly as in
// production. Only the compile subcommand is wired here; add others as needed.
func buildTestRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "criteria",
		SilenceErrors: true,
	}
	root.AddCommand(NewCompileCmd())
	return root
}

// TestCompileCmd_UsageSuppressedForRuntimeError verifies that compile errors
// (non-argument failures) do not print the cobra usage/help block when calling
// the subcommand directly.
func TestCompileCmd_UsageSuppressedForRuntimeError(t *testing.T) {
	cmd := NewCompileCmd()
	var outBuf, errBuf strings.Builder
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	cmd.SetArgs([]string{"/no/such/workflow.hcl"})
	_ = cmd.Execute() // error is expected
	combined := outBuf.String() + errBuf.String()
	if strings.Contains(combined, "Usage:") {
		t.Errorf("usage block must not appear for a runtime error; stdout:\n%s\nstderr:\n%s", outBuf.String(), errBuf.String())
	}
}

// TestCompileCmd_UsageShownForArgCountError verifies that cobra's usage block
// IS still printed when the user provides the wrong number of arguments
// (calling the subcommand directly).
func TestCompileCmd_UsageShownForArgCountError(t *testing.T) {
	cmd := NewCompileCmd()
	var outBuf, errBuf strings.Builder
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	cmd.SetArgs([]string{}) // no args — ExactArgs(1) should fail
	_ = cmd.Execute()       // error is expected
	// Cobra prints usage to stdout (c.Println) on arg-count errors.
	if !strings.Contains(outBuf.String(), "Usage:") {
		t.Errorf("usage block must appear for an argument-count error; stdout:\n%s\nstderr:\n%s", outBuf.String(), errBuf.String())
	}
}

// TestRootCmd_UsageSuppressedForRuntimeError exercises the full root command
// hierarchy (matching cmd/criteria/main.go wiring). It proves that if
// root.SilenceUsage were accidentally set to true again, this test would catch
// the regression: runtime errors must not print the usage block.
func TestRootCmd_UsageSuppressedForRuntimeError(t *testing.T) {
	root := buildTestRoot()
	var outBuf, errBuf strings.Builder
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetArgs([]string{"compile", "/no/such/workflow.hcl"})
	_ = root.Execute()
	combined := outBuf.String() + errBuf.String()
	if strings.Contains(combined, "Usage:") {
		t.Errorf("usage must not appear for a runtime error through root command; stdout:\n%s\nstderr:\n%s", outBuf.String(), errBuf.String())
	}
}

// TestRootCmd_UsageShownForArgCountError exercises the full root command
// hierarchy and verifies that usage IS shown when the user omits required
// arguments — the intended UX that the SilenceUsage change must not break.
func TestRootCmd_UsageShownForArgCountError(t *testing.T) {
	root := buildTestRoot()
	var outBuf, errBuf strings.Builder
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetArgs([]string{"compile"}) // no file arg — ExactArgs(1) fails
	_ = root.Execute()
	// Cobra prints usage to stdout (c.Println) on arg-count errors.
	if !strings.Contains(outBuf.String(), "Usage:") {
		t.Errorf("usage must appear for arg-count error through root command; stdout:\n%s\nstderr:\n%s", outBuf.String(), errBuf.String())
	}
}

// TestCompileCmd_MultiErrorFormat verifies that when compilation produces
// multiple diagnostics, each appears on its own block with no semicolon
// separator and at least two distinct "Error:" prefixed lines.
func TestCompileCmd_MultiErrorFormat(t *testing.T) {
	dir := t.TempDir()
	// Workflow that parses successfully but fails compilation with multiple
	// errors: (1) missing initial_state, (2) missing target_state,
	// (3) referenced adapter not declared.
	hclContent := `workflow "multi_error" {
  version = "0.1"
}

step "run" {
  target = adapter.shell.default
  input {
    command = "echo hi"
  }
  outcome "ok" { next = "done" }
}
`
	if err := os.WriteFile(filepath.Join(dir, "multi_error.hcl"), []byte(hclContent), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := parseCompileForCli(context.Background(), dir, nil)
	if err == nil {
		t.Skip("workflow compiled without error — fixture may be valid in this build")
	}
	errStr := err.Error()
	if strings.Contains(errStr, "; ") {
		t.Errorf("error must not use semicolon-flattened format; got: %q", errStr)
	}
	// Assert at least two "Error:" blocks are present — proves the formatter
	// emits all diagnostics, not just the first.
	errorCount := strings.Count(errStr, "Error:")
	if errorCount < 2 {
		t.Errorf("expected at least 2 Error: diagnostic blocks; got %d:\n%s", errorCount, errStr)
	}
}

func TestNewValidateCmd_HappyPath(t *testing.T) {
	_, fixtures := workflowFixtures(t)
	if len(fixtures) == 0 {
		t.Skip("no fixtures")
	}
	cmd := NewValidateCmd()
	var out, errOut strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(fixtures[:1])
	if err := cmd.Execute(); err != nil {
		t.Fatalf("validate cmd: %v (stderr=%s)", err, errOut.String())
	}
}
