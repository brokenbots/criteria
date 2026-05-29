package engine

// data_store_test.go — unit tests for DataStore.

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/workflow"
)

func newTestStore(vars map[string]*workflow.DataNode) *DataStore {
	g := &workflow.FSMGraph{
		Data: map[string]map[string]*workflow.DataNode{
			"internal": vars,
		},
		DataOrder: make([]workflow.DataRef, 0, len(vars)),
	}
	for name := range vars {
		g.DataOrder = append(g.DataOrder, workflow.DataRef{Kind: "internal", Name: name})
	}
	return NewDataStore(g)
}

func TestDataStore_GetSet_String(t *testing.T) {
	store := newTestStore(map[string]*workflow.DataNode{
		"status": {Kind: "internal", Name: "status", Type: cty.String, InitialValue: cty.StringVal("pending")},
	})

	v, err := store.Get("internal", "status")
	require.NoError(t, err)
	assert.Equal(t, "pending", v.AsString())

	require.NoError(t, store.Set("internal", "status", cty.StringVal("done")))

	v, err = store.Get("internal", "status")
	require.NoError(t, err)
	assert.Equal(t, "done", v.AsString())
}

func TestDataStore_GetSet_Number(t *testing.T) {
	store := newTestStore(map[string]*workflow.DataNode{
		"counter": {Kind: "internal", Name: "counter", Type: cty.Number, InitialValue: cty.NumberIntVal(0)},
	})

	v, err := store.Get("internal", "counter")
	require.NoError(t, err)
	f, _ := v.AsBigFloat().Float64()
	assert.Equal(t, float64(0), f)

	require.NoError(t, store.Set("internal", "counter", cty.NumberIntVal(42)))
	v, err = store.Get("internal", "counter")
	require.NoError(t, err)
	f, _ = v.AsBigFloat().Float64()
	assert.Equal(t, float64(42), f)
}

func TestDataStore_Get_Undeclared(t *testing.T) {
	store := newTestStore(map[string]*workflow.DataNode{})
	_, err := store.Get("internal", "nope")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not declared")
}

func TestDataStore_Set_Undeclared(t *testing.T) {
	store := newTestStore(map[string]*workflow.DataNode{})
	err := store.Set("internal", "nope", cty.StringVal("x"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not declared")
}

func TestDataStore_Set_TypeMismatch(t *testing.T) {
	store := newTestStore(map[string]*workflow.DataNode{
		"counter": {Kind: "internal", Name: "counter", Type: cty.Number, InitialValue: cty.NumberIntVal(0)},
	})
	err := store.Set("internal", "counter", cty.StringVal("not-a-number"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "type")
}

func TestDataStore_NoInitialValue_NullDefault(t *testing.T) {
	store := newTestStore(map[string]*workflow.DataNode{
		"flag": {Kind: "internal", Name: "flag", Type: cty.Bool, InitialValue: cty.NullVal(cty.Bool)},
	})
	v, err := store.Get("internal", "flag")
	require.NoError(t, err)
	assert.True(t, v.IsNull(), "expected null initial value when no value declared")
	assert.Equal(t, cty.Bool, v.Type())
}

func TestDataStore_Snapshot(t *testing.T) {
	store := newTestStore(map[string]*workflow.DataNode{
		"a": {Kind: "internal", Name: "a", Type: cty.String, InitialValue: cty.StringVal("x")},
		"b": {Kind: "internal", Name: "b", Type: cty.Number, InitialValue: cty.NumberIntVal(1)},
	})

	snap := store.Snapshot()
	require.Contains(t, snap, "internal")
	internal := snap["internal"]
	require.True(t, internal.Type().IsObjectType())
	vals := internal.AsValueMap()
	require.Len(t, vals, 2)
	assert.Equal(t, "x", vals["a"].GetAttr("value").AsString())
	f, _ := vals["b"].GetAttr("value").AsBigFloat().Float64()
	assert.Equal(t, float64(1), f)

	// Mutating the snapshot must not affect the store.
	vals["a"] = cty.ObjectVal(map[string]cty.Value{"value": cty.StringVal("mutated"), "type": cty.StringVal("string")})
	v, _ := store.Get("internal", "a")
	assert.Equal(t, "x", v.AsString())
}

func TestDataStore_ConcurrentReadWrite(t *testing.T) {
	store := newTestStore(map[string]*workflow.DataNode{
		"counter": {Kind: "internal", Name: "counter", Type: cty.Number, InitialValue: cty.NumberIntVal(0)},
	})

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	// Writers
	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			_ = store.Set("internal", "counter", cty.NumberIntVal(int64(i)))
		}()
	}
	// Readers
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_, _ = store.Get("internal", "counter")
		}()
	}
	wg.Wait()
	// No panic == concurrent access is safe.
}

