package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/zclconf/go-cty/cty"
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
// into a map. Entries without "=" are silently ignored.
func parseVarOverrides(raw []string) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	out := make(map[string]string, len(raw))
	for _, kv := range raw {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		out[k] = v
	}
	return out
}

// parseVarFile reads variable overrides from a file.
// Supported formats:
//   - .chcl or .hcl: flat top-level attributes (key = "value")
//   - .json: flat object { "key": "value" }
func parseVarFile(path string) (map[string]string, error) {
	ext := strings.ToLower(filepath.Ext(path))

	switch {
	case isHCLExtension(ext):
		return parseHCLVarFile(path)
	case ext == ".json":
		return parseJSONVarFile(path)
	default:
		return nil, fmt.Errorf("unsupported var-file extension %q for %q; supported extensions are %s", ext, path, strings.Join(HCLExtensions, ", ")+", .json")
	}
}

// parseHCLVarFile reads a flat key = "value" HCL file.
func parseHCLVarFile(path string) (map[string]string, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read HCL var-file %q: %w", path, err)
	}
	parser := hclparse.NewParser()
	f, diags := parser.ParseHCL(src, path)
	if diags.HasErrors() {
		return nil, fmt.Errorf("failed to parse HCL var-file %q: %w", path, diags)
	}
	attrs, diags := f.Body.JustAttributes()
	if diags.HasErrors() {
		return nil, fmt.Errorf("failed to read attributes from HCL var-file %q: %w", path, diags)
	}
	out := make(map[string]string, len(attrs))
	for name, attr := range attrs {
		val, diags := attr.Expr.Value(nil)
		if diags.HasErrors() {
			return nil, fmt.Errorf("var-file %q: failed to evaluate key %q: %w", path, name, diags)
		}
		if val.Type() != cty.String {
			return nil, fmt.Errorf("var-file %q: key %q has non-string value (%s); only string values are supported", path, name, val.Type().FriendlyName())
		}
		out[name] = val.AsString()
	}
	return out, nil
}

// parseJSONVarFile reads a flat { "key": "value" } JSON file.
func parseJSONVarFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read JSON var-file %q: %w", path, err)
	}
	var raw map[string]string
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse JSON var-file %q: %w", path, err)
	}
	return raw, nil
}

// isHCLExtension reports whether ext is one of the recognised HCL extensions.
func isHCLExtension(ext string) bool {
	for _, e := range HCLExtensions {
		if ext == e {
			return true
		}
	}
	return false
}

// mergeVarSources merges variable overrides from --var-file and --var flags.
// Files are processed left-to-right (later files overwrite earlier ones),
// then --var overrides take highest precedence.
func mergeVarSources(varFiles, varOverrides []string) (map[string]string, error) {
	merged := make(map[string]string)
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
