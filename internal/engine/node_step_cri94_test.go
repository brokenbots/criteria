package engine

import (
	"context"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/internal/adapterhost"
	"github.com/brokenbots/criteria/workflow"
)

// CRI-94 regression tests: write value expressions that are not bare
// output.<attr> traversals must be evaluated, not routed through the bare-
// traversal fast path.

func runCRI94(t *testing.T, src string, outputs map[string]string) (*outputCaptureSink, error) {
	t.Helper()
	g := compile(t, src)
	plug := &fakeOutputAdapter{name: "sw", outcome: "success", outputs: outputs}
	loader := &fakeLoader{adapters: map[string]adapterhost.Handle{"sw": plug}}
	sink := &outputCaptureSink{}
	eng := NewTestEngine(g, loader, sink)
	err := eng.Run(context.Background())
	return sink, err
}

func TestCRI94_A_ComparisonBool(t *testing.T) {
	const src = `
workflow {
  name = "cri-94-a"
  version       = "0.1"
  initial_state = "check"
  target_state  = "done"
}

data "internal" "is_open" {
  type = bool
}

step "check" {
  target = adapter.sw
  outcome "success" {
    next = step.verify
    write {
      target = data.internal.is_open.value
      value  = output.stdout == "open"
    }
  }
}

step "verify" {
  target = adapter.sw
  outcome "success" {
    next = step.done
    output = { result = data.internal.is_open.value }
  }
}

state "done" {
  terminal = true
  success  = true
}
`
	sink, err := runCRI94(t, src, map[string]string{"stdout": "open"})
	require.NoError(t, err, "shape A stdout=open should succeed")
	assert.Equal(t, "done", sink.terminal)
	assert.Equal(t, "true", sink.captured["verify"]["result"])

	sink, err = runCRI94(t, src, map[string]string{"stdout": "closed"})
	require.NoError(t, err, "shape A stdout=closed should succeed")
	assert.Equal(t, "done", sink.terminal)
	assert.Equal(t, "false", sink.captured["verify"]["result"])
}

func TestCRI94_B_FunctionWrappedComparison(t *testing.T) {
	const src = `
workflow {
  name = "cri-94-b"
  version       = "0.1"
  initial_state = "check"
  target_state  = "done"
}

data "internal" "is_open" {
  type = bool
}

step "check" {
  target = adapter.sw
  outcome "success" {
    next = step.verify
    write {
      target = data.internal.is_open.value
      value  = trimspace(output.stdout) == "open"
    }
  }
}

step "verify" {
  target = adapter.sw
  outcome "success" {
    next = step.done
    output = { result = data.internal.is_open.value }
  }
}

state "done" {
  terminal = true
  success  = true
}
`
	sink, err := runCRI94(t, src, map[string]string{"stdout": "  open  "})
	require.NoError(t, err, "shape B should succeed")
	assert.Equal(t, "done", sink.terminal)
	assert.Equal(t, "true", sink.captured["verify"]["result"])
}

func TestCRI94_C_ArithmeticExpression(t *testing.T) {
	const src = `
workflow {
  name = "cri-94-c"
  version       = "0.1"
  initial_state = "inc"
  target_state  = "done"
}

data "internal" "next_count" {
  type = number
}

step "inc" {
  target = adapter.sw
  outcome "success" {
    next = step.verify
    write {
      target = data.internal.next_count.value
      value  = output.count + 1
    }
  }
}

step "verify" {
  target = adapter.sw
  outcome "success" {
    next = step.done
    output = { result = data.internal.next_count.value }
  }
}

state "done" {
  terminal = true
  success  = true
}
`
	sink, err := runCRI94(t, src, map[string]string{"count": "7"})
	require.NoError(t, err, "shape C should succeed")
	assert.Equal(t, "done", sink.terminal)
	assert.Equal(t, "8", sink.captured["verify"]["result"])
}

