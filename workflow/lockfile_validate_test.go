package workflow

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/brokenbots/criteria/workflow/lockfile"
)

func TestValidateLockfileAgainstGraph_AllMatch(t *testing.T) {
	lf := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{Type: "a", Name: "x"},
			{Type: "b", Name: "y"},
		},
	}
	graph := &FSMGraph{
		Adapters: map[string]*AdapterNode{
			"a.x": {Type: "a", Name: "x"},
			"b.y": {Type: "b", Name: "y"},
		},
	}
	missing, stale := ValidateLockfileAgainstGraph(lf, graph)
	assert.Empty(t, missing)
	assert.Empty(t, stale)
}

func TestValidateLockfileAgainstGraph_Missing(t *testing.T) {
	lf := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{Type: "a", Name: "x"},
		},
	}
	graph := &FSMGraph{
		Adapters: map[string]*AdapterNode{
			"a.x": {Type: "a", Name: "x"},
			"b.y": {Type: "b", Name: "y"},
		},
	}
	missing, stale := ValidateLockfileAgainstGraph(lf, graph)
	assert.Equal(t, []string{"b.y"}, missing)
	assert.Empty(t, stale)
}

func TestValidateLockfileAgainstGraph_Stale(t *testing.T) {
	lf := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{Type: "a", Name: "x"},
			{Type: "b", Name: "y"},
		},
	}
	graph := &FSMGraph{
		Adapters: map[string]*AdapterNode{
			"a.x": {Type: "a", Name: "x"},
		},
	}
	missing, stale := ValidateLockfileAgainstGraph(lf, graph)
	assert.Empty(t, missing)
	assert.Equal(t, []string{"b.y"}, stale)
}

func TestValidateLockfileAgainstGraph_MissingAndStale(t *testing.T) {
	lf := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{Type: "a", Name: "x"},
			{Type: "c", Name: "z"},
		},
	}
	graph := &FSMGraph{
		Adapters: map[string]*AdapterNode{
			"a.x": {Type: "a", Name: "x"},
			"b.y": {Type: "b", Name: "y"},
		},
	}
	missing, stale := ValidateLockfileAgainstGraph(lf, graph)
	assert.Equal(t, []string{"b.y"}, missing)
	assert.Equal(t, []string{"c.z"}, stale)
}

func TestValidateLockfileAgainstGraph_NilLockfile(t *testing.T) {
	graph := &FSMGraph{
		Adapters: map[string]*AdapterNode{
			"a.x": {Type: "a", Name: "x"},
		},
	}
	missing, stale := ValidateLockfileAgainstGraph(nil, graph)
	assert.Empty(t, missing)
	assert.Empty(t, stale)
}

func TestValidateLockfileAgainstGraph_NilGraph(t *testing.T) {
	lf := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{Type: "a", Name: "x"},
		},
	}
	missing, stale := ValidateLockfileAgainstGraph(lf, nil)
	assert.Empty(t, missing)
	assert.Empty(t, stale)
}

func TestValidateLockfileAgainstGraph_SortedOutput(t *testing.T) {
	lf := &lockfile.Lockfile{
		Adapters: []lockfile.LockedAdapter{
			{Type: "z", Name: "last"},
			{Type: "a", Name: "first"},
		},
	}
	graph := &FSMGraph{
		Adapters: map[string]*AdapterNode{},
	}
	_, stale := ValidateLockfileAgainstGraph(lf, graph)
	assert.Equal(t, []string{"a.first", "z.last"}, stale)
}
