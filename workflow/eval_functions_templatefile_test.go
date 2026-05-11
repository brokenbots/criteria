package workflow

// Tests for templatefileFunction and its helpers ctyToGoMap / ctyToGoValue.
// This file is an internal test (package workflow, not package workflow_test)
// so it can invoke the unexported templatefileFunction directly.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"
)

// tmplOpts returns a FunctionOptions with the given workflowDir and a 1 KiB
// MaxBytes cap (small enough to exercise the size-cap path in tests).
func tmplOpts(workflowDir string) FunctionOptions {
	return FunctionOptions{
		WorkflowDir:  workflowDir,
		MaxBytes:     1024,
		AllowedPaths: nil,
	}
}

// callTemplateFile is a thin helper that invokes templatefileFunction and
// returns the string result or error, keeping individual tests terse.
func callTemplateFile(opts FunctionOptions, path string, vars cty.Value) (string, error) {
	val, err := templatefileFunction(opts).Call([]cty.Value{cty.StringVal(path), vars})
	if err != nil {
		return "", err
	}
	return val.AsString(), nil
}

// writeTmpl writes content to name inside dir and returns the filename.
func writeTmpl(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// Test 1: basic string substitution.
func TestTemplatefile_HappyPath_BasicSubstitution(t *testing.T) {
	dir := t.TempDir()
	writeTmpl(t, dir, "greeting.tmpl", "hello {{ .name }}")
	vars := cty.ObjectVal(map[string]cty.Value{"name": cty.StringVal("world")})
	got, err := callTemplateFile(tmplOpts(dir), "greeting.tmpl", vars)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "hello world" {
		t.Errorf("got %q; want %q", got, "hello world")
	}
}

// Test 2: nested object fields.
func TestTemplatefile_NestedFields(t *testing.T) {
	dir := t.TempDir()
	writeTmpl(t, dir, "person.tmpl", "{{ .person.name }} is {{ .person.age }}")
	vars := cty.ObjectVal(map[string]cty.Value{
		"person": cty.ObjectVal(map[string]cty.Value{
			"name": cty.StringVal("Ada"),
			"age":  cty.NumberIntVal(36),
		}),
	})
	got, err := callTemplateFile(tmplOpts(dir), "person.tmpl", vars)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "Ada is 36" {
		t.Errorf("got %q; want %q", got, "Ada is 36")
	}
}

// Test 3: list iteration with range.
func TestTemplatefile_ListIteration(t *testing.T) {
	dir := t.TempDir()
	writeTmpl(t, dir, "list.tmpl", "{{ range .items }}- {{ . }}\n{{ end }}")
	vars := cty.ObjectVal(map[string]cty.Value{
		"items": cty.ListVal([]cty.Value{
			cty.StringVal("a"),
			cty.StringVal("b"),
			cty.StringVal("c"),
		}),
	})
	got, err := callTemplateFile(tmplOpts(dir), "list.tmpl", vars)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "- a\n- b\n- c\n" {
		t.Errorf("got %q; want %q", got, "- a\n- b\n- c\n")
	}
}

// Test 4: boolean conditional.
func TestTemplatefile_BoolConditional(t *testing.T) {
	dir := t.TempDir()
	writeTmpl(t, dir, "bool.tmpl", "{{ if .ready }}go{{ else }}wait{{ end }}")
	opts := tmplOpts(dir)

	for _, tc := range []struct {
		ready bool
		want  string
	}{
		{true, "go"},
		{false, "wait"},
	} {
		vars := cty.ObjectVal(map[string]cty.Value{"ready": cty.BoolVal(tc.ready)})
		got, err := callTemplateFile(opts, "bool.tmpl", vars)
		if err != nil {
			t.Fatalf("ready=%v: unexpected error: %v", tc.ready, err)
		}
		if got != tc.want {
			t.Errorf("ready=%v: got %q; want %q", tc.ready, got, tc.want)
		}
	}
}

// Test 5: float number rendering.
func TestTemplatefile_NumberFloat(t *testing.T) {
	dir := t.TempDir()
	writeTmpl(t, dir, "num.tmpl", "{{ .pi }}")
	vars := cty.ObjectVal(map[string]cty.Value{"pi": cty.NumberFloatVal(3.14)})
	got, err := callTemplateFile(tmplOpts(dir), "num.tmpl", vars)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "3.14" {
		t.Errorf("got %q; want %q", got, "3.14")
	}
}

// Test 6: integer number rendering — must not produce "42.0".
func TestTemplatefile_NumberInt(t *testing.T) {
	dir := t.TempDir()
	writeTmpl(t, dir, "num.tmpl", "{{ .n }}")
	vars := cty.ObjectVal(map[string]cty.Value{"n": cty.NumberIntVal(42)})
	got, err := callTemplateFile(tmplOpts(dir), "num.tmpl", vars)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "42" {
		t.Errorf("got %q; want %q (must not be 42.0)", got, "42")
	}
}

// Test 7: null attribute value renders as "<no value>" (Go text/template default for nil map entry).
func TestTemplatefile_NullValueRendersAsNoValue(t *testing.T) {
	dir := t.TempDir()
	writeTmpl(t, dir, "null.tmpl", "got: {{ .x }}")
	vars := cty.ObjectVal(map[string]cty.Value{"x": cty.NullVal(cty.String)})
	got, err := callTemplateFile(tmplOpts(dir), "null.tmpl", vars)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Go's text/template renders a nil interface map value as "<no value>".
	if got != "got: <no value>" {
		t.Errorf("got %q; want %q", got, "got: <no value>")
	}
}

// Test 8: accessing a key missing from vars with missingkey=error produces an error.
func TestTemplatefile_MissingKey_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	writeTmpl(t, dir, "miss.tmpl", "{{ .b }}")
	vars := cty.ObjectVal(map[string]cty.Value{"a": cty.StringVal("x")})
	_, err := callTemplateFile(tmplOpts(dir), "miss.tmpl", vars)
	if err == nil {
		t.Fatal("expected error for missing key; got none")
	}
	msg := err.Error()
	if !strings.Contains(msg, "templatefile()") {
		t.Errorf("error %q should mention 'templatefile()'", msg)
	}
	if !strings.Contains(msg, "execute") {
		t.Errorf("error %q should mention 'execute'", msg)
	}
}

