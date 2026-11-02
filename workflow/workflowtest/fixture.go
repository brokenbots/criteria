package workflowtest

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// CalleeHCL generates a minimal valid callee workflow HCL with the given name and variables.
// vars maps variable name -> whether it has a default (true = has default, false = required).
func CalleeHCL(name string, vars map[string]bool) string {
	trueVal := true
	b := New().
		WithHeader(name, "1", "done", "done").
		WithState("done", true, &trueVal)

	// Sort variable names for deterministic HCL output
	varNames := make([]string, 0, len(vars))
	for k := range vars {
		varNames = append(varNames, k)
	}
	sort.Strings(varNames)

	for _, varName := range varNames {
		if vars[varName] {
			b.WithVariable(varName, "string", `"default_value"`)
		} else {
			b.WithVariable(varName, "string", "")
		}
	}
	return b.Build()
}

// WriteFile writes a file at dir/filename with the given content and returns the full file path.
func WriteFile(t testing.TB, dir, filename, content string) string {
	t.Helper()
	p := filepath.Join(dir, filename)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %q: %v", p, err)
	}
	return p
}

// WriteDir creates dir/name containing main.hcl with content and returns the directory path.
func WriteDir(t testing.TB, parent, name, content string) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create workflow dir %q: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.hcl"), []byte(content), 0o644); err != nil {
		t.Fatalf("write main.hcl in %q: %v", dir, err)
	}
	return dir
}

// WriteCalleeDir creates parent/name/main.hcl with a minimal callee workflow and returns the directory path.
func WriteCalleeDir(t testing.TB, parent, name string, vars map[string]bool) string {
	t.Helper()
	return WriteDir(t, parent, name, CalleeHCL(name, vars))
}

// ParentWithSubworkflow generates a parent workflow declaring a subworkflow with optional input.
func ParentWithSubworkflow(swName, source, inputAttrs string) string {
	trueVal := true
	b := New().
		WithHeader("parent", "1", "done", "done").
		WithState("done", true, &trueVal)

	if inputAttrs != "" {
		b.WithRaw(fmt.Sprintf("subworkflow %q {\n  source = %q\n  input = {\n    %s\n  }\n}", swName, source, inputAttrs))
	} else {
		b.WithSubworkflow(swName, source)
	}
	return b.Build()
}
