package workflow

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// fileEntry pairs a parsed Spec with the source block ranges from that file,
// used to produce file:line diagnostic locations for merge conflicts.
type fileEntry struct {
	spec   *Spec
	ranges map[string]hcl.Range
}

// collectFileBlockRanges parses src with the low-level hclsyntax parser and
// returns a map from "blocktype:label" (or just "blocktype" for singletons) to
// the DefRange of the first matching block in that file.
//
// Recognised keys: "step:name", "state:name", "variable:name",
// "adapter:type.name", "workflow", "policy", "permissions".
func collectFileBlockRanges(src []byte, filename string) map[string]hcl.Range {
	file, diags := hclsyntax.ParseConfig(src, filename, hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() || file == nil {
		return nil
	}
	body, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		return nil
	}
	result := make(map[string]hcl.Range)
	for _, block := range body.Blocks {
		var key string
		switch block.Type {
		case "step", "state", "variable":
			if len(block.Labels) >= 1 {
				key = block.Type + ":" + block.Labels[0]
			}
		case "adapter":
			if len(block.Labels) >= 2 {
				key = "adapter:" + block.Labels[0] + "." + block.Labels[1]
			}
		case "workflow", "policy", "permissions":
			key = block.Type
		}
		if key != "" {
			if _, already := result[key]; !already {
				rng := block.DefRange()
				result[key] = rng
			}
		}
	}
	return result
}

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
func ParseDir(dir string) (*Spec, hcl.Diagnostics) { //nolint:funlen // W17: file discovery + per-file parse loop + merge + validation are sequential, extraction would obscure the flow
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return nil, hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "cannot read workflow directory",
			Detail:   fmt.Sprintf("failed to list %q: %v", dir, err),
		}}
	}

	// Collect .hcl files in lexicographic order (ReadDir already returns sorted).
	hclFiles := make([]string, 0, len(dirEntries))
	for _, entry := range dirEntries {
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

	var entries []fileEntry
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
			entries = append(entries, fileEntry{
				spec:   spec,
				ranges: collectFileBlockRanges(src, path),
			})
		}
	}

	if allDiags.HasErrors() {
		return nil, allDiags
	}

	merged, mergeDiags := mergeSpecs(dir, entries)
	allDiags = append(allDiags, mergeDiags...)
	if allDiags.HasErrors() {
		return nil, allDiags
	}
	return merged, allDiags
}

// ParseFileOrDir is the unified CLI entry point. If path is a directory, it
// calls ParseDir. If path is a regular file it must have a ".hcl" suffix;
// ParseFileOrDir then attempts to parse the parent directory as a module
// (ParseDir of the parent) so that sibling files are merged. This handles the
// common case where a file is the entry point of a split directory module
// (e.g. workflow.hcl + steps.hcl).
//
// If ParseDir(parent) fails because the parent contains multiple workflow
// header blocks — meaning the parent is a collection of independent workflows,
// not a directory module — ParseFileOrDir falls back to parsing only the named
// file (which must then contain a complete workflow including its header).
//
// Non-".hcl" regular file paths are rejected immediately with an error.
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

	// Only .hcl files are valid workflow entry points.
	if !strings.HasSuffix(path, ".hcl") {
		return nil, hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "invalid workflow file",
			Detail:   fmt.Sprintf("%q is not a .hcl file; workflow entry points must be a directory or a .hcl file", path),
		}}
	}

	// Try to parse the parent directory as a module first. This correctly handles
	// split modules where the named file is the entry point (e.g. workflow.hcl +
	// content.hcl). Sibling files are merged together.
	dirSpec, dirDiags := ParseDir(filepath.Dir(path))
	if !dirDiags.HasErrors() {
		return dirSpec, dirDiags
	}

	// If ParseDir failed because the parent directory is a collection of
	// independent workflows (multiple workflow header blocks), fall back to
	// parsing only the named file as a standalone single-file module.
	if isSingletonConflictOnly(dirDiags) {
		return parseSingleFile(path)
	}

	// For any other ParseDir error (syntax errors in siblings, etc.), propagate.
	return nil, dirDiags
}

// isSingletonConflictOnly returns true when all error diagnostics in diags are
// singleton-conflict errors from mergeSpecs ("duplicate workflow block",
// "duplicate policy block", "duplicate permissions block"). These indicate the
// parent directory is a collection of independent single-file workflows, not a
// directory module. Non-singleton errors (syntax, parse failures) are propagated.
func isSingletonConflictOnly(diags hcl.Diagnostics) bool {
	hasError := false
	for _, d := range diags {
		if d.Severity != hcl.DiagError {
			continue
		}
		hasError = true
		switch d.Summary {
		case "duplicate workflow block", "duplicate policy block", "duplicate permissions block":
		default:
			return false
		}
	}
	return hasError
}

