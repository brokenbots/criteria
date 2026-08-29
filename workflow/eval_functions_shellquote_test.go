package workflow

import (
	"bytes"
	"fmt"
	"os/exec"
	"testing"

	"github.com/zclconf/go-cty/cty"
)

// TestShellQuote_Helper verifies the shell-quoting implementation for common
// metacharacters and edge cases.
func TestShellQuote_Helper(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "''"},
		{"hello", "'hello'"},
		{"can't", `'can'\''t'`},
		{"he said \"hi\"", `'he said "hi"'`},
		{"$var", "'$var'"},
		{";", "';'"},
		{"|", "'|'"},
		{"&&", "'&&'"},
		{"a\nb", "'a\nb'"},
		{"'single'", `''\''single'\'''`},
		{"abc'123'xyz", "'abc'\\''123'\\''xyz'"},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("%q", tc.in), func(t *testing.T) {
			got := shellQuote(tc.in)
			if got != tc.want {
				t.Errorf("shellQuote(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestShellQuote_ShellParsesAsSingleArgument runs the rendered argument through
// /bin/sh to confirm it is parsed as exactly one literal word that preserves
// the original value.
func TestShellQuote_ShellParsesAsSingleArgument(t *testing.T) {
	values := []string{
		"",
		"hello",
		"can't",
		"he said \"hi\"",
		"$var",
		";",
		"|",
		"&&",
		"a\nb",
		"'single'",
		"abc'123'xyz",
		"; rm -rf /; echo pwned",
	}

	for _, v := range values {
		t.Run(fmt.Sprintf("%q", v), func(t *testing.T) {
			quoted := shellQuote(v)
			// Build a shell command that prints the first (and only) argument
			// using printf. If quoting is wrong, sh will either fail to parse or
			// split the value into multiple words.
			script := "printf '%s' " + quoted
			out, err := exec.Command("/bin/sh", "-c", script).CombinedOutput()
			if err != nil {
				t.Fatalf("sh failed for %q (script %q): %v\noutput: %s", v, script, err, out)
			}
			if string(out) != v {
				t.Errorf("shell output did not match input: got %q; want %q", out, v)
			}
		})
	}
}

// TestTemplatefile_ShellQuote verifies that shellquote can be used inside a
// templatefile template to safely embed arbitrary strings in a shell command.
func TestTemplatefile_ShellQuote(t *testing.T) {
	dir := t.TempDir()
	writeTmpl(t, dir, "command.tmpl", "printf '%s\\n' {{ .value | shellquote }}")

	vars := cty.ObjectVal(map[string]cty.Value{
		"value": cty.StringVal("can't"),
	})
	got, err := callTemplateFile(tmplOpts(dir), "command.tmpl", vars)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := `printf '%s\n' 'can'\''t'`
	if got != want {
		t.Errorf("got %q; want %q", got, want)
	}

	// Confirm the rendered command actually runs and produces the original value.
	out, err := exec.Command("/bin/sh", "-c", got).Output()
	if err != nil {
		t.Fatalf("rendered command failed: %v\noutput: %s", err, out)
	}
	if string(bytes.TrimSuffix(out, []byte("\n"))) != "can't" {
		t.Errorf("shell output %q did not round-trip the original value", out)
	}
}

// TestTemplatefile_ShellQuote_AsFunctionCall verifies shellquote works when
// called as a function action in addition to a pipeline.
func TestTemplatefile_ShellQuote_AsFunctionCall(t *testing.T) {
	dir := t.TempDir()
	writeTmpl(t, dir, "command.tmpl", "echo {{ shellquote .value }}")

	vars := cty.ObjectVal(map[string]cty.Value{
		"value": cty.StringVal("a; b | c"),
	})
	got, err := callTemplateFile(tmplOpts(dir), "command.tmpl", vars)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := `echo 'a; b | c'`
	if got != want {
		t.Errorf("got %q; want %q", got, want)
	}
}

// TestTemplatefile_ShellQuote_DoesNotAffectUnquotedRendering confirms that
// templates that do not use shellquote continue to render exactly as before.
func TestTemplatefile_ShellQuote_DoesNotAffectUnquotedRendering(t *testing.T) {
	dir := t.TempDir()
	writeTmpl(t, dir, "command.tmpl", "printf '%s\\n' '{{ .value }}'")

	vars := cty.ObjectVal(map[string]cty.Value{
		"value": cty.StringVal("can't"),
	})
	got, err := callTemplateFile(tmplOpts(dir), "command.tmpl", vars)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The legacy behavior intentionally does not escape the value; this test
	// ensures adding shellquote did not change that.
	want := "printf '%s\\n' 'can't'"
	if got != want {
		t.Errorf("got %q; want %q", got, want)
	}
}

// TestTemplatefile_ShellQuote_Newlines ensures values containing newlines are
// rendered as a single shell-quoted literal.
func TestTemplatefile_ShellQuote_Newlines(t *testing.T) {
	dir := t.TempDir()
	writeTmpl(t, dir, "command.tmpl", "printf '%s\\n' {{ .value | shellquote }}")

	value := "line1\nline2\n$(whoami)"
	vars := cty.ObjectVal(map[string]cty.Value{
		"value": cty.StringVal(value),
	})
	got, err := callTemplateFile(tmplOpts(dir), "command.tmpl", vars)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Round-trip through the shell and verify the exact bytes come back.
	out, err := exec.Command("/bin/sh", "-c", got).Output()
	if err != nil {
		t.Fatalf("rendered command failed: %v\noutput: %s", err, out)
	}
	// printf '%s\n' appends a trailing newline to the argument.
	if string(out) != value+"\n" {
		t.Errorf("shell output did not round-trip: got %q; want %q", out, value+"\n")
	}
}
