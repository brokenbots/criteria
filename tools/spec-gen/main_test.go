package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "update golden files")

// TestExtractBlocks_FromTestdata verifies block extraction from the sample schema,
// including exact source line numbers and attribute metadata.
func TestExtractBlocks_FromTestdata(t *testing.T) {
	blocks, err := extractBlocks("testdata/schema_sample.go")
	if err != nil {
		t.Fatalf("extractBlocks: %v", err)
	}
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}

	// widget block — defined at line 15 of schema_sample.go
	w := blocks[0]
	if w.Name != "widget" {
		t.Errorf("blocks[0].Name = %q, want %q", w.Name, "widget")
	}
	if len(w.Labels) != 1 || w.Labels[0] != "name" {
		t.Errorf("blocks[0].Labels = %v, want [name]", w.Labels)
	}
	if w.SourceLine != 15 {
		t.Errorf("blocks[0].SourceLine = %d, want 15", w.SourceLine)
	}
	if len(w.Attributes) != 2 {
		t.Fatalf("blocks[0].Attributes len = %d, want 2", len(w.Attributes))
	}
	title := w.Attributes[0]
	if title.Name != "title" || title.Type != "string" || !title.Required {
		t.Errorf("title attr = %+v, want {title string required}", title)
	}
	if title.Description != "Title is the display label shown in the UI." {
		t.Errorf("title.Description = %q, want exact text", title.Description)
	}
	enabled := w.Attributes[1]
	if enabled.Name != "enabled" || enabled.Type != "bool" || enabled.Required {
		t.Errorf("enabled attr = %+v, want {enabled bool optional}", enabled)
	}
	if enabled.Description != "Enabled controls whether the widget is active." {
		t.Errorf("enabled.Description = %q, want exact text", enabled.Description)
	}

	// rule block — defined at line 26 of schema_sample.go
	r := blocks[1]
	if r.Name != "rule" {
		t.Errorf("blocks[1].Name = %q, want %q", r.Name, "rule")
	}
	if len(r.Labels) != 1 || r.Labels[0] != "id" {
		t.Errorf("blocks[1].Labels = %v, want [id]", r.Labels)
	}
	if r.SourceLine != 26 {
		t.Errorf("blocks[1].SourceLine = %d, want 26", r.SourceLine)
	}
	if len(r.Attributes) != 1 {
		t.Fatalf("blocks[1].Attributes len = %d, want 1", len(r.Attributes))
	}
	prio := r.Attributes[0]
	if prio.Name != "priority" || prio.Type != "number" || !prio.Required {
		t.Errorf("priority attr = %+v, want {priority number required}", prio)
	}
	if prio.Description != "Priority sets the evaluation order; higher values run first." {
		t.Errorf("priority.Description = %q, want exact text", prio.Description)
	}
}

// TestExtractFunctions_FromTestdata verifies function extraction including exact
// source line numbers and description text.
func TestExtractFunctions_FromTestdata(t *testing.T) {
	funcs, err := extractFunctions("testdata/functions_sample.go")
	if err != nil {
		t.Fatalf("extractFunctions: %v", err)
	}
	if len(funcs) != 2 {
		t.Fatalf("expected 2 functions, got %d", len(funcs))
	}

	// greetFunction is defined at line 20 in functions_sample.go
	greet := funcs[0]
	if greet.Name != "greet" {
		t.Errorf("funcs[0].Name = %q, want %q", greet.Name, "greet")
	}
	if len(greet.Params) != 1 || greet.Params[0].Name != "name" || greet.Params[0].Type != "string" {
		t.Errorf("greet.Params = %v, want [{name string}]", greet.Params)
	}
	if greet.ReturnType != "string" {
		t.Errorf("greet.ReturnType = %q, want %q", greet.ReturnType, "string")
	}
	if greet.SourceLine != 20 {
		t.Errorf("greet.SourceLine = %d, want 20", greet.SourceLine)
	}
	if greet.Description != "the greet(name) → string function." {
		t.Errorf("greet.Description = %q, want exact text", greet.Description)
	}

	// pingFunction is defined at line 29 in functions_sample.go
	ping := funcs[1]
	if ping.Name != "ping" {
		t.Errorf("funcs[1].Name = %q, want %q", ping.Name, "ping")
	}
	if len(ping.Params) != 0 {
		t.Errorf("ping.Params = %v, want []", ping.Params)
	}
	if ping.ReturnType != "bool" {
		t.Errorf("ping.ReturnType = %q, want %q", ping.ReturnType, "bool")
	}
	if ping.SourceLine != 29 {
		t.Errorf("ping.SourceLine = %d, want 29", ping.SourceLine)
	}
	if ping.Description != "the ping() → bool function." {
		t.Errorf("ping.Description = %q, want exact text", ping.Description)
	}
}

