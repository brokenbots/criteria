// Package llmpackcheck guards the docs/llm/ prompt pack for structural
// conformance, word-budget compliance, and drift against the example HCL files
// under examples/llm-pack/.
package llmpackcheck

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// repoRoot returns the absolute path to the repository root, which is two
// directories above this test file (tools/llmpack-check/ → repo root).
func repoRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("cannot resolve repo root: %v", err)
	}
	return abs
}

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

// requiredHeaders lists the level-2 headers every pattern file must contain,
// in this exact order, with no others.
var requiredHeaders = []string{
	"## When to use",
	"## Minimal example",
	"## Key idioms",
	"## Common pitfalls",
	"## See also",
}

// readmeHeaders lists the level-2 headers README.md must contain, in order.
var readmeHeaders = []string{
	"## How to assemble the prompt",
	"## Pattern index",
	"## Maintenance",
}

// TestPromptPack_ExactFileSet asserts docs/llm/ contains exactly README.md
// plus the 8 canonical pattern files — no extras, no missing, no renames.
func TestPromptPack_ExactFileSet(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, "docs", "llm")

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("cannot read %s: %v", dir, err)
	}

	want := make(map[string]bool)
	want["README.md"] = true
	for _, name := range canonicalFiles {
		want[name] = true
	}

	var got []string
	for _, e := range entries {
		if !e.IsDir() {
			got = append(got, e.Name())
		}
	}
	sort.Strings(got)

	// Report missing.
	for name := range want {
		found := false
		for _, g := range got {
			if g == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing expected file: docs/llm/%s", name)
		}
	}

	// Report extras.
	for _, name := range got {
		if !want[name] {
			t.Errorf("unexpected file in docs/llm/: %s", name)
		}
	}

	if len(got) != len(want) {
		t.Errorf("docs/llm/ has %d files; want exactly %d", len(got), len(want))
	}
}

// TestPromptPack_READMEConformance asserts docs/llm/README.md has the correct
// title, section order, no extra ## sections, and stays within the word budget.
func TestPromptPack_READMEConformance(t *testing.T) {
	root := repoRoot(t)
	path := filepath.Join(root, "docs", "llm", "README.md")
	body := readFile(t, path)
	lines := strings.Split(body, "\n")

	// Title check.
	if len(lines) == 0 || lines[0] != "# Criteria LLM Prompt Pack" {
		t.Errorf("README.md: first line must be '# Criteria LLM Prompt Pack'; got %q", firstLine(lines))
	}

	// Collect all ## headers.
	var foundHeaders []string
	for _, line := range lines {
		if strings.HasPrefix(line, "## ") {
			foundHeaders = append(foundHeaders, strings.TrimRight(line, " \t"))
		}
	}

	// Exact count and order.
	if len(foundHeaders) != len(readmeHeaders) {
		t.Errorf("README.md: found %d `##`-level headers, want exactly %d", len(foundHeaders), len(readmeHeaders))
		t.Logf("  found: %v", foundHeaders)
		t.Logf("  want:  %v", readmeHeaders)
	} else {
		for i, want := range readmeHeaders {
			if foundHeaders[i] != want {
				t.Errorf("README.md: header[%d] = %q; want %q", i, foundHeaders[i], want)
			}
		}
	}

	// Word budget.
	count := len(strings.Fields(body))
	if count > 250 {
		t.Errorf("README.md: %d words; must be ≤ 250", count)
	} else {
		t.Logf("README.md: %d / 250 words", count)
	}
}

// TestPromptPack_PerFileWordBudget asserts each pattern file has ≤ 350 words.
func TestPromptPack_PerFileWordBudget(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, "docs", "llm")

	for _, name := range canonicalFiles {
		name := name
		t.Run(name, func(t *testing.T) {
			body := readFile(t, filepath.Join(dir, name))
			count := len(strings.Fields(body))
			if count > 350 {
				t.Errorf("%s: %d words; must be ≤ 350", name, count)
			} else {
				t.Logf("%s: %d / 350 words", name, count)
			}
		})
	}
}

// TestPromptPack_TotalWordBudget asserts that the combined word count of all
// 8 pattern files is ≤ 2800 (≈ 4,000 tokens).
func TestPromptPack_TotalWordBudget(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, "docs", "llm")

	var total int
	for _, name := range canonicalFiles {
		body := readFile(t, filepath.Join(dir, name))
		total += len(strings.Fields(body))
	}
	if total > 2800 {
		t.Errorf("combined word count = %d; must be ≤ 2800", total)
	} else {
		t.Logf("combined word count: %d / 2800", total)
	}
}

