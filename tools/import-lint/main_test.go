package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// tempRepoWith creates a temporary directory tree from a map of
// repo-relative path → file content.
func tempRepoWith(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, src := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// --- violation cases ---

const internalImportsSDKTop = `package foo
import _ "github.com/brokenbots/overseer/sdk"
`

const internalImportsSDKOther = `package foo
import _ "github.com/brokenbots/overseer/sdk/somepkg"
`

const internalImportsSDKPb = `package foo
import _ "github.com/brokenbots/overseer/sdk/pb/overseer/v1"
`

const workflowImportsInternal = `package foo
import _ "github.com/brokenbots/overseer/internal/engine"
`

const workflowImportsSDKPb = `package foo
import _ "github.com/brokenbots/overseer/sdk/pb/overseer/v1"
`

// TestInternalImportsSDKTop_Forbidden checks that internal/ importing sdk root is caught.
func TestInternalImportsSDKTop_Forbidden(t *testing.T) {
	root := tempRepoWith(t, map[string]string{
		"internal/engine/foo.go": internalImportsSDKTop,
	})
	vs, err := lint(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 1 {
		t.Fatalf("expected 1 violation, got %d: %+v", len(vs), vs)
	}
	if !strings.Contains(vs[0].message, "sdk/") {
		t.Errorf("unexpected message: %s", vs[0].message)
	}
}

// TestInternalImportsSDKOther_Forbidden checks that internal/ importing a non-pb sdk subpackage is caught.
func TestInternalImportsSDKOther_Forbidden(t *testing.T) {
	root := tempRepoWith(t, map[string]string{
		"internal/plugin/foo.go": internalImportsSDKOther,
	})
	vs, err := lint(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 1 {
		t.Fatalf("expected 1 violation, got %d: %+v", len(vs), vs)
	}
}

// TestInternalImportsSDKPluginhost_Clean checks that internal/ importing
// sdk/pluginhost is allowed (test fixtures are plugin processes and must use
// the public surface).
func TestInternalImportsSDKPluginhost_Clean(t *testing.T) {
	root := tempRepoWith(t, map[string]string{
		"internal/plugin/testfixtures/foo.go": `package foo
import _ "github.com/brokenbots/overseer/sdk/pluginhost"
`,
	})
	vs, err := lint(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 0 {
		t.Fatalf("expected no violations for sdk/pluginhost import from internal/, got %d: %+v", len(vs), vs)
	}
}

// TestInternalImportsSDKPb_Clean checks that internal/ importing sdk/pb is allowed.
func TestInternalImportsSDKPb_Clean(t *testing.T) {
	root := tempRepoWith(t, map[string]string{
		"internal/engine/foo.go": internalImportsSDKPb,
	})
	vs, err := lint(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 0 {
		t.Fatalf("expected no violations for sdk/pb import, got %d: %+v", len(vs), vs)
	}
}

// TestWorkflowImportsInternal_Forbidden checks that workflow/ importing internal/ is caught.
func TestWorkflowImportsInternal_Forbidden(t *testing.T) {
	root := tempRepoWith(t, map[string]string{
		"workflow/eval.go": workflowImportsInternal,
	})
	vs, err := lint(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 1 {
		t.Fatalf("expected 1 violation, got %d: %+v", len(vs), vs)
	}
	if !strings.Contains(vs[0].message, "internal/") {
		t.Errorf("unexpected message: %s", vs[0].message)
	}
}

// TestWorkflowImportsSDKPb_Clean checks that workflow/ is allowed to import sdk/pb.
func TestWorkflowImportsSDKPb_Clean(t *testing.T) {
	root := tempRepoWith(t, map[string]string{
		"workflow/eval.go": workflowImportsSDKPb,
	})
	vs, err := lint(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 0 {
		t.Fatalf("expected no violations for sdk/pb in workflow/, got %d: %+v", len(vs), vs)
	}
}

// TestAllowDirective_Suppresses checks that an import-lint:allow inline comment exempts the import.
func TestAllowDirective_Suppresses(t *testing.T) {
	root := tempRepoWith(t, map[string]string{
		"internal/engine/foo.go": `package foo
import _ "github.com/brokenbots/overseer/sdk" // import-lint:allow needed for bootstrap (W08)
`,
	})
	vs, err := lint(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 0 {
		t.Fatalf("expected no violations with allow directive, got %d: %+v", len(vs), vs)
	}
}

// TestNonGoFilesSkipped checks that non-.go files are not parsed.
func TestNonGoFilesSkipped(t *testing.T) {
	root := tempRepoWith(t, map[string]string{
		"internal/engine/foo.txt": `import "github.com/brokenbots/overseer/sdk"`,
	})
	vs, err := lint(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 0 {
		t.Fatalf("expected no violations for non-Go file, got %d", len(vs))
	}
}

// TestMultipleViolations checks that all violations across multiple files are collected.
func TestMultipleViolations(t *testing.T) {
	root := tempRepoWith(t, map[string]string{
		"internal/a/a.go": internalImportsSDKTop,
		"internal/b/b.go": internalImportsSDKOther,
		"workflow/c.go":   workflowImportsInternal,
	})
	vs, err := lint(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 3 {
		t.Fatalf("expected 3 violations, got %d: %+v", len(vs), vs)
	}
}

// --- CLI contract tests ---

func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "import-lint")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = "."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	return bin
}

func TestCLI_MissingArg_Exit2(t *testing.T) {
	bin := buildBinary(t)
	cmd := exec.Command(bin)
	if err := cmd.Run(); err == nil {
		t.Fatal("expected non-zero exit for missing argument")
	}
	if code := cmd.ProcessState.ExitCode(); code != 2 {
		t.Fatalf("expected exit code 2, got %d", code)
	}
}

func TestCLI_CleanRepo_Exit0(t *testing.T) {
	bin := buildBinary(t)
	root := tempRepoWith(t, map[string]string{
		"internal/engine/foo.go": internalImportsSDKPb,
	})
	cmd := exec.Command(bin, root)
	if err := cmd.Run(); err != nil {
		t.Fatalf("expected exit 0 for clean repo, got: %v", err)
	}
}

func TestCLI_Violations_Exit1(t *testing.T) {
	bin := buildBinary(t)
	root := tempRepoWith(t, map[string]string{
		"internal/engine/foo.go": internalImportsSDKTop,
	})
	cmd := exec.Command(bin, root)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit for violations")
	}
	if code := cmd.ProcessState.ExitCode(); code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(string(out), "sdk/") {
		t.Errorf("expected violation message in output, got: %s", out)
	}
}
