package workflow

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// LocalSubWorkflowResolver resolves source strings against the local filesystem only.
// Remote schemes (git://, https://, etc.) produce an error pointing to Phase 4.
type LocalSubWorkflowResolver struct {
	// AllowedRoots restricts resolution to subdirectories of these root paths.
	// When empty, no restriction is enforced. When non-empty, resolved paths
	// must be under at least one allowed root (security guard).
	AllowedRoots []string
}

// ResolveSource resolves a source string to a directory path.
// Behavior:
// 1. If source parses as a URL with a scheme other than empty/file, error with Phase 4 forward-pointer.
// 2. If source is absolute, use it directly. Reject if AllowedRoots is non-empty and path is not under any allowed root.
// 3. If source is relative, resolve against callerDir.
// 4. Verify the resolved path is a directory; error if not.
// 5. Verify the directory contains at least one .hcl file; error if empty.
// 6. Return the absolute path.
func (r *LocalSubWorkflowResolver) ResolveSource(ctx context.Context, callerDir, source string) (string, error) {
	// Check for remote schemes.
	if err := r.checkRemoteScheme(source); err != nil {
		return "", err
	}

	resolvedPath := r.resolvePath(source, callerDir)
	if err := r.checkAllowedRoots(resolvedPath); err != nil {
		return "", err
	}

	if err := r.checkDirectory(resolvedPath); err != nil {
		return "", err
	}

	if err := r.checkHCLFiles(resolvedPath); err != nil {
		return "", err
	}

	return resolvedPath, nil
}

// checkRemoteScheme returns an error if source has a remote scheme.
func (r *LocalSubWorkflowResolver) checkRemoteScheme(source string) error {
	if u, err := url.Parse(source); err == nil && u.Scheme != "" && u.Scheme != "file" {
		return fmt.Errorf("remote schemes (e.g. %s://) are not supported in v0.3.0; see Phase 4 roadmap", u.Scheme)
	}
	return nil
}

// resolvePath resolves a source path to an absolute path.
func (r *LocalSubWorkflowResolver) resolvePath(source, callerDir string) string {
	var resolvedPath string
	if filepath.IsAbs(source) {
		resolvedPath = source
	} else {
		resolvedPath = filepath.Join(callerDir, source)
	}

	abs, _ := filepath.Abs(resolvedPath)
	return abs
}

// checkAllowedRoots verifies the resolved path is under an allowed root.
func (r *LocalSubWorkflowResolver) checkAllowedRoots(resolvedPath string) error {
	if len(r.AllowedRoots) == 0 {
		return nil
	}

	for _, root := range r.AllowedRoots {
		abs, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		if strings.HasPrefix(resolvedPath+string(filepath.Separator), abs+string(filepath.Separator)) ||
			resolvedPath == abs {
			return nil
		}
	}

	return fmt.Errorf("resolved path %q is not under any allowed root: %v", resolvedPath, r.AllowedRoots)
}

// checkDirectory verifies the resolved path is a directory.
func (r *LocalSubWorkflowResolver) checkDirectory(resolvedPath string) error {
	info, err := os.Stat(resolvedPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("subworkflow directory does not exist: %q", resolvedPath)
		}
		return fmt.Errorf("failed to stat subworkflow directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("subworkflow source must be a directory; %q is a file", resolvedPath)
	}
	return nil
}

// checkHCLFiles verifies the directory contains at least one .hcl file.
func (r *LocalSubWorkflowResolver) checkHCLFiles(resolvedPath string) error {
	entries, err := os.ReadDir(resolvedPath)
	if err != nil {
		return fmt.Errorf("failed to read subworkflow directory: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".hcl") {
			return nil
		}
	}

	return fmt.Errorf("subworkflow directory %q contains no .hcl files", resolvedPath)
}