func TestCRI94_D_StringInterpolation(t *testing.T) {
	const src = `
workflow {
  name = "cri-94-d"
  version       = "0.1"
  initial_state = "prefix"
  target_state  = "done"
}

data "internal" "combined" {
  type = string
}

step "prefix" {
  target = adapter.sw
  outcome "success" {
    next = step.verify
    write {
      target = data.internal.combined.value
      value  = "prefix:${output.token}"
    }
  }
}

step "verify" {
  target = adapter.sw
  outcome "success" {
    next = step.done
    output = { result = data.internal.combined.value }
  }
}

state "done" {
  terminal = true
  success  = true
}
`
	sink, err := runCRI94(t, src, map[string]string{"token": "abc"})
	require.NoError(t, err, "shape D should succeed")
	assert.Equal(t, "done", sink.terminal)
	assert.Equal(t, "prefix:abc", sink.captured["verify"]["result"])
}

func TestCRI94_E_BareOutputToBool(t *testing.T) {
	const src = `
workflow {
  name = "cri-94-e"
  version       = "0.1"
  initial_state = "check"
  target_state  = "done"
}

data "internal" "flag" {
  type = bool
}

step "check" {
  target = adapter.sw
  outcome "success" {
    next = step.verify
    write {
      target = data.internal.flag.value
      value  = output.stdout
    }
  }
}

step "verify" {
  target = adapter.sw
  outcome "success" {
    next = step.done
    output = { result = data.internal.flag.value }
  }
}

state "done" {
  terminal = true
  success  = true
}
`
	sink, err := runCRI94(t, src, map[string]string{"stdout": "true"})
	require.NoError(t, err, "shape E true should succeed")
	assert.Equal(t, "done", sink.terminal)
	assert.Equal(t, "true", sink.captured["verify"]["result"])

	sink, err = runCRI94(t, src, map[string]string{"stdout": "false"})
	require.NoError(t, err, "shape E false should succeed")
	assert.Equal(t, "done", sink.terminal)
	assert.Equal(t, "false", sink.captured["verify"]["result"])
}

func TestCRI94_N1_BareOutputToString(t *testing.T) {
	const src = `
workflow {
  name = "cri-94-n1"
  version       = "0.1"
  initial_state = "check"
  target_state  = "done"
}

data "internal" "msg" {
  type = string
}

step "check" {
  target = adapter.sw
  outcome "success" {
    next = step.verify
    write {
      target = data.internal.msg.value
      value  = output.stdout
    }
  }
}

step "verify" {
  target = adapter.sw
  outcome "success" {
    next = step.done
    output = { result = data.internal.msg.value }
  }
}

state "done" {
  terminal = true
  success  = true
}
`
	sink, err := runCRI94(t, src, map[string]string{"stdout": "hello"})
	require.NoError(t, err, "shape N1 should succeed")
	assert.Equal(t, "done", sink.terminal)
	assert.Equal(t, "hello", sink.captured["verify"]["result"])
}

func TestCRI94_N2_BareOutputToNumber(t *testing.T) {
	const src = `
workflow {
  name = "cri-94-n2"
  version       = "0.1"
  initial_state = "check"
  target_state  = "done"
}

data "internal" "num" {
  type = number
}

step "check" {
  target = adapter.sw
  outcome "success" {
    next = step.verify
    write {
      target = data.internal.num.value
      value  = output.val
    }
  }
}

step "verify" {
  target = adapter.sw
  outcome "success" {
    next = step.done
    output = { result = data.internal.num.value }
  }
}

state "done" {
  terminal = true
  success  = true
}
`
	sink, err := runCRI94(t, src, map[string]string{"val": "42"})
	require.NoError(t, err, "shape N2 should succeed")
	assert.Equal(t, "done", sink.terminal)
	assert.Equal(t, "42", sink.captured["verify"]["result"])
}

