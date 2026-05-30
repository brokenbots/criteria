package container

import (
	"fmt"
	"strconv"
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

// parseMemoryLimit parses a human-readable memory limit string such as
// "512M", "1Gi", "128K" and returns the value in bytes. Returns 0 for
// empty/invalid strings.
func parseMemoryLimit(s string) uint64 {
	if s == "" {
		return 0
	}
	s = strings.TrimSpace(s)
	multiplier := uint64(1)
	switch {
	case strings.HasSuffix(s, "Gi") || strings.HasSuffix(s, "gi"):
		multiplier = 1024 * 1024 * 1024
		s = s[:len(s)-2]
	case strings.HasSuffix(s, "G") || strings.HasSuffix(s, "g"):
		multiplier = 1024 * 1024 * 1024
		s = s[:len(s)-1]
	case strings.HasSuffix(s, "Mi") || strings.HasSuffix(s, "mi"):
		multiplier = 1024 * 1024
		s = s[:len(s)-2]
	case strings.HasSuffix(s, "M") || strings.HasSuffix(s, "m"):
		multiplier = 1024 * 1024
		s = s[:len(s)-1]
	case strings.HasSuffix(s, "Ki") || strings.HasSuffix(s, "ki"):
		multiplier = 1024
		s = s[:len(s)-2]
	case strings.HasSuffix(s, "K") || strings.HasSuffix(s, "k"):
		multiplier = 1024
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return n * multiplier
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
