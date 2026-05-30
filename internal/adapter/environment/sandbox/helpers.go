package sandbox

import (
	"fmt"
	"strconv"
	"strings"
	"time"

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

// parseMemoryLimit parses a human-readable memory limit string such as
// "512M", "1G", "128K" and returns the value in bytes. Returns 0 for
// empty/invalid strings.
func parseMemoryLimit(s string) uint64 {
	if s == "" {
		return 0
	}
	s = strings.TrimSpace(s)
	multiplier := uint64(1)
	switch {
	case strings.HasSuffix(s, "G") || strings.HasSuffix(s, "g"):
		multiplier = 1024 * 1024 * 1024
		s = s[:len(s)-1]
	case strings.HasSuffix(s, "M") || strings.HasSuffix(s, "m"):
		multiplier = 1024 * 1024
		s = s[:len(s)-1]
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

// parseCPULimit parses a CPU limit string such as "1" or "0.5" and
// returns the value as a float64. Returns 0 for empty/invalid strings.
func parseCPULimit(s string) float64 {
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return f
}

// parseTimeout parses a duration string. Returns 0 for empty/invalid strings.
func parseTimeout(s string) time.Duration {
	if s == "" {
		return 0
	}
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return d
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

// splitHostPort splits a network endpoint string "host:port" into host and
// port. Returns empty strings if the input is malformed.
func splitHostPort(s string) (host, port string) {
	idx := strings.LastIndex(s, ":")
	if idx < 0 {
		return "", ""
	}
	host = s[:idx]
	port = s[idx+1:]
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	return host, port
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
// environment slice. Safe for use on both Linux and Darwin.
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
		if blocked[name] {
			continue
		}
		out = append(out, e)
	}
	return out
}
