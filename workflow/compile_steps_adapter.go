package workflow

// compile_steps_adapter.go — compile path for adapter-targeted steps (non-iterating).

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2"
)

// allowToolsPatternSyntaxDoc is the canonical documentation reference used in
// diagnostics when an allow_tools entry is malformed.
const allowToolsPatternSyntaxDoc = "see docs/workflow.md#pattern-matching for the criteria Tool:<command-glob> syntax"

// compileAdapterStep compiles a non-iterating adapter-targeted step and registers
// it in g. adapterRef is the pre-resolved "<type>.<name>" string from resolveStepTarget.
func validateTopLevelStepRefs(stepName string, inputExprs, secretInputExprs map[string]hcl.Expression) hcl.Diagnostics {
	var diags hcl.Diagnostics
	diags = append(diags, validateEachRefs(stepName, inputExprs)...)
	diags = append(diags, validateWhileRefs(stepName, inputExprs)...)
	diags = append(diags, validateEachRefs(stepName, secretInputExprs)...)
	diags = append(diags, validateWhileRefs(stepName, secretInputExprs)...)
	return diags
}

func resolveOutputSchema(adapterType string, schemas map[string]AdapterInfo) map[string]ConfigField {
	if info, ok := adapterInfo(schemas, adapterType); ok {
		return info.OutputSchema
	}
	return map[string]ConfigField{}
}

func compileAdapterStep(g *FSMGraph, sp *StepSpec, spec *Spec, schemas map[string]AdapterInfo, opts CompileOpts, adapterRef string) hcl.Diagnostics {
	var diags hcl.Diagnostics

	ok, d := validateStepRegistration(g, sp)
	diags = append(diags, d...)
	if !ok {
		return diags
	}

	diags = append(diags, validateAllowToolsWithAdapter(sp, adapterRef)...)
	diags = append(diags, validateLegacyConfig(sp)...)
	diags = append(diags, validateOnFailureForNonIterating(sp)...)

	effectiveOnCrash, d := resolveStepOnCrashWithAdapter(g, sp, adapterRef)
	diags = append(diags, d...)

	timeout, d := decodeStepTimeout(sp)
	diags = append(diags, d...)

	maxVisits, d := decodeMaxVisits(sp.Name, sp.Remain, g)
	diags = append(diags, d...)

	envKey, d := resolveStepEnvironmentOverride(sp.Name, sp.Remain, g)
	diags = append(diags, d...)

	adapterType := adapterTypeFromRef(adapterRef)
	inputMap, inputExprs, d := decodeStepInput(g, sp, schemas, opts, adapterType)
	diags = append(diags, d...)

	secretInputMap, secretInputExprs, d := decodeStepSecretInput(g, sp, schemas, opts, adapterType)
	diags = append(diags, d...)

	// each.* references are only valid inside iterating steps or workflow
	// bodies (SubworkflowChain non-empty). Non-iterating top-level steps must not
	// reference them.
	if len(opts.SubworkflowChain) == 0 {
		diags = append(diags, validateTopLevelStepRefs(sp.Name, inputExprs, secretInputExprs)...)
	}

	outputSchema := resolveOutputSchema(adapterType, schemas)

	node := newAdapterStepNode(sp, spec, adapterRef, effectiveOnCrash, envKey, timeout, inputMap, inputExprs, secretInputMap, secretInputExprs, outputSchema, maxVisits)
	diags = append(diags, validateAllowTools(sp.Name, adapterType, node.AllowTools, schemas)...)
	diags = append(diags, compileOutcomeBlock(sp, node, g, opts, schemas[adapterRef].OutputSchema)...)

	if len(node.Outcomes) == 0 {
		diags = append(diags, &hcl.Diagnostic{Severity: hcl.DiagError, Summary: fmt.Sprintf("step %q: at least one outcome is required", sp.Name)})
	}

	g.Steps[sp.Name] = node
	g.stepOrder = append(g.stepOrder, sp.Name)
	return diags
}

