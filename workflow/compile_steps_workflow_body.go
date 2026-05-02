package workflow

// compile_steps_workflow_body.go — helpers for loading and validating inline
// and file-backed workflow bodies for type="workflow" steps.

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
)

// compileWorkflowBodyFromFile handles the workflow_file = "..." loading path.
func compileWorkflowBodyFromFile(sp *StepSpec, schemas map[string]AdapterInfo, opts CompileOpts) (hcl.Diagnostics, *FSMGraph, string) {
	var diags hcl.Diagnostics
	if sp.WorkflowFile == "" {
		return append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: type=\"workflow\" requires a workflow { ... } block", sp.Name),
		}), nil, ""
	}
	if opts.SubWorkflowResolver == nil {
		return append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: workflow_file requires SubWorkflowResolver in CompileOpts", sp.Name),
		}), nil, ""
	}
	for _, f := range opts.LoadedFiles {
		if f == sp.WorkflowFile {
			return append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("step %q: workflow_file %q creates a load cycle", sp.Name, sp.WorkflowFile),
			}), nil, ""
		}
	}
	loaded, err := opts.SubWorkflowResolver(sp.WorkflowFile, opts.WorkflowDir)
	if err != nil {
		return append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: failed to load workflow_file %q: %s", sp.Name, sp.WorkflowFile, err),
		}), nil, ""
	}
	childOpts := CompileOpts{
		WorkflowDir:         opts.WorkflowDir,
		LoadDepth:           opts.LoadDepth + 1,
		LoadedFiles:         append(append([]string{}, opts.LoadedFiles...), sp.WorkflowFile),
		SubWorkflowResolver: opts.SubWorkflowResolver,
	}
	body, d := CompileWithOpts(loaded, schemas, childOpts)
	diags = append(diags, d...)
	if diags.HasErrors() {
		return diags, nil, ""
	}
	return diags, body, loaded.InitialState
}

// compileWorkflowBodyInline handles the inline workflow { ... } block path.
func compileWorkflowBodyInline(sp *StepSpec, schemas map[string]AdapterInfo, opts CompileOpts) (hcl.Diagnostics, *FSMGraph, string) {
	var diags hcl.Diagnostics
	wb := sp.Workflow
	if len(wb.Steps) == 0 {
		return append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q: workflow body must contain at least one step", sp.Name),
		}), nil, ""
	}

	entry := wb.Entry
	if entry == "" {
		entry = wb.Steps[0].Name
	}

	bodySpec := buildBodySpec(sp.Name, entry, wb)
	childOpts := CompileOpts{
		WorkflowDir:         opts.WorkflowDir,
		LoadDepth:           opts.LoadDepth + 1,
		LoadedFiles:         append([]string{}, opts.LoadedFiles...),
		SubWorkflowResolver: opts.SubWorkflowResolver,
	}
	body, d := CompileWithOpts(bodySpec, schemas, childOpts)
	diags = append(diags, d...)

	if err := validateBodyHasContinuePath(sp.Name, body); err != nil {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  err.Error(),
		})
	}

	return diags, body, entry
}

// validateBodyHasContinuePath returns an error when none of the body steps
// has a transition_to = "_continue" outcome. This ensures that the iteration
// loop can terminate normally; a body with no path to _continue is a compile
// error (B-09).
func validateBodyHasContinuePath(stepName string, body *FSMGraph) error {
	if body == nil {
		return nil
	}
	for _, stepNode := range body.Steps {
		for _, target := range stepNode.Outcomes {
			if target == "_continue" {
				return nil
			}
		}
	}
	return fmt.Errorf("step %q: workflow body has no path to \"_continue\"; at least one outcome must transition_to = \"_continue\"", stepName)
}

// buildBodySpec constructs a synthetic Spec from a WorkflowBodySpec for
// recursive compilation. It adds a "_continue" terminal state that body steps
// can transition to in order to signal iteration advance.
func buildBodySpec(stepName, entry string, wb *WorkflowBodySpec) *Spec {
	// Convert pointer slices to value slices for the synthetic Spec.
	steps := make([]StepSpec, 0, len(wb.Steps))
	for _, s := range wb.Steps {
		if s != nil {
			steps = append(steps, *s)
		}
	}
	states := make([]StateSpec, 0, len(wb.States)+1)
	for _, s := range wb.States {
		if s != nil {
			states = append(states, *s)
		}
	}
	// Add the synthetic _continue terminal state (body-complete signal).
	states = append(states, StateSpec{
		Name:     "_continue",
		Terminal: true,
	})
	waits := make([]WaitSpec, 0, len(wb.Waits))
	for _, w := range wb.Waits {
		if w != nil {
			waits = append(waits, *w)
		}
	}
	approvals := make([]ApprovalSpec, 0, len(wb.Approvals))
	for _, a := range wb.Approvals {
		if a != nil {
			approvals = append(approvals, *a)
		}
	}
	branches := make([]BranchSpec, 0, len(wb.Branches))
	for _, b := range wb.Branches {
		if b != nil {
			branches = append(branches, *b)
		}
	}
	return &Spec{
		Name:         stepName + ":body",
		Version:      "1",
		InitialState: entry,
		TargetState:  "_continue",
		Steps:        steps,
		States:       states,
		Waits:        waits,
		Approvals:    approvals,
		Branches:     branches,
	}
}
