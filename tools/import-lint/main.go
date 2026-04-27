// Command import-lint enforces the import-graph boundaries for the overseer
// repository. It walks Go source files under the given root directory and
// fails with a non-zero exit code when a disallowed import pattern is found.
//
// Rules enforced:
//
//  1. No file in internal/ may import github.com/brokenbots/overseer/sdk
//     top-level (only sdk/pb subtree and sdk/pluginhost are permitted).
//  2. No file in workflow/ may import github.com/brokenbots/overseer/internal/.
//
// Usage:
//
//	import-lint <repo-root>
//
// A forbidden import can be suppressed by adding an inline comment on the
// same line as the import statement:
//
//	import _ "github.com/brokenbots/overseer/sdk" // import-lint:allow <reason>
//
// Use sparingly. Every suppressed import is a documented exception to the
// boundary rules and should include a brief reason.
package main

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// rule describes a single forbidden import pattern.
type rule struct {
	// filePrefix is the repo-relative path prefix that is subject to this rule.
	filePrefix string
	// forbidden is the import path substring that is disallowed.
	forbidden string
	// message is included in the error output.
	message string
}

var rules = []rule{
	{
		filePrefix: "internal/",
		forbidden:  "github.com/brokenbots/overseer/sdk",
		message:    "internal/ must not import sdk/ top-level; only sdk/pb subtree is permitted",
	},
	{
		filePrefix: "workflow/",
		forbidden:  "github.com/brokenbots/overseer/internal/",
		message:    "workflow/ must not import internal/",
	},
}

// violation holds a single rule breach.
type violation struct {
	file    string
	line    int
	imp     string
	message string
}

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" {
		fmt.Fprintln(os.Stderr, "usage: import-lint <repo-root>")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Walks all .go files under <repo-root> and reports import-graph violations.")
		fmt.Fprintln(os.Stderr, "Exits 0 if clean, 1 if violations found, 2 on usage error.")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "To suppress a specific import, add an inline comment on the import line:")
		fmt.Fprintln(os.Stderr, `  import _ "github.com/.../pkg" // import-lint:allow <reason>`)
		fmt.Fprintln(os.Stderr, "Use sparingly; every suppressed import is a documented exception.")
		if len(os.Args) < 2 {
			os.Exit(2)
		}
		return
	}
	root := os.Args[1]

	violations, err := lint(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	for _, v := range violations {
		fmt.Printf("%s:%d: %s (import %q)\n", v.file, v.line, v.message, v.imp)
	}
	if len(violations) > 0 {
		os.Exit(1)
	}
}

// lint walks all .go files under root and returns every violation found.
func lint(root string) ([]violation, error) {
	var violations []violation
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == "vendor" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)

		vs, err := checkFile(path, rel)
		if err != nil {
			return err
		}
		violations = append(violations, vs...)
		return nil
	})
	return violations, err
}

// checkFile parses one Go file and returns any rule violations.
// An import annotated with `// import-lint:allow` on the same line is exempt.
func checkFile(absPath, relPath string) ([]violation, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, absPath, nil, parser.ParseComments)
	if err != nil {
		// Unparseable files are skipped (generated files, syntax errors in tests).
		return nil, nil //nolint:nilerr
	}

	var violations []violation
outer:
	for _, imp := range f.Imports {
		impPath, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}

		// An inline `// import-lint:allow` comment suppresses this import.
		if imp.Comment != nil {
			for _, c := range imp.Comment.List {
				if strings.Contains(c.Text, "import-lint:allow") {
					continue outer
				}
			}
		}

		for _, r := range rules {
			if !strings.HasPrefix(relPath, r.filePrefix) {
				continue
			}
			// For the sdk rule: allow sdk/pb subtree, allow sdk/pluginhost (the
			// public plugin author surface used by test fixtures), block everything else.
			if r.filePrefix == "internal/" && strings.Contains(impPath, "github.com/brokenbots/overseer/sdk") {
				if strings.Contains(impPath, "github.com/brokenbots/overseer/sdk/pb") {
					continue // sdk/pb subtree is permitted
				}
				if strings.Contains(impPath, "github.com/brokenbots/overseer/sdk/pluginhost") {
					continue // sdk/pluginhost is the public plugin author surface; test fixtures use it
				}
			}
			if strings.Contains(impPath, r.forbidden) || impPath == strings.TrimSuffix(r.forbidden, "/") {
				pos := fset.Position(imp.Path.Pos())
				violations = append(violations, violation{
					file:    relPath,
					line:    pos.Line,
					imp:     impPath,
					message: r.message,
				})
			}
		}
	}
	return violations, nil
}
