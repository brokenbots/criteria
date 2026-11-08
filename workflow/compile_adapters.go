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
	diags := make(hcl.Diagnostics, 0, len(spec.Adapters))
	configEvalCtx := adapterConfigEvalContext(graphVars(g), graphLocals(g), opts.WorkflowDir, g.FileCache)
	for i := range spec.Adapters {
		diags = append(diags, compileOneAdapter(g, &spec.Adapters[i], schemas, configEvalCtx)...)
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

	resolved, d := resolveAdapterDecl(g, ad, schemas, key, typeName, configEvalCtx)
	diags = append(diags, d...)

	g.Adapters[key] = resolved.node(typeName, instanceName, ad.Source)
	g.Adapters[key].DynamicTools = ad.DynamicTools
	g.Adapters[key].PinnedDigest = lockedAdapterDigest(g.PinSet, typeName, instanceName)
	// Track adapter declaration order for stable iteration
	g.AdapterOrder = append(g.AdapterOrder, key)
	return diags
}

// resolvedAdapterDecl carries the per-facet resolution of one adapter
// declaration: every declared attribute validated against the graph, the
// schemas, and the pin set, ready for registration.
type resolvedAdapterDecl struct {
	onCrash           string
	env               string
	continuation      string
	config            map[string]string
	configExprs       map[string]hcl.Expression
	secrets           map[string]hcl.Expression
	staticTools       []string
	toolContracts     map[string]ToolContract
	toolContractOrder []string
}

// resolveAdapterDecl runs every facet resolver for one adapter declaration
// in declaration order and applies the resolved cache policy.
func resolveAdapterDecl(g *FSMGraph, ad *AdapterDeclSpec, schemas map[string]AdapterInfo, key, typeName string, configEvalCtx *hcl.EvalContext) (*resolvedAdapterDecl, hcl.Diagnostics) {
	var diags hcl.Diagnostics
	var resolved resolvedAdapterDecl
	var d hcl.Diagnostics

	resolved.onCrash, d = resolveAdapterOnCrash(key, ad.OnCrash)
	diags = append(diags, d...)

	envKey, d := resolveEnvironmentExpr(ad.Environment, fmt.Sprintf("adapter %q", key))
	diags = append(diags, d...)
	resolved.env, d = resolveAdapterEnv(g, key, envKey)
	diags = append(diags, d...)

	resolved.continuation, d = resolveAdapterContinuation(key, ad.Continuation)
	diags = append(diags, d...)

	resolved.config, resolved.configExprs, d = resolveAdapterConfig(key, ad, schemas, typeName, configEvalCtx)
	diags = append(diags, d...)

	resolved.secrets, d = resolveAdapterSecrets(key, ad)
	diags = append(diags, d...)

	if info, ok := adapterInfo(schemas, typeName); ok {
		diags = append(diags, checkAdapterEnvCompatibility(key, &info, resolved.env)...)
		diags = append(diags, checkWorkflowCapabilityHostability(key, &info, resolved.env)...)
	}

	cacheResolvedPolicy(g, key, resolved.env, typeName, schemas)

	resolved.staticTools, resolved.toolContracts, resolved.toolContractOrder, d = compileAdapterToolDecls(key, ad.Tools, g)
	diags = append(diags, d...)

	return &resolved, diags
}

// node assembles the resolved declaration into the graph's adapter record;
// dynamic_tools and the pinned digest are stamped on by the caller.
func (r *resolvedAdapterDecl) node(typeName, instanceName, source string) *AdapterNode {
	return &AdapterNode{
		Type:              typeName,
		Name:              instanceName,
		Source:            source,
		Environment:       r.env,
		OnCrash:           r.onCrash,
		Continuation:      r.continuation,
		Config:            r.config,
		ConfigExprs:       r.configExprs,
		Secrets:           r.secrets,
		StaticTools:       r.staticTools,
		ToolContracts:     r.toolContracts,
		ToolContractOrder: r.toolContractOrder,
	}
}

