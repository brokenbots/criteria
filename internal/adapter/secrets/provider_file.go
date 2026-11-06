package secrets

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/brokenbots/criteria/internal/dirs"
	"github.com/brokenbots/criteria/internal/envreg"
)

// FileProvider resolves secrets by reading files from a confined set of
// root directories. Every reference — absolute, relative, or reached through
// a symlink — is normalized, evaluated on both sides, and refused unless it
// stays contained within one of the roots at a path-component boundary.
//
// Roots resolution:
//
//  1. An explicit Root set in the struct wins — the provider is confined to
//     that single directory (back-compat shape; tests and future callers).
//  2. Otherwise secretFileRoots() parses $CRITERIA_SECRET_FILE_ROOTS
//     (newline-separated absolute paths the run environment explicitly
//     declares as secret sources — each CSI/SPC secret mount the job
//     template stamps), ordered after the default secrets directory.
//  3. The default secrets dir (dirs.SecretsDir()) is always the FIRST root.
//     It is not created on demand: a missing default root removes that root
//     from consideration rather than failing the resolution — reads land in
//     a declared root or fail closed.
//
// Home-wide defaults are intentionally not used: containment is only ever
// widened by paths the run environment itself declares (the operator owns
// the declaration; the workflow object's secrets entries are the single
// source of truth both CRITERIA_SECRET_VARS and CRITERIA_SECRET_FILE_ROOTS
// derive from, so the two can never drift).
type FileProvider struct {
	// Root is the single base directory when a caller wants exactly one.
	// Empty selects the multi-root resolution above.
	Root string
}

func (p *FileProvider) Name() string { return "file" }

func (p *FileProvider) CanResolve(ref OriginRef) bool {
	return ref.Kind == "file"
}

// secretFileRoots returns the provider's root list: the explicit single Root
// when set, else [dirs.SecretsDir()] + the run environment's declared secret
// file roots ($CRITERIA_SECRET_FILE_ROOTS). Duplicate and non-absolute
// entries are dropped; the default root is deduplicated after the declared
// set too (an operator may deliberately redeclare it, which must not double
// the list).
func secretFileRoots(explicit string) []string {
	if explicit != "" {
		return []string{explicit}
	}
	roots := []string{}
	seen := map[string]bool{}
	add := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" || !filepath.IsAbs(path) || seen[path] {
			return
		}
		seen[path] = true
		roots = append(roots, path)
	}
	add(mustSecretsDir())
	for _, r := range strings.Split(envreg.CRITERIASecretFileRoots.String(), "\n") {
		add(r)
	}
	return roots
}

func mustSecretsDir() string {
	dir, err := dirs.SecretsDir()
	if err != nil {
		return ""
	}
	return dir
}

func (p *FileProvider) Resolve(_ context.Context, ref OriginRef) (string, error) {
	roots := secretFileRoots(p.Root)

	requested := filepath.Clean(ref.Ref)
	if !filepath.IsAbs(requested) {
		// A relative ref is meaningful only under the default root (the
		// first one): declared mounts are absolute deployment surfaces,
		// so a relative ref names a path under the default secrets dir.
		if len(roots) == 0 {
			return "", fmt.Errorf("file provider: relative ref %q with no secret root", ref.Ref)
		}
		requested = filepath.Join(roots[0], requested)
	}

	requested, evalErr := filepath.EvalSymlinks(requested)
	if evalErr != nil {
		return "", fmt.Errorf("file provider: invalid path %q: %w", ref.Ref, evalErr)
	}

	// Containment against the FIRST root that contains the resolved path.
	// A root that fails its own symlink/directory evaluation is skipped
	// (the default root is not provisioned on demand); any remaining root
	// still has to claim the path, else the whole resolution fails closed.
	var containing string
	for _, root := range roots {
		evalRoot, rootErr := filepath.EvalSymlinks(root)
		if rootErr != nil {
			continue // root not provisioned — not a resolution error yet
		}
		if isWithinRoot(requested, evalRoot) {
			containing = evalRoot
			break
		}
	}
	if containing == "" {
		// Containment failed against every root. Report the first declared root
		// (or the count when none was even evaluable) so operators see the set;
		// the message keeps the historical 'escapes root' substring callers and
		// the existing suite assert on.
		shown := "(none evaluable)"
		if len(roots) > 0 {
			shown = roots[0]
		}
		return "", fmt.Errorf("file provider: path %q escapes root %q (%d declared secret roots)", ref.Ref, shown, len(roots))
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
