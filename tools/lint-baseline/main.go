// Command lint-baseline reads golangci-lint JSON output and emits a
// .golangci.baseline.yml file containing issues.exclude-rules entries that
// suppress every current finding. Each entry is annotated with a
// workstream-pointer comment so reviewers know which future workstream is
// responsible for removing it.
//
// Usage:
//
//	go run ./tools/lint-baseline -count .golangci.baseline.yml
//
//	go run ./tools/lint-baseline -in .lint-baseline.json -out .golangci.baseline.yml
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// position is the source location reported by golangci-lint.
type position struct {
	Filename string `json:"Filename"`
}

// issue is a single diagnostic from golangci-lint --out-format=json.
type issue struct {
	FromLinter string   `json:"FromLinter"`
	Text       string   `json:"Text"`
	Pos        position `json:"Pos"`
}

// report is the top-level JSON structure emitted by golangci-lint.
type report struct {
	Issues []issue `json:"Issues"`
}

// rule is one generated exclude-rules entry.
type rule struct {
	path    string
	linter  string
	text    string
	comment string
}

// workstream returns the workstream identifier responsible for the given linter.
func workstream(linter string) string {
	switch linter {
	case "funlen", "gocyclo", "gocognit":
		return "W03"
	case "revive", "gocritic":
		return "W06"
	default:
		return "W04"
	}
}

// stableText extracts a regexp-safe stable prefix from the diagnostic text so
// that the baseline entry does not break when a metric value (line count,
// complexity score) changes.
func stableText(linter, text string) string {
	switch linter {
	case "funlen":
		// "Function 'RunWorkflow' is too long (120 > 50 lines)"
		// "Function 'RunWorkflow' has too many statements (60 > 40 stmts)"
		// Stable prefix: "Function 'RunWorkflow'"
		if idx := strings.Index(text, "' is too"); idx >= 0 {
			return text[:idx+1]
		}
		if idx := strings.Index(text, "' has too"); idx >= 0 {
			return text[:idx+1]
		}
	case "gocyclo", "gocognit":
		// "cyclomatic complexity 22 of func `runStep` is high (> 15)"
		// "cognitive complexity 25 is too high for function `runStep` (> 20)"
		// Stable: backtick-delimited function name.
		start := strings.Index(text, "`")
		end := strings.LastIndex(text, "`")
		if start >= 0 && end > start {
			return text[start : end+1]
		}
	}
	return text
}

// hint extracts a short human-readable action suffix for the workstream comment.
func hint(linter, text string) string {
	switch linter {
	case "funlen":
		// Extract function name from "Function 'Name' ..."
		start := strings.Index(text, "'")
		end := strings.LastIndex(text, "'")
		if start >= 0 && end > start {
			return "refactor " + text[start+1:end]
		}
	case "gocyclo", "gocognit":
		// Extract function name from "`Name`"
		start := strings.Index(text, "`")
		end := strings.LastIndex(text, "`")
		if start >= 0 && end > start {
			return "simplify " + text[start+1:end]
		}
	}
	return linter + " finding"
}

// yamlScalar returns a YAML-safe scalar representation of s. Strings that
// contain YAML-special characters are wrapped in single quotes; interior
// single quotes are doubled.
func yamlScalar(s string) string {
	needsQuote := strings.ContainsAny(s, ":#{}'\"[]|&*?-<>!%@`\\,")
	if !needsQuote {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// parseReport reads the golangci-lint JSON report from path.
func parseReport(path string) ([]issue, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var r report
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}
	return r.Issues, nil
}

// buildRules converts the raw issues list into a deduplicated, sorted slice of
// exclude-rules entries. The text field is regexp-quoted so characters like
// '(', '*', ')' and '.' in pointer-receiver method names don't break the
// golangci-lint regexp engine.
func buildRules(issues []issue) []rule {
	seen := make(map[string]bool)
	var rules []rule

	for _, iss := range issues {
		if iss.Pos.Filename == "" {
			continue
		}
		text := stableText(iss.FromLinter, iss.Text)
		// Regexp-escape the pattern so function names with pointer receivers
		// (e.g. (*Engine).runLoop) or dots don't break golangci-lint's
		// regexp engine.
		textRE := regexp.QuoteMeta(text)
		key := iss.Pos.Filename + "\x00" + iss.FromLinter + "\x00" + textRE
		if seen[key] {
			continue
		}
		seen[key] = true

		ws := workstream(iss.FromLinter)
		h := hint(iss.FromLinter, iss.Text)
		rules = append(rules, rule{
			path:    iss.Pos.Filename,
			linter:  iss.FromLinter,
			text:    textRE,
			comment: ws + ": " + h,
		})
	}

	// Deterministic output: sort by (path, linter, text).
	sort.Slice(rules, func(i, j int) bool {
		if rules[i].path != rules[j].path {
			return rules[i].path < rules[j].path
		}
		if rules[i].linter != rules[j].linter {
			return rules[i].linter < rules[j].linter
		}
		return rules[i].text < rules[j].text
	})
	return rules
}

// renderYAML produces the YAML text for the baseline file.
func renderYAML(rules []rule) string {
	var sb strings.Builder
	sb.WriteString("issues:\n")
	sb.WriteString("  exclude-rules:\n")
	for _, r := range rules {
		fmt.Fprintf(&sb, "    - path: %s\n", yamlScalar(r.path))
		fmt.Fprintf(&sb, "      linters:\n        - %s\n", r.linter)
		fmt.Fprintf(&sb, "      text: %s # %s\n", yamlScalar(r.text), r.comment)
	}
	return sb.String()
}

// countBaselineRules counts top-level exclude-rules entries from a baseline file.
func countBaselineRules(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}

	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		// Baseline entries are top-level exclude-rules items rendered as:
		// "    - path: <file>".
		if strings.HasPrefix(line, "    - path:") {
			count++
		}
	}
	return count, nil
}

func main() {
	inFile := flag.String("in", "", "input JSON from golangci-lint --out-format=json")
	outFile := flag.String("out", "", "output YAML baseline file")
	countFile := flag.String("count", "", "count baseline entries in a .golangci.baseline.yml file")
	flag.Parse()

	if *countFile != "" {
		if *inFile != "" || *outFile != "" {
			fmt.Fprintln(os.Stderr, "usage: lint-baseline -count <baseline.yml> | -in <input.json> -out <output.yml>")
			os.Exit(1)
		}
		count, err := countBaselineRules(*countFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stdout, count)
		return
	}

	if *inFile == "" || *outFile == "" {
		fmt.Fprintln(os.Stderr, "usage: lint-baseline -count <baseline.yml> | -in <input.json> -out <output.yml>")
		os.Exit(1)
	}

	issues, err := parseReport(*inFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	rules := buildRules(issues)
	yaml := renderYAML(rules)

	if err := os.WriteFile(*outFile, []byte(yaml), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", *outFile, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stdout, "wrote %d baseline rules to %s\n", len(rules), *outFile)
}