// compileAdapterToolDecls compiles an adapter's tool declarations: the
// static tool list, duplicate-tool detection, and each tool's typed
// in/out contract. Contracts apply only to tools whose block declares a
// named type; contract-bearing tools keep their declaration order so the
// single-tool fallback in validateStepInputToolContract stays deterministic.
func compileAdapterToolDecls(key string, tools []ToolDeclSpec, g *FSMGraph) (staticTools []string, toolContracts map[string]ToolContract, toolContractOrder []string, diags hcl.Diagnostics) {
	staticTools = make([]string, 0, len(tools))
	toolContracts = make(map[string]ToolContract, len(tools))
	seenTools := make(map[string]bool, len(tools))
	for _, t := range tools {
		staticTools = append(staticTools, t.Name)
		if seenTools[t.Name] {
			r := t.Remain.MissingItemRange()
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("adapter %q: duplicate tool %q", key, t.Name),
				Subject:  &r,
			})
			continue
		}
		seenTools[t.Name] = true
		contract, d := compileToolContract(key, t.Name, &t, g)
		diags = append(diags, d...)
		if contract.HasAny() {
			toolContracts[t.Name] = contract
			toolContractOrder = append(toolContractOrder, t.Name)
		}
	}
	return staticTools, toolContracts, toolContractOrder, diags
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

// resolveAdapterContinuation validates the adapter's declared session-fate
// value against the continuation policy (ADR-0014 D3): "reuse" keeps the
// session and its conversation alive across subworkflow body exits via the
// detach ledger, "fresh" releases it so the next re-entry opens a new
// conversation. Empty means undeclared: the body's own undeclared instances
// detach at body exit (the KB-257 path), and the re-entry compile rule
// (compile_continuation.go, validateContinuation) keeps declarations
// mandatory for adapter instances re-declared by both a parent and a callee
// graph.
func resolveAdapterContinuation(key, continuation string) (string, hcl.Diagnostics) {
	switch continuation {
	case "", continuationReuse, continuationFresh:
		return continuation, nil
	default:
		return continuation, hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("adapter %q: invalid continuation %q", key, continuation),
			Detail:   "continuation must be \"reuse\" or \"fresh\"",
		}}
	}
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
	envType := envTypeFromKey(envKey)
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

// workflowV1Capability is the capability an ADR-0008 criteria-as-adapter peer
// advertises (both directions: the served child reports it in its Info
// handshake, the host declares it in the peer identity frame).
const workflowV1Capability = "workflow.v1"

// envTypeFromKey extracts the environment type from an "env_type.env_name"
// environment key. An empty key keeps the empty string.
func envTypeFromKey(envKey string) string {
	if idx := strings.LastIndex(envKey, "."); idx != -1 {
		return envKey[:idx]
	}
	return envKey
}

// envCannotHostCriteriaProcess reports whether an environment type cannot
// host a criteria process. Container images and remote pods are built as
// non-criteria images; the shell (and the OS-sandboxed local sandbox on the
// same machine) can host one.
func envCannotHostCriteriaProcess(envType string) bool {
	return envType == "container" || envType == "remote"
}

// checkWorkflowCapabilityHostability emits a WARNING-tier advisory when an
// adapter advertising the workflow.v1 capability (ADR-0008: it serves a
// criteria workflow as AdapterService) is bound to an environment type that
// cannot host a criteria process (container/remote). Local shell processes
// are the legitimate standalone deployment — two processes on one machine —
// so the remote-shim case stays legitimate too: the diagnostic is advisory
// only and never fails the compile.
func checkWorkflowCapabilityHostability(key string, info *AdapterInfo, envKey string) hcl.Diagnostics {
	if !adapterHasCapability(info, workflowV1Capability) || !envCannotHostCriteriaProcess(envTypeFromKey(envKey)) {
		return nil
	}
	return hcl.Diagnostics{{
		Severity: hcl.DiagWarning,
		Summary:  fmt.Sprintf("adapter %q: workflow.v1 peer bound to %q environment", key, envTypeFromKey(envKey)),
		Detail:   "a workflow.v1-capable adapter fronts a criteria child process, but this environment type is typically built without the criteria binary itself; prefer a local shell environment (the reference deployment runs both processes on one machine) unless the image intentionally ships the criteria binary.",
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
