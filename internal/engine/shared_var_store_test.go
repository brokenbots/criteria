package engine

// shared_var_store_test.go — unit tests for SharedVarStore.

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/workflow"
)

func newTestStore(vars map[string]*workflow.SharedVariableNode) *SharedVarStore {
	g := &workflow.FSMGraph{
		SharedVariables: vars,
	}
	return NewSharedVarStore(g)
}

func TestSharedVarStore_GetSet_String(t *testing.T) {
	store := newTestStore(map[string]*workflow.SharedVariableNode{
		"status": {Name: "status", Type: cty.String, InitialValue: cty.StringVal("pending")},
	})

	v, err := store.Get("status")
	require.NoError(t, err)
	assert.Equal(t, "pending", v.AsString())

	require.NoError(t, store.Set("status", cty.StringVal("done")))

	v, err = store.Get("status")
	require.NoError(t, err)
	assert.Equal(t, "done", v.AsString())
}

func TestSharedVarStore_GetSet_Number(t *testing.T) {
	store := newTestStore(map[string]*workflow.SharedVariableNode{
		"counter": {Name: "counter", Type: cty.Number, InitialValue: cty.NumberIntVal(0)},
	})

	v, err := store.Get("counter")
	require.NoError(t, err)
	f, _ := v.AsBigFloat().Float64()
	assert.Equal(t, float64(0), f)

	require.NoError(t, store.Set("counter", cty.NumberIntVal(42)))
	v, err = store.Get("counter")
	require.NoError(t, err)
	f, _ = v.AsBigFloat().Float64()
	assert.Equal(t, float64(42), f)
}

func TestSharedVarStore_Get_Undeclared(t *testing.T) {
	store := newTestStore(map[string]*workflow.SharedVariableNode{})
	_, err := store.Get("nope")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not declared")
}

func TestSharedVarStore_Set_Undeclared(t *testing.T) {
	store := newTestStore(map[string]*workflow.SharedVariableNode{})
	err := store.Set("nope", cty.StringVal("x"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not declared")
}

func TestSharedVarStore_Set_TypeMismatch(t *testing.T) {
	store := newTestStore(map[string]*workflow.SharedVariableNode{
		"counter": {Name: "counter", Type: cty.Number, InitialValue: cty.NumberIntVal(0)},
	})
	err := store.Set("counter", cty.StringVal("not-a-number"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "type")
}

func TestSharedVarStore_NoInitialValue_NullDefault(t *testing.T) {
	// When no value is declared, compile sets InitialValue = cty.NullVal(type).
	store := newTestStore(map[string]*workflow.SharedVariableNode{
		"flag": {Name: "flag", Type: cty.Bool, InitialValue: cty.NullVal(cty.Bool)},
	})
	v, err := store.Get("flag")
	require.NoError(t, err)
	assert.True(t, v.IsNull(), "expected null initial value when no value declared")
	assert.Equal(t, cty.Bool, v.Type())
}

func TestSharedVarStore_Snapshot(t *testing.T) {
	store := newTestStore(map[string]*workflow.SharedVariableNode{
		"a": {Name: "a", Type: cty.String, InitialValue: cty.StringVal("x")},
		"b": {Name: "b", Type: cty.Number, InitialValue: cty.NumberIntVal(1)},
	})

	snap := store.Snapshot()
	assert.Len(t, snap, 2)
	assert.Equal(t, "x", snap["a"].AsString())
	f, _ := snap["b"].AsBigFloat().Float64()
	assert.Equal(t, float64(1), f)

	// Mutating the snapshot must not affect the store.
	snap["a"] = cty.StringVal("mutated")
	v, _ := store.Get("a")
	assert.Equal(t, "x", v.AsString())
}

func TestSharedVarStore_ConcurrentReadWrite(t *testing.T) {
	store := newTestStore(map[string]*workflow.SharedVariableNode{
		"counter": {Name: "counter", Type: cty.Number, InitialValue: cty.NumberIntVal(0)},
	})

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	// Writers
	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			_ = store.Set("counter", cty.NumberIntVal(int64(i)))
		}()
	}
	// Readers
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_, _ = store.Get("counter")
		}()
	}
	wg.Wait()
	// No panic == concurrent access is safe.
}

func TestSharedVarStore_SnapshotConcurrent(t *testing.T) {
	store := newTestStore(map[string]*workflow.SharedVariableNode{
		"v": {Name: "v", Type: cty.String, InitialValue: cty.StringVal("init")},
	})

	var wg sync.WaitGroup
	const n = 40
	wg.Add(n * 2)

	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_ = store.Snapshot()
		}()
		go func() {
			defer wg.Done()
			_ = store.Set("v", cty.StringVal("new"))
		}()
	}
	wg.Wait()
}

func TestNewSharedVarStore_EmptyGraph(t *testing.T) {
	g := &workflow.FSMGraph{SharedVariables: map[string]*workflow.SharedVariableNode{}}
	store := NewSharedVarStore(g)
	snap := store.Snapshot()
	assert.Empty(t, snap)
}

