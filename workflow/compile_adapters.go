package workflow

// compile_adapters.go — adapter block compilation, adapter-info lookup,
// environment resolution, and workflow-level / step-level allow-tools resolution.

import (
	"fmt"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
)

// adapterConfigEvalContext builds the eval context used to validate adapter.config{}
// attributes at compile time (schema checks, type checks, required-field enforcement).
// file(), fileexists(), and trimfrontmatter() are registered so prompt files can be
// inlined. var.* and local.* are included so expressions are validated against
// declared defaults. steps.*, each.*, and data.* are intentionally absent —
// expressions that reference those namespaces correctly fail with a compile error.
// At runtime, the engine re-evaluates ConfigExprs against actual runtime vars.
//
// Always returns a non-nil context — even when workflowDir is empty — so that
// adapter.config expressions are never silently emptied. file()/fileexists()
// then produce a "workflow directory not configured" compile diagnostic for
// callers that compile without a WorkflowDir.
func adapterConfigEvalContext(vars, locals map[string]cty.Value, workflowDir string, fileCache map[string]string) *hcl.EvalContext {
	opts := DefaultFunctionOptions(workflowDir)
	opts.FileCache = fileCache
	return &hcl.EvalContext{
		Variables: map[string]cty.Value{
			"var":   ctyObjectOrEmpty(vars),
			"local": ctyObjectOrEmpty(locals),
		},
		Functions: workflowFunctions(opts),
	}
}

// compileAdapters compiles all adapter blocks from spec into g.Adapters.
//
// Adapter config is evaluated against variable defaults at compile time for
// schema validation. The engine re-evaluates ConfigExprs against runtime vars
// at session-open time (initScopeAdapters), so var.* references in config
// resolve to their actual runtime values. opts.WorkflowDir is used to register
// file(), fileexists(), and trimfrontmatter() so prompt files can be inlined.
//
// The key in g.Adapters is "<type>.<name>" (both labels concatenated with a dot).
// Environment references are validated against g.Environments at this time.
// Adapter declaration order is recorded in g.AdapterOrder for stable iteration.
func compileAdapters(g *FSMGraph, spec *Spec, schemas map[string]AdapterInfo, opts CompileOpts) hcl.Diagnostics {
	var diags hcl.Diagnostics
	configEvalCtx := adapterConfigEvalContext(graphVars(g), graphLocals(g), opts.WorkflowDir, g.FileCache)
	for _, ad := range spec.Adapters {
		diags = append(diags, compileOneAdapter(g, &ad, schemas, configEvalCtx)...)
	}
	return diags
}

// compileOneAdapter compiles a single adapter declaration into g.Adapters and
// g.AdapterOrder. Returns any diagnostics.
func compileOneAdapter(g *FSMGraph, ad *AdapterDeclSpec, schemas map[string]AdapterInfo, configEvalCtx *hcl.EvalContext) hcl.Diagnostics {
	var diags hcl.Diagnostics
	typeName := ad.Type
	instanceName := ad.Name
	key := typeName + "." + instanceName

	if d := validateAdapterDecl(g, key, typeName); d.HasErrors() {
		return append(diags, d...)
	}

	effectiveOnCrash, d := resolveAdapterOnCrash(key, ad.OnCrash)
	diags = append(diags, d...)

	envKey, d := resolveEnvironmentExpr(ad.Environment, fmt.Sprintf("adapter %q", key))
	diags = append(diags, d...)
	effectiveEnv, d := resolveAdapterEnv(g, key, envKey)
	diags = append(diags, d...)

	adapterConfig, configExprs, d := resolveAdapterConfig(key, ad, schemas, typeName, configEvalCtx)
	diags = append(diags, d...)

	secrets, d := resolveAdapterSecrets(key, ad)
	diags = append(diags, d...)

	if info, ok := adapterInfo(schemas, typeName); ok {
		diags = append(diags, checkAdapterEnvCompatibility(key, &info, effectiveEnv)...)
	}

	cacheResolvedPolicy(g, key, effectiveEnv, typeName, schemas)

	g.Adapters[key] = &AdapterNode{
		Type:        typeName,
		Name:        instanceName,
		Source:      ad.Source,
		Environment: effectiveEnv,
		OnCrash:     effectiveOnCrash,
		Config:      adapterConfig,
		ConfigExprs: configExprs,
		Secrets:     secrets,
	}
	// Track adapter declaration order for stable iteration
	g.AdapterOrder = append(g.AdapterOrder, key)
	return diags
}

