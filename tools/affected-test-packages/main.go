package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	baseRef := flag.String("base", "", "git base ref to diff against (e.g. origin/main, main)")
	forceAll := flag.Bool("all", false, "force all packages to be considered affected")
	filesFlag := flag.String("files", "", "comma-separated list of changed files (bypasses git diff)")
	format := flag.String("format", "space", "output format: space, lines, json")
	conformanceOnly := flag.Bool("conformance", false, "output only true/false if SDK conformance tests are affected")
	onlyTestable := flag.Bool("only-testable", true, "filter output to packages that contain test files")
	includeUncommitted := flag.Bool("include-uncommitted", true, "include unstaged and untracked files")
	rootPath := flag.String("root", ".", "workspace root directory")
	flag.Parse()

	absRoot, err := filepath.Abs(*rootPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: resolve root %q: %v\n", *rootPath, err)
		os.Exit(2)
	}

	pkgs, err := LoadWorkspacePackages(absRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: load workspace packages: %v\n", err)
		os.Exit(2)
	}

	var changedFiles []string
	if *filesFlag != "" {
		for _, f := range strings.Split(*filesFlag, ",") {
			f = strings.TrimSpace(f)
			if f != "" {
				changedFiles = append(changedFiles, f)
			}
		}
	} else if !*forceAll {
		base := *baseRef
		if base == "" {
			// Auto-detect base: prefer origin/main, then main, then HEAD~1
			if _, err := runGit(absRoot, "rev-parse", "--verify", "origin/main"); err == nil {
				base = "origin/main"
			} else if _, err := runGit(absRoot, "rev-parse", "--verify", "main"); err == nil {
				base = "main"
			} else {
				base = "HEAD~1"
			}
		}
		files, err := GetChangedFiles(absRoot, base, *includeUncommitted)
		if err != nil {
			fmt.Fprintf(os.Stderr, "WARNING: git diff failed: %v; falling back to all packages\n", err)
			*forceAll = true
		} else {
			changedFiles = files
		}
	}

	result := ComputeAffected(absRoot, pkgs, changedFiles, *forceAll)

	if *conformanceOnly {
		if result.ConformanceAffected {
			fmt.Println("true")
		} else {
			fmt.Println("false")
		}
		return
	}

	targets := result.Affected
	if *onlyTestable {
		targets = result.Testable
	}

	switch *format {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(result); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: encode json: %v\n", err)
			os.Exit(2)
		}
	case "lines":
		for _, pkg := range targets {
			fmt.Println(pkg)
		}
	case "space":
		if len(targets) > 0 {
			fmt.Println(strings.Join(targets, " "))
		}
	default:
		fmt.Fprintf(os.Stderr, "ERROR: unknown format %q (valid: space, lines, json)\n", *format)
		os.Exit(2)
	}
}
