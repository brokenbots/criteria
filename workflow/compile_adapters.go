package workflow

// compile_adapters.go — adapter block compilation, adapter-info lookup,
// environment resolution, and workflow-level / step-level allow-tools resolution.

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
)

// adapterConfigEvalContext builds the eval context used to resolve adapter.config{}
// attributes at compile time. file(), fileexists(), and trimfrontmatter() are
// registered so prompt files can be inlined. var.* and local.* are included so
// that config expressions can reference declared variables and compiled locals.
// steps.*, each.*, and shared_variable.* are intentionally absent — expressions
// that reference those namespaces fail with "Variables not allowed", which is
// the correct compile error since adapter config has no runtime resolution path.
//
// Always returns a non-nil context — even when workflowDir is empty — so that
// adapter.config expressions are never silently emptied. file()/fileexists()
// then produce a "workflow directory not configured" compile diagnostic for
// callers that compile without a WorkflowDir.
func adapterConfigEvalContext(vars, locals map[string]cty.Value, workflowDir string) *hcl.EvalContext {
	return &hcl.EvalContext{
		Variables: map[string]cty.Value{
			"var":   ctyObjectOrEmpty(vars),
			"local": ctyObjectOrEmpty(locals),
		},
		Functions: workflowFunctions(DefaultFunctionOptions(workflowDir)),
	}
}

// compileAdapters compiles all adapter blocks from spec into g.Adapters.
//
// Adapter config is resolved at compile time: unlike step.input{}, there is no
// runtime evaluation pass for adapter.config{}, so any expression here must
// reduce to a constant. opts.WorkflowDir is used to register file(),
// fileexists(), and trimfrontmatter() so prompt files can be inlined.
//
// The key in g.Adapters is "<type>.<name>" (both labels concatenated with a dot).
// Environment references are validated against g.Environments at this time.
// Adapter declaration order is recorded in g.AdapterOrder for stable iteration.
//
//nolint:funlen // function length due to comprehensive adapter config validation and error handling
func compileAdapters(g *FSMGraph, spec *Spec, schemas map[string]AdapterInfo, opts CompileOpts) hcl.Diagnostics {
	var diags hcl.Diagnostics
	configEvalCtx := adapterConfigEvalContext(graphVars(g), graphLocals(g), opts.WorkflowDir)
	for _, ad := range spec.Adapters {
		typeName := ad.Type
		instanceName := ad.Name
		key := typeName + "." + instanceName

		// Duplicate detection: key format is "<type>.<name>".
		if _, dup := g.Adapters[key]; dup {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("duplicate adapter %q", key),
			})
			continue
		}

		// Validate the adapter type is registered.
		if !isValidAdapterName(typeName) {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("adapter %q: invalid type %q", key, typeName),
			})
			continue
		}

		// Validate on_crash if set.
		effectiveOnCrash := ad.OnCrash
		if effectiveOnCrash == "" {
			effectiveOnCrash = onCrashFail
		} else if !isValidOnCrash(effectiveOnCrash) {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("adapter %q: invalid on_crash %q", key, ad.OnCrash),
			})
		}

		// Validate and resolve the environment reference if set.
		effectiveEnv := ad.Environment
		if effectiveEnv != "" {
			// Environment reference must exist in g.Environments (keyed by "<env_type>.<env_name>").
			if _, ok := g.Environments[effectiveEnv]; !ok {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  fmt.Sprintf("adapter %q: referenced environment %q not declared", key, effectiveEnv),
				})
			}
		} else if g.DefaultEnvironment != "" {
			// If no environment is set and a default exists, use the default.
			effectiveEnv = g.DefaultEnvironment
		}

		var adapterConfig map[string]string
		if ad.Config != nil {
			attrs, d := ad.Config.Remain.JustAttributes()
			diags = append(diags, d...)
			ctxLabel := fmt.Sprintf("adapter %q config", key)
			missingRange := ad.Config.Remain.MissingItemRange()
			if info, ok := adapterInfo(schemas, typeName); ok {
				// Schema-aware decode: validates types, unknown keys, required fields.
				adapterConfig, d = validateSchemaAttrs(ctxLabel, attrs, info.ConfigSchema, missingRange, "", configEvalCtx)
			} else {
				// Permissive decode: no schema available.
				adapterConfig, d = decodeAttrsToStringMap(attrs, configEvalCtx)
			}
			diags = append(diags, d...)
		}

		g.Adapters[key] = &AdapterNode{
			Type:        typeName,
			Name:        instanceName,
			Environment: effectiveEnv,
			OnCrash:     effectiveOnCrash,
			Config:      adapterConfig,
		}
		// Track adapter declaration order for stable iteration
		g.AdapterOrder = append(g.AdapterOrder, key)
	}
	return diags
}

// adapterInfo looks up the AdapterInfo for a given adapter type in the schemas
// map. Returns (info, true) when found and the schema is non-empty (i.e. the
// adapter declared schemas). Returns (zero, false) when permissive mode applies.
func adapterInfo(schemas map[string]AdapterInfo, adapterType string) (AdapterInfo, bool) {
	if schemas == nil {
		return AdapterInfo{}, false
	}
	info, ok := schemas[adapterType]
	return info, ok
}

// workflowAllowTools extracts the workflow-level AllowTools list from a Spec.
func workflowAllowTools(spec *Spec) []string {
	if spec.Permissions == nil {
		return nil
	}
	return spec.Permissions.AllowTools
}

// unionAllowTools returns the union of step-level and workflow-level patterns.
// Duplicates are not removed — first-match-wins semantics make them harmless.
func unionAllowTools(stepTools, workflowTools []string) []string {
	if len(stepTools) == 0 && len(workflowTools) == 0 {
		return nil
	}
	out := make([]string, 0, len(stepTools)+len(workflowTools))
	out = append(out, stepTools...)
	out = append(out, workflowTools...)
	return out
}