// TestExtractNamespaces_FromTestdata verifies that extractNamespaces discovers
// the top-level context keys and "each" sub-keys from eval_sample.go.
func TestExtractNamespaces_FromTestdata(t *testing.T) {
	namespaces, err := extractNamespaces("testdata/eval_sample.go")
	if err != nil {
		t.Fatalf("extractNamespaces: %v", err)
	}

	// eval_sample.go declares "alpha", "beta" in the initial literal, plus "each" conditionally.
	if len(namespaces) != 3 {
		t.Fatalf("expected 3 namespaces, got %d: %v", len(namespaces), namespaces)
	}

	keys := make([]string, len(namespaces))
	for i, ns := range namespaces {
		keys[i] = ns.Key
	}
	if keys[0] != "alpha" || keys[1] != "beta" || keys[2] != "each" {
		t.Errorf("namespace keys = %v, want [alpha beta each]", keys)
	}

	// "each" must carry sub-keys from WithEachBinding: "item" and "pos".
	var eachNS NamespaceDoc
	for _, ns := range namespaces {
		if ns.Key == "each" {
			eachNS = ns
			break
		}
	}
	if len(eachNS.SubKeys) != 2 {
		t.Fatalf("each.SubKeys = %v, want [item pos]", eachNS.SubKeys)
	}
	if eachNS.SubKeys[0] != "item" || eachNS.SubKeys[1] != "pos" {
		t.Errorf("each.SubKeys = %v, want [item pos]", eachNS.SubKeys)
	}
}

// TestExtractBlocks_MissingDocComment_EmitsPlaceholder verifies that when a field
// has no doc comment, the placeholder text is used.
func TestExtractBlocks_MissingDocComment_EmitsPlaceholder(t *testing.T) {
	src := `package x
type Spec struct {
	Foos []*FooSpec ` + "`hcl:\"foo,block\"`" + `
}
type FooSpec struct {
	Value string ` + "`hcl:\"value,attr\"`" + `
}
`
	tmp := t.TempDir()
	f := filepath.Join(tmp, "schema.go")
	if err := os.WriteFile(f, []byte(src), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	blocks, err := extractBlocks(f)
	if err != nil {
		t.Fatalf("extractBlocks: %v", err)
	}
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if len(blocks[0].Attributes) != 1 {
		t.Fatalf("expected 1 attribute, got %d", len(blocks[0].Attributes))
	}
	if got := blocks[0].Attributes[0].Description; got != "_(no description)_" {
		t.Errorf("expected placeholder, got %q", got)
	}
}

// TestRenderBlocks_Markdown_StableOutput is a golden-file test that ensures the
// blocks markdown output does not change unexpectedly.
func TestRenderBlocks_Markdown_StableOutput(t *testing.T) {
	blocks, err := extractBlocks("testdata/schema_sample.go")
	if err != nil {
		t.Fatalf("extractBlocks: %v", err)
	}
	got := renderBlocks(blocks, "testdata/schema_sample.go")

	const golden = "testdata/blocks.golden.md"
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatalf("WriteFile golden: %v", err)
		}
		t.Logf("updated %s", golden)
		return
	}

	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("ReadFile golden: %v (run with -update to generate)", err)
	}
	if got != string(want) {
		t.Errorf("blocks output does not match golden file %s\ndiff:\n%s", golden, computeDiff(string(want), got))
	}
}