// Test 9: unknown attribute value inside vars returns an error.
func TestTemplatefile_UnknownVar_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	writeTmpl(t, dir, "unk.tmpl", "{{ .x }}")
	// An object whose attribute value is unknown (not the object itself).
	vars := cty.ObjectVal(map[string]cty.Value{"x": cty.UnknownVal(cty.String)})
	_, err := callTemplateFile(tmplOpts(dir), "unk.tmpl", vars)
	if err == nil {
		t.Fatal("expected error for unknown var; got none")
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Errorf("error %q should mention 'unknown'", err.Error())
	}
}

// Test 10: passing null as the vars argument returns an error.
func TestTemplatefile_NullVarsArg_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	// File need not exist; AllowNull:false triggers before Impl is called.
	_, err := templatefileFunction(tmplOpts(dir)).Call([]cty.Value{
		cty.StringVal("x.tmpl"),
		cty.NullVal(cty.DynamicPseudoType),
	})
	if err == nil {
		t.Fatal("expected error for null vars; got none")
	}
	if !strings.Contains(err.Error(), "must not be null") {
		t.Errorf("error %q should mention 'must not be null'", err.Error())
	}
}

// Test 11: passing a primitive instead of an object/map returns an error.
func TestTemplatefile_PrimitiveVarsArg_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		val  cty.Value
	}{
		{"string", cty.StringVal("not a map")},
		{"list", cty.ListVal([]cty.Value{cty.StringVal("a")})},
		{"tuple", cty.TupleVal([]cty.Value{cty.StringVal("a"), cty.NumberIntVal(1)})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := templatefileFunction(tmplOpts(dir)).Call([]cty.Value{
				cty.StringVal("x.tmpl"),
				tc.val,
			})
			if err == nil {
				t.Fatalf("expected error for %s vars; got none", tc.name)
			}
			if !strings.Contains(err.Error(), "object or map") {
				t.Errorf("error %q should mention 'object or map'", err.Error())
			}
		})
	}
}

// Test 12: non-existent path returns a "no such file" error.
func TestTemplatefile_FileNotFound_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	vars := cty.ObjectVal(map[string]cty.Value{})
	_, err := callTemplateFile(tmplOpts(dir), "notexist.tmpl", vars)
	if err == nil {
		t.Fatal("expected error for missing file; got none")
	}
	msg := err.Error()
	if !strings.Contains(msg, "templatefile()") {
		t.Errorf("error %q should mention 'templatefile()'", msg)
	}
	if !strings.Contains(msg, "no such file") {
		t.Errorf("error %q should mention 'no such file'", msg)
	}
}

// Test 13: path that escapes the workflow directory returns a confinement error.
func TestTemplatefile_PathEscape_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	vars := cty.ObjectVal(map[string]cty.Value{})
	_, err := callTemplateFile(tmplOpts(dir), "../escape.tmpl", vars)
	if err == nil {
		t.Fatal("expected error for path escape; got none")
	}
	msg := err.Error()
	if !strings.Contains(msg, "templatefile()") {
		t.Errorf("error %q should mention 'templatefile()'", msg)
	}
	if !strings.Contains(msg, "escapes workflow directory") {
		t.Errorf("error %q should mention 'escapes workflow directory'", msg)
	}
}

