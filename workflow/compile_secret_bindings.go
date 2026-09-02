package workflow

// compile_secret_bindings.go — compile-time validation for secret channels.
//
// Secret values must be declared as first-class, secret-tainted variables or
// data blocks and referenced directly; literal strings and non-secret values
// are rejected in adapter.secrets and step.secret_input.

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
)

// ValidateSecretBindings checks that every secret channel expression
// (adapter.secrets and step.secret_input) references declared secret values
// rather than literal strings or non-secret values.
func ValidateSecretBindings(g *FSMGraph) hcl.Diagnostics {
	if g == nil {
		return nil
	}

	var diags hcl.Diagnostics

	for _, ad := range g.Adapters {
		for name, expr := range ad.Secrets {
			diags = append(diags, validateAdapterSecretBinding(g, ad, name, expr)...)
		}
	}

	for _, step := range g.Steps {
		for name, expr := range step.SecretInputExprs {
			diags = append(diags, validateStepSecretInputBinding(g, step, name, expr)...)
		}
	}

	return diags
}

// validateAdapterSecretBinding enforces that adapter.secrets values are direct
// references to secret variables (var.X) or secret data blocks
// (data.KIND.NAME.value).
func validateAdapterSecretBinding(g *FSMGraph, ad *AdapterNode, name string, expr hcl.Expression) hcl.Diagnostics {
	ref, ok := secretRefFromExpr(expr, g)
	if !ok {
		return hcl.Diagnostics{&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("adapter %q.%q secrets.%s: must reference a declared secret variable or data block", ad.Type, ad.Name, name),
			Detail:   "Secret bindings must be direct references: var.<name> or data.<kind>.<name>.value. Literal strings and non-secret references are not allowed.",
			Subject:  expr.Range().Ptr(),
		}}
	}

	if ref.isVariable {
		v := g.Variables[ref.name]
		if v == nil || !v.Secret {
			return hcl.Diagnostics{&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  fmt.Sprintf("adapter %q.%q secrets.%s: var.%q is not declared as a secret variable", ad.Type, ad.Name, name, ref.name),
				Subject:  expr.Range().Ptr(),
			}}
		}
		return nil
	}

	dn := g.Data[ref.dataKind][ref.dataName]
	if dn == nil || !dn.Secret {
		return hcl.Diagnostics{&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("adapter %q.%q secrets.%s: data.%q.%q is not declared as a secret data block", ad.Type, ad.Name, name, ref.dataKind, ref.dataName),
			Subject:  expr.Range().Ptr(),
		}}
	}
	return nil
}

// validateStepSecretInputBinding enforces that secret_input values are
// tainted expressions: they must reference a secret variable, secret data block,
// sensitive adapter output, or another secret-tainted origin.
func validateStepSecretInputBinding(g *FSMGraph, step *StepNode, name string, expr hcl.Expression) hcl.Diagnostics {
	if _, tainted := checkExprForTaint(expr, g, nil); !tainted {
		return hcl.Diagnostics{&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf("step %q secret_input.%s: value must be a secret-tainted expression", step.Name, name),
			Detail:   "Secret input bindings must reference declared secret variables, declared secret data blocks, sensitive step outputs, or other secret-tainted values. Literal strings and non-secret references are not allowed.",
			Subject:  expr.Range().Ptr(),
		}}
	}
	return nil
}

// secretRef describes a direct secret reference extracted from an HCL
// expression.
type secretRef struct {
	isVariable bool
	name       string // var name when isVariable
	dataKind   string // data kind when !isVariable
	dataName   string // data name when !isVariable
}

// SecretBindingRefFromExpr extracts a direct var/data reference from an
// expression. It returns ok=true when the expression is a simple traversal to
// a declared variable (isVariable=true, key=name) or to a declared data block
// value (isVariable=false, key=kind.name). This is used by the engine to map
// adapter.secret bindings back to their declared origins for snapshotting.
func SecretBindingRefFromExpr(expr hcl.Expression, g *FSMGraph) (isVariable bool, key string, ok bool) {
	ref, ok := secretRefFromExpr(expr, g)
	if !ok {
		return false, "", false
	}
	if ref.isVariable {
		return true, ref.name, true
	}
	return false, ref.dataKind + "." + ref.dataName, true
}

// secretRefFromExpr extracts a direct var/data reference from an expression.
// Returns ok=false if the expression is not a simple traversal to a var or
// data.<kind>.<name>.value.
func secretRefFromExpr(expr hcl.Expression, g *FSMGraph) (secretRef, bool) {
	trav, diags := hcl.AbsTraversalForExpr(expr)
	if diags.HasErrors() || len(trav) < 2 {
		return secretRef{}, false
	}

	root, ok1 := trav[0].(hcl.TraverseRoot)
	attr, ok2 := trav[1].(hcl.TraverseAttr)
	if !ok1 || !ok2 {
		return secretRef{}, false
	}

	switch root.Name {
	case "var":
		return parseVarRef(attr.Name, len(trav), g)
	case "data":
		return parseDataRef(trav, g)
	}
	return secretRef{}, false
}

func parseVarRef(name string, travLen int, g *FSMGraph) (secretRef, bool) {
	if travLen != 2 {
		return secretRef{}, false
	}
	if g != nil {
		if _, ok := g.Variables[name]; !ok {
			return secretRef{}, false
		}
	}
	return secretRef{isVariable: true, name: name}, true
}

func parseDataRef(trav hcl.Traversal, g *FSMGraph) (secretRef, bool) {
	if len(trav) != 4 {
		return secretRef{}, false
	}
	kindAttr, ok1 := trav[1].(hcl.TraverseAttr)
	nameAttr, ok2 := trav[2].(hcl.TraverseAttr)
	valueAttr, ok3 := trav[3].(hcl.TraverseAttr)
	if !ok1 || !ok2 || !ok3 || valueAttr.Name != "value" {
		return secretRef{}, false
	}
	if g != nil {
		if g.Data[kindAttr.Name] == nil || g.Data[kindAttr.Name][nameAttr.Name] == nil {
			return secretRef{}, false
		}
	}
	return secretRef{dataKind: kindAttr.Name, dataName: nameAttr.Name}, true
}
