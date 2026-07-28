package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"
)

func TestParseVarFile_JSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vars.json")
	content := `{"foo": "bar", "count": 42}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	got, err := parseVarFile(path)
	if err != nil {
		t.Fatalf("parseVarFile: %v", err)
	}
	want := map[string]cty.Value{
		"foo":   cty.StringVal("bar"),
		"count": cty.NumberIntVal(42),
	}
	assertCtyMapEq(t, got, want)
}

func TestParseVarFile_CHCL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vars.chcl")
	content := `foo = "bar"
count = 42
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	got, err := parseVarFile(path)
	if err != nil {
		t.Fatalf("parseVarFile: %v", err)
	}
	want := map[string]cty.Value{
		"foo":   cty.StringVal("bar"),
		"count": cty.NumberIntVal(42),
	}
	assertCtyMapEq(t, got, want)
}

func TestParseVarFile_HCL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vars.hcl")
	content := `foo = "bar"
count = 42
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	got, err := parseVarFile(path)
	if err != nil {
		t.Fatalf("parseVarFile: %v", err)
	}
	want := map[string]cty.Value{
		"foo":   cty.StringVal("bar"),
		"count": cty.NumberIntVal(42),
	}
	assertCtyMapEq(t, got, want)
}

func TestParseVarFile_JSON_Structured(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vars.json")
	content := `{"tags":["a","b"],"cfg":{"a":"1"}}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	got, err := parseVarFile(path)
	if err != nil {
		t.Fatalf("parseVarFile: %v", err)
	}
	want := map[string]cty.Value{
		"tags": cty.TupleVal([]cty.Value{cty.StringVal("a"), cty.StringVal("b")}),
		"cfg":  cty.ObjectVal(map[string]cty.Value{"a": cty.StringVal("1")}),
	}
	assertCtyMapEq(t, got, want)
}