// Test 14: absolute path is rejected.
func TestTemplatefile_AbsolutePath_Rejected(t *testing.T) {
	dir := t.TempDir()
	vars := cty.ObjectVal(map[string]cty.Value{})
	_, err := callTemplateFile(tmplOpts(dir), "/etc/passwd", vars)
	if err == nil {
		t.Fatal("expected error for absolute path; got none")
	}
	if !strings.Contains(err.Error(), "absolute paths are not supported") {
		t.Errorf("error %q should mention 'absolute paths are not supported'", err.Error())
	}
}

// Test 15: file larger than MaxBytes is rejected.
func TestTemplatefile_OverSizeCap_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	// Write 2 KiB — larger than the 1 KiB cap in tmplOpts.
	big := make([]byte, 2*1024)
	for i := range big {
		big[i] = 'x'
	}
	if err := os.WriteFile(filepath.Join(dir, "big.tmpl"), big, 0o644); err != nil {
		t.Fatalf("write big.tmpl: %v", err)
	}
	vars := cty.ObjectVal(map[string]cty.Value{})
	_, err := callTemplateFile(tmplOpts(dir), "big.tmpl", vars)
	if err == nil {
		t.Fatal("expected error for oversized file; got none")
	}
	if !strings.Contains(err.Error(), "max is") {
		t.Errorf("error %q should mention 'max is'", err.Error())
	}
}

// Test 16: file with invalid UTF-8 bytes is rejected.
func TestTemplatefile_InvalidUTF8_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	// 0x80 is an invalid UTF-8 start byte.
	if err := os.WriteFile(filepath.Join(dir, "bad.tmpl"), []byte{0x80, 0x90}, 0o644); err != nil {
		t.Fatalf("write bad.tmpl: %v", err)
	}
	vars := cty.ObjectVal(map[string]cty.Value{})
	_, err := callTemplateFile(tmplOpts(dir), "bad.tmpl", vars)
	if err == nil {
		t.Fatal("expected error for invalid UTF-8; got none")
	}
	if !strings.Contains(err.Error(), "invalid UTF-8") {
		t.Errorf("error %q should mention 'invalid UTF-8'", err.Error())
	}
}

// Test 17: empty template file returns an empty string.
func TestTemplatefile_EmptyTemplate_ReturnsEmptyString(t *testing.T) {
	dir := t.TempDir()
	writeTmpl(t, dir, "empty.tmpl", "")
	vars := cty.ObjectVal(map[string]cty.Value{"x": cty.StringVal("ignored")})
	got, err := callTemplateFile(tmplOpts(dir), "empty.tmpl", vars)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("got %q; want empty string", got)
	}
}

// Test 18: template outside WorkflowDir but inside AllowedPaths is accessible.
func TestTemplatefile_AllowedPathsHonored(t *testing.T) {
	parent := t.TempDir()
	workflowDir := filepath.Join(parent, "workflow")
	sharedDir := filepath.Join(parent, "shared")
	for _, d := range []string{workflowDir, sharedDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeTmpl(t, sharedDir, "common.tmpl", "shared: {{ .key }}")

	opts := FunctionOptions{
		WorkflowDir:  workflowDir,
		MaxBytes:     1024,
		AllowedPaths: []string{sharedDir},
	}
	vars := cty.ObjectVal(map[string]cty.Value{"key": cty.StringVal("yes")})
	got, err := callTemplateFile(opts, "../shared/common.tmpl", vars)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "shared: yes" {
		t.Errorf("got %q; want %q", got, "shared: yes")
	}
}

// Test 19: malformed template (unclosed action) returns a parse error.
func TestTemplatefile_TemplateParseError_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	writeTmpl(t, dir, "bad.tmpl", "{{ .unclosed")
	vars := cty.ObjectVal(map[string]cty.Value{})
	_, err := callTemplateFile(tmplOpts(dir), "bad.tmpl", vars)
	if err == nil {
		t.Fatal("expected parse error; got none")
	}
	msg := err.Error()
	if !strings.Contains(msg, "parse") {
		t.Errorf("error %q should mention 'parse'", msg)
	}
	if !strings.Contains(msg, "bad.tmpl") {
		t.Errorf("error %q should mention the template path", msg)
	}
}

// Test 20: concurrent calls produce no data races (run with -race).
func TestTemplatefile_ConcurrentCalls_NoRace(t *testing.T) {
	dir := t.TempDir()
	writeTmpl(t, dir, "greet.tmpl", "hello {{ .name }}")
	opts := tmplOpts(dir)

	const n = 50
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			vars := cty.ObjectVal(map[string]cty.Value{
				"name": cty.StringVal(fmt.Sprintf("caller-%d", i)),
			})
			val, err := templatefileFunction(opts).Call([]cty.Value{
				cty.StringVal("greet.tmpl"),
				vars,
			})
			if err != nil {
				errs <- fmt.Errorf("goroutine %d: %w", i, err)
				return
			}
			want := fmt.Sprintf("hello caller-%d", i)
			if got := val.AsString(); got != want {
				errs <- fmt.Errorf("goroutine %d: got %q; want %q", i, got, want)
				return
			}
			errs <- nil
		}(i)
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
}