// resolveAdapterOnCrash validates and returns the effective on_crash value.
// An empty value is replaced by the default (fail). An invalid non-empty value
// appends an error diagnostic.
// validateAdapterDecl checks that the adapter key is not already declared and
// that the adapter type name is valid. Returns diagnostics on failure.
func validateAdapterDecl(g *FSMGraph, key, typeName string) hcl.Diagnostics {
	// Duplicate detection: key format is "<type>.<name>".
	if _, dup := g.Adapters[key]; dup {
		return hcl.Diagnostics{{Severity: hcl.DiagError, Summary: fmt.Sprintf("duplicate adapter %q", key)}}
	}
	// Validate the adapter type is registered.
	if !isValidAdapterName(typeName) {
		return hcl.Diagnostics{{Severity: hcl.DiagError, Summary: fmt.Sprintf("adapter %q: invalid type %q", key, typeName)}}
	}
	return nil
}

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

// cacheResolvedPolicy resolves and caches the environment policy for an
// (adapter, environment) pair when both are valid.
func cacheResolvedPolicy(g *FSMGraph, adapterKey, envKey, typeName string, schemas map[string]AdapterInfo) {
	if envKey == "" {
		return
	}
	var hints *PolicyHints
	if info, ok := adapterInfo(schemas, typeName); ok {
		hints = info.PolicyHints
	}
	if envNode, ok := g.Environments[envKey]; ok {
		g.ResolvedPolicies[adapterKey+":"+envKey] = resolveEnvironmentPolicy(envNode, hints)
	}
}

// resolveAdapterConfig decodes the adapter config block. When the adapter type
// has a registered schema, it validates attribute types and required fields.
// Without a schema, it falls back to a permissive string-map decode.
// Returns the folded string map, the raw expression map, and any diagnostics.
func resolveAdapterConfig(key string, ad *AdapterDeclSpec, schemas map[string]AdapterInfo, typeName string, configEvalCtx *hcl.EvalContext) (folded map[string]string, raw map[string]hcl.Expression, diags hcl.Diagnostics) {
	if ad.Config == nil {
		return nil, nil, nil
	}
	attrs, d := ad.Config.Remain.JustAttributes()
	diags = append(diags, d...)

	exprs := make(map[string]hcl.Expression, len(attrs))
	for k, attr := range attrs {
		exprs[k] = attr.Expr
	}

	ctxLabel := fmt.Sprintf("adapter %q config", key)
	missingRange := ad.Config.Remain.MissingItemRange()
	if info, ok := adapterInfo(schemas, typeName); ok {
		folded, d = validateSchemaAttrs(ctxLabel, attrs, info.ConfigSchema, missingRange, "", configEvalCtx)
	} else {
		folded, d = decodeAttrsToStringMap(attrs, configEvalCtx)
	}
	diags = append(diags, d...)
	return folded, exprs, diags
}

// resolveAdapterSecrets extracts the optional `secrets { }` block from the
// adapter declaration. Each attribute in the block is preserved as an
// hcl.Expression so that it can be evaluated at runtime.
func resolveAdapterSecrets(_key string, ad *AdapterDeclSpec) (map[string]hcl.Expression, hcl.Diagnostics) {
	if ad.Secrets == nil {
		return nil, nil
	}
	attrs, diags := ad.Secrets.Remain.JustAttributes()
	if diags.HasErrors() {
		return nil, diags
	}
	out := make(map[string]hcl.Expression, len(attrs))
	for k, attr := range attrs {
		out[k] = attr.Expr
	}
	return out, nil
}

// checkAdapterEnvCompatibility emits a diagnostic when an adapter's schema
// declares a set of compatible environments and the resolved environment type
// is not among them. A wildcard "*" in the compatible list matches any type.
func checkAdapterEnvCompatibility(key string, info *AdapterInfo, envKey string) hcl.Diagnostics {
	if len(info.CompatibleEnvironments) == 0 {
		return nil
	}
	if envKey == "" {
		return nil
	}
	// Extract environment type from "env_type.env_name" key.
	envType := envKey
	if idx := strings.LastIndex(envKey, "."); idx != -1 {
		envType = envKey[:idx]
	}
	for _, compat := range info.CompatibleEnvironments {
		if compat == envType || compat == "*" {
			return nil
		}
	}
	return hcl.Diagnostics{{
		Severity: hcl.DiagError,
		Summary:  fmt.Sprintf("adapter %q: environment type %q is not in the adapter's compatible_environments %v", key, envType, info.CompatibleEnvironments),
	}}
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
func adapterHasCapability(info *AdapterInfo, capName string) bool {
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
