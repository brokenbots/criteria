package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/internal/adapter/oci"
)

// ResolveContext carries the inputs for reference resolution.
type ResolveContext struct {
	// GlobalConfigPath is the path to ~/.criteria/config.hcl. If empty,
	// defaultGlobalConfigPath is used.
	GlobalConfigPath string

	// WorkflowAliases are alias definitions parsed from the workflow HCL
	// (registry "<alias>" { source = "..." } blocks). These override globals.
	WorkflowAliases map[string]string
}

// Resolve turns a user-supplied string into a fully-qualified oci.Reference.
//
// Supported forms:
//   - "ghcr.io/org/name:1.2.3"      -> as-is
//   - "name:1.2.3"                  -> looks up "name" alias in config; errors if absent
//   - "@sha256:..."                 -> requires --resolve flag (rare; for repair scenarios)
func Resolve(ctx ResolveContext, raw string) (oci.Reference, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return oci.Reference{}, fmt.Errorf("resolve: empty reference")
	}

	// Digest-only repair scenario.
	if strings.HasPrefix(raw, "@") {
		return oci.Reference{}, fmt.Errorf("resolve: digest-only reference %q requires the --resolve flag", raw)
	}

	// If it already contains a registry (has a slash before any colon that
	// isn't part of a port), treat as fully-qualified.
	ref, err := oci.Parse(raw)
	if err == nil && ref.Registry != "" && ref.Repo != "" {
		return ref, nil
	}

	// Try alias resolution for "alias:tag" or "alias".
	alias, tag, hasTag := strings.Cut(raw, ":")
	if !hasTag {
		return oci.Reference{}, fmt.Errorf("resolve: %q is not a fully-qualified reference and has no tag/version", raw)
	}

	source, err := lookupAlias(ctx, alias)
	if err != nil {
		return oci.Reference{}, fmt.Errorf("resolve: %w", err)
	}

	fq := source + ":" + tag
	return oci.Parse(fq)
}

// lookupAlias searches workflow aliases first, then global config.
func lookupAlias(ctx ResolveContext, alias string) (string, error) {
	if ctx.WorkflowAliases != nil {
		if src, ok := ctx.WorkflowAliases[alias]; ok {
			return src, nil
		}
	}

	globals, err := loadGlobalAliases(ctx.GlobalConfigPath)
	if err != nil {
		return "", err
	}
	if src, ok := globals[alias]; ok {
		return src, nil
	}
	return "", fmt.Errorf("alias %q not found in workflow or global config", alias)
}

// loadGlobalAliases parses ~/.criteria/config.hcl for registry blocks.
func loadGlobalAliases(configPath string) (map[string]string, error) {
	if configPath == "" {
		var err error
		configPath, err = defaultGlobalConfigPath()
		if err != nil {
			return nil, err
		}
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("read global config: %w", err)
	}

	file, diags := hclsyntax.ParseConfig(data, configPath, hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		return nil, fmt.Errorf("parse global config: %w", diags)
	}

	body, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		return nil, fmt.Errorf("global config body is not hclsyntax")
	}

	aliases := make(map[string]string)
	for _, block := range body.Blocks {
		if block.Type != "registry" || len(block.Labels) != 1 {
			continue
		}
		alias := block.Labels[0]
		attrs, _, attrDiags := block.Body.PartialContent(&hcl.BodySchema{
			Attributes: []hcl.AttributeSchema{{Name: "source"}},
		})
		if attrDiags.HasErrors() {
			continue
		}
		if attr, ok := attrs.Attributes["source"]; ok {
			val, valDiags := attr.Expr.Value(nil)
			if !valDiags.HasErrors() && val.Type() == cty.String && !val.IsNull() {
				aliases[alias] = val.AsString()
			}
		}
	}
	return aliases, nil
}
