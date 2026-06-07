package lockfile_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/brokenbots/criteria/workflow"
	"github.com/brokenbots/criteria/workflow/lockfile"
)

func TestValidateAgainstWorkflow_AllMatch(t *testing.T) {
	lf := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{Type: "a", Name: "x"},
			{Type: "b", Name: "y"},
		},
	}
	graph := &workflow.FSMGraph{
		Adapters: map[string]*workflow.AdapterNode{
			"a.x": {Type: "a", Name: "x"},
			"b.y": {Type: "b", Name: "y"},
		},
	}
	missing, stale := lockfile.ValidateAgainstWorkflow(lf, graph)
	assert.Empty(t, missing)
	assert.Empty(t, stale)
}

func TestValidateAgainstWorkflow_Missing(t *testing.T) {
	lf := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{Type: "a", Name: "x"},
		},
	}
	graph := &workflow.FSMGraph{
		Adapters: map[string]*workflow.AdapterNode{
			"a.x": {Type: "a", Name: "x"},
			"b.y": {Type: "b", Name: "y"},
		},
	}
	missing, stale := lockfile.ValidateAgainstWorkflow(lf, graph)
	assert.Equal(t, []string{"b.y"}, missing)
	assert.Empty(t, stale)
}

func TestValidateAgainstWorkflow_Stale(t *testing.T) {
	lf := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{Type: "a", Name: "x"},
			{Type: "b", Name: "y"},
		},
	}
	graph := &workflow.FSMGraph{
		Adapters: map[string]*workflow.AdapterNode{
			"a.x": {Type: "a", Name: "x"},
		},
	}
	missing, stale := lockfile.ValidateAgainstWorkflow(lf, graph)
	assert.Empty(t, missing)
	assert.Equal(t, []string{"b.y"}, stale)
}

func TestValidateAgainstWorkflow_MissingAndStale(t *testing.T) {
	lf := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{Type: "a", Name: "x"},
			{Type: "c", Name: "z"},
		},
	}
	graph := &workflow.FSMGraph{
		Adapters: map[string]*workflow.AdapterNode{
			"a.x": {Type: "a", Name: "x"},
			"b.y": {Type: "b", Name: "y"},
		},
	}
	missing, stale := lockfile.ValidateAgainstWorkflow(lf, graph)
	assert.Equal(t, []string{"b.y"}, missing)
	assert.Equal(t, []string{"c.z"}, stale)
}

func TestValidateAgainstWorkflow_NilLockfile(t *testing.T) {
	graph := &workflow.FSMGraph{
		Adapters: map[string]*workflow.AdapterNode{
			"a.x": {Type: "a", Name: "x"},
		},
	}
	missing, stale := lockfile.ValidateAgainstWorkflow(nil, graph)
	assert.Empty(t, missing)
	assert.Empty(t, stale)
}

func TestValidateAgainstWorkflow_NilGraph(t *testing.T) {
	lf := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{Type: "a", Name: "x"},
		},
	}
	missing, stale := lockfile.ValidateAgainstWorkflow(lf, nil)
	assert.Empty(t, missing)
	assert.Empty(t, stale)
}

func TestValidateAgainstWorkflow_SortedOutput(t *testing.T) {
	lf := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{Type: "z", Name: "last"},
			{Type: "a", Name: "first"},
		},
	}
	graph := &workflow.FSMGraph{
		Adapters: map[string]*workflow.AdapterNode{},
	}
	_, stale := lockfile.ValidateAgainstWorkflow(lf, graph)
	assert.Equal(t, []string{"a.first", "z.last"}, stale)
}
