package engine

// data_subworkflow_test.go — isolation tests for data scoping across subworkflow
// boundaries (W18, Phase 3).
//
// Each subworkflow body gets its own DataStore so writes inside a
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

// dataGraph returns a minimal FSMGraph that declares one data block
// "msg" (kind=internal, type=string, initial=initial) and terminates immediately.
func dataGraph(initial string) *workflow.FSMGraph {
	dn := &workflow.DataNode{
		Kind:         "internal",
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
		Variables: map[string]*workflow.VariableNode{},
		Data: map[string]map[string]*workflow.DataNode{
			"internal": {"msg": dn},
		},
		DataOrder: []workflow.DataRef{{Kind: "internal", Name: "msg"}},
		Adapters:  map[string]*workflow.AdapterNode{},
	}
}

// TestData_StoresAreIndependentAcrossBodies verifies that two calls to
// NewDataStore from two different FSMGraphs produce independent stores:
// mutating one does not affect the other.
func TestData_StoresAreIndependentAcrossBodies(t *testing.T) {
	parentStore := NewDataStore(dataGraph("parent-initial"))
	childStore := NewDataStore(dataGraph("child-initial"))

	// Mutate the child store.
	require.NoError(t, childStore.Set("internal", "msg", cty.StringVal("child-written")))

	// Parent store must still see its own initial value.
	parentVal, err := parentStore.Get("internal", "msg")
	require.NoError(t, err)
	assert.Equal(t, cty.StringVal("parent-initial"), parentVal,
		"parent store must not be affected by child store writes")

	// Child store reflects the write.
	childVal, err := childStore.Get("internal", "msg")
	require.NoError(t, err)
	assert.Equal(t, cty.StringVal("child-written"), childVal)
}

// TestData_ParentMutationNotVisibleInChildStore verifies that writes to a
// parent store are not visible in a separately created child store.
func TestData_ParentMutationNotVisibleInChildStore(t *testing.T) {
	parentStore := NewDataStore(dataGraph("parent-initial"))
	childStore := NewDataStore(dataGraph("child-default"))

	// Parent updates its store.
	require.NoError(t, parentStore.Set("internal", "msg", cty.StringVal("parent-updated")))

	// Child store must see its own initial value, not the parent's updated value.
	childVal, err := childStore.Get("internal", "msg")
	require.NoError(t, err)
	assert.Equal(t, cty.StringVal("child-default"), childVal,
		"child store must be initialised from its own graph, not affected by parent store")
}

// TestData_MultipleStoresFromSameGraphAreIndependent verifies that even
// when two stores are created from the same graph (e.g. two sequential body
// invocations), they do not share state — each starts from the graph's initial values.
func TestData_MultipleStoresFromSameGraphAreIndependent(t *testing.T) {
	graph := dataGraph("initial")

	// Simulate two sequential runWorkflowBody calls: each creates a fresh store.
	store1 := NewDataStore(graph)
	store2 := NewDataStore(graph)

	require.NoError(t, store1.Set("internal", "msg", cty.StringVal("first-run")))

	// store2 must start from "initial", not see store1's write.
	val2, err := store2.Get("internal", "msg")
	require.NoError(t, err)
	assert.Equal(t, cty.StringVal("initial"), val2,
		"second store must start from initial value, not inherit first store's state")

	// store1 is unaffected by store2 state (no writes to store2).
	val1, err := store1.Get("internal", "msg")
	require.NoError(t, err)
	assert.Equal(t, cty.StringVal("first-run"), val1)
}

// TestDataStore_SetBatch_AllOrNothing verifies that SetBatch is atomic:
// if any entry fails (type mismatch), no writes are committed to the store.
func TestDataStore_SetBatch_AllOrNothing(t *testing.T) {
	g := &workflow.FSMGraph{
		Data: map[string]map[string]*workflow.DataNode{
			"internal": {
				"msg": {
					Kind:         "internal",
					Name:         "msg",
					Type:         cty.String,
					InitialValue: cty.StringVal("initial"),
				},
				"count": {
					Kind:         "internal",
					Name:         "count",
					Type:         cty.Number,
					InitialValue: cty.NumberIntVal(0),
				},
			},
		},
		DataOrder: []workflow.DataRef{
			{Kind: "internal", Name: "msg"},
			{Kind: "internal", Name: "count"},
		},
	}
	store := NewDataStore(g)

	// Batch has one valid write ("msg") and one invalid write ("count" receives
	// a string that cannot be coerced to number — batch must fail atomically).
	err := store.SetBatch([]DataWrite{
		{Kind: "internal", Name: "msg", Value: cty.StringVal("new-value")},
		{Kind: "internal", Name: "count", Value: cty.StringVal("not-a-number")},
	})
	if err == nil {
		t.Fatal("expected SetBatch to fail on type mismatch")
	}

	// Neither variable must be modified (all-or-nothing).
	got, _ := store.Get("internal", "msg")
	assert.Equal(t, cty.StringVal("initial"), got, "msg must not be modified when batch fails")

	gotCount, _ := store.Get("internal", "count")
	assert.Equal(t, cty.NumberIntVal(0), gotCount, "count must not be modified when batch fails")
}
