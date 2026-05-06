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
