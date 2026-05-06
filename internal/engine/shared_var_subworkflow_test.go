package engine

// shared_var_subworkflow_test.go — isolation tests for shared_variable scoping
// across subworkflow boundaries (W18, Phase 3).
//
// Each subworkflow body gets its own SharedVarStore so writes inside a
// subworkflow do not affect the parent workflow's store, and parent writes
// do not propagate to the child. The tests verify this invariant at the store
// API level without requiring full HCL compilation.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/workflow"
)

// sharedVarGraph returns a minimal FSMGraph that declares one shared_variable
// "msg" (type=string, initial=initial) and terminates immediately.
func sharedVarGraph(initial string) *workflow.FSMGraph {
	sv := &workflow.SharedVariableNode{
		Name:         "msg",
		Type:         cty.String,
		InitialValue: cty.StringVal(initial),
	}
	return &workflow.FSMGraph{
		Name:         "body",
		InitialState: "done",
		TargetState:  "done",
		Policy:       workflow.DefaultPolicy,
		Steps:        map[string]*workflow.StepNode{},
		States: map[string]*workflow.StateNode{
			"done": {Name: "done", Terminal: true, Success: true},
		},
		Variables:           map[string]*workflow.VariableNode{},
		SharedVariables:     map[string]*workflow.SharedVariableNode{"msg": sv},
		SharedVariableOrder: []string{"msg"},
		Adapters:            map[string]*workflow.AdapterNode{},
	}
}

// TestSharedVar_StoresAreIndependentAcrossBodies verifies that two calls to
// NewSharedVarStore from two different FSMGraphs produce independent stores:
// mutating one does not affect the other.
func TestSharedVar_StoresAreIndependentAcrossBodies(t *testing.T) {
	parentStore := NewSharedVarStore(sharedVarGraph("parent-initial"))
	childStore := NewSharedVarStore(sharedVarGraph("child-initial"))

	// Mutate the child store.
	require.NoError(t, childStore.Set("msg", cty.StringVal("child-written")))

	// Parent store must still see its own initial value.
	parentVal, err := parentStore.Get("msg")
	require.NoError(t, err)
	assert.Equal(t, cty.StringVal("parent-initial"), parentVal,
		"parent store must not be affected by child store writes")

	// Child store reflects the write.
	childVal, err := childStore.Get("msg")
	require.NoError(t, err)
	assert.Equal(t, cty.StringVal("child-written"), childVal)
}

// TestSharedVar_ParentMutationNotVisibleInChildStore verifies that writes to a
// parent store are not visible in a separately created child store.
func TestSharedVar_ParentMutationNotVisibleInChildStore(t *testing.T) {
	parentStore := NewSharedVarStore(sharedVarGraph("parent-initial"))
	childStore := NewSharedVarStore(sharedVarGraph("child-default"))

	// Parent updates its store.
	require.NoError(t, parentStore.Set("msg", cty.StringVal("parent-updated")))

	// Child store must see its own initial value, not the parent's updated value.
	childVal, err := childStore.Get("msg")
	require.NoError(t, err)
	assert.Equal(t, cty.StringVal("child-default"), childVal,
		"child store must be initialised from its own graph, not affected by parent store")
}

// TestSharedVar_MultipleStoresFromSameGraphAreIndependent verifies that even
// when two stores are created from the same graph (e.g. two sequential body
// invocations), they do not share state — each starts from the graph's initial values.
func TestSharedVar_MultipleStoresFromSameGraphAreIndependent(t *testing.T) {
	graph := sharedVarGraph("initial")

	// Simulate two sequential runWorkflowBody calls: each creates a fresh store.
	store1 := NewSharedVarStore(graph)
	store2 := NewSharedVarStore(graph)

	require.NoError(t, store1.Set("msg", cty.StringVal("first-run")))

	// store2 must start from "initial", not see store1's write.
	val2, err := store2.Get("msg")
	require.NoError(t, err)
	assert.Equal(t, cty.StringVal("initial"), val2,
		"second store must start from initial value, not inherit first store's state")

	// store1 is unaffected by store2 state (no writes to store2).
	val1, err := store1.Get("msg")
	require.NoError(t, err)
	assert.Equal(t, cty.StringVal("first-run"), val1)
}

// TestSharedVarStore_SetBatch_AllOrNothing verifies that SetBatch is atomic:
// if any entry fails (type mismatch), no writes are committed to the store.
func TestSharedVarStore_SetBatch_AllOrNothing(t *testing.T) {
	store := newTestStore(map[string]*workflow.SharedVariableNode{
		"msg": {
			Name:         "msg",
			Type:         cty.String,
			InitialValue: cty.StringVal("initial"),
		},
		"count": {
			Name:         "count",
			Type:         cty.Number,
			InitialValue: cty.NumberIntVal(0),
		},
	})

	// Batch has one valid write ("msg") and one invalid write ("count" receives
	// a string that cannot be coerced to number — batch must fail atomically).
	err := store.SetBatch(map[string]cty.Value{
		"msg":   cty.StringVal("new-value"),
		"count": cty.StringVal("not-a-number"),
	})
	if err == nil {
		t.Fatal("expected SetBatch to fail on type mismatch")
	}

	// Neither variable must be modified (all-or-nothing).
	got, _ := store.Get("msg")
	assert.Equal(t, cty.StringVal("initial"), got, "msg must not be modified when batch fails")

	gotCount, _ := store.Get("count")
	assert.Equal(t, cty.NumberIntVal(0), gotCount, "count must not be modified when batch fails")
}
