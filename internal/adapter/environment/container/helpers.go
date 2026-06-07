package container

import (
	"fmt"
	"strings"

	"github.com/zclconf/go-cty/cty"
)

// ctyString extracts a string from a cty.Value. Returns the empty string
// if the value is null or unknown.
func ctyString(v cty.Value) string {
	if v.IsNull() || !v.IsKnown() {
		return ""
	}
	if v.Type() == cty.String {
		return v.AsString()
	}
	return ""
}

// ctyStringList extracts a slice of strings from a cty.Value that is
// expected to be a list/tuple/set of strings. Returns nil if the value
// is null, unknown, or not a collection.
func ctyStringList(v cty.Value) []string {
	if v.IsNull() || !v.IsKnown() {
		return nil
	}
	var out []string
	if v.Type().IsListType() || v.Type().IsTupleType() || v.Type().IsSetType() {
		it := v.ElementIterator()
		for it.Next() {
			_, ev := it.Element()
			if ev.Type() == cty.String && ev.IsKnown() && !ev.IsNull() {
				out = append(out, ev.AsString())
			}
		}
	}
	return out
}

// stringFromObject extracts a string from a nested object field.
func stringFromObject(obj cty.Value, field string) string {
	if obj.IsNull() || !obj.IsKnown() || !obj.Type().IsObjectType() {
		return ""
	}
	if !obj.Type().HasAttribute(field) {
		return ""
	}
	return ctyString(obj.GetAttr(field))
}

// stringListFromObject extracts a string list from a nested object field.
func stringListFromObject(obj cty.Value, field string) []string {
	if obj.IsNull() || !obj.IsKnown() || !obj.Type().IsObjectType() {
		return nil
	}
	if !obj.Type().HasAttribute(field) {
		return nil
	}
	return ctyStringList(obj.GetAttr(field))
}

// validatePath ensures a path is absolute and does not contain "..".
// Returns an error if the path is invalid.
func validatePath(p string) error {
	if p == "" {
		return fmt.Errorf("path is empty")
	}
	if !strings.HasPrefix(p, "/") {
		return fmt.Errorf("path %q is not absolute", p)
	}
	parts := strings.Split(p, "/")
	for _, part := range parts {
		if part == ".." {
			return fmt.Errorf("path %q contains parent-directory traversal", p)
		}
	}
	return nil
}

// makeNetworkName sanitizes an adapter reference into a valid Docker network
// name. Docker network names must be alphanumeric plus hyphens/underscores.
func makeNetworkName(adapterRef string) string {
	// Replace common separator characters with hyphens.
	name := strings.ReplaceAll(adapterRef, ".", "-")
	name = strings.ReplaceAll(name, "/", "-")
	name = strings.ReplaceAll(name, ":", "-")
	name = strings.ReplaceAll(name, "_", "-")
	name = "criteria-net-" + name
	// Docker network names must be 2–64 chars.
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}