// TestPromptPack_StructureConformance asserts that each pattern file:
//   - starts with a `# Pattern:` heading,
//   - contains the five required `##`-level sections in order,
//   - contains no extra `## ` sections beyond the required ones.
func TestPromptPack_StructureConformance(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, "docs", "llm")

	for _, name := range canonicalFiles {
		name := name
		t.Run(name, func(t *testing.T) {
			body := readFile(t, filepath.Join(dir, name))
			lines := strings.Split(body, "\n")

			// Check top-level `# Pattern:` heading.
			if len(lines) == 0 || !strings.HasPrefix(lines[0], "# Pattern:") {
				t.Errorf("%s: first line must start with '# Pattern:'; got %q", name, firstLine(lines))
			}

			// Collect all `## ` headers in order.
			var foundHeaders []string
			for _, line := range lines {
				if strings.HasPrefix(line, "## ") {
					foundHeaders = append(foundHeaders, strings.TrimRight(line, " \t"))
				}
			}

			// Check exact count and order.
			if len(foundHeaders) != len(requiredHeaders) {
				t.Errorf("%s: found %d `##`-level headers, want exactly %d", name, len(foundHeaders), len(requiredHeaders))
				t.Logf("  found: %v", foundHeaders)
				t.Logf("  want:  %v", requiredHeaders)
				return
			}
			for i, want := range requiredHeaders {
				if foundHeaders[i] != want {
					t.Errorf("%s: header[%d] = %q; want %q", name, i, foundHeaders[i], want)
				}
			}
		})
	}
}

// TestPromptPack_HCLMirroredToExamples extracts the HCL block from each
// docs/llm/NN-name.md and compares it byte-for-byte (after trailing-whitespace
// normalisation) against examples/llm-pack/NN-name/main.hcl.
func TestPromptPack_HCLMirroredToExamples(t *testing.T) {
	root := repoRoot(t)
	docsDir := filepath.Join(root, "docs", "llm")
	examplesDir := filepath.Join(root, "examples", "llm-pack")

	for _, name := range canonicalFiles {
		name := name
		t.Run(name, func(t *testing.T) {
			// docs/llm/NN-name.md → examples/llm-pack/NN-name/main.hcl
			base := strings.TrimSuffix(name, ".md")
			examplePath := filepath.Join(examplesDir, base, "main.hcl")

			mdBody := readFile(t, filepath.Join(docsDir, name))
			hclFromMD := extractHCLBlock(t, name, mdBody)
			hclFromFile := readFile(t, examplePath)

			norm := normaliseTrailingWhitespace
			if norm(hclFromMD) != norm(hclFromFile) {
				t.Errorf("%s: HCL in markdown does not match %s\n--- markdown HCL ---\n%s\n--- file HCL ---\n%s",
					name, examplePath, hclFromMD, hclFromFile)
			}
		})
	}
}

// allowedExampleFiles is the explicit allowlist of every file permitted to
// exist under examples/llm-pack/ (relative paths from that directory).
// Any file not in this list causes TestExamplesLLMPack_NoOrphanFiles to fail.
// New fixtures must be added here deliberately.
var allowedExampleFiles = func() map[string]bool {
	m := map[string]bool{
		// Child workflow fixture for example 05.
		filepath.Join("05-subworkflow", "child", "main.hcl"): true,
		// Prompt fixtures for example 08.
		filepath.Join("08-fileset-template", "prompts", "alpha.md"): true,
		filepath.Join("08-fileset-template", "prompts", "beta.md"):  true,
	}
	// The 8 canonical main.hcl mirrors.
	for _, name := range canonicalFiles {
		base := strings.TrimSuffix(name, ".md")
		m[filepath.Join(base, "main.hcl")] = true
	}
	return m
}()

// TestExamplesLLMPack_NoOrphanFiles walks examples/llm-pack/ and fails if any
// file is not in the explicit allowedExampleFiles allowlist.  This prevents
// stale fixture files from accumulating silently.
func TestExamplesLLMPack_NoOrphanFiles(t *testing.T) {
	root := repoRoot(t)
	examplesDir := filepath.Join(root, "examples", "llm-pack")

	err := filepath.WalkDir(examplesDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(examplesDir, path)
		if relErr != nil {
			t.Errorf("cannot make %s relative: %v", path, relErr)
			return nil
		}
		if !allowedExampleFiles[rel] {
			t.Errorf("unexpected file in examples/llm-pack/: %s (add to allowedExampleFiles if intentional)", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("error walking %s: %v", examplesDir, err)
	}

	// Also verify every expected file actually exists (catch renames/deletions).
	for rel := range allowedExampleFiles {
		full := filepath.Join(examplesDir, rel)
		if _, statErr := os.Stat(full); os.IsNotExist(statErr) {
			t.Errorf("allowlisted file missing from examples/llm-pack/: %s", rel)
		}
	}
}

// extractHCLBlock extracts the content of the first ```hcl ... ``` fenced
// block from the given markdown body.
func extractHCLBlock(t *testing.T, filename, body string) string {
	t.Helper()
	const fence = "```"
	lines := strings.Split(body, "\n")
	var inBlock bool
	var result []string
	for _, line := range lines {
		if !inBlock {
			if line == fence+"hcl" {
				inBlock = true
			}
			continue
		}
		if line == fence {
			return strings.Join(result, "\n") + "\n"
		}
		result = append(result, line)
	}
	t.Fatalf("%s: no ```hcl ... ``` block found", filename)
	return ""
}

// normaliseTrailingWhitespace strips trailing spaces/tabs from every line.
func normaliseTrailingWhitespace(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}
	return strings.Join(lines, "\n")
}

// readFile reads a file and returns its content; it fails the test on error.
func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read %s: %v", path, err)
	}
	return string(data)
}

// firstLine returns the first element of a slice or an empty string.
func firstLine(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return lines[0]
}

