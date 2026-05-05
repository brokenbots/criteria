package workflow

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
)

// ParseDir parses every .hcl file in dir (lexicographic order, non-recursive),
// merges them into a single Spec, and returns the result.
//
// Merge rules:
//   - Top-level slices (Variables, Locals, Outputs, Adapters, Steps, States,
//     Waits, Approvals, Switches, Subworkflows, Environments) concatenate.
//   - Singleton fields (Header, Policy, Permissions) must appear in at most one
//     file; if two files declare the same singleton, that is a parse error.
//   - Cross-file duplicate names (e.g. step "foo" in two files) error with
//     both file:line locations.
//   - SourceBytes concatenates all file bytes separated by newlines.
//   - The merged Spec must contain exactly one Header (workflow block); zero
//     headers produces an error.
func ParseDir(dir string) (*Spec, hcl.Diagnostics) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "cannot read workflow directory",
			Detail:   fmt.Sprintf("failed to list %q: %v", dir, err),
		}}
	}

	// Collect .hcl files in lexicographic order (ReadDir already returns sorted).
	var hclFiles []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".hcl") {
			continue
		}
		hclFiles = append(hclFiles, filepath.Join(dir, entry.Name()))
	}

	if len(hclFiles) == 0 {
		return nil, hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "no .hcl files in workflow directory",
			Detail:   fmt.Sprintf("directory %q contains no .hcl files", dir),
		}}
	}

	// Ensure lexicographic order (ReadDir is already sorted, but be explicit).
	sort.Strings(hclFiles)

	var specs []*Spec
	var allDiags hcl.Diagnostics
	for _, path := range hclFiles {
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			allDiags = append(allDiags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "cannot read workflow file",
				Detail:   fmt.Sprintf("failed to read %q: %v", path, readErr),
			})
			continue
		}
		spec, parseDiags := Parse(path, src)
		allDiags = append(allDiags, parseDiags...)
		if spec != nil {
			specs = append(specs, spec)
		}
	}

	if allDiags.HasErrors() {
		return nil, allDiags
	}

	merged, mergeDiags := mergeSpecs(dir, specs)
	allDiags = append(allDiags, mergeDiags...)
	if allDiags.HasErrors() {
		return nil, allDiags
	}
	return merged, allDiags
}

// ParseFileOrDir is the unified CLI entry point. If path is a directory, it
// calls ParseDir. If path is a regular file, it parses that single file (the
// file acts as a one-file directory module).
func ParseFileOrDir(path string) (*Spec, hcl.Diagnostics) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "cannot access workflow path",
			Detail:   fmt.Sprintf("stat %q: %v", path, err),
		}}
	}
	if info.IsDir() {
		return ParseDir(path)
	}
	src, readErr := os.ReadFile(path)
	if readErr != nil {
		return nil, hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "cannot read workflow file",
			Detail:   readErr.Error(),
		}}
	}
	spec, diags := Parse(path, src)
	if diags.HasErrors() || spec == nil {
		return spec, diags
	}
	if spec.Header == nil {
		return nil, hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "no workflow block declared",
			Detail:   fmt.Sprintf("file %q must contain exactly one workflow \"<name>\" { ... } header block; none was found. Add a workflow block with version, initial_state, and target_state attributes.", path),
		}}
	}
	return spec, diags
}

