package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseVarFile_JSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vars.json")
	content := `{"foo": "bar", "count": "42"}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	got, err := parseVarFile(path)
	if err != nil {
		t.Fatalf("parseVarFile: %v", err)
	}
	want := map[string]string{"foo": "bar", "count": "42"}
	assertMapEq(t, got, want)
}

func TestParseVarFile_CHCL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vars.chcl")
	content := `foo = "bar"
count = "42"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	got, err := parseVarFile(path)
	if err != nil {
		t.Fatalf("parseVarFile: %v", err)
	}
	want := map[string]string{"foo": "bar", "count": "42"}
	assertMapEq(t, got, want)
}

func TestParseVarFile_HCL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vars.hcl")
	content := `foo = "bar"
count = "42"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	got, err := parseVarFile(path)
	if err != nil {
		t.Fatalf("parseVarFile: %v", err)
	}
	want := map[string]string{"foo": "bar", "count": "42"}
	assertMapEq(t, got, want)
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

func TestParseVarFile_NonStringValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vars.hcl")
	content := `foo = 42`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	_, err := parseVarFile(path)
	if err == nil {
		t.Fatal("expected error for non-string value")
	}
	if !strings.Contains(err.Error(), "non-string value") {
		t.Errorf("expected error mentioning non-string value, got: %v", err)
	}
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
	if got["foo"] != "cli" {
		t.Errorf("foo = %q, want %q", got["foo"], "cli")
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
	if got["foo"] != "b" {
		t.Errorf("foo = %q, want %q", got["foo"], "b")
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
	assertMapEq(t, got, map[string]string{"x": "a", "y": "b"})
}

func TestMergeVarSources_VarFileErrorPropagated(t *testing.T) {
	_, err := mergeVarSources([]string{"/nonexistent/vars.json"}, nil)
	if err == nil {
		t.Fatal("expected error for missing var-file")
	}
}

func assertMapEq(t *testing.T, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len=%d want %d (got=%v want=%v)", len(got), len(want), got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("key %q: got %q want %q", k, got[k], v)
		}
	}
}
