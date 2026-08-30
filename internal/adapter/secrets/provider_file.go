package secrets

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FileProvider resolves secrets by reading files from a confined root directory.
type FileProvider struct {
	// Root is the base directory that all file reads are confined to.
	// Defaults to the user's home directory if empty.
	Root string
}

func (p *FileProvider) Name() string { return "file" }

func (p *FileProvider) CanResolve(ref OriginRef) bool {
	return ref.Kind == "file"
}

func (p *FileProvider) Resolve(_ context.Context, ref OriginRef) (string, error) {
	root := p.Root
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("file provider: cannot determine home dir: %w", err)
		}
		root = home
	}

	// Normalize and confine the requested path to Root.
	requested := filepath.Clean(ref.Ref)
	if !filepath.IsAbs(requested) {
		requested = filepath.Join(root, requested)
	}
	requested, err := filepath.EvalSymlinks(requested)
	if err != nil {
		return "", fmt.Errorf("file provider: invalid path %q: %w", ref.Ref, err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("file provider: invalid root %q: %w", p.Root, err)
	}
	if !isWithinRoot(requested, root) {
		return "", fmt.Errorf("file provider: path %q escapes root %q", ref.Ref, root)
	}

	data, err := os.ReadFile(requested)
	if err != nil {
		return "", fmt.Errorf("file provider: read %q: %w", ref.Ref, err)
	}
	return strings.TrimRight(string(data), "\r\n"), nil
}

// isWithinRoot reports whether path is root itself or a descendant contained
// inside root at a path-component boundary. Both arguments must already be
// cleaned and evaluated (e.g. via filepath.Clean and filepath.EvalSymlinks).
func isWithinRoot(path, root string) bool {
	if path == root {
		return true
	}

	rootParts := splitPath(root)
	pathParts := splitPath(path)
	if len(pathParts) < len(rootParts) {
		return false
	}
	for i, part := range rootParts {
		if pathParts[i] != part {
			return false
		}
	}
	return true
}

// splitPath splits a cleaned path into its component names. It discards the
// trailing empty element produced by a root path ending in a separator so that
// the root directory is represented by a single component list.
func splitPath(p string) []string {
	parts := strings.Split(p, string(filepath.Separator))
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}
