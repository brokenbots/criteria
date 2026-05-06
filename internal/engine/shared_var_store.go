package engine

// shared_var_store.go — runtime store for shared_variable values.
// SharedVarStore is a workflow-scoped, engine-managed key/value store for
// shared_variable blocks. It is created fresh per workflow (and per subworkflow
// body) and is safe for concurrent use.

import (
	"fmt"
	"sync"

	"github.com/zclconf/go-cty/cty"

	"github.com/brokenbots/criteria/workflow"
)

// SharedVarStore holds the runtime state for all shared_variable blocks in a
// workflow graph. A single mutex guards the entire store; per-variable locking
// is not needed at v0.3.0 scale (simplicity wins).
type SharedVarStore struct {
	mu     sync.Mutex
	values map[string]cty.Value
	types  map[string]cty.Type
}

// NewSharedVarStore creates a SharedVarStore pre-populated with initial values
// from the FSMGraph's SharedVariables. If a shared_variable has no initial
// value declared, the store entry is initialised to cty.NullVal of the declared
// type (matching the compiled node's InitialValue field).
func NewSharedVarStore(g *workflow.FSMGraph) *SharedVarStore {
	s := &SharedVarStore{
		values: make(map[string]cty.Value, len(g.SharedVariables)),
		types:  make(map[string]cty.Type, len(g.SharedVariables)),
	}
	for name, node := range g.SharedVariables {
		s.types[name] = node.Type
		s.values[name] = node.InitialValue
	}
	return s
}

// Get returns the current value for name. Returns an error if name is not a
// declared shared_variable.
func (s *SharedVarStore) Get(name string) (cty.Value, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.values[name]
	if !ok {
		return cty.NilVal, fmt.Errorf("shared_variable %q is not declared", name)
	}
	return v, nil
}

// Set stores v under name. Returns an error if:
//   - name is not a declared shared_variable, or
//   - v's type does not match the declared type.
func (s *SharedVarStore) Set(name string, v cty.Value) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	want, ok := s.types[name]
	if !ok {
		return fmt.Errorf("shared_variable %q is not declared", name)
	}
	if v.Type() != want {
		return fmt.Errorf("shared_variable %q expects type %s; got %s", name, want.FriendlyName(), v.Type().FriendlyName())
	}
	s.values[name] = v
	return nil
}

// SetBatch atomically applies all writes in the map. The entire write set is
// validated and committed under a single mutex lock, so readers cannot observe
// a partially-applied write set. Returns an error if any entry has an undeclared
// name or a type mismatch; no writes are committed on error.
func (s *SharedVarStore) SetBatch(writes map[string]cty.Value) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Validate all entries before committing any.
	for name, v := range writes {
		want, ok := s.types[name]
		if !ok {
			return fmt.Errorf("shared_variable %q is not declared", name)
		}
		if v.Type() != want {
			return fmt.Errorf("shared_variable %q expects type %s; got %s", name, want.FriendlyName(), v.Type().FriendlyName())
		}
	}
	for name, v := range writes {
		s.values[name] = v
	}
	return nil
}

// Snapshot returns a shallow copy of the current values map. The returned map
// is safe for use in eval-context construction without holding the lock.
func (s *SharedVarStore) Snapshot() map[string]cty.Value {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := make(map[string]cty.Value, len(s.values))
	for k, v := range s.values {
		snap[k] = v
	}
	return snap
}

// TypeOf returns the declared cty.Type for name. Returns (cty.NilType, false)
// when name is not a declared shared_variable.
func (s *SharedVarStore) TypeOf(name string) (cty.Type, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.types[name]
	return t, ok
}

// coerceStringToCty converts a raw adapter string output to the given cty type.
// Supports string, number, and bool. Returns an error if conversion fails.
func coerceStringToCty(s string, t cty.Type) (cty.Value, error) {
	switch t {
	case cty.String:
		return cty.StringVal(s), nil
	case cty.Number:
		var f float64
		if _, err := fmt.Sscanf(s, "%g", &f); err != nil {
			return cty.NilVal, fmt.Errorf("cannot coerce %q to type number: %w", s, err)
		}
		return cty.NumberFloatVal(f), nil
	case cty.Bool:
		switch s {
		case "true", "1":
			return cty.BoolVal(true), nil
		case "false", "0":
			return cty.BoolVal(false), nil
		default:
			return cty.NilVal, fmt.Errorf("cannot coerce %q to type bool: expected true/false/1/0", s)
		}
	default:
		return cty.NilVal, fmt.Errorf("unsupported type %s for string coercion", t.FriendlyName())
	}
}
