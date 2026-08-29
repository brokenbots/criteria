package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrintSpec_SpecOnly(t *testing.T) {
	var buf bytes.Buffer
	if err := printSpec(&buf, false); err != nil {
		t.Fatalf("printSpec error: %v", err)
	}
	out := buf.String()
	// Spec must contain the normative section headers
	for _, anchor := range []string{"## Blocks", "## Functions", "## Iteration semantics", "## Outcome model"} {
		if !strings.Contains(out, anchor) {
			t.Errorf("spec output missing expected anchor %q", anchor)
		}
	}
}

func TestPrintSpec_WithPatterns(t *testing.T) {
	var buf bytes.Buffer
	if err := printSpec(&buf, true); err != nil {
		t.Fatalf("printSpec error: %v", err)
	}
	out := buf.String()
	// All eight patterns must appear
	for _, marker := range []string{
		"Pattern: Linear", "Pattern: Branching switch",
		"Pattern: Sequential iteration", "Pattern: Concurrent iteration",
		"Pattern: Subworkflow", "Pattern: Human-in-the-loop",
		"Pattern: Mutable shared state", "Pattern: File-driven",
	} {
		if !strings.Contains(out, marker) {
			t.Errorf("combined output missing pattern marker %q", marker)
		}
	}
}

// TestPrintSpec_AllowToolsDocumentation is a regression test for CRI-28. It
// asserts that the generated language spec and source docs accurately describe
// allow_tools pattern matching semantics.
func TestPrintSpec_AllowToolsDocumentation(t *testing.T) {
	root := filepath.Join("..", "..")

	for _, withPatterns := range []bool{false, true} {
		name := "spec"
		if withPatterns {
			name = "spec-with-patterns"
		}
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := printSpec(&buf, withPatterns); err != nil {
				t.Fatalf("printSpec error: %v", err)
			}
			out := buf.String()

			stepSection := extractMarkdownSection(out, "### `step \"name\" { ... }`", "### `state")
			if stepSection == "" {
				t.Fatal("could not find step block section in spec output")
			}

			allowLine := findAllowToolsLine(stepSection)
			if allowLine == "" {
				t.Fatal("could not find step-level allow_tools row in spec output")
			}
			if strings.Contains(allowLine, "_(no description)_") {
				t.Errorf("step-level allow_tools is still undocumented: %s", allowLine)
			}
			if withPatterns {
				required := []string{
					"does not cross /",
					"An empty or absent list denies all tool requests",
					"The first matching pattern wins",
					"tool:<glob>",
					"shell:<glob>",
					"scopes by adapter/tool kind and arguments",
				}
				for _, r := range required {
					if !strings.Contains(allowLine, r) {
						t.Errorf("allow_tools description missing required text %q\nhave: %s", r, allowLine)
					}
				}
			}
		})
	}

	t.Run("docs/workflow.md", func(t *testing.T) {
		data, err := os.ReadFile(filepath.Join(root, "docs", "workflow.md"))
		if err != nil {
			t.Fatalf("reading docs/workflow.md: %v", err)
		}
		if strings.Contains(string(data), "shell:* permits all shell commands") {
			t.Error("docs/workflow.md still contains the false claim that shell:* permits all shell commands")
		}
	})

	t.Run("internal/adapterhost/policy.go", func(t *testing.T) {
		data, err := os.ReadFile(filepath.Join(root, "internal", "adapterhost", "policy.go"))
		if err != nil {
			t.Fatalf("reading policy.go: %v", err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, `NewPolicy([]string{"shell:*"}`) &&
				strings.Contains(line, "allows any shell command") {
				t.Errorf("policy.go:%d still contains a misleading shell:* example: %s", i+1, line)
			}
		}
	})
}

// extractMarkdownSection returns the substring of md that starts with startMarker
// and ends just before endMarker (or the end of the string if endMarker is absent).
func extractMarkdownSection(md, startMarker, endMarker string) string {
	start := strings.Index(md, startMarker)
	if start < 0 {
		return ""
	}
	end := strings.Index(md[start:], endMarker)
	if end < 0 {
		return md[start:]
	}
	return md[start : start+end]
}

// findAllowToolsLine returns the markdown table row for allow_tools within a
// step block section, or an empty string if none is found.
func findAllowToolsLine(section string) string {
	for _, line := range strings.Split(section, "\n") {
		if strings.Contains(line, "`allow_tools`") {
			return line
		}
	}
	return ""
}
