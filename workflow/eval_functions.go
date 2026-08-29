package workflow

// eval_functions.go — HCL expression functions for workflow evaluation.
// Implements file(), fileexists(), fileset(), templatefile(), and trimfrontmatter().

import (
	"bytes"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"unicode/utf8"

	"github.com/hashicorp/hcl/v2/ext/tryfunc"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
	"github.com/zclconf/go-cty/cty/function/stdlib"
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
//     file(), fileexists(), and templatefile() resolve paths relative to
//     this directory. When empty, these functions always error with
//     "workflow directory not configured".
//   - RootDir is the project root directory (criteria invocation cwd).
//   - Cwd is the process working directory at evaluation time.
//   - MaxBytes is the read cap for file() and templatefile(). Sourced from
//     CRITERIA_FILE_FUNC_MAX_BYTES; defaults to 1 MiB.
//   - AllowedPaths is the list of directories that file(), fileexists(),
//     and templatefile() may access outside WorkflowDir. Sourced from
//     CRITERIA_WORKFLOW_ALLOWED_PATHS (OS path-list separator).
type FunctionOptions struct {
	WorkflowDir  string
	RootDir      string
	Cwd          string
	MaxBytes     int64
	AllowedPaths []string
	// FileCache, when non-nil, is a map from resolved absolute path to file
	// content. file() and templatefile() return cached content without reading
	// disk when a path is present, and record newly-read content into the map.
	// This lets the compiler capture static prompt files and the runtime
	// re-evaluate adapter config against live var.* values without re-reading
	// files from disk.
	FileCache map[string]string
}

// DefaultFunctionOptions builds FunctionOptions from environment variables
// and the provided workflow directory. Callers pass this to
// BuildEvalContextWithOpts or ResolveInputExprsWithOpts.
//
// workflowDir is resolved to an absolute path if it is non-empty and relative;
// this ensures path confinement checks work correctly regardless of CWD.
//
// Environment variables read:
//   - CRITERIA_FILE_FUNC_MAX_BYTES: integer, clamped to [1024, 64 MiB]; applies to file() and templatefile().
//   - CRITERIA_WORKFLOW_ALLOWED_PATHS: OS path-list-separated list of directories (filepath.SplitList); applies to file(), fileexists(), and templatefile().
func DefaultFunctionOptions(workflowDir string) *FunctionOptions {
	if workflowDir != "" {
		if abs, err := filepath.Abs(workflowDir); err == nil {
			workflowDir = abs
		}
	}
	cwd, _ := os.Getwd()
	rootDir := cwd
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

	return &FunctionOptions{
		WorkflowDir:  workflowDir,
		RootDir:      rootDir,
		Cwd:          cwd,
		MaxBytes:     maxBytes,
		AllowedPaths: allowed,
	}
}

// workflowFunctions returns the map of HCL expression functions to register
// in the workflow evaluation context.
//
// Registration order: stdlib goes first, then Criteria-specific functions
// are layered on top. This means our custom implementations (file, fileexists,
// etc.) take precedence if a name collision ever occurs. In practice we rely on
// community stdlib implementations and do not intentionally override.
func workflowFunctions(opts *FunctionOptions) map[string]function.Function {
	out := map[string]function.Function{}
	for k, v := range stdlibFunctions() {
		out[k] = v
	}
	for k, v := range registerHashFunctions() {
		out[k] = v
	}
	for k, v := range registerEncodingFunctions() {
		out[k] = v
	}
	for k, v := range registerDynamicFunctions() {
		out[k] = v
	}
	for k, v := range registerStringFunctions() {
		out[k] = v
	}
	out["file"] = fileFunction(opts)
	out["fileexists"] = fileExistsFunction(opts)
	out["fileset"] = filesetFunction(opts)
	out["templatefile"] = templatefileFunction(opts)
	out["trimfrontmatter"] = trimFrontmatterFunction()
	out["abspath"] = absPathFunction(opts)
	out["dirname"] = dirNameFunction()
	out["basename"] = baseNameFunction()
	out["can"] = tryfunc.CanFunc
	out["try"] = tryfunc.TryFunc
	out["hasattr"] = hasAttributeFunction()
	return out
}

// registerStringFunctions provides string utility functions that exist in
// Terraform but are not yet available in cty/function/stdlib.
func registerStringFunctions() map[string]function.Function {
	return map[string]function.Function{
		"startswith": startswithFunction(),
		"endswith":   endswithFunction(),
		"strrev":     strrevFunction(),
	}
}

func startswithFunction() function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{
			{Name: "string", Type: cty.String},
			{Name: "prefix", Type: cty.String},
		},
		Type: function.StaticReturnType(cty.Bool),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			return cty.BoolVal(strings.HasPrefix(args[0].AsString(), args[1].AsString())), nil
		},
	})
}