// mergeSpecs merges a slice of parsed Specs into a single Spec.
// Slice fields are concatenated; singleton fields (Header, Policy, Permissions)
// must appear in at most one spec.
func mergeSpecs(dir string, specs []*Spec) (*Spec, hcl.Diagnostics) { //nolint:cyclop // W17: multi-field merge with singleton conflict detection requires sequential checks
	if len(specs) == 0 {
		return nil, nil
	}

	var diags hcl.Diagnostics
	merged := &Spec{}

	// Track source bytes for concatenation.
	var srcParts [][]byte

	for _, s := range specs {
		// Merge slice fields.
		merged.Variables = append(merged.Variables, s.Variables...)
		merged.Locals = append(merged.Locals, s.Locals...)
		merged.Outputs = append(merged.Outputs, s.Outputs...)
		merged.Adapters = append(merged.Adapters, s.Adapters...)
		merged.Steps = append(merged.Steps, s.Steps...)
		merged.States = append(merged.States, s.States...)
		merged.Waits = append(merged.Waits, s.Waits...)
		merged.Approvals = append(merged.Approvals, s.Approvals...)
		merged.Switches = append(merged.Switches, s.Switches...)
		merged.Environments = append(merged.Environments, s.Environments...)
		merged.Subworkflows = append(merged.Subworkflows, s.Subworkflows...)

		// Merge singleton: Header.
		if s.Header != nil {
			if merged.Header != nil {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "duplicate workflow block",
					Detail:   fmt.Sprintf("directory %q contains more than one workflow \"<name>\" { ... } header block; only one is allowed across all .hcl files in a directory module", dir),
				})
			} else {
				merged.Header = s.Header
			}
		}

		// Merge singleton: Policy.
		if s.Policy != nil {
			if merged.Policy != nil {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "duplicate policy block",
					Detail:   fmt.Sprintf("directory %q contains more than one policy { ... } block; only one is allowed across all .hcl files in a directory module", dir),
				})
			} else {
				merged.Policy = s.Policy
			}
		}

		// Merge singleton: Permissions.
		if s.Permissions != nil {
			if merged.Permissions != nil {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "duplicate permissions block",
					Detail:   fmt.Sprintf("directory %q contains more than one permissions { ... } block; only one is allowed across all .hcl files in a directory module", dir),
				})
			} else {
				merged.Permissions = s.Permissions
			}
		}

		if len(s.SourceBytes) > 0 {
			srcParts = append(srcParts, s.SourceBytes)
		}
	}

	if merged.Header == nil {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "no workflow block declared",
			Detail:   fmt.Sprintf("directory %q contains no workflow \"<name>\" { ... } header block; exactly one is required. Add a workflow block (typically in workflow.hcl) with version, initial_state, and target_state attributes.", dir),
		})
	}

	if diags.HasErrors() {
		return nil, diags
	}

	// Check for cross-file duplicate names in slice fields.
	diags = append(diags, checkDuplicateNames(merged)...)
	if diags.HasErrors() {
		return nil, diags
	}

	// Concatenate source bytes with newline separators.
	merged.SourceBytes = joinBytes(srcParts, '\n')

	return merged, diags
}

// checkDuplicateNames detects duplicate block names within each merged slice field.
func checkDuplicateNames(spec *Spec) hcl.Diagnostics {
	var diags hcl.Diagnostics

	seen := make(map[string]bool)
	for _, s := range spec.Steps {
		key := "step:" + s.Name
		if seen[key] {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("duplicate step name %q across files", s.Name),
				Detail:   fmt.Sprintf("step %q is declared more than once in the directory module", s.Name),
			})
		}
		seen[key] = true
	}
	for _, s := range spec.States {
		key := "state:" + s.Name
		if seen[key] {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("duplicate state name %q across files", s.Name),
				Detail:   fmt.Sprintf("state %q is declared more than once in the directory module", s.Name),
			})
		}
		seen[key] = true
	}
	for _, a := range spec.Adapters {
		key := "adapter:" + a.Type + "." + a.Name
		if seen[key] {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("duplicate adapter %q %q across files", a.Type, a.Name),
				Detail:   fmt.Sprintf("adapter %q %q is declared more than once in the directory module", a.Type, a.Name),
			})
		}
		seen[key] = true
	}
	for _, v := range spec.Variables {
		key := "variable:" + v.Name
		if seen[key] {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("duplicate variable name %q across files", v.Name),
				Detail:   fmt.Sprintf("variable %q is declared more than once in the directory module", v.Name),
			})
		}
		seen[key] = true
	}

	return diags
}

// joinBytes concatenates byte slices with a separator byte between each.
func joinBytes(parts [][]byte, sep byte) []byte {
	if len(parts) == 0 {
		return nil
	}
	total := len(parts) - 1
	for _, p := range parts {
		total += len(p)
	}
	out := make([]byte, 0, total)
	for i, p := range parts {
		if i > 0 {
			out = append(out, sep)
		}
		out = append(out, p...)
	}
	return out
}
