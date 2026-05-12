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
func compileAdapters(g *FSMGraph, spec *Spec, schemas map[string]AdapterInfo, opts CompileOpts) hcl.Diagnostics {
	var diags hcl.Diagnostics
	configEvalCtx := adapterConfigEvalContext(graphVars(g), graphLocals(g), opts.WorkflowDir)
	for _, ad := range spec.Adapters {
		diags = append(diags, compileOneAdapter(g, ad, schemas, configEvalCtx)...)
	}
	return diags
}

// compileOneAdapter compiles a single adapter declaration into g.Adapters and
// g.AdapterOrder. Returns any diagnostics.
func compileOneAdapter(g *FSMGraph, ad AdapterDeclSpec, schemas map[string]AdapterInfo, configEvalCtx *hcl.EvalContext) hcl.Diagnostics {
	var diags hcl.Diagnostics
	typeName := ad.Type
	instanceName := ad.Name
	key := typeName + "." + instanceName

	// Duplicate detection: key format is "<type>.<name>".
	if _, dup := g.Adapters[key]; dup {
		return append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("duplicate adapter %q", key),
		})
	}

	// Validate the adapter type is registered.
	if !isValidAdapterName(typeName) {
		return append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("adapter %q: invalid type %q", key, typeName),
		})
	}

	effectiveOnCrash, d := resolveAdapterOnCrash(key, ad.OnCrash)
	diags = append(diags, d...)

	effectiveEnv, d := resolveAdapterEnv(g, key, ad.Environment)
	diags = append(diags, d...)

	adapterConfig, d := resolveAdapterConfig(key, ad, schemas, typeName, configEvalCtx)
	diags = append(diags, d...)

	g.Adapters[key] = &AdapterNode{
		Type:        typeName,
		Name:        instanceName,
		Environment: effectiveEnv,
		OnCrash:     effectiveOnCrash,
		Config:      adapterConfig,
	}
	// Track adapter declaration order for stable iteration
	g.AdapterOrder = append(g.AdapterOrder, key)
	return diags
}

// resolveAdapterOnCrash validates and returns the effective on_crash value.
// An empty value is replaced by the default (fail). An invalid non-empty value
// appends an error diagnostic.
func resolveAdapterOnCrash(key, onCrash string) (string, hcl.Diagnostics) {
	if onCrash == "" {
		return onCrashFail, nil
	}
	if !isValidOnCrash(onCrash) {
		return onCrash, hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("adapter %q: invalid on_crash %q", key, onCrash),
		}}
	}
	return onCrash, nil
}

// resolveAdapterEnv validates the adapter's environment reference against
// g.Environments and falls back to the graph default when no reference is set.
func resolveAdapterEnv(g *FSMGraph, key, envRef string) (string, hcl.Diagnostics) {
	if envRef == "" {
		return g.DefaultEnvironment, nil
	}
	// Environment reference must exist in g.Environments (keyed by "<env_type>.<env_name>").
	if _, ok := g.Environments[envRef]; !ok {
		return envRef, hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("adapter %q: referenced environment %q not declared", key, envRef),
		}}
	}
	return envRef, nil
}

// resolveAdapterConfig decodes the adapter config block. When the adapter type
// has a registered schema, it validates attribute types and required fields.
// Without a schema, it falls back to a permissive string-map decode.
func resolveAdapterConfig(key string, ad AdapterDeclSpec, schemas map[string]AdapterInfo, typeName string, configEvalCtx *hcl.EvalContext) (map[string]string, hcl.Diagnostics) {
	if ad.Config == nil {
		return nil, nil
	}
	var diags hcl.Diagnostics
	attrs, d := ad.Config.Remain.JustAttributes()
	diags = append(diags, d...)
	ctxLabel := fmt.Sprintf("adapter %q config", key)
	missingRange := ad.Config.Remain.MissingItemRange()
	var adapterConfig map[string]string
	if info, ok := adapterInfo(schemas, typeName); ok {
		adapterConfig, d = validateSchemaAttrs(ctxLabel, attrs, info.ConfigSchema, missingRange, "", configEvalCtx)
	} else {
		adapterConfig, d = decodeAttrsToStringMap(attrs, configEvalCtx)
	}
	diags = append(diags, d...)
	return adapterConfig, diags
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

// adapterHasCapability reports whether info declares capName in its Capabilities
// slice. Used to gate parallel = [...] steps at compile time.
func adapterHasCapability(info AdapterInfo, capName string) bool {
	for _, c := range info.Capabilities {
		if c == capName {
			return true
		}
	}
	return false
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
