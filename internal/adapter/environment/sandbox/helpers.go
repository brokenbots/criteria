package sandbox

import (
	"fmt"
	"strings"

	"github.com/zclconf/go-cty/cty"
)

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

// ctyBool extracts a bool from a cty.Value. Returns the fallback if
// the value is null, unknown, or not a bool.
func ctyBool(v cty.Value, fallback bool) bool {
	if v.IsNull() || !v.IsKnown() {
		return fallback
	}
	if v.Type() == cty.Bool {
		return v.True()
	}
	return fallback
}

// pathListFromObject extracts a string list from a nested object field.
// obj is expected to be a cty.Object; field is the key inside that object.
func pathListFromObject(obj cty.Value, field string) []string {
	if obj.IsNull() || !obj.IsKnown() || !obj.Type().IsObjectType() {
		return nil
	}
	if !obj.Type().HasAttribute(field) {
		return nil
	}
	return ctyStringList(obj.GetAttr(field))
}

// boolFromObject extracts a bool from a nested object field.
func boolFromObject(obj cty.Value, field string, fallback bool) bool {
	if obj.IsNull() || !obj.IsKnown() || !obj.Type().IsObjectType() {
		return fallback
	}
	if !obj.Type().HasAttribute(field) {
		return fallback
	}
	return ctyBool(obj.GetAttr(field), fallback)
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

// scrubEnv removes sensitive or privilege-escalating variables from the
// environment slice. Safe for use on both Linux and Darwin. It drops a fixed
// set of privilege-escalation variables (SUDO_*, CRITERIA_PLUGIN) and any
// variable whose name looks like it carries a secret (see looksLikeSecret), so
// the sandboxed adapter does not inherit host credentials it was not granted.
func scrubEnv(env []string) []string {
	blocked := map[string]bool{
		"SUDO_UID":        true,
		"SUDO_GID":        true,
		"SUDO_USER":       true,
		"SUDO_COMMAND":    true,
		"SUDO_EDITOR":     true,
		"CRITERIA_PLUGIN": true,
	}
	out := make([]string, 0, len(env))
	for _, e := range env {
		name, _, _ := strings.Cut(e, "=")
		if blocked[name] || looksLikeSecret(name) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// looksLikeSecret reports whether an environment variable name suggests it
// carries a credential and should not be inherited by a sandboxed adapter.
func looksLikeSecret(name string) bool {
	upper := strings.ToUpper(name)
	for _, marker := range []string{
		"SECRET", "TOKEN", "PASSWORD", "PASSWD",
		"APIKEY", "API_KEY", "ACCESS_KEY", "PRIVATE_KEY", "CREDENTIAL",
	} {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return false
}
