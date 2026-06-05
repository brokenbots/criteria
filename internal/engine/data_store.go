package engine

// data_store.go — runtime store for data block values.
// DataStore is a workflow-scoped, engine-managed (kind, name) → value store.
// Only kind = "internal" is mutable at runtime; other kinds are read-only.
// It is created fresh per workflow (and per subworkflow body) and is safe for
// concurrent use.

import (
	"fmt"
	"sync"

	"github.com/hashicorp/hcl/v2/ext/typeexpr"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"

	"github.com/brokenbots/criteria/workflow"
)

// DataStore holds the runtime state for all data blocks in a workflow graph.
// A single mutex guards the entire store; per-variable locking is not needed
// at v0.3.0 scale (simplicity wins).
type DataStore struct {
	mu     sync.Mutex
	values map[string]map[string]cty.Value
	types  map[string]map[string]cty.Type
}

// NewDataStore creates a DataStore pre-populated with initial values from the
// FSMGraph's Data. If a data block has no initial value declared, the store
// entry is initialised to cty.NullVal of the declared type.
func NewDataStore(g *workflow.FSMGraph) *DataStore {
	s := &DataStore{
		values: make(map[string]map[string]cty.Value),
		types:  make(map[string]map[string]cty.Type),
	}
	for kind, byName := range g.Data {
		s.values[kind] = make(map[string]cty.Value, len(byName))
		s.types[kind] = make(map[string]cty.Type, len(byName))
		for name, node := range byName {
			s.types[kind][name] = node.Type
			s.values[kind][name] = node.InitialValue
		}
	}
	return s
}

// Get returns the current value for (kind, name). Returns an error if the
// data block is not declared.
func (s *DataStore) Get(kind, name string) (cty.Value, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	byName, ok := s.values[kind]
	if !ok {
		return cty.NilVal, fmt.Errorf("data %q %q is not declared", kind, name)
	}
	v, ok := byName[name]
	if !ok {
		return cty.NilVal, fmt.Errorf("data %q %q is not declared", kind, name)
	}
	return v, nil
}

// Set stores v under (kind, name). Returns an error if:
//   - the data block is not declared, or
//   - v's type does not match the declared type and cannot be converted to it.
//
// Conversion is attempted via go-cty's convert package when there is a type
// mismatch.
func (s *DataStore) Set(kind, name string, v cty.Value) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	byName, ok := s.types[kind]
	if !ok {
		return fmt.Errorf("data %q %q is not declared", kind, name)
	}
	want, ok := byName[name]
	if !ok {
		return fmt.Errorf("data %q %q is not declared", kind, name)
	}
	if v.Type() != want {
		converted, err := convert.Convert(v, want)
		if err != nil {
			return fmt.Errorf("data %q %q expects type %s; got %s", kind, name, want.FriendlyName(), v.Type().FriendlyName())
		}
		v = converted
	}
	s.values[kind][name] = v
	return nil
}

// DataWrite represents a single write operation to be applied atomically.
type DataWrite struct {
	Kind  string
	Name  string
	Value cty.Value
}

// SetBatch atomically applies all writes. The entire write set is validated
// and committed under a single mutex lock, so readers cannot observe a
// partially-applied write set. Returns an error if any entry has an undeclared
// name or a type mismatch that cannot be resolved via conversion; no writes are
// committed on error.
func (s *DataStore) SetBatch(writes []DataWrite) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Validate and coerce all entries before committing any.
	coerced := make([]DataWrite, 0, len(writes))
	for _, w := range writes {
		byName, ok := s.types[w.Kind]
		if !ok {
			return fmt.Errorf("data %q %q is not declared", w.Kind, w.Name)
		}
		want, ok := byName[w.Name]
		if !ok {
			return fmt.Errorf("data %q %q is not declared", w.Kind, w.Name)
		}
		v := w.Value
		if v.Type() != want {
			converted, err := convert.Convert(v, want)
			if err != nil {
				return fmt.Errorf("data %q %q expects type %s; got %s", w.Kind, w.Name, want.FriendlyName(), v.Type().FriendlyName())
			}
			v = converted
		}
		coerced = append(coerced, DataWrite{Kind: w.Kind, Name: w.Name, Value: v})
	}
	for _, w := range coerced {
		s.values[w.Kind][w.Name] = w.Value
	}
	return nil
}

// Snapshot returns a shallow copy of the current values as a nested cty object
// suitable for injection into the HCL eval context:
//
//	data = { internal = { name1 = { value = ..., type = "..." }, ... } }
//
// The returned map is safe for use in eval-context construction without holding
// the lock.
func (s *DataStore) Snapshot() map[string]cty.Value {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := make(map[string]cty.Value, len(s.values))
	for kind, byName := range s.values {
		entries := make(map[string]cty.Value, len(byName))
		for name, v := range byName {
			ty := s.types[kind][name]
			entries[name] = cty.ObjectVal(map[string]cty.Value{
				"value": v,
				"type":  cty.StringVal(typeexpr.TypeString(ty)),
			})
		}
		snap[kind] = cty.ObjectVal(entries)
	}
	return snap
}

// TypeOf returns the declared cty.Type for (kind, name). Returns (cty.NilType,
// false) when the data block is not declared.
func (s *DataStore) TypeOf(kind, name string) (cty.Type, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	byName, ok := s.types[kind]
	if !ok {
		return cty.NilType, false
	}
	t, ok := byName[name]
	return t, ok
}

// coerceStringToCty converts a raw adapter string output to the given cty type.
// It delegates to workflow.CoerceStringToCty, the shared coercion primitive, so
// data writes and typed-output storage stay consistent. (The primitive lives in
// the workflow package because engine depends on workflow, not the reverse.)
func coerceStringToCty(s string, t cty.Type) (cty.Value, error) {
	return workflow.CoerceStringToCty(s, t)
}