func endswithFunction() function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{
			{Name: "string", Type: cty.String},
			{Name: "suffix", Type: cty.String},
		},
		Type: function.StaticReturnType(cty.Bool),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			return cty.BoolVal(strings.HasSuffix(args[0].AsString(), args[1].AsString())), nil
		},
	})
}

func strrevFunction() function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "string", Type: cty.String}},
		Type:   function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			s := args[0].AsString()
			// Reverse by rune to preserve multi-byte UTF-8 characters.
			runes := []rune(s)
			for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
				runes[i], runes[j] = runes[j], runes[i]
			}
			return cty.StringVal(string(runes)), nil
		},
	})
}

// stdlibFunctions registers all functions from cty/function/stdlib.
// Criteria-specific functions are layered on top in workflowFunctions so that
// custom implementations (file, fileexists, etc.) take precedence only when
// we explicitly choose to override.
func stdlibFunctions() map[string]function.Function {
	out := make(map[string]function.Function)
	mergeFunctions(out, stdlibArithmeticFunctions())
	mergeFunctions(out, stdlibStringFunctions())
	mergeFunctions(out, stdlibCollectionFunctions())
	mergeFunctions(out, stdlibSetFunctions())
	mergeFunctions(out, stdlibEncodingFunctions())
	mergeFunctions(out, stdlibLogicalFunctions())
	mergeFunctions(out, stdlibDateFunctions())
	return out
}

func mergeFunctions(dst, src map[string]function.Function) {
	for k, v := range src {
		dst[k] = v
	}
}

func stdlibArithmeticFunctions() map[string]function.Function {
	return map[string]function.Function{
		"abs":      stdlib.AbsoluteFunc,
		"add":      stdlib.AddFunc,
		"ceil":     stdlib.CeilFunc,
		"divide":   stdlib.DivideFunc,
		"floor":    stdlib.FloorFunc,
		"int":      stdlib.IntFunc,
		"log":      stdlib.LogFunc,
		"max":      stdlib.MaxFunc,
		"min":      stdlib.MinFunc,
		"modulo":   stdlib.ModuloFunc,
		"multiply": stdlib.MultiplyFunc,
		"negate":   stdlib.NegateFunc,
		"parseint": stdlib.ParseIntFunc,
		"pow":      stdlib.PowFunc,
		"signum":   stdlib.SignumFunc,
		"subtract": stdlib.SubtractFunc,
	}
}

func stdlibStringFunctions() map[string]function.Function {
	return map[string]function.Function{
		"chomp":      stdlib.ChompFunc,
		"format":     stdlib.FormatFunc,
		"formatdate": stdlib.FormatDateFunc,
		"formatlist": stdlib.FormatListFunc,
		"indent":     stdlib.IndentFunc,
		"join":       stdlib.JoinFunc,
		"length":     stdlib.LengthFunc,
		"lower":      stdlib.LowerFunc,
		"replace":    stdlib.ReplaceFunc,
		"reverse":    stdlib.ReverseFunc,
		"split":      stdlib.SplitFunc,
		"strlen":     stdlib.StrlenFunc,
		"substr":     stdlib.SubstrFunc,
		"title":      stdlib.TitleFunc,
		"trim":       stdlib.TrimFunc,
		"trimprefix": stdlib.TrimPrefixFunc,
		"trimspace":  stdlib.TrimSpaceFunc,
		"trimsuffix": stdlib.TrimSuffixFunc,
		"upper":      stdlib.UpperFunc,
	}
}