// parseSingleFile parses exactly one .hcl file and requires it to contain a
// workflow header block. Used as a fallback when the parent directory is not a
// directory module.
func parseSingleFile(path string) (*Spec, hcl.Diagnostics) {
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

// mergeSpecs merges a slice of parsed file entries into a single Spec.
// Slice fields are concatenated; singleton fields (Header, Policy, Permissions)
// must appear in at most one file. Block ranges from each entry are used to
// populate Subject/Detail fields in conflict diagnostics with file:line info.
func mergeSpecs(dir string, entries []fileEntry) (*Spec, hcl.Diagnostics) { //nolint:cyclop,gocognit,gocyclo,funlen // W17: multi-field merge with singleton conflict detection requires sequential checks
	if len(entries) == 0 {
		return nil, nil
	}

	var diags hcl.Diagnostics
	merged := &Spec{}

	// Track source bytes for concatenation.
	var srcParts [][]byte

	// Track first-seen ranges for singleton blocks.
	var headerRange, policyRange, permissionsRange *hcl.Range

	for _, entry := range entries {
		s := entry.spec
		ranges := entry.ranges

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
				detail := fmt.Sprintf("directory %q contains more than one workflow \"<name>\" { ... } header block; only one is allowed across all .hcl files in a directory module", dir)
				if headerRange != nil {
					detail += fmt.Sprintf("; previously declared at %s", headerRange.String())
				}
				d := &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "duplicate workflow block",
					Detail:   detail,
				}
				if rng, ok := ranges["workflow"]; ok {
					d.Subject = &rng
				}
				diags = append(diags, d)
			} else {
				merged.Header = s.Header
				if rng, ok := ranges["workflow"]; ok {
					headerRange = &rng
				}
			}
		}

		// Merge singleton: Policy.
		if s.Policy != nil {
			if merged.Policy != nil {
				detail := fmt.Sprintf("directory %q contains more than one policy { ... } block; only one is allowed across all .hcl files in a directory module", dir)
				if policyRange != nil {
					detail += fmt.Sprintf("; previously declared at %s", policyRange.String())
				}
				d := &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "duplicate policy block",
					Detail:   detail,
				}
				if rng, ok := ranges["policy"]; ok {
					d.Subject = &rng
				}
				diags = append(diags, d)
			} else {
				merged.Policy = s.Policy
				if rng, ok := ranges["policy"]; ok {
					policyRange = &rng
				}
			}
		}

		// Merge singleton: Permissions.
		if s.Permissions != nil {
			if merged.Permissions != nil {
				detail := fmt.Sprintf("directory %q contains more than one permissions { ... } block; only one is allowed across all .hcl files in a directory module", dir)
				if permissionsRange != nil {
					detail += fmt.Sprintf("; previously declared at %s", permissionsRange.String())
				}
				d := &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "duplicate permissions block",
					Detail:   detail,
				}
				if rng, ok := ranges["permissions"]; ok {
					d.Subject = &rng
				}
				diags = append(diags, d)
			} else {
				merged.Permissions = s.Permissions
				if rng, ok := ranges["permissions"]; ok {
					permissionsRange = &rng
				}
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
	diags = append(diags, checkDuplicateNames(entries)...)
	if diags.HasErrors() {
		return nil, diags
	}

	// Concatenate source bytes with newline separators.
	merged.SourceBytes = joinBytes(srcParts, '\n')

	return merged, diags
}

// checkDuplicateNames detects duplicate block names across the set of parsed
// file entries. It iterates entries in order so that the first occurrence can
// be referenced in the diagnostic detail for the second occurrence.
func checkDuplicateNames(entries []fileEntry) hcl.Diagnostics {
	var diags hcl.Diagnostics

	type firstSeen struct {
		rng hcl.Range
	}
	seen := make(map[string]*firstSeen)

	addDup := func(blockType, label string, ranges map[string]hcl.Range) {
		key := blockType + ":" + label
		rng, hasRng := ranges[key]
		if first, already := seen[key]; already {
			detail := fmt.Sprintf("%s %q is declared more than once in the directory module", blockType, label)
			if first.rng != (hcl.Range{}) {
				detail += fmt.Sprintf("; previously declared at %s", first.rng.String())
			}
			d := &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("duplicate %s name %q across files", blockType, label),
				Detail:   detail,
			}
			if hasRng {
				d.Subject = &rng
			}
			diags = append(diags, d)
		} else {
			seen[key] = &firstSeen{rng: rng}
		}
	}

	for _, entry := range entries {
		for i := range entry.spec.Steps {
			addDup("step", entry.spec.Steps[i].Name, entry.ranges)
		}
		for _, s := range entry.spec.States {
			addDup("state", s.Name, entry.ranges)
		}
		for _, a := range entry.spec.Adapters {
			addDup("adapter", a.Type+"."+a.Name, entry.ranges)
		}
		for _, v := range entry.spec.Variables {
			addDup("variable", v.Name, entry.ranges)
		}
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
