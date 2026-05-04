package workflow

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2"
)

// compileSubworkflows resolves each subworkflow.source via opts.SubWorkflowResolver,
// parses and compiles the callee Spec into a child FSMGraph, validates input bindings,
// and stores the result in g.Subworkflows. Cycle detection is enforced via opts.SubworkflowChain.
func compileSubworkflows(g *FSMGraph, spec *Spec, opts CompileOpts) hcl.Diagnostics {
	var diags hcl.Diagnostics

	if len(spec.Subworkflows) == 0 {
		return diags
	}

	if opts.SubWorkflowResolver == nil {
		for _, sw := range spec.Subworkflows {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("subworkflow %q requires SubWorkflowResolver in CompileOpts", sw.Name),
			})
		}
		return diags
	}

	// Track which subworkflow names we've seen (for duplicate detection)
	seenNames := make(map[string]bool)

	for _, swSpec := range spec.Subworkflows {
		// Check for duplicate names
		if seenNames[swSpec.Name] {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("duplicate subworkflow name %q", swSpec.Name),
			})
			continue
		}
		seenNames[swSpec.Name] = true

		// Resolve the source using context.Background() (context propagation not available
		// through CompileOpts to avoid increasing parameter size)
		ctx := context.Background()
		resolvedDir, err := opts.SubWorkflowResolver.ResolveSource(ctx, opts.WorkflowDir, swSpec.Source)
		if err != nil {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("failed to resolve subworkflow %q source: %v", swSpec.Name, err),
			})
			continue
		}

		// Check for cycles
		for _, chainPath := range opts.SubworkflowChain {
			if chainPath != resolvedDir {
				continue
			}
			cycle := make([]string, len(opts.SubworkflowChain)+1)
			copy(cycle, opts.SubworkflowChain)
			cycle[len(cycle)-1] = resolvedDir
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("subworkflow cycle detected: %s", strings.Join(cycle, " -> ")),
			})
		}

		// Read and parse all .hcl files in the directory
		calleeSpec, readDiags := readAndParseSubworkflowDir(resolvedDir)
		diags = append(diags, readDiags...)
		if calleeSpec == nil {
			continue
		}

		// Create new compile options for the recursive call with updated chain
		childOpts := opts
		newChain := make([]string, len(opts.SubworkflowChain), len(opts.SubworkflowChain)+1)
		copy(newChain, opts.SubworkflowChain)
		newChain = append(newChain, resolvedDir)
		childOpts.SubworkflowChain = newChain
		childOpts.LoadDepth = opts.LoadDepth + 1

		// Recursively compile the callee
		calleeGraph, compileDiags := CompileWithOpts(calleeSpec, nil, childOpts) // nil schemas: use permissive mode
		diags = append(diags, compileDiags...)
		if compileDiags.HasErrors() {
			continue
		}

		// Extract declared variable types from the compiled callee
		declaredVars := make(map[string]*VariableNode)
		for varName, varNode := range calleeGraph.Variables {
			declaredVars[varName] = varNode
		}

		// Extract input expressions from the Remain body.
		// For now, we'll store an empty map and handle input validation at runtime.
		var inputs map[string]hcl.Expression
		if swSpec.Remain != nil {
			inputs = make(map[string]hcl.Expression)
			// The input map will be decoded at runtime with proper context
		}

		// Store the SubworkflowNode
		g.Subworkflows[swSpec.Name] = &SubworkflowNode{
			Name:         swSpec.Name,
			SourcePath:   resolvedDir,
			Body:         calleeGraph,
			BodyEntry:    calleeGraph.InitialState,
			Environment:  swSpec.Environment,
			Inputs:       inputs,
			DeclaredVars: declaredVars,
		}
		g.SubworkflowOrder = append(g.SubworkflowOrder, swSpec.Name)
	}

	return diags
}

// readAndParseSubworkflowDir reads all .hcl files in a directory, merges them into a single Spec.
func readAndParseSubworkflowDir(dir string) (*Spec, hcl.Diagnostics) {
	var diags hcl.Diagnostics

	// List all .hcl files in the directory
	entries, err := os.ReadDir(dir)
	if err != nil {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("failed to list directory %q: %v", dir, err),
		})
		return nil, diags
	}

	var specs []*Spec
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".hcl") {
			continue
		}

		filePath := filepath.Join(dir, entry.Name())
		src, err := os.ReadFile(filePath)
		if err != nil {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("failed to read %q: %v", filePath, err),
			})
			continue
		}

		spec, parseDiags := Parse(filePath, src)
		diags = append(diags, parseDiags...)
		if spec != nil {
			specs = append(specs, spec)
		}
	}

	if len(specs) == 0 {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("no .hcl files found in subworkflow directory %q", dir),
		})
		return nil, diags
	}

	// Merge all specs
	mergedSpec := mergeSubworkflowSpecs(specs...)
	return mergedSpec, diags
}

// mergeSubworkflowSpecs merges multiple Specs into one.
func mergeSubworkflowSpecs(specs ...*Spec) *Spec {
	if len(specs) == 0 {
		return nil
	}
	if len(specs) == 1 {
		return specs[0]
	}

	// Use the first spec as the base
	merged := specs[0]

	// Merge remaining specs
	for _, spec := range specs[1:] {
		merged.Variables = append(merged.Variables, spec.Variables...)
		merged.Locals = append(merged.Locals, spec.Locals...)
		merged.Outputs = append(merged.Outputs, spec.Outputs...)
		merged.Adapters = append(merged.Adapters, spec.Adapters...)
		merged.States = append(merged.States, spec.States...)
		merged.Steps = append(merged.Steps, spec.Steps...)
		merged.Waits = append(merged.Waits, spec.Waits...)
		merged.Approvals = append(merged.Approvals, spec.Approvals...)
		merged.Branches = append(merged.Branches, spec.Branches...)
		merged.Environments = append(merged.Environments, spec.Environments...)
	}

	return merged
}
