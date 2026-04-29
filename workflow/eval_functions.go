package workflow

// eval_functions.go — HCL expression functions for workflow evaluation.
// Implements file(), fileexists(), and trimfrontmatter().

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
)

const (
	defaultMaxBytes int64 = 1 * 1024 * 1024  // 1 MiB
	minMaxBytes     int64 = 1024             // 1 KiB lower bound
	maxMaxBytes     int64 = 64 * 1024 * 1024 // 64 MiB upper bound

	// trimFrontmatterSearchLimit is the maximum byte offset within content
	// where the closing "---\n" delimiter must appear.
	trimFrontmatterSearchLimit = 64 * 1024
)

// FunctionOptions carries the configuration needed to construct the
// workflow expression functions.
//
//   - WorkflowDir is the directory of the HCL file being evaluated.
//     file() and fileexists() resolve paths relative to this directory.
//     When empty, file() and fileexists() always error with
//     "workflow directory not configured".
//   - MaxBytes is the read cap for file(). Sourced from
//     CRITERIA_FILE_FUNC_MAX_BYTES; defaults to 1 MiB.
//   - AllowedPaths is the list of directories that file() and fileexists()
//     may access outside WorkflowDir. Sourced from
//     CRITERIA_WORKFLOW_ALLOWED_PATHS (OS path-list separator).
type FunctionOptions struct {
	WorkflowDir  string
	MaxBytes     int64
	AllowedPaths []string
}

// DefaultFunctionOptions builds FunctionOptions from environment variables
// and the provided workflow directory. Callers pass this to
// BuildEvalContextWithOpts or ResolveInputExprsWithOpts.
//
// workflowDir is resolved to an absolute path if it is non-empty and relative;
// this ensures path confinement checks work correctly regardless of CWD.
//
// Environment variables read:
//   - CRITERIA_FILE_FUNC_MAX_BYTES: integer, clamped to [1024, 64 MiB].
//   - CRITERIA_WORKFLOW_ALLOWED_PATHS: OS path-list-separated list of directories (filepath.SplitList).
func DefaultFunctionOptions(workflowDir string) FunctionOptions {
	if workflowDir != "" {
		if abs, err := filepath.Abs(workflowDir); err == nil {
			workflowDir = abs
		}
	}
	maxBytes := defaultMaxBytes
	if raw := os.Getenv("CRITERIA_FILE_FUNC_MAX_BYTES"); raw != "" {
		if v, err := strconv.ParseInt(raw, 10, 64); err == nil {
			switch {
			case v < minMaxBytes:
				maxBytes = minMaxBytes
			case v > maxMaxBytes:
				maxBytes = maxMaxBytes
			default:
				maxBytes = v
			}
		}
	}

	var allowed []string
	if raw := os.Getenv("CRITERIA_WORKFLOW_ALLOWED_PATHS"); raw != "" {
		for _, p := range filepath.SplitList(raw) {
			if p == "" {
				continue
			}
			if abs, err := filepath.Abs(p); err == nil {
				p = abs
			}
			allowed = append(allowed, p)
		}
	}

	return FunctionOptions{
		WorkflowDir:  workflowDir,
		MaxBytes:     maxBytes,
		AllowedPaths: allowed,
	}
}

// workflowFunctions returns the map of HCL expression functions to register
// in the workflow evaluation context.
func workflowFunctions(opts FunctionOptions) map[string]function.Function {
	return map[string]function.Function{
		"file":            fileFunction(opts),
		"fileexists":      fileExistsFunction(opts),
		"trimfrontmatter": trimFrontmatterFunction(),
	}
}