func stdlibCollectionFunctions() map[string]function.Function {
	return map[string]function.Function{
		"chunklist":    stdlib.ChunklistFunc,
		"coalesce":     stdlib.CoalesceFunc,
		"coalescelist": stdlib.CoalesceListFunc,
		"compact":      stdlib.CompactFunc,
		"concat":       stdlib.ConcatFunc,
		"contains":     stdlib.ContainsFunc,
		"csvdecode":    stdlib.CSVDecodeFunc,
		"distinct":     stdlib.DistinctFunc,
		"element":      stdlib.ElementFunc,
		"flatten":      stdlib.FlattenFunc,
		"index":        stdlib.IndexFunc,
		"keys":         stdlib.KeysFunc,
		"lookup":       stdlib.LookupFunc,
		"merge":        stdlib.MergeFunc,
		"range":        stdlib.RangeFunc,
		"regex":        stdlib.RegexFunc,
		"regexall":     stdlib.RegexAllFunc,
		"regexreplace": stdlib.RegexReplaceFunc,
		"reverselist":  stdlib.ReverseListFunc,
		"slice":        stdlib.SliceFunc,
		"sort":         stdlib.SortFunc,
		"values":       stdlib.ValuesFunc,
		"zipmap":       stdlib.ZipmapFunc,
	}
}

func stdlibSetFunctions() map[string]function.Function {
	return map[string]function.Function{
		"sethaselement":          stdlib.SetHasElementFunc,
		"setintersection":        stdlib.SetIntersectionFunc,
		"setproduct":             stdlib.SetProductFunc,
		"setsubtract":            stdlib.SetSubtractFunc,
		"setsymmetricdifference": stdlib.SetSymmetricDifferenceFunc,
		"setunion":               stdlib.SetUnionFunc,
	}
}

func stdlibEncodingFunctions() map[string]function.Function {
	return map[string]function.Function{
		"byteslen":   stdlib.BytesLenFunc,
		"bytesslice": stdlib.BytesSliceFunc,
		"jsondecode": stdlib.JSONDecodeFunc,
		"jsonencode": stdlib.JSONEncodeFunc,
	}
}

func stdlibLogicalFunctions() map[string]function.Function {
	return map[string]function.Function{
		"and":                  stdlib.AndFunc,
		"assertnotnull":        stdlib.AssertNotNullFunc,
		"equal":                stdlib.EqualFunc,
		"greaterthan":          stdlib.GreaterThanFunc,
		"greaterthanorequalto": stdlib.GreaterThanOrEqualToFunc,
		"hasindex":             stdlib.HasIndexFunc,
		"lessthan":             stdlib.LessThanFunc,
		"lessthanorequalto":    stdlib.LessThanOrEqualToFunc,
		"not":                  stdlib.NotFunc,
		"notequal":             stdlib.NotEqualFunc,
		"or":                   stdlib.OrFunc,
	}
}

func stdlibDateFunctions() map[string]function.Function {
	return map[string]function.Function{
		"timeadd": stdlib.TimeAddFunc,
	}
}

