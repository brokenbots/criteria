package main

import (
	"os"
	"testing"
)

// TestGoldenRoundTrip runs the full JSON→YAML transform against the checked-in
// fixture and compares the output byte-for-byte with the golden file.
//
// To regenerate the golden file after an intentional format change:
//
//	go run . -in testdata/input.json -out testdata/golden.yml
func TestGoldenRoundTrip(t *testing.T) {
	issues, err := parseReport("testdata/input.json")
	if err != nil {
		t.Fatalf("parseReport: %v", err)
	}

	got := renderYAML(buildRules(issues))

	wantBytes, err := os.ReadFile("testdata/golden.yml")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	want := string(wantBytes)

	if got != want {
		t.Errorf("output does not match golden file\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
}

// TestDeduplication verifies that identical issues are collapsed to one rule.
func TestDeduplication(t *testing.T) {
	issues := []issue{
		{FromLinter: "funlen", Text: "Function 'Foo' is too long (60 > 50 lines)", Pos: position{Filename: "pkg/a.go"}},
		{FromLinter: "funlen", Text: "Function 'Foo' is too long (60 > 50 lines)", Pos: position{Filename: "pkg/a.go"}},
	}
	rules := buildRules(issues)
	if len(rules) != 1 {
		t.Errorf("expected 1 rule after dedup, got %d", len(rules))
	}
}

// TestEmptyInput verifies that a null/empty Issues array produces valid YAML.
func TestEmptyInput(t *testing.T) {
	got := renderYAML(buildRules(nil))
	want := "issues:\n  exclude-rules:\n"
	if got != want {
		t.Errorf("empty input: got %q, want %q", got, want)
	}
}

// TestWorkstreamMapping verifies the linter-to-workstream assignment.
func TestWorkstreamMapping(t *testing.T) {
	cases := []struct {
		linter string
		want   string
	}{
		{"funlen", "W03"},
		{"gocyclo", "W03"},
		{"gocognit", "W03"},
		{"revive", "W06"},
		{"gocritic", "W06"},
		{"errcheck", "W04"},
		{"staticcheck", "W04"},
		{"govet", "W04"},
	}
	for _, c := range cases {
		if got := workstream(c.linter); got != c.want {
			t.Errorf("workstream(%q) = %q, want %q", c.linter, got, c.want)
		}
	}
}

// TestStableText verifies metric-stripping for complexity linters.
func TestStableText(t *testing.T) {
	cases := []struct {
		linter string
		text   string
		want   string
	}{
		{
			"funlen",
			"Function 'RunWorkflow' is too long (120 > 50 lines)",
			"Function 'RunWorkflow'",
		},
		{
			"funlen",
			"Function 'RunWorkflow' has too many statements (60 > 40 stmts)",
			"Function 'RunWorkflow'",
		},
		{
			"gocyclo",
			"cyclomatic complexity 22 of func `runStep` is high (> 15)",
			"`runStep`",
		},
		{
			"gocognit",
			"cognitive complexity 25 is too high for function `myFunc` (> 20)",
			"`myFunc`",
		},
		{
			"revive",
			"exported function Foo should have comment or be unexported",
			"exported function Foo should have comment or be unexported",
		},
	}
	for _, c := range cases {
		if got := stableText(c.linter, c.text); got != c.want {
			t.Errorf("stableText(%q, %q) = %q, want %q", c.linter, c.text, got, c.want)
		}
	}
}

// TestYAMLScalar verifies that strings with special chars are single-quoted.
func TestYAMLScalar(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"simple", "simple"},
		{"Function 'Foo'", "'Function ''Foo'''"},
		{"`runStep`", "'`runStep`'"},
		{"no:quotes:needed", "'no:quotes:needed'"},
		{"internal/pkg/file.go", "internal/pkg/file.go"},
	}
	for _, c := range cases {
		if got := yamlScalar(c.input); got != c.want {
			t.Errorf("yamlScalar(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}
