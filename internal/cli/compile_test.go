package cli

import (
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
	for _, path := range fixtures {
		path := path
		relPath, _ := filepath.Rel(repoRoot, path)
		name := strings.TrimSuffix(filepath.Base(path), ".hcl") + "__" + sanitizeFixturePath(relPath)
		t.Run(name+"_json", func(t *testing.T) {
			out, err := compileWorkflowOutput(context.Background(), path, "json")
			if err != nil {
				t.Fatalf("compile json: %v", err)
			}
			assertGoldenFile(t, filepath.Join("testdata", "compile", name+".json.golden"), out)
		})

		t.Run(name+"_dot", func(t *testing.T) {
			out, err := compileWorkflowOutput(context.Background(), path, "dot")
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

// workflowFixtures returns (repoRoot, absoluteHCLPaths) for all .hcl files in
// examples/ and workflow/testdata/.  The repoRoot is the canonical repo root
// derived from the caller's file path, so that tests can compute repo-relative
// names for golden files and remain portable across checkout paths.
func workflowFixtures(t *testing.T) (string, []string) {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve caller")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
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
			if ent.IsDir() || filepath.Ext(ent.Name()) != ".hcl" {
				continue
			}
			out = append(out, filepath.Join(dir, ent.Name()))
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
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(relativePath), 0o755); err != nil {
			t.Fatalf("mkdir golden dir: %v", err)
		}
		if err := os.WriteFile(relativePath, got, 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}

	want, err := os.ReadFile(relativePath)
	if err != nil {
		t.Fatalf("read golden %s: %v", relativePath, err)
	}
	if string(want) != string(got) {
		t.Fatalf("golden mismatch for %s\nwant:\n%s\n\ngot:\n%s", relativePath, string(want), string(got))
	}
}