// fileFunction implements the file(path) → string expression function.
// Reads the UTF-8 file at path (resolved relative to WorkflowDir),
// enforcing path confinement and the MaxBytes size cap.
// readFileWithCache resolves raw relative to opts.WorkflowDir, returns cached
// content if present, otherwise reads, validates, and caches the result when
// opts.FileCache is non-nil.
func readFileWithCache(raw string, opts *FunctionOptions, funcName string) (string, error) {
	if opts.WorkflowDir == "" {
		return "", fmt.Errorf("%s(): workflow directory not configured", funcName)
	}
	resolved, err := resolveConfinedPath(raw, opts.WorkflowDir, opts.AllowedPaths)
	if err != nil {
		return "", err
	}
	if opts.FileCache != nil {
		if cached, ok := opts.FileCache[resolved]; ok {
			return cached, nil
		}
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return "", mapOSError(raw, err)
	}
	if info.Size() > opts.MaxBytes {
		return "", fmt.Errorf(
			"%s(): %q is %d bytes; max is %d (set CRITERIA_FILE_FUNC_MAX_BYTES to raise)",
			funcName, raw, info.Size(), opts.MaxBytes)
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		return "", mapOSError(raw, err)
	}

	if !utf8.Valid(data) {
		offset := invalidUTF8Offset(data)
		return "", fmt.Errorf(
			"%s(): %q contains invalid UTF-8 at byte %d", funcName, raw, offset)
	}
	if opts.FileCache != nil {
		opts.FileCache[resolved] = string(data)
	}
	return string(data), nil
}

func fileFunction(opts *FunctionOptions) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "path", Type: cty.String}},
		Type:   function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			content, err := readFileWithCache(args[0].AsString(), opts, "file")
			if err != nil {
				return cty.StringVal(""), err
			}
			return cty.StringVal(content), nil
		},
	})
}

// templatefileFunction implements templatefile(path, vars) → string.
//
// Reads the UTF-8 file at path (resolved relative to WorkflowDir using the
// same path-confinement and size-cap machinery as file()), then renders the
// file contents as a Go text/template template with vars as the data context.
// vars must be an object or map value; its attributes become the template's
// . fields.
//
// Template syntax: Go text/template ({{ .field }}), not HCL native (${field}).
// This is an intentional divergence from Terraform's templatefile() — rationale:
// text/template is in the stdlib and does not auto-escape, which is desirable
// for prompt content.
//
// Missing keys in vars cause a render error (missingkey=error is set). Null
// values in vars become nil in the template context and render as "<no value>"
// (Go text/template's default for nil map entries).
//
// Note: vars size is not capped; only the template file size is bounded by
// MaxBytes. For large vars objects, callers own the performance consequences.
func templatefileFunction(opts *FunctionOptions) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{
			{Name: "path", Type: cty.String},
			{Name: "vars", Type: cty.DynamicPseudoType, AllowNull: false},
		},
		Type: function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			return renderTemplateFile(opts, args[0].AsString(), args[1])
		},
	})
}

