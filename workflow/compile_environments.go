package workflow

// compile_environments.go — compile path for environment "<type>" "<name>" blocks.
// Environments are compile-time-resolved typed execution contexts that can be
// bound to adapters and steps.

import (
	"fmt"
	"regexp"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
)

// Registered environment types for v0.3.0. Phase 4 will introduce a plugin-based
// type registry; for now we hardcode "shell" as the only supported type.
var registeredEnvironmentTypes = map[string]bool{
	"shell": true,
}

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
func compileEnvironments(g *FSMGraph, spec *Spec, opts CompileOpts) hcl.Diagnostics {
	if len(spec.Environments) == 0 {
		return nil
	}

	var diags hcl.Diagnostics

	// Validate all environment declarations and fold their variables/config.
	seen := make(map[string]bool) // tracks "<type>.<name>" uniqueness
	for _, envSpec := range spec.Environments {
		diags = append(diags, compileEnvironmentBlock(g, envSpec, opts, seen)...)
	}

	// Resolve default environment rules.
	diags = append(diags, resolveDefaultEnvironment(g, spec)...)

	return diags
}

// compileEnvironmentBlock validates and compiles a single environment declaration.
func compileEnvironmentBlock(g *FSMGraph, envSpec EnvironmentSpec, opts CompileOpts, seen map[string]bool) hcl.Diagnostics {
	// Validate block basics (type, name, duplicates)
	diags := validateEnvironmentBasics(envSpec, seen)
	if diags.HasErrors() {
		return diags
	}

	key := fmt.Sprintf("%s.%s", envSpec.Type, envSpec.Name)
	seen[key] = true

	// Parse variables and config attributes
	attrs, d := envSpec.Remain.JustAttributes()
	diags = append(diags, d...)

	// Decode variables and config
	variables, d := decodeEnvironmentVariables(attrs, opts)
	diags = append(diags, d...)
	config, d := decodeEnvironmentConfig(attrs, opts)
	diags = append(diags, d...)

	if diags.HasErrors() {
		return diags
	}

	// Check for controlled-set conflicts
	diags = append(diags, checkShellControlledSetConflicts(envSpec.Type, variables, attrs)...)

	// Store the compiled environment
	g.Environments[key] = &EnvironmentNode{
		Type:      envSpec.Type,
		Name:      envSpec.Name,
		Variables: variables,
		Config:    config,
	}

	return diags
}

// validateEnvironmentBasics validates type, name, and duplicate checks for an environment block.
func validateEnvironmentBasics(envSpec EnvironmentSpec, seen map[string]bool) hcl.Diagnostics {
	var diags hcl.Diagnostics

	// Validate type is registered.
	if !registeredEnvironmentTypes[envSpec.Type] {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("environment type %q is not registered (v0.3.0 only supports 'shell'; other types are Phase 4 work)", envSpec.Type),
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

// resolveDefaultEnvironment implements the default-environment resolution rules.
// If multiple environments are declared and no explicit default is set,
// error if any consumer uses an environment.
func resolveDefaultEnvironment(g *FSMGraph, spec *Spec) hcl.Diagnostics {
	var diags hcl.Diagnostics

	// If the workflow header specifies an explicit default, use it.
	if spec.DefaultEnvironment != "" {
		g.DefaultEnvironment = spec.DefaultEnvironment
		// Validate that the referenced environment exists.
		if _, ok := g.Environments[spec.DefaultEnvironment]; !ok {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("workflow environment %q does not refer to a declared environment block", spec.DefaultEnvironment),
			})
		}
		return diags
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