func TestParseVarFile_HCL_Structured(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vars.hcl")
	content := `tags = ["a", "b"]
cfg  = { a = "1" }
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	got, err := parseVarFile(path)
	if err != nil {
		t.Fatalf("parseVarFile: %v", err)
	}
	want := map[string]cty.Value{
		"tags": cty.TupleVal([]cty.Value{cty.StringVal("a"), cty.StringVal("b")}),
		"cfg":  cty.ObjectVal(map[string]cty.Value{"a": cty.StringVal("1")}),
	}
	assertCtyMapEq(t, got, want)
}

func TestParseVarFile_UnsupportedExtension(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vars.txt")
	if err := os.WriteFile(path, []byte("foo=bar"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	_, err := parseVarFile(path)
	if err == nil {
		t.Fatal("expected error for unsupported extension")
	}
	if !strings.Contains(err.Error(), ".txt") {
		t.Errorf("expected error mentioning .txt, got: %v", err)
	}
	if !strings.Contains(err.Error(), ".chcl") || !strings.Contains(err.Error(), ".hcl") {
		t.Errorf("expected error listing supported extensions, got: %v", err)
	}
}

func TestParseVarFile_NonExistent(t *testing.T) {
	_, err := parseVarFile("/nonexistent/path/vars.json")
	if err == nil {
		t.Fatal("expected error for non-existent file")
	}
}

func TestParseVarFile_MalformedJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vars.json")
	if err := os.WriteFile(path, []byte(`{"foo": `), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	_, err := parseVarFile(path)
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestParseVarFile_MalformedHCL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vars.hcl")
	if err := os.WriteFile(path, []byte(`foo = `), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	_, err := parseVarFile(path)
	if err == nil {
		t.Fatal("expected error for malformed HCL")
	}
}

func TestParseVarFile_NumericValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vars.hcl")
	content := `foo = 42`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	got, err := parseVarFile(path)
	if err != nil {
		t.Fatalf("parseVarFile: %v", err)
	}
	assertCtyMapEq(t, got, map[string]cty.Value{"foo": cty.NumberIntVal(42)})
}

func TestMergeVarSources_VarAndFileDisjointKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vars.json")
	if err := os.WriteFile(path, []byte(`{"env": "prod"}`), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	got, err := mergeVarSources([]string{path}, []string{"region=us-west"})
	if err != nil {
		t.Fatalf("mergeVarSources: %v", err)
	}
	want := map[string]cty.Value{
		"env":    cty.StringVal("prod"),
		"region": cty.StringVal("us-west"),
	}
	assertCtyMapEq(t, got, want)
}

func TestMergeVarSources_Precedence_VarOverridesVarFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vars.json")
	if err := os.WriteFile(path, []byte(`{"foo": "file"}`), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	got, err := mergeVarSources([]string{path}, []string{"foo=cli"})
	if err != nil {
		t.Fatalf("mergeVarSources: %v", err)
	}
	if !got["foo"].RawEquals(cty.StringVal("cli")) {
		t.Errorf("foo = %#v, want %#v", got["foo"], cty.StringVal("cli"))
	}
}

func TestMergeVarSources_Precedence_LaterFileWins(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.json")
	b := filepath.Join(dir, "b.json")
	if err := os.WriteFile(a, []byte(`{"foo": "a"}`), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := os.WriteFile(b, []byte(`{"foo": "b"}`), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	got, err := mergeVarSources([]string{a, b}, nil)
	if err != nil {
		t.Fatalf("mergeVarSources: %v", err)
	}
	if !got["foo"].RawEquals(cty.StringVal("b")) {
		t.Errorf("foo = %#v, want %#v", got["foo"], cty.StringVal("b"))
	}
}

func TestMergeVarSources_MergesMultipleFiles(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.json")
	b := filepath.Join(dir, "b.json")
	if err := os.WriteFile(a, []byte(`{"x": "a"}`), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := os.WriteFile(b, []byte(`{"y": "b"}`), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	got, err := mergeVarSources([]string{a, b}, nil)
	if err != nil {
		t.Fatalf("mergeVarSources: %v", err)
	}
	assertCtyMapEq(t, got, map[string]cty.Value{
		"x": cty.StringVal("a"),
		"y": cty.StringVal("b"),
	})
}

func TestMergeVarSources_VarFileErrorPropagated(t *testing.T) {
	_, err := mergeVarSources([]string{"/nonexistent/vars.json"}, nil)
	if err == nil {
		t.Fatal("expected error for missing var-file")
	}
}

func assertCtyMapEq(t *testing.T, got, want map[string]cty.Value) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len=%d want %d (got=%v want=%v)", len(got), len(want), got, want)
	}
	for k, v := range want {
		gv, ok := got[k]
		if !ok {
			t.Fatalf("missing key %q", k)
		}
		if !gv.RawEquals(v) {
			t.Fatalf("key %q: got %#v want %#v", k, gv, v)
		}
	}
}

func TestParseVarOverrides_List(t *testing.T) {
	got := parseVarOverrides([]string{`tags=["a", "b"]`})
	want := cty.TupleVal([]cty.Value{cty.StringVal("a"), cty.StringVal("b")})
	if !got["tags"].RawEquals(want) {
		t.Errorf("tags = %#v, want %#v", got["tags"], want)
	}
}

func TestParseVarOverrides_Map(t *testing.T) {
	got := parseVarOverrides([]string{`labels={"env"="prod", "app"="demo"}`})
	want := cty.ObjectVal(map[string]cty.Value{
		"env": cty.StringVal("prod"),
		"app": cty.StringVal("demo"),
	})
	if !got["labels"].RawEquals(want) {
		t.Errorf("labels = %#v, want %#v", got["labels"], want)
	}
}

func TestParseVarOverrides_Object(t *testing.T) {
	got := parseVarOverrides([]string{`config={enabled=true, retries=3}`})
	want := cty.ObjectVal(map[string]cty.Value{
		"enabled": cty.True,
		"retries": cty.NumberIntVal(3),
	})
	if !got["config"].RawEquals(want) {
		t.Errorf("config = %#v, want %#v", got["config"], want)
	}
}

func TestParseVarOverrides_StringFallback(t *testing.T) {
	// us-west is not a valid HCL expression, so it falls back to a plain string.
	got := parseVarOverrides([]string{"region=us-west"})
	assertCtyMapEq(t, got, map[string]cty.Value{
		"region": cty.StringVal("us-west"),
	})
}

func TestParseVarOverrides_BoolAndNumber(t *testing.T) {
	got := parseVarOverrides([]string{"enabled=true", "count=42"})
	assertCtyMapEq(t, got, map[string]cty.Value{
		"enabled": cty.True,
		"count":   cty.NumberIntVal(42),
	})
}
