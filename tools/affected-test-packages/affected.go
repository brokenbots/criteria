package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// PackageInfo holds metadata about a Go package extracted from `go list -json`.
type PackageInfo struct {
	ImportPath   string   `json:"ImportPath"`
	Dir          string   `json:"Dir"`
	GoFiles      []string `json:"GoFiles"`
	TestGoFiles  []string `json:"TestGoFiles"`
	XTestGoFiles []string `json:"XTestGoFiles"`
	Imports      []string `json:"Imports"`
	TestImports  []string `json:"TestImports"`
	XTestImports []string `json:"XTestImports"`
}

// HasTests reports whether the package has in-package or external tests.
func (p *PackageInfo) HasTests() bool {
	return len(p.TestGoFiles) > 0 || len(p.XTestGoFiles) > 0
}

// AllImports returns the unique set of packages imported by source and tests.
func (p *PackageInfo) AllImports() []string {
	seen := make(map[string]struct{}, len(p.Imports)+len(p.TestImports)+len(p.XTestImports))
	for _, imp := range p.Imports {
		seen[imp] = struct{}{}
	}
	for _, imp := range p.TestImports {
		seen[imp] = struct{}{}
	}
	for _, imp := range p.XTestImports {
		seen[imp] = struct{}{}
	}
	res := make([]string, 0, len(seen))
	for imp := range seen {
		res = append(res, imp)
	}
	sort.Strings(res)
	return res
}

// AffectedResult holds the analysis output.
type AffectedResult struct {
	All                 bool     `json:"all"`
	DirectlyChanged     []string `json:"directly_changed"`
	Affected            []string `json:"affected"`
	Testable            []string `json:"testable"`
	ConformanceAffected bool     `json:"conformance_affected"`
}

// LoadWorkspacePackages runs `go list -json` across all workspace modules and returns the loaded packages.
func LoadWorkspacePackages(root string) ([]*PackageInfo, error) {
	cmd := exec.Command("go", "list", "-json", "./...", "./sdk/...", "./workflow/...", "./tools/...")
	cmd.Dir = root
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start go list: %w", err)
	}

	var pkgs []*PackageInfo
	decoder := json.NewDecoder(stdout)
	for {
		var pkg PackageInfo
		if err := decoder.Decode(&pkg); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("decode go list json: %w", err)
		}
		pkgs = append(pkgs, &pkg)
	}

	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("go list wait: %w", err)
	}
	return pkgs, nil
}

// isNonCodeFile reports whether a changed file should be ignored from Go package dependency triggering.
func isNonCodeFile(relPath string) bool {
	clean := filepath.ToSlash(relPath)

	// Markdown, licenses, plain text docs
	if strings.HasSuffix(clean, ".md") || strings.HasSuffix(clean, ".txt") {
		// tools/coverage-floors.txt is checked separately; don't trigger test runs if only floors changed
		return true
	}
	if clean == "LICENSE" || strings.HasPrefix(clean, "docs/") || strings.HasPrefix(clean, "examples/") {
		return true
	}
	if strings.HasPrefix(clean, ".github/") {
		// Only ci.yml or workflows might affect tests, but workflow edits themselves don't change Go code
		return true
	}
	if strings.HasSuffix(clean, ".png") || strings.HasSuffix(clean, ".svg") || strings.HasSuffix(clean, ".jpg") {
		return true
	}
	if strings.HasSuffix(clean, ".gitignore") {
		return true
	}
	return false
}

// isGlobalTrigger reports whether changing a file requires testing all packages in the workspace.
func isGlobalTrigger(relPath string) bool {
	clean := filepath.ToSlash(relPath)
	switch clean {
	case "go.work", "go.work.sum", "go.mod", "go.sum", "Makefile":
		return true
	}
	if strings.HasPrefix(clean, "tools/affected-test-packages/") {
		return true
	}
	return false
}