// renderTemplateFile is the core implementation of templatefile(). It is
// extracted from the Impl closure to keep cognitive complexity manageable.
func renderTemplateFile(opts *FunctionOptions, raw string, varsVal cty.Value) (cty.Value, error) {
	// Validate vars is an object (or map). Reject primitives and lists.
	ty := varsVal.Type()
	if !ty.IsObjectType() && !ty.IsMapType() {
		return cty.StringVal(""), fmt.Errorf(
			"templatefile(): vars must be an object or map; got %s", ty.FriendlyName())
	}

	content, err := readFileWithCache(raw, opts, "templatefile")
	if err != nil {
		return cty.StringVal(""), rewriteFuncName(err, "file()", "templatefile()")
	}

	// Template name is the basename of path so error messages reference
	// the source file.
	tmpl, err := template.New(filepath.Base(raw)).
		Option("missingkey=error").
		Parse(content)
	if err != nil {
		return cty.StringVal(""), fmt.Errorf("templatefile(): %q parse: %w", raw, err)
	}

	// If any template variable is unknown (e.g. a var.* with no default during
	// validation), we cannot render the template yet, but we have already
	// verified the file exists and the template parses. Return an unknown
	// string so the expression folds correctly at validation time and is
	// re-rendered once runtime values are available.
	if hasUnknownValue(varsVal) {
		return cty.UnknownVal(cty.String), nil
	}

	// Convert cty vars to Go map for text/template.
	ctxMap, err := ctyToGoMap(varsVal)
	if err != nil {
		return cty.StringVal(""), fmt.Errorf("templatefile(): converting vars: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, ctxMap); err != nil {
		return cty.StringVal(""), fmt.Errorf("templatefile(): %q execute: %w", raw, err)
	}
	return cty.StringVal(buf.String()), nil
}

// hasUnknownValue reports whether v or any nested value inside it is unknown.
// It recurses through objects, maps, lists, tuples, and sets.
func hasUnknownValue(v cty.Value) bool {
	if !v.IsKnown() {
		return true
	}
	if v.IsNull() {
		return false
	}
	ty := v.Type()
	if ty.IsObjectType() || ty.IsMapType() || ty.IsListType() || ty.IsTupleType() || ty.IsSetType() {
		it := v.ElementIterator()
		for it.Next() {
			_, elem := it.Element()
			if hasUnknownValue(elem) {
				return true
			}
		}
	}
	return false
}

// fileExistsFunction implements the fileexists(path) → bool expression function.
// Returns true only when path resolves to a readable regular file.
// Directories return false. Errors other than "not exists" propagate.
func fileExistsFunction(opts *FunctionOptions) function.Function {
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
func fileExistsResolved(raw string, opts *FunctionOptions) (bool, error) {
	if filepath.IsAbs(raw) {
		return false, fmt.Errorf("fileexists(): absolute paths are not supported; use a path relative to the workflow directory")
	}

	abs := filepath.Clean(filepath.Join(opts.WorkflowDir, raw))
	if err := checkConfinement("fileexists()", raw, abs, opts.WorkflowDir, opts.AllowedPaths); err != nil {
		if _, statErr := os.Stat(abs); os.IsNotExist(statErr) {
			return false, nil
		}
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

// filesetFunction implements fileset(path, pattern) → list(string).
//
// Lists regular files inside `path` (resolved relative to WorkflowDir, with
// the same confinement as file()) whose basename matches the glob `pattern`.
// Returns matches as a sorted list of paths relative to WorkflowDir, suitable
// for passing to file() / templatefile() via each.value.
//
// Glob syntax follows Go's filepath.Match: '*' matches any sequence of
// non-slash chars, '?' matches a single non-slash char, and '[a-z]' matches a
// character class. There is no '**' (recursive) syntax in v1; fileset does
// not descend into subdirectories.
//
// Returns an empty list if no files match. Returns an error if path does not
// exist, is not a directory, escapes the workflow directory, or pattern is
// syntactically invalid.
func filesetFunction(opts *FunctionOptions) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{
			{Name: "path", Type: cty.String},
			{Name: "pattern", Type: cty.String},
		},
		Type: function.StaticReturnType(cty.List(cty.String)),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			if opts.WorkflowDir == "" {
				return cty.ListValEmpty(cty.String), fmt.Errorf("fileset(): workflow directory not configured")
			}
			rawPath := args[0].AsString()
			pattern := args[1].AsString()

			// Validate the pattern syntax up-front (filepath.Glob silently
			// returns no matches on invalid pattern; we want a clear error).
			if _, err := filepath.Match(pattern, ""); err != nil {
				return cty.ListValEmpty(cty.String), fmt.Errorf(
					"fileset(): invalid pattern %q: %w", pattern, err)
			}

			// Resolve and confine the directory path.
			resolvedDir, err := resolveConfinedDir(rawPath, opts.WorkflowDir, opts.AllowedPaths)
			if err != nil {
				return cty.ListValEmpty(cty.String), err
			}

			matches, err := collectMatchingFiles(resolvedDir, rawPath, pattern)
			if err != nil {
				return cty.ListValEmpty(cty.String), err
			}

			sort.Strings(matches)

			if len(matches) == 0 {
				return cty.ListValEmpty(cty.String), nil
			}
			vals := make([]cty.Value, len(matches))
			for i, m := range matches {
				vals[i] = cty.StringVal(m)
			}
			return cty.ListVal(vals), nil
		},
	})
}