// fileFunction implements the file(path) → string expression function.
// Reads the UTF-8 file at path (resolved relative to WorkflowDir),
// enforcing path confinement and the MaxBytes size cap.
func fileFunction(opts FunctionOptions) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "path", Type: cty.String}},
		Type:   function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			if opts.WorkflowDir == "" {
				return cty.StringVal(""), fmt.Errorf("file(): workflow directory not configured")
			}
			raw := args[0].AsString()
			resolved, err := resolveConfinedPath(raw, opts.WorkflowDir, opts.AllowedPaths)
			if err != nil {
				return cty.StringVal(""), err
			}

			info, err := os.Stat(resolved)
			if err != nil {
				return cty.StringVal(""), mapOSError(raw, err)
			}
			if info.Size() > opts.MaxBytes {
				return cty.StringVal(""), fmt.Errorf(
					"file(): %q is %d bytes; max is %d (set CRITERIA_FILE_FUNC_MAX_BYTES to raise)",
					raw, info.Size(), opts.MaxBytes)
			}

			data, err := os.ReadFile(resolved)
			if err != nil {
				return cty.StringVal(""), mapOSError(raw, err)
			}

			if !utf8.Valid(data) {
				offset := invalidUTF8Offset(data)
				return cty.StringVal(""), fmt.Errorf(
					"file(): %q contains invalid UTF-8 at byte %d", raw, offset)
			}
			return cty.StringVal(string(data)), nil
		},
	})
}

// fileExistsFunction implements the fileexists(path) → bool expression function.
// Returns true only when path resolves to a readable regular file.
// Directories return false. Errors other than "not exists" propagate.
func fileExistsFunction(opts FunctionOptions) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "path", Type: cty.String}},
		Type:   function.StaticReturnType(cty.Bool),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			if opts.WorkflowDir == "" {
				return cty.False, fmt.Errorf("fileexists(): workflow directory not configured")
			}
			raw := args[0].AsString()
			exists, err := fileExistsResolved(raw, opts)
			if err != nil {
				return cty.False, err
			}
			return cty.BoolVal(exists), nil
		},
	})
}

// fileExistsResolved checks whether raw resolves to an existing regular file
// within the confined directories, following symlinks and performing a
// post-symlink confinement check. Returns (false, nil) for not-found paths.
func fileExistsResolved(raw string, opts FunctionOptions) (bool, error) {
	if filepath.IsAbs(raw) {
		return false, fmt.Errorf("fileexists(): absolute paths are not supported; use a path relative to the workflow directory")
	}

	abs := filepath.Clean(filepath.Join(opts.WorkflowDir, raw))
	if err := checkConfinement("fileexists()", raw, abs, opts.WorkflowDir, opts.AllowedPaths); err != nil {
		return false, err
	}

	// EvalSymlinks requires the target to exist; treat not-found as false.
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		if os.IsPermission(err) {
			return false, fmt.Errorf("fileexists(): permission denied: %s", raw)
		}
		return false, fmt.Errorf("fileexists(): %w", err)
	}
	resolved = filepath.Clean(resolved)

	// Resolve base and allowed dirs through symlinks for the post-symlink check.
	// Required on systems where WorkflowDir itself is a symlink (e.g. macOS /tmp).
	resolvedBase := evalSymlinksOrSelf(opts.WorkflowDir)
	resolvedAllowed := evalSymlinksAll(opts.AllowedPaths)

	if err := checkConfinement("fileexists()", raw, resolved, resolvedBase, resolvedAllowed); err != nil {
		return false, err
	}

	info, err := os.Stat(resolved)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		if os.IsPermission(err) {
			return false, fmt.Errorf("fileexists(): permission denied: %s", raw)
		}
		return false, fmt.Errorf("fileexists(): %w", err)
	}
	return info.Mode().IsRegular(), nil
}

// trimFrontmatterFunction implements the trimfrontmatter(content) → string
// expression function. Pure string function (no I/O).
//
// Detects a YAML frontmatter block delimited by leading "---\n" and a closing
// "---\n" that must appear within the first 64 KiB. If the pattern is not
// present, or the closing delimiter is beyond 64 KiB, the input is returned
// unchanged.
func trimFrontmatterFunction() function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "content", Type: cty.String}},
		Type:   function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			s := args[0].AsString()
			return cty.StringVal(trimFrontmatter(s)), nil
		},
	})
}

