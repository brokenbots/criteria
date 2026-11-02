package engine

import (
	"context"
	"fmt"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// step_writes_test.go — regression test suite for step outcome write value
// expression evaluation (CRI-94).
//
// Write value expressions that are not bare output.<attr> traversals must be
// evaluated through the full expression engine rather than routed through the
// bare-traversal fast path.

func runWriteExprTest(t *testing.T, src string, outputs map[string]string) (*outputCaptureSink, error) {
	t.Helper()
	g := compile(t, src)
	plug := &fakeOutputAdapter{name: "sw", outcome: "success", outputs: outputs}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"sw": plug}}
	sink := &outputCaptureSink{}
	eng := NewTestEngine(g, loader, sink)
	err := eng.Run(context.Background())
	return sink, err
}

// TestStep_WriteValueExpressions tests full expression evaluation on outcome write blocks.
func TestStep_WriteValueExpressions(t *testing.T) {
	cases := []struct {
		name       string
		dataType   string
		writeVal   string
		locals     string
		adapterOut map[string]string
		wantResult string
	}{
		{
			name:       "ComparisonBool_Open",
			dataType:   "bool",
			writeVal:   `output.stdout == "open"`,
			adapterOut: map[string]string{"stdout": "open"},
			wantResult: "true",
		},
		{
			name:       "ComparisonBool_Closed",
			dataType:   "bool",
			writeVal:   `output.stdout == "open"`,
			adapterOut: map[string]string{"stdout": "closed"},
			wantResult: "false",
		},
		{
			name:       "FunctionWrappedComparison",
			dataType:   "bool",
			writeVal:   `trimspace(output.stdout) == "open"`,
			adapterOut: map[string]string{"stdout": "  open  "},
			wantResult: "true",
		},
		{
			name:       "ArithmeticExpression",
			dataType:   "number",
			writeVal:   `output.count + 5`,
			adapterOut: map[string]string{"count": "10"},
			wantResult: "15",
		},
		{
			name:       "StringInterpolation",
			dataType:   "string",
			writeVal:   `"hello ${output.name}"`,
			adapterOut: map[string]string{"name": "world"},
			wantResult: "hello world",
		},
		{
			name:       "BareOutputToBool",
			dataType:   "bool",
			writeVal:   `output.flag`,
			adapterOut: map[string]string{"flag": "true"},
			wantResult: "true",
		},
		{
			name:       "BareOutputToString",
			dataType:   "string",
			writeVal:   `output.val`,
			adapterOut: map[string]string{"val": "hello"},
			wantResult: "hello",
		},
		{
			name:       "BareOutputToNumber",
			dataType:   "number",
			writeVal:   `output.num`,
			adapterOut: map[string]string{"num": "42"},
			wantResult: "42",
		},
		{
			name:       "LocalPlusOutput",
			dataType:   "bool",
			locals:     `local "threshold" { value = 5 }`,
			writeVal:   `output.count > local.threshold`,
			adapterOut: map[string]string{"count": "10"},
			wantResult: "true",
		},
		{
			name:       "NoOutputTraversal_Literal",
			dataType:   "string",
			writeVal:   `"constant_val"`,
			adapterOut: map[string]string{},
			wantResult: "constant_val",
		},
		{
			name:       "SingleOutputTraversalInParentheses",
			dataType:   "string",
			writeVal:   `(output.val)`,
			adapterOut: map[string]string{"val": "wrapped"},
			wantResult: "wrapped",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := fmt.Sprintf(`workflow {
  name          = "write_expr_test"
  version       = "0.1"
  initial_state = "check"
  target_state  = "done"
}

%s

data "internal" "target_val" {
  type = %s
}

step "check" {
  target = adapter.sw
  outcome "success" {
    next = step.verify
    write {
      target = data.internal.target_val.value
      value  = %s
    }
  }
  outcome "error" { next = step.verify }
}

step "verify" {
  target = adapter.sw
  outcome "success" {
    next = step.done
    output = { result = data.internal.target_val.value }
  }
  outcome "error" { next = step.done }
}

state "done" {
  terminal = true
  success  = true
}
`, tc.locals, tc.dataType, tc.writeVal)
			sink, err := runWriteExprTest(t, src, tc.adapterOut)
			require.NoError(t, err)
			assert.Equal(t, "done", sink.terminal)
			assert.Equal(t, tc.wantResult, sink.captured["verify"]["result"])
		})
	}
}

// TestStep_WriteValueExpressions_TypedProjectionKey tests write expressions referencing
// a projected output key on the outcome block.
func TestStep_WriteValueExpressions_TypedProjectionKey(t *testing.T) {
	const src = `
workflow {
  name          = "write_projection_test"
  version       = "0.1"
  initial_state = "collect"
  target_state  = "done"
}

data "internal" "clean" {
  type = string
}

step "collect" {
  target = adapter.sw
  outcome "success" {
    next = step.verify
    output = { cleaned = lower(step.output.raw) }
    write {
      target = data.internal.clean.value
      value  = output.cleaned
    }
  }
  outcome "error" { next = step.verify }
}

step "verify" {
  target = adapter.sw
  outcome "success" {
    next = step.done
    output = { result = data.internal.clean.value }
  }
  outcome "error" { next = step.done }
}

state "done" {
  terminal = true
  success  = true
}
`
	sink, err := runWriteExprTest(t, src, map[string]string{"raw": "HELLO"})
	require.NoError(t, err)
	assert.Equal(t, "done", sink.terminal)
	assert.Equal(t, "hello", sink.captured["verify"]["result"])
}

// TestStep_Unit_BareTraversalAST exercises resolveBareOutputTraversal directly
// to ensure only exact output.<attr> traversal expressions take the fast path.
func TestStep_Unit_BareTraversalAST(t *testing.T) {
	parseExpr := func(src string) hcl.Expression {
		t.Helper()
		expr, diags := hclsyntax.ParseExpression([]byte(src), "test.hcl", hcl.Pos{Line: 1, Column: 1})
		require.False(t, diags.HasErrors(), "parse %q: %s", src, diags.Error())
		return expr
	}

	store := newTestStore(map[string]*workflow.DataNode{
		"flag": {Kind: "internal", Name: "flag", Type: cty.Bool, InitialValue: cty.NullVal(cty.Bool)},
	})
	w := workflow.CompiledWrite{
		DataKind: "internal",
		DataName: "flag",
	}

	cases := []struct {
		expr   string
		wantOK bool
	}{
		{"output.stdout", true},
		{"output.count", true},
		{"output.foo.bar", false},
		{"output.stdout == \"open\"", false},
		{"trimspace(output.stdout) == \"open\"", false},
		{"output.count + 1", false},
		{"\"prefix:${output.token}\"", false},
		{"(output.stdout)", false},
		{"output.count > 5", false},
	}
	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			w.ValueExpr = parseExpr(tc.expr)
			_, ok, err := resolveBareOutputTraversal(w, nil, map[string]cty.Value{
				"stdout": cty.StringVal("true"),
				"count":  cty.StringVal("7"),
				"token":  cty.StringVal("abc"),
			}, store)
			_ = err
			assert.Equal(t, tc.wantOK, ok, "fast path decision for %q", tc.expr)
		})
	}
}