// collectMatchingFiles reads resolvedDir and returns regular files whose
// basenames match pattern. Results use rawPath as the directory prefix so
// they are relative to WorkflowDir. The caller is responsible for sorting.
func collectMatchingFiles(resolvedDir, rawPath, pattern string) ([]string, error) {
	entries, err := os.ReadDir(resolvedDir)
	if err != nil {
		return nil, fmt.Errorf("fileset(): %w", err)
	}
	var matches []string
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue // skip dirs, symlinks, devices, sockets
		}
		name := entry.Name()
		ok, err := filepath.Match(pattern, name)
		if err != nil {
			// Pattern was validated up-front; this is defensive only.
			return nil, fmt.Errorf("fileset(): pattern %q: %w", pattern, err)
		}
		if ok {
			// Build path relative to WorkflowDir so each.value works
			// with file() / templatefile() without adjustment.
			matches = append(matches, filepath.Join(rawPath, name))
		}
	}
	return matches, nil
}

// resolveConfinedDir is like resolveConfinedPath but verifies the resolved
// path is a directory (not a regular file). Applies the same two-phase
// confinement check (pre- and post-EvalSymlinks) as resolveConfinedPath.
func resolveConfinedDir(raw, base string, allowed []string) (string, error) {
	if filepath.IsAbs(raw) {
		return "", fmt.Errorf("fileset(): absolute paths are not supported; use a path relative to the workflow directory")
	}
	abs := filepath.Clean(filepath.Join(base, raw))

	if err := checkConfinement("fileset()", raw, abs, base, allowed); err != nil {
		if _, statErr := os.Stat(abs); os.IsNotExist(statErr) {
			return "", fmt.Errorf("fileset(): %q does not exist", raw)
		}
		return "", err
	}

	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("fileset(): %q does not exist", raw)
		}
		if os.IsPermission(err) {
			return "", fmt.Errorf("fileset(): permission denied: %s", raw)
		}
		return "", fmt.Errorf("fileset(): %w", err)
	}
	resolved = filepath.Clean(resolved)

	resolvedBase := evalSymlinksOrSelf(base)
	resolvedAllowed := evalSymlinksAll(allowed)

	if err := checkConfinement("fileset()", raw, resolved, resolvedBase, resolvedAllowed); err != nil {
		return "", err
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("fileset(): %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("fileset(): %q is not a directory", raw)
	}
	return resolved, nil
}

// absPathFunction implements abspath(path) → string. Resolves a path relative
// to WorkflowDir and returns an absolute path.
func absPathFunction(opts *FunctionOptions) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "path", Type: cty.String}},
		Type:   function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			raw := args[0].AsString()
			if filepath.IsAbs(raw) {
				return cty.StringVal(filepath.Clean(raw)), nil
			}
			base := opts.WorkflowDir
			if base == "" {
				base = opts.Cwd
			}
			if base == "" {
				return cty.NilVal, fmt.Errorf("abspath(): workflow directory not configured")
			}
			abs := filepath.Clean(filepath.Join(base, raw))
			return cty.StringVal(abs), nil
		},
	})
}

// dirNameFunction implements dirname(path) → string. Returns the parent directory
// component of the given path.
func dirNameFunction() function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "path", Type: cty.String}},
		Type:   function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			return cty.StringVal(filepath.Dir(args[0].AsString())), nil
		},
	})
}

// baseNameFunction implements basename(path) → string. Returns the last element
// of the given path.
func baseNameFunction() function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "path", Type: cty.String}},
		Type:   function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			return cty.StringVal(filepath.Base(args[0].AsString())), nil
		},
	})
}