func TestDataStore_SnapshotConcurrent(t *testing.T) {
	store := newTestStore(map[string]*workflow.DataNode{
		"v": {Kind: "internal", Name: "v", Type: cty.String, InitialValue: cty.StringVal("init")},
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
			_ = store.Set("internal", "v", cty.StringVal("new"))
		}()
	}
	wg.Wait()
}

func TestNewDataStore_EmptyGraph(t *testing.T) {
	g := &workflow.FSMGraph{Data: map[string]map[string]*workflow.DataNode{}}
	store := NewDataStore(g)
	snap := store.Snapshot()
	assert.Empty(t, snap)
}

// TestDataStore_SetBatch_ListType proves that the store accepts a
// list(string) value via SetBatch. Non-scalar data can only be
// written through a typed outcome output projection (not raw string coercion),
// but the store itself is type-agnostic — it enforces the declared cty.Type.
func TestDataStore_SetBatch_ListType(t *testing.T) {
	listType := cty.List(cty.String)
	store := newTestStore(map[string]*workflow.DataNode{
		"tags": {Kind: "internal", Name: "tags", Type: listType, InitialValue: cty.NullVal(listType)},
	})

	// Simulate the typed write path: projectedCty produces a proper list value.
	listVal := cty.ListVal([]cty.Value{cty.StringVal("foo"), cty.StringVal("bar")})
	require.NoError(t, store.SetBatch([]DataWrite{{Kind: "internal", Name: "tags", Value: listVal}}))

	v, err := store.Get("internal", "tags")
	require.NoError(t, err)
	assert.Equal(t, listType, v.Type())

	var elems []string
	for it := v.ElementIterator(); it.Next(); {
		_, elem := it.Element()
		elems = append(elems, elem.AsString())
	}
	assert.Equal(t, []string{"foo", "bar"}, elems)
}

// TestDataStore_SetBatch_ListType_TypeMismatch proves that setting a
// scalar value into a list-typed data is rejected.
func TestDataStore_SetBatch_ListType_TypeMismatch(t *testing.T) {
	listType := cty.List(cty.String)
	store := newTestStore(map[string]*workflow.DataNode{
		"tags": {Kind: "internal", Name: "tags", Type: listType, InitialValue: cty.NullVal(listType)},
	})
	err := store.SetBatch([]DataWrite{{Kind: "internal", Name: "tags", Value: cty.StringVal("not-a-list")}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "type")
}

// TestDataStore_SetBatch_TupleConvertsToList proves that an HCL tuple
// value (the type produced by `[a, b]` expressions) is converted to the
// declared list type via go-cty's convert package. This is the mechanism that
// enables `output = { items = [step.output.x, step.output.y] }` projections
// to write to list(string) data blocks.
func TestDataStore_SetBatch_TupleConvertsToList(t *testing.T) {
	listType := cty.List(cty.String)
	store := newTestStore(map[string]*workflow.DataNode{
		"tags": {Kind: "internal", Name: "tags", Type: listType, InitialValue: cty.NullVal(listType)},
	})
	// HCL evaluates `[expr, expr]` as a cty.Tuple, not a cty.List.
	tupleVal := cty.TupleVal([]cty.Value{cty.StringVal("alpha"), cty.StringVal("beta")})
	require.NoError(t, store.SetBatch([]DataWrite{{Kind: "internal", Name: "tags", Value: tupleVal}}))

	v, err := store.Get("internal", "tags")
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
		// Shell commands append a trailing newline; whitespace must be trimmed.
		{"1\n", 1},
		{" 42\n", 42},
		{"3.14\n", 3.14},
		{" 7", 7},
		{"7 ", 7},
	}
	for _, tc := range cases {
		v, err := coerceStringToCty(tc.input, cty.Number)
		require.NoError(t, err, "input %q", tc.input)
		f, _ := v.AsBigFloat().Float64()
		assert.InDelta(t, tc.expected, f, 1e-9, "input %q", tc.input)
	}
}

func TestCoerceStringToCty_MalformedNumbers(t *testing.T) {
	// These inputs must be rejected — they have non-numeric content that is
	// not just surrounding whitespace. The previous fmt.Sscanf implementation
	// silently accepted them (e.g. "7abc" → 7, "1e2x" → 100).
	malformed := []string{"7abc", "1e2x", "abc", "7.0.0", "--7", ""}
	for _, bad := range malformed {
		_, err := coerceStringToCty(bad, cty.Number)
		require.Error(t, err, "expected error for malformed number input %q", bad)
	}
}

func TestCoerceStringToCty_Bool(t *testing.T) {
	for _, s := range []string{"true", "1", "true\n", " 1\n"} {
		v, err := coerceStringToCty(s, cty.Bool)
		require.NoError(t, err, "input %q", s)
		assert.True(t, v.True(), "input %q", s)
	}
	for _, s := range []string{"false", "0", "false\n", " 0\n"} {
		v, err := coerceStringToCty(s, cty.Bool)
		require.NoError(t, err, "input %q", s)
		assert.False(t, v.True(), "input %q", s)
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