// TestRenderFunctions_Markdown_StableOutput is a golden-file test for function markdown.
func TestRenderFunctions_Markdown_StableOutput(t *testing.T) {
	funcs, err := extractFunctions("testdata/functions_sample.go")
	if err != nil {
		t.Fatalf("extractFunctions: %v", err)
	}
	got := renderFunctions(funcs, "testdata/functions_sample.go")

	const golden = "testdata/functions.golden.md"
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatalf("WriteFile golden: %v", err)
		}
		t.Logf("updated %s", golden)
		return
	}

	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("ReadFile golden: %v (run with -update to generate)", err)
	}
	if got != string(want) {
		t.Errorf("functions output does not match golden file %s\ndiff:\n%s", golden, computeDiff(string(want), got))
	}
}

// TestRenderNamespaces_Markdown_StableOutput is a golden-file test for namespace markdown.
func TestRenderNamespaces_Markdown_StableOutput(t *testing.T) {
	namespaces, err := extractNamespaces("testdata/eval_sample.go")
	if err != nil {
		t.Fatalf("extractNamespaces: %v", err)
	}
	got := renderNamespaces(namespaces)

	const golden = "testdata/namespaces.golden.md"
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatalf("WriteFile golden: %v", err)
		}
		t.Logf("updated %s", golden)
		return
	}

	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("ReadFile golden: %v (run with -update to generate)", err)
	}
	if got != string(want) {
		t.Errorf("namespaces output does not match golden file %s\ndiff:\n%s", golden, computeDiff(string(want), got))
	}
}

// TestExtractBlocks_NestedBFS verifies that extractBlocks follows nested block
// references transitively via BFS and emits one BlockDoc per discovered block type.
func TestExtractBlocks_NestedBFS(t *testing.T) {
	blocks, err := extractBlocks("testdata/schema_nested_sample.go")
	if err != nil {
		t.Fatalf("extractBlocks: %v", err)
	}
	// Expect container (top-level) and item (BFS-discovered nested block).
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks (container + nested item), got %d: %v",
			len(blocks), func() []string {
				names := make([]string, len(blocks))
				for i, b := range blocks {
					names[i] = b.Name
				}
				return names
			}())
	}

	container := blocks[0]
	if container.Name != "container" {
		t.Errorf("blocks[0].Name = %q, want %q", container.Name, "container")
	}
	if len(container.Labels) != 1 || container.Labels[0] != "name" {
		t.Errorf("container.Labels = %v, want [name]", container.Labels)
	}
	if len(container.Attributes) != 1 || container.Attributes[0].Name != "count" {
		t.Errorf("container.Attributes names = %v, want [count]",
			func() []string {
				names := make([]string, len(container.Attributes))
				for i, a := range container.Attributes {
					names[i] = a.Name
				}
				return names
			}())
	}
	if len(container.NestedBlocks) != 1 || container.NestedBlocks[0] != "item" {
		t.Errorf("container.NestedBlocks = %v, want [item]", container.NestedBlocks)
	}

	item := blocks[1]
	if item.Name != "item" {
		t.Errorf("blocks[1].Name = %q, want %q", item.Name, "item")
	}
	if len(item.Labels) != 1 || item.Labels[0] != "key" {
		t.Errorf("item.Labels = %v, want [key]", item.Labels)
	}
	if len(item.Attributes) != 1 || item.Attributes[0].Name != "value" {
		t.Errorf("item.Attributes names = %v, want [value]",
			func() []string {
				names := make([]string, len(item.Attributes))
				for i, a := range item.Attributes {
					names[i] = a.Name
				}
				return names
			}())
	}
	if len(item.NestedBlocks) != 0 {
		t.Errorf("item.NestedBlocks = %v, want empty", item.NestedBlocks)
	}
}