// hasAttributeFunction implements hasattr(obj, name) → bool. Returns true if
// the given object value has an attribute with the given name. For object
// types the check is static; for dynamic/unknown values the result is unknown.
func hasAttributeFunction() function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{
			{Name: "obj", Type: cty.DynamicPseudoType, AllowNull: true, AllowDynamicType: true},
			{Name: "name", Type: cty.String},
		},
		Type: function.StaticReturnType(cty.Bool),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			obj := args[0]
			name := args[1].AsString()

			if !obj.IsKnown() {
				return cty.UnknownVal(cty.Bool), nil
			}
			if obj.IsNull() {
				return cty.False, nil
			}

			ty := obj.Type()
			if ty == cty.DynamicPseudoType {
				return cty.UnknownVal(cty.Bool), nil
			}
			if ty.IsObjectType() {
				return cty.BoolVal(ty.HasAttribute(name)), nil
			}
			return cty.False, nil
		},
	})
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
	// If confinement fails but the path doesn't exist, report "no such file"
	// so callers with wrong ../ depth get a useful error instead of a
	// misleading "escapes workflow directory" message.
	if err := checkConfinement("file()", raw, abs, base, allowed); err != nil {
		if _, statErr := os.Stat(abs); os.IsNotExist(statErr) {
			return "", mapOSError(raw, statErr)
		}
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

// rewriteFuncName rewrites the prefix <from> to <to> in err's message.
// Used to retag errors from shared confinement helpers with the calling
// function's name (e.g. file()-prefixed errors become templatefile()-prefixed).
func rewriteFuncName(err error, from, to string) error {
	msg := err.Error()
	if strings.HasPrefix(msg, from) {
		return fmt.Errorf("%s%s", to, strings.TrimPrefix(msg, from))
	}
	return err
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

// ctyToGoMap converts a cty object or map value into a Go map[string]any
// suitable for text/template. Nested objects/maps recurse; lists/tuples/sets
// become []any; primitives become string/int64/float64/bool. Null values
// become nil. Unknown values return an error (templatefile cannot meaningfully
// render an unknown).
func ctyToGoMap(v cty.Value) (map[string]any, error) {
	if !v.IsKnown() {
		return nil, fmt.Errorf("vars value is unknown")
	}
	if v.IsNull() {
		return nil, fmt.Errorf("vars must not be null")
	}
	out := make(map[string]any)
	it := v.ElementIterator()
	for it.Next() {
		k, val := it.Element()
		kStr := k.AsString()
		gv, err := ctyToGoValue(val)
		if err != nil {
			return nil, fmt.Errorf("key %q: %w", kStr, err)
		}
		out[kStr] = gv
	}
	return out, nil
}

// ctyToGoValue converts a single cty.Value to its Go-template equivalent.
func ctyToGoValue(v cty.Value) (any, error) {
	if !v.IsKnown() {
		return nil, fmt.Errorf("value is unknown")
	}
	if v.IsNull() {
		return nil, nil
	}
	ty := v.Type()
	switch {
	case ty == cty.String:
		return v.AsString(), nil
	case ty == cty.Bool:
		return v.True(), nil
	case ty == cty.Number:
		// Prefer int64 when exactly representable; else float64.
		if i, acc := v.AsBigFloat().Int64(); acc == big.Exact {
			return i, nil
		}
		f, _ := v.AsBigFloat().Float64()
		return f, nil
	case ty.IsListType() || ty.IsTupleType() || ty.IsSetType():
		var out []any
		it := v.ElementIterator()
		for it.Next() {
			_, elem := it.Element()
			gv, err := ctyToGoValue(elem)
			if err != nil {
				return nil, err
			}
			out = append(out, gv)
		}
		return out, nil
	case ty.IsObjectType() || ty.IsMapType():
		return ctyToGoMap(v)
	default:
		return nil, fmt.Errorf("unsupported type: %s", ty.FriendlyName())
	}
}
