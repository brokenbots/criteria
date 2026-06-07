//go:build linux

package sandbox

import (
	"strconv"
	"strings"
	"time"

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