// TestRenderBlocks_NestedLinks verifies that when a block has NestedBlocks entries
// that resolve to other blocks in the same slice, renderBlocks emits markdown links
// instead of plain code spans. Also verifies the fallback to plain code spans when
// the referenced block is not in the slice.
func TestRenderBlocks_NestedLinks(t *testing.T) {
	blocks := []BlockDoc{
		{
			Name:         "container",
			Labels:       []string{"name"},
			NestedBlocks: []string{"item", "unknown"},
			SourceLine:   11,
		},
		{
			Name:       "item",
			Labels:     []string{"key"},
			SourceLine: 20,
			Attributes: []AttrDoc{
				{Name: "value", Type: "string", Required: true, Description: "Value is the item payload."},
			},
		},
	}
	got := renderBlocks(blocks, "testdata/schema_nested_sample.go")

	// The container section must link to the item anchor (GitHub slug for `item "key" { ... }`),
	// not use a plain code span.
	if !strings.Contains(got, "[`item`](#item-key---)") {
		t.Errorf("expected nested block link [`item`](#item-key---) in container section, got:\n%s", got)
	}
	// The item section must be present as a heading.
	if !strings.Contains(got, "### `item") {
		t.Errorf("expected item heading in output, got:\n%s", got)
	}
	// Unknown nested blocks (not in the slice) must fall back to plain code spans.
	if strings.Contains(got, "[`unknown`]") {
		t.Errorf("expected no link for unknown nested block, got:\n%s", got)
	}
	if !strings.Contains(got, "`unknown`") {
		t.Errorf("expected plain code span for unknown nested block, got:\n%s", got)
	}
}

