package workflow

// compile_agents.go — agent block compilation, adapter-info lookup, and
// workflow-level / step-level allow-tools resolution.

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
)

// agentConfigEvalContext builds the eval context used to resolve agent.config{}
// attributes at compile time. file(), fileexists(), and trimfrontmatter() are
// registered so prompt files can be inlined; var/steps/each are intentionally
// absent because agent config has no runtime resolution path and must reduce
// to a constant. Returns nil when workflowDir is empty so callers fall back to
// the legacy nil-context decode (which silently empties on eval failure).
func agentConfigEvalContext(workflowDir string) *hcl.EvalContext {
	if workflowDir == "" {
		return nil
	}
	return &hcl.EvalContext{
		Functions: workflowFunctions(DefaultFunctionOptions(workflowDir)),
	}
}

// compileAgents compiles all agent blocks from spec into g.Agents.
//
// Agent config is resolved at compile time: unlike step.input{}, there is no
// runtime evaluation pass for agent.config{}, so any expression here must
// reduce to a constant. opts.WorkflowDir is used to register file(),
// fileexists(), and trimfrontmatter() so prompt files can be inlined.
func compileAgents(g *FSMGraph, spec *Spec, schemas map[string]AdapterInfo, opts CompileOpts) hcl.Diagnostics {
	var diags hcl.Diagnostics
	configEvalCtx := agentConfigEvalContext(opts.WorkflowDir)
	for _, ag := range spec.Agents {
		name := ag.Name
		if _, dup := g.Agents[name]; dup {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("duplicate agent %q", name)})
			continue
		}
		if !isValidAdapterName(ag.Adapter) {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("agent %q: invalid adapter %q", name, ag.Adapter)})
		}
		effectiveOnCrash := ag.OnCrash
		if effectiveOnCrash == "" {
			effectiveOnCrash = onCrashFail
		} else if !isValidOnCrash(effectiveOnCrash) {
			diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("agent %q: invalid on_crash %q", name, ag.OnCrash)})
		}
		var agentConfig map[string]string
		if ag.Config != nil {
			attrs, d := ag.Config.Remain.JustAttributes()
			diags = append(diags, d...)
			ctxLabel := fmt.Sprintf("agent %q config", name)
			missingRange := ag.Config.Remain.MissingItemRange()
			if info, ok := adapterInfo(schemas, ag.Adapter); ok {
				// Schema-aware decode: validates types, unknown keys, required fields.
				agentConfig, d = validateSchemaAttrs(ctxLabel, attrs, info.ConfigSchema, missingRange, "", configEvalCtx)
			} else {
				// Permissive decode: no schema available.
				agentConfig, d = decodeAttrsToStringMap(attrs, configEvalCtx)
			}
			diags = append(diags, d...)
		}
		g.Agents[name] = &AgentNode{
			Name:    name,
			Adapter: ag.Adapter,
			OnCrash: effectiveOnCrash,
			Config:  agentConfig,
		}
	}
	return diags
}

// adapterInfo looks up the AdapterInfo for a given adapter name in the schemas
// map. Returns (info, true) when found and the schema is non-empty (i.e. the
// adapter declared schemas). Returns (zero, false) when permissive mode applies.
func adapterInfo(schemas map[string]AdapterInfo, adapterName string) (AdapterInfo, bool) {
	if schemas == nil {
		return AdapterInfo{}, false
	}
	info, ok := schemas[adapterName]
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
