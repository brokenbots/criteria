package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// GetChangedFiles runs git diff commands to detect all files changed between baseRef and HEAD,
// and optionally includes uncommitted and untracked files in the working directory.
func GetChangedFiles(root, baseRef string, includeUncommitted bool) ([]string, error) {
	seen := make(map[string]struct{})

	// 1. Git diff against baseRef
	if baseRef != "" {
		out, err := runGit(root, "diff", "--name-only", baseRef+"...HEAD")
		if err != nil {
			// Fallback: direct two-dot diff or single-ref diff
			out, err = runGit(root, "diff", "--name-only", baseRef, "HEAD")
			if err != nil {
				out, err = runGit(root, "diff", "--name-only", baseRef)
			}
		}
		if err == nil {
			for _, line := range strings.Split(out, "\n") {
				line = strings.TrimSpace(line)
				if line != "" {
					seen[filepath.ToSlash(line)] = struct{}{}
				}
			}
		}
	}

	// 2. Local uncommitted changes (unstaged, staged, untracked)
	if includeUncommitted {
		// Unstaged changes
		if out, err := runGit(root, "diff", "--name-only"); err == nil {
			for _, line := range strings.Split(out, "\n") {
				line = strings.TrimSpace(line)
				if line != "" {
					seen[filepath.ToSlash(line)] = struct{}{}
				}
			}
		}
		// Staged changes
		if out, err := runGit(root, "diff", "--name-only", "--cached"); err == nil {
			for _, line := range strings.Split(out, "\n") {
				line = strings.TrimSpace(line)
				if line != "" {
					seen[filepath.ToSlash(line)] = struct{}{}
				}
			}
		}
		// Untracked files
		if out, err := runGit(root, "status", "--porcelain"); err == nil {
			for _, line := range strings.Split(out, "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "?? ") {
					file := strings.TrimPrefix(line, "?? ")
					seen[filepath.ToSlash(file)] = struct{}{}
				}
			}
		}
	}

	files := make([]string, 0, len(seen))
	for f := range seen {
		files = append(files, f)
	}
	return files, nil
}

func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
