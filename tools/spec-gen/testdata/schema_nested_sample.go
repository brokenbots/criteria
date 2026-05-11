// Package testdata provides nested-block schema types for spec-gen unit tests.
// This file exercises BFS extraction of nested block types.
package testdata

// Spec is the root schema with a nested block hierarchy.
type Spec struct {
	Containers []*ContainerSpec `hcl:"container,block"`
}

// ContainerSpec defines a container block that holds items.
type ContainerSpec struct {
	// Name labels this container instance.
	Name  string      `hcl:"name,label"`
	// Count is the maximum number of items.
	Count int         `hcl:"count,attr"`
	Items []*ItemSpec `hcl:"item,block"`
}

// ItemSpec defines an item nested inside a container.
type ItemSpec struct {
	// Key uniquely identifies this item.
	Key   string `hcl:"key,label"`
	// Value is the item's string payload.
	Value string `hcl:"value,attr"`
}