// TestCheckMode_DetectsDrift invokes run() in -check mode and asserts that a
// stale spec file produces a non-zero exit code with a diff on stderr.
func TestCheckMode_DetectsDrift(t *testing.T) {
	const stale = `# Spec
<!-- BEGIN GENERATED:blocks -->
old content
<!-- END GENERATED:blocks -->
<!-- BEGIN GENERATED:functions -->
old functions
<!-- END GENERATED:functions -->
<!-- BEGIN GENERATED:namespaces -->
old namespaces
<!-- END GENERATED:namespaces -->
`
	tmp := t.TempDir()
	specFile := filepath.Join(tmp, "LANGUAGE-SPEC.md")
	if err := os.WriteFile(specFile, []byte(stale), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var stdout, stderr strings.Builder
	code := run(
		[]string{
			"-check",
			"-out", specFile,
			"-schema", "testdata/schema_sample.go",
			"-functions", "testdata/functions_sample.go",
			"-eval", "testdata/eval_sample.go",
		},
		&stdout, &stderr,
	)

	if code == 0 {
		t.Error("expected non-zero exit code for stale spec")
	}
	if !strings.Contains(stderr.String(), "FAIL") {
		t.Errorf("expected FAIL in stderr; got: %s", stderr.String())
	}
	// Diff should show the removed stale lines.
	if !strings.Contains(stderr.String(), "old content") && !strings.Contains(stderr.String(), "-old content") {
		t.Errorf("expected stale block line in diff; stderr: %s", stderr.String())
	}
}

// TestCheckMode_PassesWhenUpToDate verifies that run() returns 0 in -check mode
// when the spec is already current.
func TestCheckMode_PassesWhenUpToDate(t *testing.T) {
	// First write a fresh spec via non-check run, then check it.
	const template = `# Spec
<!-- BEGIN GENERATED:blocks -->
<!-- END GENERATED:blocks -->
<!-- BEGIN GENERATED:functions -->
<!-- END GENERATED:functions -->
<!-- BEGIN GENERATED:namespaces -->
<!-- END GENERATED:namespaces -->
`
	tmp := t.TempDir()
	specFile := filepath.Join(tmp, "LANGUAGE-SPEC.md")
	if err := os.WriteFile(specFile, []byte(template), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Generate.
	var stdout, stderr strings.Builder
	code := run(
		[]string{
			"-out", specFile,
			"-schema", "testdata/schema_sample.go",
			"-functions", "testdata/functions_sample.go",
			"-eval", "testdata/eval_sample.go",
		},
		&stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("generate run failed (code %d): %s", code, stderr.String())
	}

	// Now check — should be OK.
	var stdout2, stderr2 strings.Builder
	code2 := run(
		[]string{
			"-check",
			"-out", specFile,
			"-schema", "testdata/schema_sample.go",
			"-functions", "testdata/functions_sample.go",
			"-eval", "testdata/eval_sample.go",
		},
		&stdout2, &stderr2,
	)
	if code2 != 0 {
		t.Errorf("check run should pass on up-to-date spec; stderr: %s", stderr2.String())
	}
	if !strings.Contains(stdout2.String(), "OK") {
		t.Errorf("expected OK in stdout; got: %s", stdout2.String())
	}
}

// TestMarkers_MissingPair_Errors verifies errors for missing marker pairs.
func TestMarkers_MissingPair_Errors(t *testing.T) {
	t.Run("missing_both", func(t *testing.T) {
		// Missing the blocks pair entirely.
		content := "# Spec\n<!-- BEGIN GENERATED:functions -->\n<!-- END GENERATED:functions -->\n<!-- BEGIN GENERATED:namespaces -->\n<!-- END GENERATED:namespaces -->\n"
		_, err := replaceMarkers(content, nil, nil, nil, "", "")
		if err == nil {
			t.Fatal("expected error for missing blocks marker pair")
		}
		if !strings.Contains(err.Error(), "blocks") {
			t.Errorf("error should mention blocks, got: %v", err)
		}
	})

	t.Run("missing_end", func(t *testing.T) {
		// BEGIN present but END missing.
		content := "# Spec\n<!-- BEGIN GENERATED:blocks -->\n<!-- BEGIN GENERATED:functions -->\n<!-- END GENERATED:functions -->\n<!-- BEGIN GENERATED:namespaces -->\n<!-- END GENERATED:namespaces -->\n"
		_, err := replaceMarkers(content, nil, nil, nil, "", "")
		if err == nil {
			t.Fatal("expected error for missing END marker")
		}
		if !strings.Contains(err.Error(), "blocks") {
			t.Errorf("error should mention blocks, got: %v", err)
		}
	})
}

// TestMarkers_Unbalanced_Errors verifies that ordering and nesting errors are caught.
func TestMarkers_Unbalanced_Errors(t *testing.T) {
	t.Run("end_before_begin", func(t *testing.T) {
		content := `# Spec
<!-- END GENERATED:blocks -->
<!-- BEGIN GENERATED:blocks -->
<!-- BEGIN GENERATED:functions -->
<!-- END GENERATED:functions -->
<!-- BEGIN GENERATED:namespaces -->
<!-- END GENERATED:namespaces -->
`
		_, err := replaceMarkers(content, nil, nil, nil, "", "")
		if err == nil {
			t.Fatal("expected error for unbalanced markers")
		}
		if !strings.Contains(err.Error(), "unbalanced") && !strings.Contains(err.Error(), "before") {
			t.Errorf("error should mention ordering, got: %v", err)
		}
	})

	t.Run("same_name_nesting", func(t *testing.T) {
		// Nested same-name markers: BEGIN blocks inside BEGIN blocks.
		content := `# Spec
<!-- BEGIN GENERATED:blocks -->
<!-- BEGIN GENERATED:blocks -->
inner content
<!-- END GENERATED:blocks -->
<!-- END GENERATED:blocks -->
<!-- BEGIN GENERATED:functions -->
<!-- END GENERATED:functions -->
<!-- BEGIN GENERATED:namespaces -->
<!-- END GENERATED:namespaces -->
`
		_, err := replaceMarkers(content, nil, nil, nil, "", "")
		if err == nil {
			t.Fatal("expected error for same-name nested markers")
		}
		if !strings.Contains(err.Error(), "nested") && !strings.Contains(err.Error(), "overlap") {
			t.Errorf("error should mention nesting or overlap, got: %v", err)
		}
	})
}

