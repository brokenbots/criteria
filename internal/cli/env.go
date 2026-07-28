package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

// envOrDefault resolves configuration with precedence env -> default.
// Cobra flags can then override this default value.
func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// parseVarOverrides converts a slice of "key=value" strings (from --var flags)
// into typed cty.Values. Each value is parsed as an HCL expression first, so
// structured literals like ["a", "b"] and {a = 1} are preserved; values that do
// not parse as HCL (or reference variables) fall back to plain strings.
// Entries without "=" are silently ignored.
func parseVarOverrides(raw []string) map[string]cty.Value {
	if len(raw) == 0 {
		return nil
	}
	out := make(map[string]cty.Value, len(raw))
	for _, kv := range raw {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		val, err := parseVarOverrideValue(v)
		if err != nil {
			// Defensive fallback: treat the raw text as a string so scalar overrides
			// always have a value even if HCL parsing surprises us.
			out[k] = cty.StringVal(v)
			continue
		}
		out[k] = val
	}
	return out
}

// parseVarOverrideValue parses a raw --var value as an HCL expression. If the
// expression references variables or cannot be parsed, it returns an error and
// the caller should fall back to a plain string.
func parseVarOverrideValue(raw string) (cty.Value, error) {
	expr, diags := hclsyntax.ParseExpression([]byte(raw), "<var>", hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		return cty.NilVal, diags
	}
	val, diags := expr.Value(nil)
	if diags.HasErrors() {
		return cty.NilVal, diags
	}
	if !val.IsKnown() {
		return cty.NilVal, fmt.Errorf("value is unknown")
	}
	return val, nil
}

// parseVarFile reads variable overrides from a file.
// Supported formats:
//   - .chcl or .hcl: top-level attributes (key = value) of any cty type
//   - .json: object whose values are decoded through the cty JSON codec so that
//     lists, maps, and objects keep their native shape
func parseVarFile(path string) (map[string]cty.Value, error) {
	switch {
	case hasHCLExtension(path):
		return parseHCLVarFile(path)
	case filepath.Ext(path) == ".json":
		return parseJSONVarFile(path)
	default:
		ext := filepath.Ext(path)
		return nil, fmt.Errorf("unsupported var-file extension %q for %q; supported extensions are %s", ext, path, strings.Join(HCLExtensions, ", ")+", .json")
	}
}

// parseHCLVarFile reads top-level HCL attributes and evaluates them to cty values.
func parseHCLVarFile(path string) (map[string]cty.Value, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read HCL var-file %q: %w", path, err)
	}
	parser := hclparse.NewParser()
	f, diags := parser.ParseHCL(src, path)
	if diags.HasErrors() {
		return nil, fmt.Errorf("failed to parse HCL var-file %q: %w", path, diags)
	}
	if f == nil {
		return nil, fmt.Errorf("failed to parse HCL var-file %q: parser returned nil file without errors", path)
	}
	attrs, diags := f.Body.JustAttributes()
	if diags.HasErrors() {
		return nil, fmt.Errorf("failed to read attributes from HCL var-file %q: %w", path, diags)
	}
	out := make(map[string]cty.Value, len(attrs))
	for name, attr := range attrs {
		val, diags := attr.Expr.Value(nil)
		if diags.HasErrors() {
			return nil, fmt.Errorf("var-file %q: failed to evaluate key %q: %w", path, name, diags)
		}
		out[name] = val
	}
	return out, nil
}

// parseJSONVarFile reads a JSON object and decodes each value with the cty JSON
// codec so structured values survive the file round-trip.
func parseJSONVarFile(path string) (map[string]cty.Value, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read JSON var-file %q: %w", path, err)
	}
	ty, err := ctyjson.ImpliedType(data)
	if err != nil {
		return nil, fmt.Errorf("failed to infer JSON var-file %q structure: %w", path, err)
	}
	if !ty.IsObjectType() {
		return nil, fmt.Errorf("JSON var-file %q: top-level value must be an object", path)
	}
	root, err := ctyjson.Unmarshal(data, ty)
	if err != nil {
		return nil, fmt.Errorf("failed to parse JSON var-file %q: %w", path, err)
	}
	out := make(map[string]cty.Value, len(ty.AttributeTypes()))
	for name := range ty.AttributeTypes() {
		out[name] = root.GetAttr(name)
	}
	return out, nil
}

// mergeVarSources merges variable overrides from --var-file and --var flags.
// Files are processed left-to-right (later files overwrite earlier ones),
// then --var overrides take highest precedence.
func mergeVarSources(varFiles, varOverrides []string) (map[string]cty.Value, error) {
	merged := make(map[string]cty.Value)
	for _, path := range varFiles {
		fileVars, err := parseVarFile(path)
		if err != nil {
			return nil, err
		}
		for k, v := range fileVars {
			merged[k] = v
		}
	}
	for k, v := range parseVarOverrides(varOverrides) {
		merged[k] = v
	}
	return merged, nil
}