// allowToolsForStep returns the effective AllowTools for a step.
func allowToolsForStep(sp *StepSpec, spec *Spec) []string {
	return unionAllowTools(sp.AllowTools, workflowAllowTools(spec))
}

// adapterTypeFromRef extracts the adapter type from a dotted "<type>.<name>" reference.
func adapterTypeFromRef(adapterRef string) string {
	if adapterRef == "" {
		return ""
	}
	parts := strings.Split(adapterRef, ".")
	if len(parts) == 2 {
		return parts[0]
	}
	return ""
}

// validateOnFailureForNonIterating validates on_failure for steps that do not
// carry for_each, count, parallel, or while. It checks the value is recognised and
// always errors because on_failure requires an iterating modifier.
func validateOnFailureForNonIterating(sp *StepSpec) hcl.Diagnostics {
	diags := validateOnFailureValue(sp)
	if sp.OnFailure != "" {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: on_failure requires for_each, count, parallel, or while", sp.Name),
		})
	}
	return diags
}

// validateAllowTools checks each allow_tools entry for problems that would
// prevent it from ever granting a runtime tool request against the target
// adapter. It emits at least one warning per problematic entry for:
//   - invalid filepath.Match glob syntax (e.g. an unclosed '['),
//   - the Claude Code settings.json argument-scoping form (e.g. "Tool(...)"),
//     which is not criteria's "Tool:<command-glob>" form,
//   - tool names not declared in the adapter's permissions vocabulary,
//   - recognized aliases, pointing toward the canonical SDK kind.
func validateAllowTools(stepName, adapterType string, tools []string, schemas map[string]AdapterInfo) hcl.Diagnostics {
	info, _ := adapterInfo(schemas, adapterType)
	perms := make(map[string]struct{}, len(info.Permissions))
	for _, p := range info.Permissions {
		perms[p] = struct{}{}
	}
	aliases := info.PermissionAliases
	if aliases == nil {
		aliases = map[string]string{}
	}

	var diags hcl.Diagnostics
	for _, tool := range tools {
		diags = append(diags, validateAllowToolsEntry(stepName, tool, perms, aliases)...)
	}
	return diags
}

// validateAllowToolsEntry checks a single allow_tools pattern.
func validateAllowToolsEntry(stepName, tool string, perms map[string]struct{}, aliases map[string]string) hcl.Diagnostics {
	var diags hcl.Diagnostics

	// 1. Glob syntax check. filepath.Match with an empty name is enough to
	// surface ErrBadPattern without producing a false-positive match.
	if _, err := filepath.Match(tool, ""); err != nil {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagWarning,
			Summary:  fmt.Sprintf("step %q allow_tools: %q is not a valid glob pattern", stepName, tool),
			Detail:   fmt.Sprintf("%v. %s", err, allowToolsPatternSyntaxDoc),
		})
	}

	// 2. Wrong argument-scoping form check. The Claude Code settings.json shape
	// uses parentheses to scope arguments ("Tool(command:pattern)"); criteria
	// uses a colon ("Tool:<command-glob>").
	if strings.ContainsRune(tool, '(') {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagWarning,
			Summary:  fmt.Sprintf("step %q allow_tools: %q looks like the Claude Code argument-scoping form", stepName, tool),
			Detail:   fmt.Sprintf("criteria uses the colon form (e.g. \"shell:git *\"), not parentheses. %s", allowToolsPatternSyntaxDoc),
		})
	}

	// 3. Alias warning. Aliases are accepted at runtime, but we warn so users
	// can move to the canonical form.
	if canonical, ok := aliases[tool]; ok {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagWarning,
			Summary:  fmt.Sprintf("step %q allow_tools: %q is a recognized alias for the %q SDK kind; consider using the canonical form for clarity", stepName, tool, canonical),
		})
		// An alias resolves to a canonical permission at runtime, so skip the
		// raw vocabulary check for the alias string itself.
		return diags
	}

	// 4. Vocabulary check. Only check when the adapter declares a vocabulary and
	// the tool-name prefix is a literal (no glob metacharacters).
	if len(perms) == 0 {
		return diags
	}
	toolName := tool
	if i := strings.Index(tool, ":"); i >= 0 {
		toolName = tool[:i]
	}
	if toolName == "" || hasGlobMetacharacters(toolName) {
		return diags
	}
	if _, ok := perms[toolName]; !ok {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagWarning,
			Summary:  fmt.Sprintf("step %q allow_tools: %q names tool %q which is not declared in the adapter's permissions vocabulary", stepName, tool, toolName),
			Detail:   fmt.Sprintf("Declared permissions: %s. %s", sortedPermList(perms), allowToolsPatternSyntaxDoc),
		})
	}
	return diags
}