// ComputeAffected calculates the affected packages given workspace packages and a list of changed repo-relative files.
func ComputeAffected(root string, pkgs []*PackageInfo, changedFiles []string, forceAll bool) *AffectedResult {
	if forceAll {
		return allResult(pkgs)
	}

	// Index packages by import path and normalized directory
	byImport := make(map[string]*PackageInfo, len(pkgs))
	byDir := make(map[string]*PackageInfo, len(pkgs))
	reverseDeps := make(map[string][]string) // dependency import path -> list of dependent import paths

	for _, p := range pkgs {
		byImport[p.ImportPath] = p
		relDir, err := filepath.Rel(root, p.Dir)
		if err == nil {
			byDir[filepath.ToSlash(relDir)] = p
		} else {
			byDir[filepath.ToSlash(p.Dir)] = p
		}
	}

	for _, p := range pkgs {
		for _, dep := range p.AllImports() {
			reverseDeps[dep] = append(reverseDeps[dep], p.ImportPath)
		}
	}

	directlyChanged := make(map[string]struct{})

	for _, file := range changedFiles {
		clean := filepath.ToSlash(file)
		if clean == "" {
			continue
		}

		// 1. Global build triggers
		if isGlobalTrigger(clean) {
			return allResult(pkgs)
		}

		// 2. Module manifest triggers
		if clean == "sdk/go.mod" || clean == "sdk/go.sum" {
			for _, p := range pkgs {
				if strings.HasPrefix(p.ImportPath, "github.com/brokenbots/criteria/sdk") {
					directlyChanged[p.ImportPath] = struct{}{}
				}
			}
			continue
		}
		if clean == "workflow/go.mod" || clean == "workflow/go.sum" {
			for _, p := range pkgs {
				if strings.HasPrefix(p.ImportPath, "github.com/brokenbots/criteria/workflow") {
					directlyChanged[p.ImportPath] = struct{}{}
				}
			}
			continue
		}
		if clean == "tools/go.mod" || clean == "tools/go.sum" {
			for _, p := range pkgs {
				if strings.HasPrefix(p.ImportPath, "github.com/brokenbots/criteria/tools") {
					directlyChanged[p.ImportPath] = struct{}{}
				}
			}
			continue
		}

		// 3. Ignore non-code files
		if isNonCodeFile(clean) {
			continue
		}

		// 4. Find the package enclosing this file
		dir := filepath.ToSlash(filepath.Dir(clean))
		matched := false
		for d := dir; d != "." && d != "/" && d != ""; d = filepath.ToSlash(filepath.Dir(d)) {
			if pkg, ok := byDir[d]; ok {
				directlyChanged[pkg.ImportPath] = struct{}{}
				matched = true
				break
			}
		}
		if !matched {
			if pkg, ok := byDir["."]; ok && dir == "." {
				directlyChanged[pkg.ImportPath] = struct{}{}
			}
		}
	}

	if len(directlyChanged) == 0 {
		return &AffectedResult{
			All:                 false,
			DirectlyChanged:     nil,
			Affected:            nil,
			Testable:            nil,
			ConformanceAffected: false,
		}
	}

	// Transitive closure: propagate downstream to dependent packages
	affectedMap := make(map[string]struct{})
	queue := make([]string, 0, len(directlyChanged))
	directList := make([]string, 0, len(directlyChanged))

	for imp := range directlyChanged {
		affectedMap[imp] = struct{}{}
		queue = append(queue, imp)
		directList = append(directList, imp)
	}
	sort.Strings(directList)

	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]

		for _, dep := range reverseDeps[curr] {
			if _, seen := affectedMap[dep]; !seen {
				affectedMap[dep] = struct{}{}
				queue = append(queue, dep)
			}
		}
	}

	affectedList := make([]string, 0, len(affectedMap))
	testableList := make([]string, 0, len(affectedMap))
	conformanceAffected := false

	for imp := range affectedMap {
		affectedList = append(affectedList, imp)
		if pkg, ok := byImport[imp]; ok && pkg.HasTests() {
			testableList = append(testableList, imp)
		}
		if isConformanceCritical(imp) {
			conformanceAffected = true
		}
	}
	sort.Strings(affectedList)
	sort.Strings(testableList)

	return &AffectedResult{
		All:                 false,
		DirectlyChanged:     directList,
		Affected:            affectedList,
		Testable:            testableList,
		ConformanceAffected: conformanceAffected,
	}
}

func isConformanceCritical(importPath string) bool {
	if strings.HasPrefix(importPath, "github.com/brokenbots/criteria/sdk") {
		return true
	}
	if strings.HasPrefix(importPath, "github.com/brokenbots/criteria/workflow") {
		return true
	}
	if strings.HasPrefix(importPath, "github.com/brokenbots/criteria/internal/adapter") {
		return true
	}
	return false
}

func allResult(pkgs []*PackageInfo) *AffectedResult {
	affected := make([]string, 0, len(pkgs))
	testable := make([]string, 0, len(pkgs))
	for _, p := range pkgs {
		affected = append(affected, p.ImportPath)
		if p.HasTests() {
			testable = append(testable, p.ImportPath)
		}
	}
	sort.Strings(affected)
	sort.Strings(testable)

	return &AffectedResult{
		All:                 true,
		DirectlyChanged:     affected,
		Affected:            affected,
		Testable:            testable,
		ConformanceAffected: true,
	}
}
