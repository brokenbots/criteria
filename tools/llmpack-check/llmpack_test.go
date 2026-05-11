package llmpackcheck_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// canonicalFiles lists the 8 expected pattern files in canonical order.
var canonicalFiles = []string{
	"01-linear.md",
	"02-branching-switch.md",
	"03-iteration-for-each.md",
	"04-iteration-parallel.md",
	"05-subworkflow.md",
	"06-approval-and-wait.md",
	"07-shared-variable.md",
	"08-fileset-template.md",
}

// requiredSections lists the ## headings required in order in each pattern file.
var requiredSections = []string{
	"## When to use",
	"## Minimal example",
	"## Key idioms",
	"## Common pitfalls",
	"## See also",
}

const (
	docsDir     = "../../docs/llm"
	examplesDir = "../../examples/llm-pack"
	perFileCap  = 350
	totalCap    = 2800
)

func readDoc(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(docsDir, name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read %s: %v", path, err)
	}
	return string(data)
}

// wordCount returns len(strings.Fields(s)).
func wordCount(s string) int {
	return len(strings.Fields(s))
}

// extractHCLBlock returns the content of the first ```hcl ... ``` fenced block.
func extractHCLBlock(body string) (string, bool) {
	const opener = "```hcl\n"
	const closer = "```"
	start := strings.Index(body, opener)
	if start == -1 {
		return "", false
	}
	inner := body[start+len(opener):]
	end := strings.Index(inner, closer)
	if end == -1 {
		return "", false
	}
	return inner[:end], true
}

// normalizeHCL trims trailing whitespace from each line and drops trailing blank lines.
func normalizeHCL(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t\r")
	}
	// Drop trailing blank lines.
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

// TestPromptPack_FilesPresent asserts that all 8 expected files exist in docs/llm/.
func TestPromptPack_FilesPresent(t *testing.T) {
	for _, name := range canonicalFiles {
		path := filepath.Join(docsDir, name)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("missing expected file: %s", path)
		}
	}
}

// TestPromptPack_PerFileWordBudget asserts each file is <= 350 words.
func TestPromptPack_PerFileWordBudget(t *testing.T) {
	for _, name := range canonicalFiles {
		body := readDoc(t, name)
		count := wordCount(body)
		if count > perFileCap {
			t.Errorf("%s: word count %d exceeds cap %d", name, count, perFileCap)
		}
	}
}

// TestPromptPack_StructureConformance asserts required ## headings appear in order and
// no unexpected ## headings exist.
func TestPromptPack_StructureConformance(t *testing.T) {
	for _, name := range canonicalFiles {
		body := readDoc(t, name)
		lines := strings.Split(body, "\n")

		// Collect all ## headings (not # or ###).
		var found []string
		for _, l := range lines {
			if strings.HasPrefix(l, "## ") {
				found = append(found, strings.TrimSpace(l))
			}
		}

		// Check required sections appear in order.
		cursor := 0
		for _, req := range requiredSections {
			advanced := false
			for cursor < len(found) {
				if found[cursor] == req {
					cursor++
					advanced = true
					break
				}
				cursor++
			}
			if !advanced {
				t.Errorf("%s: missing required section %q", name, req)
			}
		}

		// Check no unexpected ## headings beyond the required set.
		for _, h := range found {
			ok := false
			for _, req := range requiredSections {
				if h == req {
					ok = true
					break
				}
			}
			if !ok {
				t.Errorf("%s: unexpected section heading %q", name, h)
			}
		}
	}
}

// TestPromptPack_HCLMirroredToExamples asserts that the HCL block in each docs/llm/NN-name.md
// matches the corresponding examples/llm-pack/NN-name/main.hcl after normalisation.
func TestPromptPack_HCLMirroredToExamples(t *testing.T) {
	for _, name := range canonicalFiles {
		name := name
		t.Run(name, func(t *testing.T) {
			body := readDoc(t, name)
			hclBlock, ok := extractHCLBlock(body)
			if !ok {
				t.Fatalf("%s: no ```hcl block found", name)
			}

			exampleDir := strings.TrimSuffix(name, ".md")
			examplePath := filepath.Join(examplesDir, exampleDir, "main.hcl")
			exampleData, err := os.ReadFile(examplePath)
			if err != nil {
				t.Fatalf("cannot read example %s: %v", examplePath, err)
			}

			got := normalizeHCL(hclBlock)
			want := normalizeHCL(string(exampleData))
			if got != want {
				t.Errorf("%s: HCL block does not match %s\n--- docs ---\n%s\n--- example ---\n%s",
					name, examplePath, got, want)
			}
		})
	}
}

// TestPromptPack_TotalWordBudget asserts the combined word count of all 8 files is <= 2800.
func TestPromptPack_TotalWordBudget(t *testing.T) {
	total := 0
	for _, name := range canonicalFiles {
		body := readDoc(t, name)
		total += wordCount(body)
	}
	if total > totalCap {
		t.Errorf("total word count %d exceeds cap %d", total, totalCap)
	}
}
