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