// TestSharedVarStore_SetBatch_ListType proves that the store accepts a
// list(string) value via SetBatch. Non-scalar shared variables can only be
// written through a typed outcome output projection (not raw string coercion),
// but the store itself is type-agnostic — it enforces the declared cty.Type.
func TestSharedVarStore_SetBatch_ListType(t *testing.T) {
	listType := cty.List(cty.String)
	store := newTestStore(map[string]*workflow.SharedVariableNode{
		"tags": {Name: "tags", Type: listType, InitialValue: cty.NullVal(listType)},
	})

	// Simulate the typed write path: projectedCty produces a proper list value.
	listVal := cty.ListVal([]cty.Value{cty.StringVal("foo"), cty.StringVal("bar")})
	require.NoError(t, store.SetBatch(map[string]cty.Value{"tags": listVal}))

	v, err := store.Get("tags")
	require.NoError(t, err)
	assert.Equal(t, listType, v.Type())

	var elems []string
	for it := v.ElementIterator(); it.Next(); {
		_, elem := it.Element()
		elems = append(elems, elem.AsString())
	}
	assert.Equal(t, []string{"foo", "bar"}, elems)
}

// TestSharedVarStore_SetBatch_ListType_TypeMismatch proves that setting a
// scalar value into a list-typed shared variable is rejected.
func TestSharedVarStore_SetBatch_ListType_TypeMismatch(t *testing.T) {
	listType := cty.List(cty.String)
	store := newTestStore(map[string]*workflow.SharedVariableNode{
		"tags": {Name: "tags", Type: listType, InitialValue: cty.NullVal(listType)},
	})
	err := store.SetBatch(map[string]cty.Value{"tags": cty.StringVal("not-a-list")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "type")
}

// TestSharedVarStore_SetBatch_TupleConvertsToList proves that an HCL tuple
// value (the type produced by `[a, b]` expressions) is converted to the
// declared list type via go-cty's convert package. This is the mechanism that
// enables `output = { items = [step.output.x, step.output.y] }` projections
// to write to list(string) shared variables.
func TestSharedVarStore_SetBatch_TupleConvertsToList(t *testing.T) {
	listType := cty.List(cty.String)
	store := newTestStore(map[string]*workflow.SharedVariableNode{
		"tags": {Name: "tags", Type: listType, InitialValue: cty.NullVal(listType)},
	})
	// HCL evaluates `[expr, expr]` as a cty.Tuple, not a cty.List.
	tupleVal := cty.TupleVal([]cty.Value{cty.StringVal("alpha"), cty.StringVal("beta")})
	require.NoError(t, store.SetBatch(map[string]cty.Value{"tags": tupleVal}))

	v, err := store.Get("tags")
	require.NoError(t, err)
	assert.Equal(t, listType, v.Type())
	var elems []string
	for it := v.ElementIterator(); it.Next(); {
		_, elem := it.Element()
		elems = append(elems, elem.AsString())
	}
	assert.Equal(t, []string{"alpha", "beta"}, elems)
}

func TestCoerceStringToCty_ValidNumbers(t *testing.T) {
	cases := []struct {
		input    string
		expected float64
	}{
		{"0", 0},
		{"42", 42},
		{"3.14", 3.14},
		{"1e5", 1e5},
		{"-7", -7},
		{"0.001", 0.001},
	}
	for _, tc := range cases {
		v, err := coerceStringToCty(tc.input, cty.Number)
		require.NoError(t, err, "input %q", tc.input)
		f, _ := v.AsBigFloat().Float64()
		assert.InDelta(t, tc.expected, f, 1e-9, "input %q", tc.input)
	}
}

func TestCoerceStringToCty_MalformedNumbers(t *testing.T) {
	// These inputs must be rejected — they have trailing non-numeric content
	// or are otherwise invalid. The previous fmt.Sscanf implementation
	// silently accepted them (e.g. "7abc" → 7, "1e2x" → 100).
	malformed := []string{"7abc", "1e2x", "abc", " 7", "7 ", "7.0.0", "--7", ""}
	for _, bad := range malformed {
		_, err := coerceStringToCty(bad, cty.Number)
		require.Error(t, err, "expected error for malformed number input %q", bad)
	}
}

func TestCoerceStringToCty_Bool(t *testing.T) {
	for _, s := range []string{"true", "1"} {
		v, err := coerceStringToCty(s, cty.Bool)
		require.NoError(t, err)
		assert.True(t, v.True())
	}
	for _, s := range []string{"false", "0"} {
		v, err := coerceStringToCty(s, cty.Bool)
		require.NoError(t, err)
		assert.False(t, v.True())
	}
	_, err := coerceStringToCty("yes", cty.Bool)
	require.Error(t, err)
}

func TestCoerceStringToCty_UnsupportedType(t *testing.T) {
	// Non-scalar types must return an error from coercion.
	unsupported := []cty.Type{
		cty.List(cty.String),
		cty.Map(cty.String),
		cty.List(cty.Number),
	}
	for _, typ := range unsupported {
		_, err := coerceStringToCty("value", typ)
		require.Error(t, err, "expected error for unsupported type %s", typ.FriendlyName())
	}
}