// hasGlobMetacharacters reports whether s contains filepath.Match wildcard
// characters. A tool name containing them cannot be checked statically against
// the permissions vocabulary.
func hasGlobMetacharacters(s string) bool {
	return strings.ContainsAny(s, "*?[")
}

// sortedPermList returns the vocabulary keys as a sorted, comma-separated string.
func sortedPermList(perms map[string]struct{}) string {
	list := make([]string, 0, len(perms))
	for p := range perms {
		list = append(list, p)
	}
	sort.Strings(list)
	return strings.Join(list, ", ")
}

// newAdapterStepNode constructs a StepNode for an adapter-targeted step.
func newAdapterStepNode(sp *StepSpec, spec *Spec, adapterRef string, effectiveOnCrash string, envKey string, timeout time.Duration,
	inputMap map[string]string, inputExprs map[string]hcl.Expression,
	secretInputMap map[string]string, secretInputExprs map[string]hcl.Expression,
	outputSchema map[string]ConfigField, maxVisits int) *StepNode {
	return &StepNode{
		Name:             sp.Name,
		TargetKind:       StepTargetAdapter,
		AdapterRef:       adapterRef,
		OnCrash:          effectiveOnCrash,
		OnFailure:        sp.OnFailure,
		MaxVisits:        maxVisits,
		Input:            inputMap,
		InputExprs:       inputExprs,
		SecretInputs:     secretInputMap,
		SecretInputExprs: secretInputExprs,
		Timeout:          timeout,
		Outcomes:         map[string]*CompiledOutcome{},
		AllowTools:       allowToolsForStep(sp, spec),
		Environment:      envKey,
		OutputSchema:     outputSchema,
	}
}

// validateAllowToolsWithAdapter checks that allow_tools is only set on adapter-targeted steps.
func validateAllowToolsWithAdapter(sp *StepSpec, adapterRef string) hcl.Diagnostics {
	var diags hcl.Diagnostics
	if len(sp.AllowTools) > 0 && adapterRef == "" {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: allow_tools requires an adapter reference", sp.Name),
		})
	}
	return diags
}

// resolveStepOnCrashWithAdapter returns the effective on_crash for a step,
// falling back to the backing adapter's on_crash if the step doesn't specify one.
// adapterRef is the resolved adapter "<type>.<name>" reference.
func resolveStepOnCrashWithAdapter(g *FSMGraph, sp *StepSpec, adapterRef string) (string, hcl.Diagnostics) {
	var diags hcl.Diagnostics
	if sp.OnCrash != "" && !isValidOnCrash(sp.OnCrash) {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: invalid on_crash %q", sp.Name, sp.OnCrash),
		})
		return "", diags
	}
	if sp.OnCrash != "" {
		return sp.OnCrash, nil // step explicitly specifies on_crash
	}
	// Fall back to the adapter's on_crash (if set).
	if adapterRef != "" {
		if adapterNode, ok := g.Adapters[adapterRef]; ok {
			return adapterNode.OnCrash, nil
		}
	}
	return "", nil
}

