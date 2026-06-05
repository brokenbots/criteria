package workflow

// compile_environments.go — compile path for environment "<type>" "<name>" blocks.
// Environments are compile-time-resolved typed execution contexts that can be
// bound to adapters and steps.

import (
	"fmt"
	"regexp"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// environmentNamePattern validates that environment names match ^[a-zA-Z][a-zA-Z0-9_-]*$
var environmentNamePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]*$`)

// ShellControlledEnvVars is the set of environment variable names that the shell adapter
// reserves and will not override, even if declared in an environment block.
// These vars are enforced for security and consistency reasons (see sandbox.go).
// Exported for use in runtime filtering (internal/engine/node_step.go).
var ShellControlledEnvVars = map[string]bool{
	"HOME":    true,
	"USER":    true,
	"LOGNAME": true,
	"LANG":    true,
	"TZ":      true,
	"PATH":    true, // sanitized by shell adapter
}

// IsShellLCPrefix reports whether a variable name is a locale-control prefix (LC_*).
// These are reserved by the shell adapter for locale settings.
func IsShellLCPrefix(name string) bool {
	return len(name) >= 3 && name[:3] == "LC_"
}

// compileEnvironments folds and stores every environment block.
// Both variables and config maps must fold at compile (no runtime-only refs).
func compileEnvironments(g *FSMGraph, spec *Spec, opts CompileOpts, envReg EnvRegistry) hcl.Diagnostics {
	if len(spec.Environments) == 0 {
		return nil
	}

	var diags hcl.Diagnostics

	// Validate all environment declarations and fold their variables/config.
	seen := make(map[string]bool) // tracks "<type>.<name>" uniqueness
	for _, envSpec := range spec.Environments {
		diags = append(diags, compileEnvironmentBlock(g, envSpec, opts, envReg, seen)...)
	}

	// Resolve default environment rules.
	diags = append(diags, resolveDefaultEnvironment(g, spec)...)

	return diags
}

// compileEnvironmentBlock validates and compiles a single environment declaration.
//
//nolint:gocognit,gocyclo,funlen // WS09: multi-phase validation is intentionally sequential; length/complexity from policy+os+variables+config+type-specific paths
func compileEnvironmentBlock(g *FSMGraph, envSpec EnvironmentSpec, opts CompileOpts, envReg EnvRegistry, seen map[string]bool) hcl.Diagnostics {
	// Validate block basics (type, name, duplicates)
	diags := validateEnvironmentBasics(envSpec, envReg, seen)
	if diags.HasErrors() {
		return diags
	}

	key := fmt.Sprintf("%s.%s", envSpec.Type, envSpec.Name)
	seen[key] = true

	handler := envReg.Lookup(envSpec.Type)
	if handler == nil {
		// Already diagnosed in validateEnvironmentBasics; skip further work.
		return diags
	}

	// Validate known fields via handler.
	diags = append(diags, handler.ValidateFields(envSpec.Remain)...)

	// Parse variables and config attributes.
	// Remote environments may contain mtls { ... } blocks which JustAttributes()
	// rejects; tolerate them here since ValidateFields already checked them.
	attrs, d := BodyJustAttributesToleratingBlocks(envSpec.Remain, HandlerAllowedBlocks(envSpec.Type))
	diags = append(diags, d...)

	// Parse optional policy_mode (default "permissive").
	policyMode := "permissive"
	if attr, ok := attrs["policy_mode"]; ok {
		val, valDiags := attr.Expr.Value(nil)
		diags = append(diags, valDiags...)
		if valDiags.HasErrors() {
			return diags
		}
		if val.Type() == cty.String && val.IsKnown() && !val.IsNull() {
			pm := val.AsString()
			if pm != "permissive" && pm != "strict" {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  fmt.Sprintf("environment %q: policy_mode must be \"permissive\" or \"strict\"", key),
					Subject:  attr.Expr.Range().Ptr(),
				})
			} else {
				policyMode = pm
			}
		}
	}

	// Parse optional os attribute.
	var osVal string
	if attr, ok := attrs["os"]; ok {
		val, valDiags := attr.Expr.Value(nil)
		diags = append(diags, valDiags...)
		if valDiags.HasErrors() {
			return diags
		}
		if val.Type() == cty.String && val.IsKnown() && !val.IsNull() {
			osVal = val.AsString()
		}
	}

	// Parse optional working_directory attribute. Container environments reject
	// it in their ValidateFields (they isolate paths rather than relocate cwd);
	// shell, sandbox, and remote environments inject it into the step's
	// working_directory input at runtime.
	var workingDir string
	if attr, ok := attrs["working_directory"]; ok {
		val, valDiags := attr.Expr.Value(nil)
		diags = append(diags, valDiags...)
		if valDiags.HasErrors() {
			return diags
		}
		if val.Type() == cty.String && val.IsKnown() && !val.IsNull() {
			workingDir = val.AsString()
		}
	}

	// OS gate: if os is set and does not match the host GOOS, emit error.
	if osVal != "" && osVal != envRegistryHostOS {
		var supportedList string
		if supported := handler.SupportedOSes(); len(supported) > 0 {
			supportedList = fmt.Sprintf("; handler supports %v", supported)
		}
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("environment %q: os %q does not match host %q%s", key, osVal, envRegistryHostOS, supportedList),
			Subject:  attrRangePtr(attrs, "os"),
		})
	}

	// Decode variables and config.
	variables, d := decodeEnvironmentVariables(attrs, opts)
	diags = append(diags, d...)
	config, d := decodeEnvironmentConfig(attrs, opts)
	diags = append(diags, d...)

	if diags.HasErrors() {
		return diags
	}

	// Decode secrets policy if present.
	var secretsPolicy *SecretsPolicy
	if attr, ok := attrs["secrets"]; ok {
		sp, spDiags := decodeSecretsPolicy(attr)
		diags = append(diags, spDiags...)
		if !spDiags.HasErrors() {
			secretsPolicy = sp
		}
	}

	// Collect type-specific attributes (everything other than known ones).
	typeSpecific := make(map[string]cty.Value)
	for name, attr := range attrs {
		switch name {
		case "variables", "config", "policy_mode", "os", "working_directory", "secrets":
			continue
		}
		val, valDiags := attr.Expr.Value(nil)
		diags = append(diags, valDiags...)
		if !valDiags.HasErrors() {
			typeSpecific[name] = val
		}
	}

	// Check for controlled-set conflicts.
	diags = append(diags, checkShellControlledSetConflicts(envSpec.Type, variables, attrs)...)

	// Store the compiled environment.
	g.Environments[key] = &EnvironmentNode{
		Type:             envSpec.Type,
		Name:             envSpec.Name,
		Variables:        variables,
		Config:           config,
		PolicyMode:       policyMode,
		OS:               osVal,
		WorkingDirectory: workingDir,
		Secrets:          secretsPolicy,
		TypeSpecific:     typeSpecific,
		RawBody:          envSpec.Remain,
	}

	return diags
}

// attrRangePtr returns the source range pointer for an attribute by name,
// or nil if the attribute is absent.
func attrRangePtr(attrs hcl.Attributes, name string) *hcl.Range {
	if attr, ok := attrs[name]; ok {
		r := attr.Range
		return &r
	}
	return nil
}

// HandlerAllowedBlocks returns the block types that a given environment type
// is permitted to contain. Remote tolerates mtls, network, filesystem, and
// resources blocks; only mtls is actively parsed at this time.
func HandlerAllowedBlocks(envType string) []string {
	switch envType {
	case "remote":
		return []string{"mtls", "network", "filesystem", "resources"}
	default:
		return nil
	}
}

// BodyJustAttributesToleratingBlocks extracts attributes from an HCL body.
// For *hclsyntax.Body it ignores blocks whose type is in allowedBlockTypes
// and returns diagnostics for any unexpected blocks. For other body types it
// falls back to hcl.Body.JustAttributes.
func BodyJustAttributesToleratingBlocks(body hcl.Body, allowedBlockTypes []string) (hcl.Attributes, hcl.Diagnostics) {
	if raw, ok := body.(*hclsyntax.Body); ok {
		attrs := make(hcl.Attributes)
		for name, attr := range raw.Attributes {
			attrs[name] = attr.AsHCLAttribute()
		}
		var diags hcl.Diagnostics
		for _, block := range raw.Blocks {
			if !isAllowedBlockType(block.Type, allowedBlockTypes) {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  fmt.Sprintf("Unexpected %q block", block.Type),
					Detail:   "Blocks are not allowed here.",
					Subject:  &block.OpenBraceRange,
				})
			}
		}
		return attrs, diags
	}
	return body.JustAttributes()
}

func isAllowedBlockType(blockType string, allowed []string) bool {
	for _, a := range allowed {
		if a == blockType {
			return true
		}
	}
	return false
}

// validateEnvironmentBasics validates type, name, and duplicate checks for an environment block.
func validateEnvironmentBasics(envSpec EnvironmentSpec, envReg EnvRegistry, seen map[string]bool) hcl.Diagnostics {
	var diags hcl.Diagnostics

	if handler := envReg.Lookup(envSpec.Type); handler == nil {
		types := envReg.Registered()
		var typesList string
		if len(types) == 0 {
			typesList = "(none registered)"
		} else {
			typesList = fmt.Sprintf("registered types: %v", types)
		}
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("environment type %q is not registered (%s)", envSpec.Type, typesList),
			Subject:  envSpec.Remain.MissingItemRange().Ptr(),
		})
	}

	// Validate name matches pattern.
	if !environmentNamePattern.MatchString(envSpec.Name) {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("environment name %q must match ^[a-zA-Z][a-zA-Z0-9_-]*$", envSpec.Name),
			Subject:  envSpec.Remain.MissingItemRange().Ptr(),
		})
	}

	// Check for duplicate <type>.<name>.
	key := fmt.Sprintf("%s.%s", envSpec.Type, envSpec.Name)
	if seen[key] {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("duplicate environment %q", key),
			Subject:  envSpec.Remain.MissingItemRange().Ptr(),
		})
	}

	return diags
}

// decodeEnvironmentVariables extracts and folds the optional "variables" attribute.
// Must fold to cty.Map(cty.String) (every value coerced to string).
func decodeEnvironmentVariables(attrs hcl.Attributes, opts CompileOpts) (map[string]string, hcl.Diagnostics) {
	var diags hcl.Diagnostics
	result := make(map[string]string)

	varAttr, ok := attrs["variables"]
	if !ok {
		return result, nil
	}

	// Fold the variables expression in the compile-time closure (var + local + literal + funcs).
	// Note: environments are compiled before agents/steps, so we only have variables/locals available.
	// We pass nil for the graph here; the fold happens in the context of declared variables/locals,
	// and environment expressions cannot reference steps or runtime-only values anyway.
	val, foldable, d := FoldExpr(varAttr.Expr, nil, nil, opts.WorkflowDir)
	diags = append(diags, d...)

	if !foldable {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "environment variables must fold at compile time (no runtime-only references like each.value or steps.X.outputs.Y)",
			Subject:  varAttr.Expr.Range().Ptr(),
		})
		return result, diags
	}

	if diags.HasErrors() {
		return result, diags
	}

	// Coerce the value to map(string).
	d = coerceEnvironmentVariablesToString(val, result, varAttr)
	diags = append(diags, d...)
	return result, diags
}

// coerceEnvironmentVariablesToString coerces map/object values to strings, handling string/number/bool types.
func coerceEnvironmentVariablesToString(val cty.Value, result map[string]string, varAttr *hcl.Attribute) hcl.Diagnostics {
	var diags hcl.Diagnostics
	if val.Type().IsObjectType() || val.Type().IsMapType() {
		// Convert to map[string]string, coercing each value to string.
		for k, v := range val.AsValueMap() {
			switch {
			case v.Type() == cty.String:
				result[k] = v.AsString()
			case v.Type() == cty.Number:
				// Coerce number to string.
				bf := v.AsBigFloat()
				result[k] = bf.Text('f', -1)
			case v.Type() == cty.Bool:
				// Coerce bool to string.
				if v.True() {
					result[k] = "true"
				} else {
					result[k] = "false"
				}
			default:
				// Unsupported type for variables.
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  fmt.Sprintf("environment variables must be string, number, or bool; got %s for key %q", v.Type().FriendlyName(), k),
					Subject:  varAttr.Expr.Range().Ptr(),
				})
			}
		}
	} else {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("environment variables must be a map or object; got %s", val.Type().FriendlyName()),
			Subject:  varAttr.Expr.Range().Ptr(),
		})
	}
	return diags
}

// decodeEnvironmentConfig extracts and folds the optional "config" attribute.
// For v0.3.0, shape is unenforced; the config is stored as-is for Phase 4 consumption.
func decodeEnvironmentConfig(attrs hcl.Attributes, opts CompileOpts) (map[string]cty.Value, hcl.Diagnostics) {
	var diags hcl.Diagnostics
	result := make(map[string]cty.Value)

	cfgAttr, ok := attrs["config"]
	if !ok {
		return result, nil
	}

	// Fold the config expression in the compile-time closure (var + local + literal + funcs).
	val, foldable, d := FoldExpr(cfgAttr.Expr, nil, nil, opts.WorkflowDir)
	diags = append(diags, d...)

	if !foldable {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "environment config must fold at compile time (no runtime-only references like each.value or steps.X.outputs.Y)",
			Subject:  cfgAttr.Expr.Range().Ptr(),
		})
		return result, diags
	}

	if diags.HasErrors() {
		return result, diags
	}

	// Store as map[string]cty.Value. Shape validation lands in Phase 4.
	if val.Type().IsObjectType() || val.Type().IsMapType() {
		result = val.AsValueMap()
	} else {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("environment config must be a map or object; got %s", val.Type().FriendlyName()),
			Subject:  cfgAttr.Expr.Range().Ptr(),
		})
	}

	return result, diags
}

// decodeSecretsPolicy parses the optional `secrets` attribute into a SecretsPolicy.
func decodeSecretsPolicy(attr *hcl.Attribute) (*SecretsPolicy, hcl.Diagnostics) {
	var diags hcl.Diagnostics

	val, valDiags := attr.Expr.Value(nil)
	diags = append(diags, valDiags...)
	if valDiags.HasErrors() {
		return nil, diags
	}

	if !val.Type().IsObjectType() && !val.Type().IsMapType() {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "secrets must be an object with provider and optional fallback",
			Subject:  attr.Expr.Range().Ptr(),
		})
		return nil, diags
	}

	m := val.AsValueMap()
	sp := &SecretsPolicy{}

	if pv, ok := m["provider"]; ok {
		sp.Provider, diags = decodeSecretsProvider(pv, diags, attr)
	}
	if fv, ok := m["fallback"]; ok {
		sp.Fallback, diags = decodeSecretsFallback(fv, diags, attr)
	}

	return sp, diags
}

func decodeSecretsProvider(pv cty.Value, diags hcl.Diagnostics, attr *hcl.Attribute) (string, hcl.Diagnostics) {
	if pv.Type() == cty.String && pv.IsKnown() && !pv.IsNull() {
		return pv.AsString(), diags
	}
	diags = append(diags, &hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  "secrets provider must be a string",
		Subject:  attr.Expr.Range().Ptr(),
	})
	return "", diags
}

func decodeSecretsFallback(fv cty.Value, diags hcl.Diagnostics, attr *hcl.Attribute) ([]string, hcl.Diagnostics) {
	if !fv.Type().IsTupleType() && !fv.Type().IsListType() {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "secrets fallback must be a list of strings",
			Subject:  attr.Expr.Range().Ptr(),
		})
		return nil, diags
	}

	var out []string
	for _, elem := range fv.AsValueSlice() {
		if elem.Type() == cty.String && elem.IsKnown() && !elem.IsNull() {
			out = append(out, elem.AsString())
			continue
		}
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "secrets fallback must be a list of strings",
			Subject:  attr.Expr.Range().Ptr(),
		})
	}
	return out, diags
}

// resolveEnvironmentExpr evaluates an environment expression (e.g. shell.ci) and
// returns the "<type>.<name>" key, or "" if the expression is absent.
// It emits a diagnostic if the expression is not a bare traversal.
func resolveEnvironmentExpr(expr hcl.Expression, context string) (string, hcl.Diagnostics) {
	if isAbsentExpr(expr) {
		return "", nil
	}
	trav, diags := hcl.AbsTraversalForExpr(expr)
	if diags.HasErrors() {
		return "", hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("%s: environment must be a bareword reference (e.g. shell.ci), not a quoted string", context),
			Subject:  expr.Range().Ptr(),
		}}
	}
	if len(trav) != 2 {
		return "", hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("%s: environment must have exactly 2 segments (<type>.<name>); got %d", context, len(trav)),
			Subject:  expr.Range().Ptr(),
		}}
	}
	typeRoot, typeOK := trav[0].(hcl.TraverseRoot)
	nameAttr, nameOK := trav[1].(hcl.TraverseAttr)
	if !typeOK || !nameOK {
		return "", hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("%s: environment segments must be bareword identifiers (<type>.<name>)", context),
			Subject:  expr.Range().Ptr(),
		}}
	}
	return fmt.Sprintf("%s.%s", typeRoot.Name, nameAttr.Name), nil
}

// resolveDefaultEnvironment implements the default-environment resolution rules.
// If multiple environments are declared and no explicit default is set,
// error if any consumer uses an environment.
func resolveDefaultEnvironment(g *FSMGraph, spec *Spec) hcl.Diagnostics {
	var diags hcl.Diagnostics

	// If the workflow header specifies an explicit default, use it.
	if spec.Header != nil {
		key, d := resolveEnvironmentExpr(spec.Header.DefaultEnvironment, "workflow")
		diags = append(diags, d...)
		if key != "" {
			g.DefaultEnvironment = key
			// Validate that the referenced environment exists.
			if _, ok := g.Environments[key]; !ok {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  fmt.Sprintf("workflow environment %q does not refer to a declared environment block", key),
				})
			}
			return diags
		}
	}

	// If exactly one environment is declared, make it the default.
	if len(g.Environments) == 1 {
		for key := range g.Environments {
			g.DefaultEnvironment = key
			break
		}
	}

	// If multiple environments are declared with no explicit default,
	// the error is deferred to the consumer resolution phase (Step 3 / Step 4),
	// which will fire "ambiguous default environment" if a consumer is unbound.
	// For now, this is valid; the error only fires if someone actually tries to use
	// the unambiguous default.

	return diags
}

// checkShellControlledSetConflicts emits warnings for environment variables that
// conflict with the shell adapter's controlled set. These variables will be filtered
// out during runtime and never reach the subprocess.
func checkShellControlledSetConflicts(envType string, variables map[string]string, attrs hcl.Attributes) hcl.Diagnostics {
	var diags hcl.Diagnostics

	// Only shell type needs controlled-set warnings.
	if envType != "shell" {
		return diags
	}

	varAttr, ok := attrs["variables"]
	if !ok || len(variables) == 0 {
		return diags
	}

	for varName := range variables {
		// Check for exact matches in the controlled set
		if ShellControlledEnvVars[varName] {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagWarning,
				Summary:  fmt.Sprintf("environment variable %q conflicts with the shell adapter's controlled set and will be filtered out", varName),
				Detail:   fmt.Sprintf("The shell adapter enforces %q for security and consistency; this value will not be injected into the subprocess. If you need to set this, use the corresponding step input field instead (e.g., input.command_path for PATH).", varName),
				Subject:  varAttr.Expr.Range().Ptr(),
			})
		}
		// Check for LC_* prefixes
		if IsShellLCPrefix(varName) {
			// LC_* is controlled by the shell adapter for locale support; warn but allow
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagWarning,
				Summary:  fmt.Sprintf("environment variable %q matches LC_* prefix which is controlled by the shell adapter", varName),
				Detail:   "The shell adapter enforces LC_* variables for locale support; this value will be filtered out and not injected into the subprocess.",
				Subject:  varAttr.Expr.Range().Ptr(),
			})
		}
	}

	return diags
}

// resolveEnvironmentPolicy applies D37's three-rule field resolution to an
// environment node and an adapter's optional manifest hints. It returns a
// ResolvedPolicy with the final values for each policy field.
//
//nolint:gocyclo // WS09: sequential field-by-field resolution is clearer than a table-driven loop
func resolveEnvironmentPolicy(env *EnvironmentNode, hints *PolicyHints) *ResolvedPolicy {
	if env == nil {
		return &ResolvedPolicy{PolicyMode: "permissive"}
	}

	// PolicyMode is always set (defaults to "permissive" during parsing).
	mode := env.PolicyMode
	if mode == "" {
		mode = "permissive"
	}

	rp := &ResolvedPolicy{PolicyMode: mode}

	// OS
	if env.OS != "" {
		rp.OS = env.OS
	} else if mode == "permissive" && hints != nil && hints.OS != "" {
		rp.OS = hints.OS
	} // strict → zero value ("")

	// Filesystem
	if env.Filesystem != nil {
		rp.Filesystem = env.Filesystem
	} else if mode == "permissive" && hints != nil && hints.Filesystem != nil {
		rp.Filesystem = hints.Filesystem
	} // strict → nil (default-deny)

	// Network
	if env.Network != nil {
		rp.Network = env.Network
	} else if mode == "permissive" && hints != nil && hints.Network != nil {
		rp.Network = hints.Network
	} // strict → nil

	// Secrets
	if env.Secrets != nil {
		rp.Secrets = env.Secrets
	} else if mode == "permissive" && hints != nil && hints.Secrets != nil {
		rp.Secrets = hints.Secrets
	} // strict → nil

	// Resources
	if env.Resources != nil {
		rp.Resources = env.Resources
	} else if mode == "permissive" && hints != nil && hints.Resources != nil {
		rp.Resources = hints.Resources
	} // strict → nil

	// TypeSpecific
	if len(env.TypeSpecific) > 0 {
		rp.TypeSpecific = env.TypeSpecific
	} else if mode == "permissive" && hints != nil && len(hints.TypeSpecific) > 0 {
		rp.TypeSpecific = hints.TypeSpecific
	} // strict → nil

	return rp
}
