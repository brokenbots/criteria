// Command spec-gen generates and validates the docs/LANGUAGE-SPEC.md reference
// tables from workflow/schema.go, workflow/eval_functions.go, and workflow/eval.go.
//
// Usage:
//
//	spec-gen [-check] [-out docs/LANGUAGE-SPEC.md]
//
// Default mode regenerates the file; -check exits non-zero with a diff when the
// file would change. Used by CI to detect spec drift.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the testable entry-point. It returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("spec-gen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	check := fs.Bool("check", false, "check mode: fail if spec is out of date")
	out := fs.String("out", "docs/LANGUAGE-SPEC.md", "path to the spec file")
	schemaFile := fs.String("schema", "workflow/schema.go", "path to schema.go")
	functionsFile := fs.String("functions", "workflow/eval_functions.go", "path to eval_functions.go")
	evalFile := fs.String("eval", "workflow/eval.go", "path to eval.go for namespace extraction")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	blocks, err := extractBlocks(*schemaFile)
	if err != nil {
		fmt.Fprintf(stderr, "spec-gen: error extracting blocks: %v\n", err)
		return 1
	}
	funcs, err := extractFunctions(*functionsFile)
	if err != nil {
		fmt.Fprintf(stderr, "spec-gen: error extracting functions: %v\n", err)
		return 1
	}
	namespaces, err := extractNamespaces(*evalFile)
	if err != nil {
		fmt.Fprintf(stderr, "spec-gen: error extracting namespaces: %v\n", err)
		return 1
	}

	data, err := os.ReadFile(*out)
	if err != nil {
		fmt.Fprintf(stderr, "spec-gen: error reading %s: %v\n", *out, err)
		return 1
	}
	current := string(data)

	updated, err := replaceMarkers(current, blocks, funcs, namespaces, *schemaFile, *functionsFile)
	if err != nil {
		fmt.Fprintf(stderr, "spec-gen: %v\n", err)
		return 1
	}

	if *check {
		if current == updated {
			fmt.Fprintln(stdout, "spec-check: OK")
			return 0
		}
		fmt.Fprintf(stderr, "spec-check: FAIL — spec is out of date; run `make spec-gen` to regenerate\n\n")
		fmt.Fprint(stderr, computeDiff(current, updated))
		return 1
	}

	if err := os.WriteFile(*out, []byte(updated), 0o644); err != nil {
		fmt.Fprintf(stderr, "spec-gen: error writing %s: %v\n", *out, err)
		return 1
	}
	fmt.Fprintf(stdout, "spec-gen: wrote %s\n", *out)
	return 0
}

var markerNames = []string{"blocks", "functions", "namespaces"}

const (
	beginPrefix  = "<!-- BEGIN GENERATED:"
	endPrefix    = "<!-- END GENERATED:"
	markerSuffix = " -->"
)

// replaceMarkers replaces the content between each marker pair in content with
// freshly rendered output. Content outside markers is preserved byte-for-byte.
func replaceMarkers(content string, blocks []BlockDoc, funcs []FuncDoc, namespaces []NamespaceDoc, schemaFile, functionsFile string) (string, error) {
	// Validate all marker pairs are present and balanced.
	for _, name := range markerNames {
		begin := beginPrefix + name + markerSuffix
		end := endPrefix + name + markerSuffix

		bi := strings.Index(content, begin)
		ei := strings.Index(content, end)

		switch {
		case bi < 0 && ei < 0:
			return "", fmt.Errorf("marker pair %q / %q missing from spec file", begin, end)
		case bi < 0:
			return "", fmt.Errorf("found %q but %q is missing", end, begin)
		case ei < 0:
			return "", fmt.Errorf("found %q but %q is missing", begin, end)
		case ei < bi:
			return "", fmt.Errorf("%q appears before %q (unbalanced markers)", end, begin)
		}
	}

	// Check for nested markers: no BEGIN should appear between a BEGIN and its END.
	// This includes same-name nesting (e.g. BEGIN blocks inside BEGIN blocks).
	for _, name := range markerNames {
		begin := beginPrefix + name + markerSuffix
		end := endPrefix + name + markerSuffix
		bi := strings.Index(content, begin)
		ei := strings.Index(content, end)
		inner := content[bi+len(begin) : ei]
		for _, other := range markerNames {
			otherBegin := beginPrefix + other + markerSuffix
			if strings.Contains(inner, otherBegin) {
				return "", fmt.Errorf("marker %q is nested inside %q (markers must not overlap)", otherBegin, begin)
			}
		}
	}

	generated := map[string]string{
		"blocks":     renderBlocks(blocks, schemaFile),
		"functions":  renderFunctions(funcs, functionsFile),
		"namespaces": renderNamespaces(namespaces),
	}

	result := content
	for _, name := range markerNames {
		begin := beginPrefix + name + markerSuffix
		end := endPrefix + name + markerSuffix

		bi := strings.Index(result, begin)
		// Advance past the begin marker line.
		afterBegin := bi + len(begin)
		// Skip the newline immediately after the marker.
		if afterBegin < len(result) && result[afterBegin] == '\n' {
			afterBegin++
		}
		ei := strings.Index(result, end)

		newBody := generated[name]
		if newBody != "" {
			result = result[:afterBegin] + newBody + "\n" + result[ei:]
		} else {
			result = result[:afterBegin] + result[ei:]
		}
	}
	return result, nil
}

// computeDiff returns a simple line-by-line unified diff using LCS.
func computeDiff(oldContent, newContent string) string {
	if oldContent == newContent {
		return ""
	}
	old := strings.Split(oldContent, "\n")
	nw := strings.Split(newContent, "\n")

	m, n := len(old), len(nw)

	// LCS table.
	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
	}
	for i := m - 1; i >= 0; i-- {
		for j := n - 1; j >= 0; j-- {
			if old[i] == nw[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] > dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}

	var sb strings.Builder
	sb.WriteString("--- current\n+++ expected\n")

	i, j := 0, 0
	for i < m || j < n {
		switch {
		case i < m && j < n && old[i] == nw[j]:
			i++
			j++
		case j < n && (i >= m || dp[i][j+1] >= dp[i+1][j]):
			fmt.Fprintf(&sb, "+%s\n", nw[j])
			j++
		default:
			fmt.Fprintf(&sb, "-%s\n", old[i])
			i++
		}
	}
	return sb.String()
}