func TestCRI94_N3_LocalPlusOutput(t *testing.T) {
	const src = `
workflow {
  name = "cri-94-n3"
  version       = "0.1"
  initial_state = "check"
  target_state  = "done"
}

local "threshold" {
  value = 5
}

data "internal" "above" {
  type = bool
}

step "check" {
  target = adapter.sw
  outcome "success" {
    next = step.verify
    write {
      target = data.internal.above.value
      value  = output.count > local.threshold
    }
  }
}

step "verify" {
  target = adapter.sw
  outcome "success" {
    next = step.done
    output = { result = data.internal.above.value }
  }
}

state "done" {
  terminal = true
  success  = true
}
`
	sink, err := runCRI94(t, src, map[string]string{"count": "7"})
	require.NoError(t, err, "shape N3 should succeed")
	assert.Equal(t, "done", sink.terminal)
	assert.Equal(t, "true", sink.captured["verify"]["result"])
}

func TestCRI94_N4_NoOutputTraversal(t *testing.T) {
	const src = `
workflow {
  name = "cri-94-n4"
  version       = "0.1"
  initial_state = "check"
  target_state  = "done"
}

local "const_value" {
  value = "static"
}

data "internal" "msg" {
  type = string
}

step "check" {
  target = adapter.sw
  outcome "success" {
    next = step.verify
    write {
      target = data.internal.msg.value
      value  = local.const_value
    }
  }
}

step "verify" {
  target = adapter.sw
  outcome "success" {
    next = step.done
    output = { result = data.internal.msg.value }
  }
}

state "done" {
  terminal = true
  success  = true
}
`
	sink, err := runCRI94(t, src, map[string]string{})
	require.NoError(t, err, "shape N4 should succeed")
	assert.Equal(t, "done", sink.terminal)
	assert.Equal(t, "static", sink.captured["verify"]["result"])
}

func TestCRI94_N5_TypedProjectionKey(t *testing.T) {
	const src = `
workflow {
  name = "cri-94-n5"
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
}

step "verify" {
  target = adapter.sw
  outcome "success" {
    next = step.done
    output = { result = data.internal.clean.value }
  }
}

state "done" {
  terminal = true
  success  = true
}
`
	sink, err := runCRI94(t, src, map[string]string{"raw": "HELLO"})
	require.NoError(t, err, "shape N5 should succeed")
	assert.Equal(t, "done", sink.terminal)
	assert.Equal(t, "hello", sink.captured["verify"]["result"])
}

// TestCRI94_N6_SingleOutputTraversalInParentheses checks that a parenthesized
// bare traversal is treated as a non-bare expression and evaluated normally.
// It still produces the correct result because evaluation is equivalent, but it
// does not receive the bare-traversal type-conversion fast path.
func TestCRI94_N6_SingleOutputTraversalInParentheses(t *testing.T) {
	const src = `
workflow {
  name = "cri-94-n6"
  version       = "0.1"
  initial_state = "check"
  target_state  = "done"
}

data "internal" "msg" {
  type = string
}

step "check" {
  target = adapter.sw
  outcome "success" {
    next = step.verify
    write {
      target = data.internal.msg.value
      value  = (output.stdout)
    }
  }
}

step "verify" {
  target = adapter.sw
  outcome "success" {
    next = step.done
    output = { result = data.internal.msg.value }
  }
}

state "done" {
  terminal = true
  success  = true
}
`
	sink, err := runCRI94(t, src, map[string]string{"stdout": "hello"})
	require.NoError(t, err, "shape N6 should succeed")
	assert.Equal(t, "done", sink.terminal)
	assert.Equal(t, "hello", sink.captured["verify"]["result"])
}

// TestCRI94_Unit_BareTraversalAST exercises resolveBareOutputTraversal
// directly to ensure only exact output.<attr> traversal expressions take
// the fast path.
func TestCRI94_Unit_BareTraversalAST(t *testing.T) {
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
			// The fast path decision is independent of whether the resulting raw
			// value can be converted to the data block's declared type; that is
			// covered by the integration tests above.
			_ = err
			assert.Equal(t, tc.wantOK, ok, "fast path decision for %q", tc.expr)
		})
	}
}