// trimFrontmatter strips a YAML frontmatter block from s and returns the
// remainder. If s does not start with "---\n", or the closing "---\n" does
// not appear within the first 64 KiB, s is returned unchanged.
func trimFrontmatter(s string) string {
	if !strings.HasPrefix(s, "---\n") {
		return s
	}

	// Search for the closing delimiter only within the first 64 KiB to
	// protect against huge inputs that look like they start with frontmatter.
	limit := len(s)
	if limit > trimFrontmatterSearchLimit {
		limit = trimFrontmatterSearchLimit
	}

	// The opening "---\n" is 4 bytes; look for "\n---\n" after it.
	closeIdx := strings.Index(s[4:limit], "\n---\n")
	if closeIdx < 0 {
		return s
	}

	// Skip past the opening "---\n" (4 bytes), the YAML body (closeIdx bytes),
	// and the closing "\n---\n" (5 bytes).
	end := 4 + closeIdx + 5
	return s[end:]
}

// resolveConfinedPath resolves raw relative to base, evaluates symlinks, and
// checks that the resolved path is confined to base or one of the allowed
// directories. Returns the resolved absolute path or an error.
func resolveConfinedPath(raw, base string, allowed []string) (string, error) {
	if filepath.IsAbs(raw) {
		return "", fmt.Errorf("file(): absolute paths are not supported; use a path relative to the workflow directory")
	}
	abs := filepath.Clean(filepath.Join(base, raw))

	// Check confinement on the pre-symlink cleaned path (catches .. escapes).
	if err := checkConfinement("file()", raw, abs, base, allowed); err != nil {
		return "", err
	}

	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", mapOSError(raw, err)
	}
	resolved = filepath.Clean(resolved)

	// Resolve base and allowed dirs through symlinks for the post-symlink check.
	// Required on systems where base itself is a symlink (e.g. macOS /tmp).
	resolvedBase := evalSymlinksOrSelf(base)
	resolvedAllowed := evalSymlinksAll(allowed)

	// Re-check confinement on the symlink-resolved path (catches symlink escapes).
	if err := checkConfinement("file()", raw, resolved, resolvedBase, resolvedAllowed); err != nil {
		return "", err
	}
	return resolved, nil
}

// checkConfinement returns an error if absPath is not under base or any of
// the allowed directories. funcName is included in the error message to
// identify which function triggered the check (e.g. "file()" or "fileexists()").
func checkConfinement(funcName, raw, absPath, base string, allowed []string) error {
	if isUnderDir(absPath, filepath.Clean(base)) {
		return nil
	}
	for _, a := range allowed {
		if a == "" {
			continue
		}
		if isUnderDir(absPath, filepath.Clean(a)) {
			return nil
		}
	}
	return fmt.Errorf("%s: path %q escapes workflow directory; add to CRITERIA_WORKFLOW_ALLOWED_PATHS to permit", funcName, raw)
}

// isUnderDir reports whether path is equal to dir or is a direct or indirect
// child of dir. Both arguments must be cleaned (filepath.Clean) absolute paths.
func isUnderDir(path, dir string) bool {
	if path == dir {
		return true
	}
	return strings.HasPrefix(path, dir+string(filepath.Separator))
}

// mapOSError converts an os-level I/O error to a human-readable file()
// function error using the original (user-visible) path.
func mapOSError(path string, err error) error {
	if os.IsNotExist(err) {
		return fmt.Errorf("file(): no such file: %s", path)
	}
	if os.IsPermission(err) {
		return fmt.Errorf("file(): permission denied: %s", path)
	}
	return fmt.Errorf("file(): %w", err)
}

// evalSymlinksOrSelf resolves dir through symlinks. If EvalSymlinks fails
// (e.g. the directory does not exist), the original value is returned unchanged.
func evalSymlinksOrSelf(dir string) string {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return dir
	}
	return filepath.Clean(resolved)
}

// evalSymlinksAll resolves each directory in dirs through symlinks. Entries
// that fail EvalSymlinks are preserved unchanged.
func evalSymlinksAll(dirs []string) []string {
	if len(dirs) == 0 {
		return dirs
	}
	out := make([]string, len(dirs))
	for i, d := range dirs {
		out[i] = evalSymlinksOrSelf(d)
	}
	return out
}

// invalidUTF8Offset returns the byte offset of the first invalid UTF-8
// sequence in data. Callers must only call this when utf8.Valid(data) is false.
func invalidUTF8Offset(data []byte) int {
	for i := 0; i < len(data); {
		r, size := utf8.DecodeRune(data[i:])
		if r == utf8.RuneError && size == 1 {
			return i
		}
		i += size
	}
	return len(data)
}