// validateLegacyConfig emits a migration diagnostic when a step uses the
// deprecated config = { ... } attribute instead of input { }.
func validateLegacyConfig(sp *StepSpec) hcl.Diagnostics {
	if len(sp.Config) == 0 {
		return nil
	}
	var subject *hcl.Range
	if sp.LegacyConfigRange != nil {
		r := *sp.LegacyConfigRange
		subject = &r
	}
	return hcl.Diagnostics{&hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  fmt.Sprintf("step %q: \"config\" attribute removed; use \"input { }\" block instead (Phase 1.5)", sp.Name),
		Detail:   "Replace `config = { key = \"value\" }` with `input { key = \"value\" }` in your workflow.",
		Subject:  subject,
	}}
}

// decodeStepTimeout parses sp.Timeout and returns the duration and any
// diagnostic.
func decodeStepTimeout(sp *StepSpec) (time.Duration, hcl.Diagnostics) {
	if sp.Timeout == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(sp.Timeout)
	if err != nil {
		return 0, hcl.Diagnostics{&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: invalid timeout %q: %v", sp.Name, sp.Timeout, err),
		}}
	}
	return d, nil
}

// decodeStepInput decodes the input { } block for sp, validates against the
// adapter schema when one is known, and returns the static map and expression
// map.
func decodeStepInput(g *FSMGraph, sp *StepSpec, schemas map[string]AdapterInfo, opts CompileOpts, adapterName string) (inputMap map[string]string, inputExprs map[string]hcl.Expression, diags hcl.Diagnostics) {
	if sp.Input == nil {
		return nil, nil, nil
	}
	attrs, d := sp.Input.Remain.JustAttributes()
	diags = append(diags, d...)
	ctxLabel := fmt.Sprintf("step %q input", sp.Name)
	missingRange := sp.Input.Remain.MissingItemRange()
	if adapterName != "" {
		if info, ok := adapterInfo(schemas, adapterName); ok {
			inputMap, d = validateSchemaAttrs(ctxLabel, attrs, info.InputSchema, missingRange, adapterName, nil)
		} else {
			inputMap, d = decodeAttrsToStringMap(attrs, nil)
		}
	} else {
		inputMap, d = decodeAttrsToStringMap(attrs, nil)
	}
	diags = append(diags, d...)
	inputExprs = make(map[string]hcl.Expression, len(attrs))
	for k, attr := range attrs {
		inputExprs[k] = attr.Expr
	}
	diags = append(diags, validateFoldableAttrs(attrs, graphVars(g), graphLocals(g), opts.WorkflowDir)...)
	return inputMap, inputExprs, diags
}

// decodeStepSecretInput decodes the secret_input { } block for sp.
func decodeStepSecretInput(g *FSMGraph, sp *StepSpec, schemas map[string]AdapterInfo, opts CompileOpts, adapterName string) (secretInputMap map[string]string, secretInputExprs map[string]hcl.Expression, diags hcl.Diagnostics) {
	if sp.SecretInput == nil {
		return nil, nil, nil
	}
	attrs, d := sp.SecretInput.Remain.JustAttributes()
	diags = append(diags, d...)
	ctxLabel := fmt.Sprintf("step %q secret_input", sp.Name)
	missingRange := sp.SecretInput.Remain.MissingItemRange()
	if adapterName != "" {
		if info, ok := adapterInfo(schemas, adapterName); ok {
			secretInputMap, d = validateSchemaAttrs(ctxLabel, attrs, info.InputSchema, missingRange, adapterName, nil)
		} else {
			secretInputMap, d = decodeAttrsToStringMap(attrs, nil)
		}
	} else {
		secretInputMap, d = decodeAttrsToStringMap(attrs, nil)
	}
	diags = append(diags, d...)
	secretInputExprs = make(map[string]hcl.Expression, len(attrs))
	for k, attr := range attrs {
		secretInputExprs[k] = attr.Expr
	}
	diags = append(diags, validateFoldableAttrs(attrs, graphVars(g), graphLocals(g), opts.WorkflowDir)...)
	return secretInputMap, secretInputExprs, diags
}
